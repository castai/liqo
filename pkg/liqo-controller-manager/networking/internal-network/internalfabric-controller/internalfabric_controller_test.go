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

package internalfabriccontroller

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/internal-network/id"
)

var _ = Describe("InternalFabric Controller Tests", func() {

	var (
		ctx    context.Context
		scheme *runtime.Scheme
		cl     client.Client
		rec    *InternalFabricReconciler
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(networkingv1beta1.SchemeBuilder.AddToScheme(scheme)).To(Succeed())

		// Reset the ECMP mark singleton so tests are isolated.
		id.ResetECMPMarkManager()
	})

	Context("ECMP mark allocation", func() {

		It("should allocate a mark for an InternalFabric without one", func() {
			fabric := &networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fabric",
					Namespace: corev1.NamespaceDefault,
					Labels: map[string]string{
						consts.RemoteClusterID: "cluster-a",
					},
				},
				Spec: networkingv1beta1.InternalFabricSpec{
					Interface: networkingv1beta1.InternalFabricSpecInterface{
						Node: networkingv1beta1.InternalFabricSpecInterfaceNode{Name: "liqo-node"},
						Gateway: networkingv1beta1.InternalFabricSpecInterfaceGateway{
							IP: networkingv1beta1.IP("10.0.0.1"),
						},
					},
				},
			}
			cl = fake.NewClientBuilder().WithScheme(scheme).WithObjects(fabric).WithStatusSubresource(fabric).Build()
			rec = &InternalFabricReconciler{Client: cl, Scheme: scheme}

			Expect(rec.ensureECMPMark(ctx, fabric)).To(Succeed())
			Expect(fabric.Status.ECMPMark).To(Not(BeNil()))
			Expect(*fabric.Status.ECMPMark).To(BeNumerically(">=", consts.ECMPReplicaMarkBase))
		})

		It("should not allocate marks for siblings", func() {
			fabric1 := &networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fabric-1",
					Namespace: corev1.NamespaceDefault,
					Labels: map[string]string{
						consts.RemoteClusterID: "cluster-a",
					},
				},
				Spec: networkingv1beta1.InternalFabricSpec{
					Interface: networkingv1beta1.InternalFabricSpecInterface{
						Node: networkingv1beta1.InternalFabricSpecInterfaceNode{Name: "liqo-node"},
						Gateway: networkingv1beta1.InternalFabricSpecInterfaceGateway{
							IP: networkingv1beta1.IP("10.0.0.1"),
						},
					},
				},
			}
			fabric2 := &networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fabric-2",
					Namespace: corev1.NamespaceDefault,
					Labels: map[string]string{
						consts.RemoteClusterID: "cluster-a",
					},
				},
				Spec: networkingv1beta1.InternalFabricSpec{
					Interface: networkingv1beta1.InternalFabricSpecInterface{
						Node: networkingv1beta1.InternalFabricSpecInterfaceNode{Name: "liqo-node-2"},
						Gateway: networkingv1beta1.InternalFabricSpecInterfaceGateway{
							IP: networkingv1beta1.IP("10.0.0.2"),
						},
					},
				},
			}
			cl = fake.NewClientBuilder().WithScheme(scheme).WithObjects(fabric1, fabric2).
				WithStatusSubresource(fabric1, fabric2).Build()
			rec = &InternalFabricReconciler{Client: cl, Scheme: scheme}

			Expect(rec.ensureECMPMark(ctx, fabric1)).To(Succeed())
			Expect(fabric1.Status.ECMPMark).To(Not(BeNil()))
			Expect(fabric2.Status.ECMPMark).To(BeNil())
		})

		It("should keep an existing mark stable", func() {
			existingMark := consts.ECMPReplicaMarkBase + 7
			fabric := &networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fabric",
					Namespace: corev1.NamespaceDefault,
					Labels: map[string]string{
						consts.RemoteClusterID: "cluster-a",
					},
				},
				Spec: networkingv1beta1.InternalFabricSpec{
					Interface: networkingv1beta1.InternalFabricSpecInterface{
						Node: networkingv1beta1.InternalFabricSpecInterfaceNode{Name: "liqo-node"},
						Gateway: networkingv1beta1.InternalFabricSpecInterfaceGateway{
							IP: networkingv1beta1.IP("10.0.0.1"),
						},
					},
				},
				Status: networkingv1beta1.InternalFabricStatus{
					ECMPMark: &existingMark,
				},
			}
			cl = fake.NewClientBuilder().WithScheme(scheme).WithObjects(fabric).WithStatusSubresource(fabric).Build()
			rec = &InternalFabricReconciler{Client: cl, Scheme: scheme}

			Expect(rec.ensureECMPMark(ctx, fabric)).To(Succeed())
			Expect(*fabric.Status.ECMPMark).To(Equal(existingMark))
		})
	})
})
