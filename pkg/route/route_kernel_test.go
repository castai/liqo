//go:build linux

// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package route

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
)

const (
	// RFC 2544 benchmark ranges: guaranteed not to collide with real traffic.
	testRouteChangedDst  = "198.18.0.0/25"
	testRouteRemovedDst  = "198.18.1.0/25"
	testRuleOldDst       = "198.18.2.0/25"
	testRuleNewDst       = "198.18.2.0/24"
	testLinkA            = "liqotesta0"
	testLinkB            = "liqotestb0"
	testRouteRulePrio    = 32001
	testTableMarkerRange = "198.18.0.0/15"
)

// testEnvironment holds the kernel fixtures shared by the reconciler tests:
// two dummy links and a routing table identified by a unique name.
type testEnvironment struct {
	tableName string
	tableID   uint32
	linkA     netlink.Link
	linkB     netlink.Link
}

func newTestEnvironment(t *testing.T) *testEnvironment {
	t.Helper()

	if os.Geteuid() != 0 {
		t.Skip("test requires root privileges to manipulate network namespaces")
	}

	tableName := fmt.Sprintf("liqotest-%d", time.Now().UnixNano())
	tableID, err := GetTableID(tableName)
	if err != nil {
		t.Fatalf("computing table ID: %v", err)
	}

	linkA := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: testLinkA}}
	linkB := &netlink.Dummy{LinkAttrs: netlink.LinkAttrs{Name: testLinkB}}
	for _, link := range []netlink.Link{linkA, linkB} {
		if err := netlink.LinkAdd(link); err != nil {
			t.Fatalf("adding dummy link %s: %v", link.Attrs().Name, err)
		}
		if err := netlink.LinkSetUp(link); err != nil {
			t.Fatalf("setting link %s up: %v", link.Attrs().Name, err)
		}
	}

	env := &testEnvironment{tableName: tableName, tableID: tableID, linkA: linkA, linkB: linkB}

	t.Cleanup(func() {
		// Best-effort cleanup of the kernel state and the rt_tables entry.
		_ = FlushRulesByTableID(tableID)
		_ = FlushRoutesByTableID(tableID)
		_ = EnsureTableAbsence(tableID)
		_ = netlink.LinkDel(linkA)
		_ = netlink.LinkDel(linkB)
	})

	return env
}

// addRoute installs a link-scoped route in the test table through the given link.
func (e *testEnvironment) addRoute(t *testing.T, dst string, link netlink.Link) {
	t.Helper()
	route := &netlink.Route{
		Dst:       mustIPNet(dst),
		Table:     int(e.tableID),
		LinkIndex: link.Attrs().Index,
		Scope:     netlink.SCOPE_LINK,
	}
	if err := netlink.RouteAdd(route); err != nil {
		t.Fatalf("seeding route %s: %v", dst, err)
	}
}

func (e *testEnvironment) routes() ([]netlink.Route, error) {
	return GetRoutesByTableID(e.tableID)
}

func (e *testEnvironment) rules() ([]netlink.Rule, error) {
	return GetRulesByTableID(e.tableID)
}

// forgeRouteConfiguration builds a RouteConfiguration with a single rule
// routing all destinations through the given device.
func forgeRouteConfiguration(name, tableName, dev string, dsts ...string) *networkingv1beta1.RouteConfiguration {
	routes := make([]networkingv1beta1.Route, 0, len(dsts))
	for _, dst := range dsts {
		routes = append(routes, networkingv1beta1.Route{
			Dst:   ptr.To(networkingv1beta1.CIDR(dst)),
			Dev:   ptr.To(dev),
			Scope: ptr.To(networkingv1beta1.LinkScope),
		})
	}
	return &networkingv1beta1.RouteConfiguration{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "default"},
		Spec: networkingv1beta1.RouteConfigurationSpec{
			Table: networkingv1beta1.Table{
				Name: tableName,
				Rules: []networkingv1beta1.Rule{
					{
						Priority: ptr.To(testRouteRulePrio),
						Routes:   routes,
					},
				},
			},
		},
	}
}

