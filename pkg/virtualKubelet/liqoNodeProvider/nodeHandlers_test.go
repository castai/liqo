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

package liqonodeprovider

import (
	"context"
	"errors"
	"path/filepath"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"
	storagev1 "k8s.io/api/storage/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/utils/ptr"

	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/utils/testutil"
)

var _ = Describe("NodeHandlers", func() {

	var (
		cluster      testutil.Cluster
		client       kubernetes.Interface
		nodeProvider *LiqoNodeProvider
		ctx          context.Context
		cancel       context.CancelFunc
		err          error
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())

		cluster, _, err = testutil.NewTestCluster([]string{
			filepath.Join("..", "..", "..", "deployments", "liqo", "charts", "liqo-crds", "crds"),
		})
		Expect(err).To(BeNil())

		client = cluster.GetClient()

		nodeProvider = NewLiqoNodeProvider(&InitConfig{
			NodeName:  nodeName,
			Namespace: kubeletNamespace,

			HomeConfig:      cluster.GetCfg(),
			RemoteConfig:    cluster.GetCfg(), /* not actually used in tests */
			RemoteClusterID: foreignClusterID,
		})
	})

	AfterEach(func() {
		cancel()

		err := cluster.GetEnv().Stop()
		Expect(err).To(BeNil())
	})

	It("Should create the node and the associated CSINode when the node does not exist", func() {
		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("node not found"))).To(Succeed())

		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(node.GetLabels()).To(HaveKeyWithValue(corev1.LabelHostname, nodeName))

		csiNode, err := client.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(csiNode.Spec.Drivers).To(BeEmpty())
		Expect(csiNode.OwnerReferences).To(HaveLen(1))
		Expect(csiNode.OwnerReferences[0].UID).To(Equal(node.GetUID()))
	})

	It("Should update the node status when the node already exists", func() {
		existing, err := client.CoreV1().Nodes().Create(ctx, nodeProvider.GetNode(), metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		nodeProvider.GetNode().Status.Capacity = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}
		nodeProvider.GetNode().Status.Allocatable = corev1.ResourceList{corev1.ResourceCPU: resource.MustParse("2")}

		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("status update failed"))).To(Succeed())

		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(node.GetUID()).To(Equal(existing.GetUID()))
		Expect(node.GetResourceVersion()).ToNot(Equal(existing.GetResourceVersion()))
		Expect(node.Status.Allocatable.Cpu().String()).To(Equal("2"))
	})

	It("Should not re-create the node when it is terminating", func() {
		nodeProvider.updateMutex.Lock()
		nodeProvider.terminating = true
		nodeProvider.updateMutex.Unlock()

		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("node not found"))).To(Succeed())

		_, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		_, err = client.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())
	})

	It("Should consider the node terminating while the virtual node is being deleted", func() {
		virtualNode := &offloadingv1beta1.VirtualNode{ObjectMeta: metav1.ObjectMeta{Name: nodeName}}
		nodeProvider.syncTerminatingState(virtualNode)
		Expect(nodeProvider.IsTerminating()).To(BeFalse())

		virtualNode.SetDeletionTimestamp(ptr.To(metav1.Now()))
		nodeProvider.syncTerminatingState(virtualNode)
		Expect(nodeProvider.IsTerminating()).To(BeTrue())

		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("node not found"))).To(Succeed())
		_, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(apierrors.IsNotFound(err)).To(BeTrue())

		virtualNode.SetDeletionTimestamp(nil)
		nodeProvider.syncTerminatingState(virtualNode)
		Expect(nodeProvider.IsTerminating()).To(BeFalse())
	})

	It("Should re-create the node when it is deleted while the virtual node is not being deleted", func() {
		// The node lifecycle follows the VirtualNode: a node deleted by someone else must be restored.
		nodeProvider.node.SetDeletionTimestamp(ptr.To(metav1.Now()))
		Expect(nodeProvider.IsTerminating()).To(BeFalse())

		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("node not found"))).To(Succeed())

		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(node.GetDeletionTimestamp()).To(BeNil())
	})

	It("Should not re-create the node with the finalizers added by other controllers", func() {
		previousNode, err := client.CoreV1().Nodes().Create(ctx, nodeProvider.GetNode(), metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		// An external controller protects the node with a finalizer of its own.
		previousNode.SetFinalizers(append(previousNode.GetFinalizers(), "external-controller.example.com/node-protection"))
		previousNode, err = client.CoreV1().Nodes().Update(ctx, previousNode, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred())

		// The provider keeps the last observed version of the node in memory (the object is deep-copied, as the
		// provider holds its own snapshot and the test keeps mutating the API server object below).
		nodeProvider.updateMutex.Lock()
		nodeProvider.node = previousNode.DeepCopy()
		nodeProvider.updateMutex.Unlock()

		// The external controller releases the finalizer and the node is actually deleted.
		released := previousNode.DeepCopy()
		released.SetFinalizers(nil)
		_, err = client.CoreV1().Nodes().Update(ctx, released, metav1.UpdateOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})).To(Succeed())
		Eventually(func() bool {
			_, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
			return apierrors.IsNotFound(err)
		}, timeout, interval).Should(BeTrue())

		// The missing node is re-created on the next failed status update, without inheriting the finalizer above.
		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("node not found"))).To(Succeed())

		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(node.GetUID()).ToNot(Equal(previousNode.GetUID()))
		Expect(node.GetFinalizers()).To(BeEmpty())
		Expect(node.GetLabels()).To(HaveKeyWithValue(corev1.LabelHostname, nodeName))
	})

	It("Should return an error if the node is created but its CSINode cannot be set up", func() {
		// Shorten the CSINode setup budget, so that the expected failure is reached quickly.
		timeout, interval := csinodeEnsureTimeout, csinodeEnsurePollInterval
		csinodeEnsureTimeout, csinodeEnsurePollInterval = time.Second, 100*time.Millisecond
		DeferCleanup(func() { csinodeEnsureTimeout, csinodeEnsurePollInterval = timeout, interval })

		// Create a node and then delete it, leaving behind a CSINode owned by a dead UID (i.e. the garbage
		// collector has not removed it yet). The environment does not run the garbage collector, hence the
		// stale object is created explicitly.
		previousNode, err := client.CoreV1().Nodes().Create(ctx, nodeProvider.GetNode(), metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(client.CoreV1().Nodes().Delete(ctx, nodeName, metav1.DeleteOptions{})).To(Succeed())

		_, err = client.StorageV1().CSINodes().Create(ctx, &storagev1.CSINode{
			ObjectMeta: metav1.ObjectMeta{
				Name: nodeName,
				OwnerReferences: []metav1.OwnerReference{{
					APIVersion: "v1",
					Kind:       "Node",
					Name:       nodeName,
					UID:        previousNode.GetUID(),
					Controller: ptr.To(true),
				}},
			},
			Spec: storagev1.CSINodeSpec{Drivers: []storagev1.CSINodeDriver{}},
		}, metav1.CreateOptions{})
		Expect(err).ToNot(HaveOccurred())

		Expect(nodeProvider.HandleNodeStatusUpdateError(ctx, errors.New("node not found"))).ToNot(Succeed())

		// The node is created anyway, as it precedes the CSINode setup, and it is left without the CSINode.
		node, err := client.CoreV1().Nodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(node.GetUID()).ToNot(Equal(previousNode.GetUID()))

		csiNode, err := client.StorageV1().CSINodes().Get(ctx, nodeName, metav1.GetOptions{})
		Expect(err).ToNot(HaveOccurred())
		Expect(csiNode.OwnerReferences).To(HaveLen(1))
		Expect(csiNode.OwnerReferences[0].UID).To(Equal(previousNode.GetUID()))
	})
})
