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

	discovery "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	kubescheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ipamapi "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	liqofake "github.com/liqotech/liqo/pkg/client/clientset/versioned/fake"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/options"
)

func TestExportUsesActiveReflectedAllocation(t *testing.T) {
	ctx := context.Background()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{kubescheme.AddToScheme, api.AddToScheme} {
		if err := add(scheme); err != nil {
			t.Fatal(err)
		}
	}
	source := &discovery.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{Namespace: "services", Name: "export"}, AddressType: discovery.AddressTypeIPv4,
		Endpoints: []discovery.Endpoint{{Addresses: []string{"172.26.4.7"}}},
	}
	allocation := &ipamapi.IP{ObjectMeta: metav1.ObjectMeta{Namespace: "services", Name: "alloc", UID: "allocation-uid"}}
	allocation.Spec.IP = "172.26.4.7"
	allocation.Status.IP = "10.70.0.42"
	shadow := &api.ShadowEndpointSlice{ObjectMeta: metav1.ObjectMeta{Namespace: "mapped-services", Name: "export", Labels: forge.EndpointSliceLabels()}}
	shadow.Labels[forge.LiqoOriginClusterIDKey] = "consumer"
	shadow.Spec.Template.Endpoints = []discovery.Endpoint{{Addresses: []string{"10.70.0.42"}}}
	local := fake.NewClientBuilder().WithScheme(scheme).WithObjects(source).Build()
	remote := fake.NewClientBuilder().WithScheme(scheme).WithObjects(shadow).Build()
	r := New(local, remote, "consumer", "provider", "liqo", []string{"10.20.0.0/16"}, 3)
	opts := &options.NamespacedOpts{LocalNamespace: "services", RemoteNamespace: "mapped-services", LocalLiqoClient: liqofake.NewSimpleClientset(allocation)}
	var out api.PolicySourceContext
	localRanges := []netip.Prefix{netip.MustParsePrefix("10.20.0.0/16")}
	if err := r.exports(ctx, opts, localRanges, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Exports) != 1 || out.Exports[0].Original != "172.26.4.7" || out.Exports[0].Allocated != "10.70.0.42" || out.Exports[0].Namespace != "mapped-services" {
		t.Fatalf("incorrect endpoint export: %+v", out)
	}
	if err := remote.Get(ctx, client.ObjectKeyFromObject(shadow), shadow); err != nil {
		t.Fatal(err)
	}
	shadow.Spec.Template.Endpoints[0].Addresses[0] = "10.70.0.99"
	if err := remote.Update(ctx, shadow); err != nil {
		t.Fatal(err)
	}
	out = api.PolicySourceContext{}
	if err := r.exports(ctx, opts, localRanges, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Exports) != 0 || len(out.PendingExports) != 1 || out.PendingExports[0] != "172.26.4.7" {
		t.Fatalf("stale EndpointSlice should be pending: %+v", out)
	}
}
