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

package ipmapping

import (
	"context"
	"testing"

	. "github.com/onsi/gomega"
	discoveryv1 "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ipamv1alpha1 "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
)

const testIPNamespace = "liqo"

var endpointSliceRequest = ctrl.Request{NamespacedName: types.NamespacedName{
	Namespace: kubernetesServiceNamespace,
	Name:      kubernetesServiceName,
}}

func kubernetesEndpointSlice(addresses ...string) *discoveryv1.EndpointSlice {
	endpoints := []discoveryv1.Endpoint{}
	for _, addr := range addresses {
		endpoints = append(endpoints, discoveryv1.Endpoint{
			Addresses:  []string{addr},
			Conditions: discoveryv1.EndpointConditions{Ready: ptr.To(true)},
		})
	}

	return &discoveryv1.EndpointSlice{
		ObjectMeta: metav1.ObjectMeta{
			Name:      kubernetesServiceName,
			Namespace: kubernetesServiceNamespace,
			Labels:    map[string]string{discoveryv1.LabelServiceName: kubernetesServiceName},
		},
		AddressType: discoveryv1.AddressTypeIPv4,
		Endpoints:   endpoints,
	}
}

func apiServerIP(name, addr string) *ipamv1alpha1.IP {
	return &ipamv1alpha1.IP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: testIPNamespace,
			Labels: map[string]string{
				consts.IPEndpointSliceNameLabelKey: kubernetesServiceName,
				consts.IPTypeLabelKey:              consts.IPTypeAPIServer,
			},
		},
		Spec: ipamv1alpha1.IPSpec{
			IP:         networkingv1beta1.IP(addr),
			Masquerade: ptr.To(true),
		},
	}
}

func newReconciler(t *testing.T, objs ...client.Object) (*EndpointSliceIPReconciler, client.Client) {
	t.Helper()

	scheme := runtime.NewScheme()
	g := NewWithT(t)
	g.Expect(discoveryv1.AddToScheme(scheme)).To(Succeed())
	g.Expect(ipamv1alpha1.AddToScheme(scheme)).To(Succeed())

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()

	return &EndpointSliceIPReconciler{
		Client:      cl,
		Scheme:      scheme,
		ipNamespace: testIPNamespace,
	}, cl
}

func getIPs(ctx context.Context, cl client.Client) ([]ipamv1alpha1.IP, error) {
	var ipList ipamv1alpha1.IPList
	if err := cl.List(ctx, &ipList, client.InNamespace(testIPNamespace)); err != nil {
		return nil, err
	}
	return ipList.Items, nil
}

func TestEndpointSliceIPReconcilerCreatesMapping(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, cl := newReconciler(t, kubernetesEndpointSlice("172.23.0.2"))

	res, err := r.Reconcile(ctx, endpointSliceRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	ips, err := getIPs(ctx, cl)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(ips[0].Name).To(Equal("kubernetes-172-23-0-2"))
	g.Expect(ips[0].Spec.IP).To(Equal(networkingv1beta1.IP("172.23.0.2")))
	g.Expect(ips[0].Spec.Masquerade).To(Equal(ptr.To(true)))
	g.Expect(ips[0].Labels).To(HaveKeyWithValue(consts.IPEndpointSliceNameLabelKey, kubernetesServiceName))
	g.Expect(ips[0].Labels).To(HaveKeyWithValue(consts.IPTypeLabelKey, consts.IPTypeAPIServer))
	g.Expect(ips[0].Annotations).To(HaveKeyWithValue(consts.PreinstalledAnnotKey, consts.PreinstalledAnnotValue))
}

func TestEndpointSliceIPReconcilerRecreatesMissingMapping(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, cl := newReconciler(t, kubernetesEndpointSlice("172.23.0.2"))

	_, err := r.Reconcile(ctx, endpointSliceRequest)
	g.Expect(err).NotTo(HaveOccurred())

	// Simulate the mapping being removed (manual/accidental deletion, or a previous stale-prune).
	ip := apiServerIP("kubernetes-172-23-0-2", "172.23.0.2")
	g.Expect(cl.Delete(ctx, ip)).To(Succeed())

	// The reconcile triggered by the IP deletion event must recreate it.
	res, err := r.Reconcile(ctx, endpointSliceRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res).To(Equal(ctrl.Result{}))

	ips, err := getIPs(ctx, cl)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(ips).To(HaveLen(1))
	g.Expect(ips[0].Name).To(Equal("kubernetes-172-23-0-2"))
}

func TestEndpointSliceIPReconcilerSkipsPruningWhenEndpointsEmpty(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	// The EndpointSlice momentarily advertises no endpoints (e.g., during API-server endpoint churn),
	// while the previously created mapping still exists. It must NOT be pruned: deleting the last
	// API-server mapping tears down working connectivity and is not self-healed by EPS events alone.
	r, cl := newReconciler(t,
		kubernetesEndpointSlice(),
		apiServerIP("kubernetes-172-23-0-2", "172.23.0.2"),
	)

	res, err := r.Reconcile(ctx, endpointSliceRequest)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(res.RequeueAfter).To(Equal(emptyEndpointsRequeueInterval))

	var ip ipamv1alpha1.IP
	err = cl.Get(ctx, types.NamespacedName{Namespace: testIPNamespace, Name: "kubernetes-172-23-0-2"}, &ip)
	g.Expect(err).NotTo(HaveOccurred(), "mapping must be preserved while the endpoint set is empty")
}

func TestEndpointSliceIPReconcilerPrunesWhenAddressChanges(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()

	r, cl := newReconciler(t,
		kubernetesEndpointSlice("172.23.0.3"),
		apiServerIP("kubernetes-172-23-0-2", "172.23.0.2"),
	)

	_, err := r.Reconcile(ctx, endpointSliceRequest)
	g.Expect(err).NotTo(HaveOccurred())

	var stale ipamv1alpha1.IP
	err = cl.Get(ctx, types.NamespacedName{Namespace: testIPNamespace, Name: "kubernetes-172-23-0-2"}, &stale)
	g.Expect(apierrors.IsNotFound(err)).To(BeTrue(), "stale mapping must be pruned when a different address is advertised")

	var created ipamv1alpha1.IP
	err = cl.Get(ctx, types.NamespacedName{Namespace: testIPNamespace, Name: "kubernetes-172-23-0-3"}, &created)
	g.Expect(err).NotTo(HaveOccurred())
	g.Expect(created.Spec.IP).To(Equal(networkingv1beta1.IP("172.23.0.3")))
}

func TestIPToEndpointSliceRequests(t *testing.T) {
	g := NewWithT(t)
	ctx := context.Background()
	r, _ := newReconciler(t)

	g.Expect(r.ipToEndpointSliceRequests(ctx, apiServerIP("kubernetes-172-23-0-2", "172.23.0.2"))).
		To(Equal([]ctrl.Request{endpointSliceRequest}))
	g.Expect(r.ipToEndpointSliceRequests(ctx, &ipamv1alpha1.IP{ObjectMeta: metav1.ObjectMeta{
		Name:      "unrelated",
		Namespace: testIPNamespace,
	}})).To(BeEmpty())
}
