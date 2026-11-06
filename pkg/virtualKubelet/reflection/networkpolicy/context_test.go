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

package networkpolicy

import (
	"context"
	"net/netip"
	"testing"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ipamapi "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	netapi "github.com/liqotech/liqo/apis/networking/v1beta1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	liqofake "github.com/liqotech/liqo/pkg/client/clientset/versioned/fake"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/options"
)

func TestExportsUseLiveIPAllocations(t *testing.T) {
	allocation := &ipamapi.IP{
		ObjectMeta: metav1.ObjectMeta{Namespace: "services", Name: "alloc", UID: "allocation-uid"},
		Spec:       ipamapi.IPSpec{IP: "172.26.4.7"},
		Status:     ipamapi.IPStatus{IP: "10.70.0.42", CIDR: "10.70.0.0/16"},
	}
	for _, tt := range []struct {
		name             string
		mutate           func(*ipamapi.IP)
		exports, pending int
	}{
		{name: "allocated without Service or EndpointSlice", exports: 1},
		{name: "missing allocated IP", mutate: func(ip *ipamapi.IP) { ip.Status.IP = "" }, pending: 1},
		{name: "missing allocation CIDR", mutate: func(ip *ipamapi.IP) { ip.Status.CIDR = "" }, pending: 1},
		{name: "outside allocation CIDR", mutate: func(ip *ipamapi.IP) { ip.Status.IP = "10.71.0.42" }, pending: 1},
		{name: "missing identity", mutate: func(ip *ipamapi.IP) { ip.UID = "" }, pending: 1},
		{name: "unreachable", mutate: func(ip *ipamapi.IP) {
			ip.Labels = map[string]string{consts.IPHostUnreachableKey: consts.IPHostUnreachableValue}
		}, pending: 1},
		{name: "deleting", mutate: func(ip *ipamapi.IP) { now := metav1.Now(); ip.DeletionTimestamp = &now }},
		{name: "consumer local", mutate: func(ip *ipamapi.IP) { ip.Spec.IP = "10.20.0.7" }},
		{name: "destination provider", mutate: func(ip *ipamapi.IP) { ip.Spec.IP = "172.28.0.7" }},
	} {
		t.Run(tt.name, func(t *testing.T) {
			ip := allocation.DeepCopy()
			if tt.mutate != nil {
				tt.mutate(ip)
			}
			opts := &options.NamespacedOpts{LocalNamespace: "services", LocalLiqoClient: liqofake.NewSimpleClientset(ip)}
			// No Kubernetes or remote client is needed to discover exports.
			r := &Reflector{}
			var out api.PolicySourceContext
			local := []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16"), netip.MustParsePrefix("172.28.0.0/16")}
			if err := r.exports(context.Background(), opts, local, &out); err != nil {
				t.Fatal(err)
			}
			if len(out.Exports) != tt.exports || len(out.PendingExports) != tt.pending {
				t.Fatalf("unexpected context: %+v", out)
			}
			if tt.exports != 0 && out.Exports[0] != (api.PolicyEndpointExport{Original: "172.26.4.7", Allocated: "10.70.0.42", AllocationUID: "allocation-uid"}) {
				t.Fatalf("incorrect allocation: %+v", out.Exports)
			}
		})
	}
}

func TestExportAllocationDeletion(t *testing.T) {
	ctx := context.Background()
	var objects []runtime.Object
	for _, entry := range []struct{ name, original, allocated string }{
		{"first", "172.26.4.7", "10.70.0.42"},
		{"second", "172.26.4.8", "10.70.0.43"},
	} {
		objects = append(objects, &ipamapi.IP{
			ObjectMeta: metav1.ObjectMeta{Namespace: "services", Name: entry.name, UID: "allocation-uid"},
			Spec:       ipamapi.IPSpec{IP: netapi.IP(entry.original)},
			Status:     ipamapi.IPStatus{IP: netapi.IP(entry.allocated), CIDR: "10.70.0.0/16"},
		})
	}
	cl := liqofake.NewSimpleClientset(objects...)
	opts := &options.NamespacedOpts{LocalNamespace: "services", LocalLiqoClient: cl}
	r := &Reflector{}
	var before api.PolicySourceContext
	if err := r.exports(ctx, opts, nil, &before); err != nil {
		t.Fatal(err)
	}
	if len(before.Exports) != 2 {
		t.Fatalf("expected both allocations: %+v", before)
	}
	if err := cl.IpamV1alpha1().IPs("services").Delete(ctx, "first", metav1.DeleteOptions{}); err != nil {
		t.Fatal(err)
	}
	var after api.PolicySourceContext
	if err := r.exports(ctx, opts, nil, &after); err != nil {
		t.Fatal(err)
	}
	if len(after.Exports) != 1 || after.Exports[0].Original != "172.26.4.8" {
		t.Fatalf("deleted allocation still exported: %+v", after)
	}
}

func TestSourceContextUsesAllocationsAcrossNamespaces(t *testing.T) {
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{ipamapi.AddToScheme, netapi.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cfg := &netapi.Configuration{ObjectMeta: metav1.ObjectMeta{Namespace: "liqo", Name: "provider", Labels: map[string]string{consts.RemoteClusterID: "provider"}}}
	cfg.Spec.Remote.CIDR.Pod = []netapi.CIDR{"10.80.0.0/16"}
	cfg.Status.Remote = &netapi.ClusterConfig{CIDR: netapi.ClusterConfigCIDR{Pod: []netapi.CIDR{"172.28.0.0/16"}}}
	service := &ipamapi.Network{ObjectMeta: metav1.ObjectMeta{Namespace: "liqo", Name: "services", Labels: map[string]string{consts.NetworkTypeLabelKey: string(consts.NetworkTypeServiceCIDR)}}}
	service.Status.CIDR = "10.96.0.0/16"
	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, service).Build()
	var objects []runtime.Object
	for _, entry := range []struct{ namespace, name, original string }{
		{"apps", "local", "10.20.0.7"},
		{"apps", "provider", "172.28.0.7"},
		{"services", "indirect", "172.26.4.7"},
	} {
		objects = append(objects, &ipamapi.IP{
			ObjectMeta: metav1.ObjectMeta{Namespace: entry.namespace, Name: entry.name, UID: "allocation-uid"},
			Spec:       ipamapi.IPSpec{IP: netapi.IP(entry.original)},
			Status:     ipamapi.IPStatus{IP: "10.70.0.42", CIDR: "10.70.0.0/16"},
		})
	}
	cl := liqofake.NewSimpleClientset(objects...)
	r := New(local, nil, "consumer", "provider", "liqo", []string{"10.20.0.0/16"}, 3)
	out, err := r.sourceContext(context.Background(), []*options.NamespacedOpts{
		{LocalNamespace: "apps", LocalLiqoClient: cl},
		{LocalNamespace: "services", LocalLiqoClient: cl},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(out.Exports) != 1 || out.Exports[0].Original != "172.26.4.7" {
		t.Fatalf("expected only the indirect allocation from the other namespace: %+v", out)
	}
	if len(out.LocalProviderPairs) != 1 || out.LocalProviderPairs[0] != (api.PolicyPrefixPair{Source: "172.28.0.0/16", Destination: "10.80.0.0/16"}) {
		t.Fatalf("lost provider pod mapping: %+v", out.LocalProviderPairs)
	}
}
