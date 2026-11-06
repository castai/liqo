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

package shadownetworkpolicy

import (
	"context"
	"path/filepath"
	"testing"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	netapi "github.com/liqotech/liqo/apis/networking/v1beta1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/utils/networkpolicy"
)

func TestIndirectInventoryAndRevocation(t *testing.T) {
	ctx := context.Background()
	env := envtest.Environment{CRDDirectoryPaths: []string{filepath.Join("..", "..", "..", "..", "deployments", "liqo", "charts", "liqo-crds", "crds")}, ErrorIfCRDPathMissing: true}
	cfg, err := env.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := env.Stop(); err != nil {
			t.Error(err)
		}
	})
	scheme := runtime.NewScheme()
	for _, register := range []func(*runtime.Scheme) error{kubescheme.AddToScheme, api.AddToScheme, netapi.AddToScheme} {
		if err := register(scheme); err != nil {
			t.Fatal(err)
		}
	}
	cl, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatal(err)
	}
	require := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}
	create := func(obj client.Object) { t.Helper(); require(cl.Create(ctx, obj)) }
	create(&core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "tenant"}})
	create(&core.Namespace{ObjectMeta: metav1.ObjectMeta{Name: "destination", Labels: map[string]string{consts.RemoteClusterID: "consumer"},
		Annotations: map[string]string{consts.RemoteNamespaceManagedByAnnotationKey: "tenant/mapping", consts.RemoteNamespaceOriginalNameAnnotationKey: "apps"}}})
	nm := &api.NamespaceMap{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "mapping"}, Spec: api.NamespaceMapSpec{DesiredMapping: map[string]string{"apps": "destination"}}}
	create(nm)
	nm.Status.CurrentMapping = map[string]api.RemoteNamespaceStatus{"apps": {Phase: api.MappingAccepted, RemoteNamespace: "destination"}}
	require(cl.Status().Update(ctx, nm))
	configuration := &netapi.Configuration{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "consumer", Labels: map[string]string{consts.RemoteClusterID: "consumer"}}}
	configuration.Spec.Remote.CIDR.Pod = []netapi.CIDR{"10.20.0.0/16"}
	configuration.Spec.Remote.CIDR.External = []netapi.CIDR{"10.70.0.0/16"}
	create(configuration)
	configuration.Status.Remote = &netapi.ClusterConfig{CIDR: netapi.ClusterConfigCIDR{Pod: []netapi.CIDR{"172.20.0.0/16"}, External: []netapi.CIDR{"172.31.0.0/16"}}}
	configuration.Status.Conditions = []metav1.Condition{{Type: netapi.ConfigurationConditionNetworkCIDRsConfigured, Status: metav1.ConditionTrue,
		Reason: "Configured", LastTransitionTime: metav1.Now(), ObservedGeneration: configuration.Generation}}
	require(cl.Status().Update(ctx, configuration))

	source := api.PolicySource{Cluster: "consumer", Namespace: "apps", NamespaceUID: "source-uid"}
	shadow := &api.ShadowNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Namespace: "destination", Name: "policy"}, Spec: api.ShadowNetworkPolicySpec{
		Source: source, PolicyName: "allow-one", PolicyUID: "policy-uid",
		Template: networking.NetworkPolicySpec{PolicyTypes: []networking.PolicyType{networking.PolicyTypeEgress}, Egress: []networking.NetworkPolicyEgressRule{{
			To: []networking.NetworkPolicyPeer{{IPBlock: &networking.IPBlock{CIDR: "172.26.4.7/32"}}},
		}}},
		Context: api.PolicySourceContext{IndirectCIDRs: []string{"172.26.0.0/16"}, Exports: []api.PolicyEndpointExport{{
			Original: "172.26.4.7", Allocated: "10.70.0.42", AllocationUID: "allocation-uid",
		}}},
	}}
	create(shadow)
	set := &api.ShadowNetworkPolicySet{ObjectMeta: metav1.ObjectMeta{Namespace: "destination", Name: networkpolicy.InventoryName}, Spec: api.ShadowNetworkPolicySetSpec{
		Source: source, Revision: "revision", PolicyRevision: "source-revision", Committed: true,
		Members: []api.PolicySetMember{{Name: shadow.Name, UID: string(shadow.UID), Generation: shadow.Generation}},
	}}
	create(set)
	r := &Reconciler{Client: cl, APIReader: cl, Scheme: scheme, PodCIDRs: []string{"10.80.0.0/16"}}
	reconcile := func() {
		t.Helper()
		_, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: client.ObjectKeyFromObject(set)})
		require(err)
	}
	reconcile()
	require(r.CheckReady(ctx, "destination", "consumer"))
	var policy networking.NetworkPolicy
	require(cl.Get(ctx, client.ObjectKeyFromObject(shadow), &policy))
	if len(policy.Spec.Egress) != 1 || len(policy.Spec.Egress[0].To) != 1 || policy.Spec.Egress[0].To[0].IPBlock.CIDR != "172.31.0.42/32" {
		t.Fatalf("expected the exact exported host, got %+v", policy.Spec)
	}
	// Allocation deletion is conveyed by a new source inventory, without EndpointSlices.
	require(cl.Get(ctx, client.ObjectKeyFromObject(shadow), shadow))
	shadow.Spec.Context.Exports = nil
	require(cl.Update(ctx, shadow))
	require(cl.Get(ctx, client.ObjectKeyFromObject(set), set))
	set.Spec.Revision = "allocation-removed"
	set.Spec.Members[0].Generation = shadow.Generation
	require(cl.Update(ctx, set))
	if err := r.CheckReady(ctx, "destination", "consumer"); err == nil {
		t.Fatal("stale export left startup gate open")
	}
	reconcile()
	require(cl.Get(ctx, client.ObjectKeyFromObject(shadow), &policy))
	if len(policy.Spec.Egress) != 0 || len(policy.Spec.PolicyTypes) != 1 || policy.Spec.PolicyTypes[0] != networking.PolicyTypeEgress {
		t.Fatalf("expected egress isolation after export removal, got %+v", policy.Spec)
	}
}