// reconcile drives the RouteConfigurationReconciler against the given object.
func runReconcile(t *testing.T, rc *networkingv1beta1.RouteConfiguration) {
	t.Helper()

	scheme := runtime.NewScheme()
	utilruntime.Must(networkingv1beta1.AddToScheme(scheme))

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(rc).Build()
	reconciler, err := NewRouteConfigurationReconcilerWithoutFinalizer(
		cl, scheme, "testpod", record.NewFakeRecorder(16), nil)
	if err != nil {
		t.Fatalf("creating reconciler: %v", err)
	}

	res, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Name: rc.Name, Namespace: rc.Namespace},
	})
	if err != nil {
		t.Fatalf("reconcile returned error: %v", err)
	}
	if res.RequeueAfter != 0 {
		t.Fatalf("reconcile requested requeue: %+v", res)
	}
}

type routeEvents struct {
	added   map[string]bool
	deleted map[string]bool
}

func collectRouteEvents(t *testing.T, tableID uint32, run func()) routeEvents {
	t.Helper()

	ch := make(chan netlink.RouteUpdate, 128)
	done := make(chan struct{})
	if err := netlink.RouteSubscribe(ch, done); err != nil {
		t.Fatalf("subscribing to route notifications: %v", err)
	}

	events := routeEvents{added: map[string]bool{}, deleted: map[string]bool{}}
	collected := make(chan struct{})
	go func() {
		defer close(collected)
		for update := range ch {
			if update.Table != int(tableID) {
				continue
			}
			if update.Dst == nil {
				continue
			}
			switch update.Type {
			case unix.RTM_NEWROUTE:
				events.added[update.Dst.String()] = true
			case unix.RTM_DELROUTE:
				events.deleted[update.Dst.String()] = true
			}
		}
	}()

	run()

	// Give the kernel a moment to flush the already-queued notifications,
	// then close the subscription and wait for the collector goroutine.
	time.Sleep(500 * time.Millisecond)
	close(done)
	<-collected
	return events
}

func TestReconcileMakeBeforeBreak(t *testing.T) {
	env := newTestEnvironment(t)

	// Seed the "old" state: the changed route through link A, plus an
	// undesired route that the reconcile must remove.
	env.addRoute(t, testRouteChangedDst, env.linkA)
	env.addRoute(t, testRouteRemovedDst, env.linkA)

	rc := forgeRouteConfiguration("test-mbb", env.tableName, testLinkB, testRouteChangedDst)

	events := collectRouteEvents(t, env.tableID, func() {
		runReconcile(t, rc)
	})
	t.Logf("captured events: added=%v deleted=%v", events.added, events.deleted)

	// The changed route must have been replaced, never deleted.
	if events.deleted[testRouteChangedDst] {
		t.Errorf("route %s was deleted during the reconcile (break-before-make); "+
			"changed entries must be atomically replaced", testRouteChangedDst)
	}
	if !events.added[testRouteChangedDst] {
		t.Errorf("route %s was not (re)installed during the reconcile", testRouteChangedDst)
	}
	// The undesired route must have been removed.
	if !events.deleted[testRouteRemovedDst] {
		t.Errorf("stale route %s was not deleted during the reconcile", testRouteRemovedDst)
	}

	// Final state: the changed route egresses the new link, the stale one is gone.
	routes, err := env.routes()
	if err != nil {
		t.Fatalf("listing routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected exactly one route after the reconcile, got %d: %v", len(routes), routes)
	}
	if routes[0].Dst.String() != testRouteChangedDst {
		t.Errorf("unexpected surviving route %v, expected %s", routes[0], testRouteChangedDst)
	}
	if routes[0].LinkIndex != env.linkB.Attrs().Index {
		t.Errorf("route %s still egresses link index %d, expected %d (link B)",
			testRouteChangedDst, routes[0].LinkIndex, env.linkB.Attrs().Index)
	}
}

