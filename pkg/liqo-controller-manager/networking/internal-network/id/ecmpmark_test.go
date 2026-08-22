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

package id

import (
	"context"
	"sync"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
)

var _ = Describe("ECMP Mark Manager Tests", func() {

	var (
		cl      client.Client
		ctx     context.Context
		scheme  *runtime.Scheme
		markMgr *Manager[uint32]
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		Expect(networkingv1beta1.SchemeBuilder.AddToScheme(scheme)).To(Succeed())

		// Build a fake client with an InternalFabric that already has a mark.
		cl = fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fabric-with-mark",
					Namespace: corev1.NamespaceDefault,
				},
				Status: networkingv1beta1.InternalFabricStatus{
					ECMPMark: ptr.To(consts.ECMPReplicaMarkBase + 42),
				},
			},
			&networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "fabric-without-mark",
					Namespace: corev1.NamespaceDefault,
				},
			},
		).Build()

		// Reset the singleton so each test starts fresh.
		ecmpMarkManager = nil
		ecmpMarkOnce = sync.Once{}
		markMgr = GetECMPMarkManager(ctx, cl)
	})

	It("should recover existing marks from InternalFabric status", func() {
		mark, err := markMgr.Allocate("default/fabric-with-mark")
		Expect(err).To(Succeed())
		Expect(mark).To(BeNumerically("==", consts.ECMPReplicaMarkBase+42))
	})

	It("should allocate marks in the ECMP range", func() {
		mark, err := markMgr.Allocate("default/fabric-without-mark")
		Expect(err).To(Succeed())
		Expect(mark).To(BeNumerically(">=", consts.ECMPReplicaMarkBase))
		Expect(mark).To(BeNumerically("<", consts.ECMPReplicaMarkBase+(1<<24)))
	})

	It("should allocate different marks for different fabrics", func() {
		mark1, err := markMgr.Allocate("default/fabric-without-mark")
		Expect(err).To(Succeed())

		cl2 := fake.NewClientBuilder().WithScheme(scheme).WithObjects(
			&networkingv1beta1.InternalFabric{
				ObjectMeta: metav1.ObjectMeta{
					Name:      "another-fabric",
					Namespace: corev1.NamespaceDefault,
				},
			},
		).Build()

		mark2, err := GetECMPMarkManager(ctx, cl2).Allocate("default/another-fabric")
		Expect(err).To(Succeed())
		Expect(mark1).To(Not(Equal(mark2)))
	})
})