func TestAllocationMappingValidation(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := netapi.AddToScheme(scheme); err != nil {
		t.Fatal(err)
	}
	configuration := &netapi.Configuration{ObjectMeta: metav1.ObjectMeta{Namespace: "tenant", Name: "consumer", Labels: map[string]string{consts.RemoteClusterID: "consumer"}}}
	configuration.Spec.Remote.CIDR.Pod = []netapi.CIDR{"10.20.0.0/16"}
	configuration.Spec.Remote.CIDR.External = []netapi.CIDR{"10.70.0.0/16"}
	configuration.Status.Remote = &netapi.ClusterConfig{CIDR: netapi.ClusterConfigCIDR{Pod: []netapi.CIDR{"172.20.0.0/16"}, External: []netapi.CIDR{"172.31.0.0/16"}}}
	configuration.Status.Conditions = []metav1.Condition{{Type: netapi.ConfigurationConditionNetworkCIDRsConfigured, Status: metav1.ConditionTrue}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(configuration).Build()
	r := &Reconciler{Client: cl}
	for _, tt := range []struct {
		name      string
		exports   []api.PolicyEndpointExport
		wantError bool
	}{
		{"valid allocation", []api.PolicyEndpointExport{{Original: "172.26.4.7", Allocated: "10.70.0.42", AllocationUID: "first"}}, false},
		{"outside external range", []api.PolicyEndpointExport{{Original: "172.26.4.7", Allocated: "10.71.0.42", AllocationUID: "first"}}, true},
		{"conflicting allocations", []api.PolicyEndpointExport{
			{Original: "172.26.4.7", Allocated: "10.70.0.42", AllocationUID: "first"},
			{Original: "172.26.4.7", Allocated: "10.70.0.43", AllocationUID: "second"},
		}, true},
		{"alias collision", []api.PolicyEndpointExport{
			{Original: "172.26.4.7", Allocated: "10.70.0.42", AllocationUID: "first"},
			{Original: "172.26.4.8", Allocated: "10.70.0.42", AllocationUID: "second"},
		}, true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			shadow := &api.ShadowNetworkPolicy{Spec: api.ShadowNetworkPolicySpec{
				Source: api.PolicySource{Cluster: "consumer"},
				Template: networking.NetworkPolicySpec{PolicyTypes: []networking.PolicyType{networking.PolicyTypeEgress}, Egress: []networking.NetworkPolicyEgressRule{{
					To: []networking.NetworkPolicyPeer{{IPBlock: &networking.IPBlock{CIDR: "172.26.0.0/16"}}},
				}}},
				Context: api.PolicySourceContext{IndirectCIDRs: []string{"172.26.0.0/16"}, Exports: tt.exports},
			}}
			policy, err := r.compile(context.Background(), cl, shadow)
			if (err != nil) != tt.wantError {
				t.Fatalf("unexpected error: %v", err)
			}
			if tt.wantError {
				if len(policy.Egress) != 0 {
					t.Fatalf("invalid mapping retained permissions: %+v", policy)
				}
			} else if len(policy.Egress) != 1 || len(policy.Egress[0].To) != 1 || policy.Egress[0].To[0].IPBlock.CIDR != "172.31.0.42/32" {
				t.Fatalf("unexpected translation: %+v", policy)
			}
		})
	}
}