func TestReconcileRemoveStaleRoutes(t *testing.T) {
	env := newTestEnvironment(t)

	env.addRoute(t, testRouteChangedDst, env.linkA)
	env.addRoute(t, testRouteRemovedDst, env.linkA)

	// The spec no longer contains the removed destination.
	rc := forgeRouteConfiguration("test-remove", env.tableName, testLinkA, testRouteChangedDst)
	runReconcile(t, rc)

	routes, err := env.routes()
	if err != nil {
		t.Fatalf("listing routes: %v", err)
	}
	if len(routes) != 1 || routes[0].Dst.String() != testRouteChangedDst {
		t.Fatalf("expected only route %s to survive, got %v", testRouteChangedDst, routes)
	}
}

func TestReconcileReplaceStaleRule(t *testing.T) {
	env := newTestEnvironment(t)

	// Seed the old rule: narrower destination than the desired one.
	oldRule := &networkingv1beta1.Rule{
		Dst:      mustCIDR(testRuleOldDst),
		Priority: ptr.To(testRouteRulePrio),
	}
	if err := AddRule(oldRule, env.tableID); err != nil {
		t.Fatalf("seeding old rule: %v", err)
	}

	// Desired rule: same priority, wider destination, routing through link A.
	rc := forgeRouteConfiguration("test-rule", env.tableName, testLinkA, testTableMarkerRange)
	rc.Spec.Table.Rules[0].Dst = mustCIDR(testRuleNewDst)
	runReconcile(t, rc)

	rules, err := env.rules()
	if err != nil {
		t.Fatalf("listing rules: %v", err)
	}

	var hasNew, hasOld bool
	for i := range rules {
		if rules[i].Priority == testRouteRulePrio && rules[i].Dst != nil && rules[i].Dst.String() == testRuleNewDst {
			hasNew = true
		}
		if rules[i].Priority == testRouteRulePrio && rules[i].Dst != nil && rules[i].Dst.String() == testRuleOldDst {
			hasOld = true
		}
	}
	if !hasNew {
		t.Errorf("desired rule (dst %s) not installed: %v", testRuleNewDst, rules)
	}
	if hasOld {
		t.Errorf("superseded rule (dst %s) still present: %v", testRuleOldDst, rules)
	}
}

func TestReconcileIdempotent(t *testing.T) {
	env := newTestEnvironment(t)

	rc := forgeRouteConfiguration("test-idempotent", env.tableName, testLinkA, testRouteChangedDst)
	runReconcile(t, rc)

	events := collectRouteEvents(t, env.tableID, func() {
		runReconcile(t, rc)
	})
	t.Logf("captured events: added=%v deleted=%v", events.added, events.deleted)

	if len(events.added) != 0 || len(events.deleted) != 0 {
		t.Errorf("second reconcile of a converged table emitted notifications: added %v, deleted %v",
			events.added, events.deleted)
	}
}

func TestCleanRoutesFailsSafeOnUnforgeableRoute(t *testing.T) {
	env := newTestEnvironment(t)

	// Healthy kernel state: a real route through a real link.
	env.addRoute(t, testRouteChangedDst, env.linkA)
	routes, err := env.routes()
	if err != nil {
		t.Fatalf("listing routes: %v", err)
	}
	if len(routes) != 1 {
		t.Fatalf("expected 1 seeded route, got %d", len(routes))
	}

	// Desired state: one valid route plus one referencing a device that does
	// not exist (e.g. a tunnel being re-established).
	desired := []networkingv1beta1.Route{
		{Dst: ptr.To(networkingv1beta1.CIDR(testRouteChangedDst)), Gw: nil, Dev: ptr.To(testLinkA)},
		{Dst: ptr.To(networkingv1beta1.CIDR(testRouteRemovedDst)), Dev: ptr.To("liqo.does-not-exist")},
	}

	err = CleanRoutes(desired, routes)
	if err == nil {
		t.Fatal("expected CleanRoutes to fail on the unforgeable desired route")
	}

	// The healthy route must have survived the failed comparison.
	routes, err = env.routes()
	if err != nil {
		t.Fatalf("listing routes after clean: %v", err)
	}
	if len(routes) != 1 || routes[0].Dst.String() != testRouteChangedDst {
		t.Fatalf("fail-safe violated: expected route %s to survive, got %v", testRouteChangedDst, routes)
	}
}
