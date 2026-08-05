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

package forge

import (
	"strconv"
	"strings"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
)

// MutateSecretArg mutates the foreigncluster kubeconfig secret name in the virtual kubelet deployment.
func MutateSecretArg(vn *offloadingv1beta1.VirtualNode) {
	ksref := vn.Spec.KubeconfigSecretRef
	if ksref == nil {
		return
	}
	argSecret := StringifyArgument(string(ForeignClusterKubeconfigSecretName), ksref.Name)
	container := &vn.Spec.Template.Spec.Template.Spec.Containers[0]

	for i, arg := range container.Args {
		if strings.HasPrefix(arg, string(ForeignClusterKubeconfigSecretName)) {
			if arg == argSecret {
				return
			}
			container.Args[i] = argSecret
			return
		}
	}

	container.Args = append(container.Args, argSecret)
}

// MutateNodeCreate mutates the creation of the remote cluster node.
func MutateNodeCreate(vn *offloadingv1beta1.VirtualNode) {
	argCreateNode := StringifyArgument(string(CreateNode), strconv.FormatBool(*vn.Spec.CreateNode))
	container := &vn.Spec.Template.Spec.Template.Spec.Containers[0]
	for i, arg := range container.Args {
		if strings.HasPrefix(arg, string(CreateNode)) {
			if arg == argCreateNode {
				return
			}
			container.Args[i] = argCreateNode
			return
		}
	}

	container.Args = append(container.Args, argCreateNode)
}

// MutateNodeCheckNetwork mutates the check network flag.
func MutateNodeCheckNetwork(vn *offloadingv1beta1.VirtualNode) {
	argCheckNetwork := StringifyArgument(string(NodeCheckNetwork), strconv.FormatBool(!*vn.Spec.DisableNetworkCheck))
	container := &vn.Spec.Template.Spec.Template.Spec.Containers[0]
	for i, arg := range container.Args {
		if strings.HasPrefix(arg, string(NodeCheckNetwork)) {
			if arg == argCheckNetwork {
				return
			}
			container.Args[i] = argCheckNetwork
			return
		}
	}

	container.Args = append(container.Args, argCheckNetwork)
}

// MutateReplicas mutates the replicas field of the virtual kubelet deployment.
func MutateReplicas(vn *offloadingv1beta1.VirtualNode, vkOpts *offloadingv1beta1.VkOptionsTemplate) {
	if vkOpts.Spec.Replicas != nil {
		vn.Spec.Template.Spec.Replicas = vkOpts.Spec.Replicas
	}
}

// ForgeVirtualNodeTemplate forges the VirtualNode deployment template from the VkOptionsTemplate.
// This is the central function used by both the webhook (on creation) and the controller (on reconciliation)
// to ensure the deployment template is always up to date with the latest VkOptionsTemplate.
// It re-forges the deployment template with the current options, preserving the per-virtual-node fields
// (such as the kubeconfig secret name, createNode and disableNetworkCheck flags) which are taken from the VN itself.
// Callers should call MutateSpecInTemplate separately after ensuring CreateNode and DisableNetworkCheck are set.
func ForgeVirtualNodeTemplate(vn *offloadingv1beta1.VirtualNode, opts *offloadingv1beta1.VkOptionsTemplate,
	homeCluster liqov1beta1.ClusterID, liqoNamespace string, localPodCIDRs []string) {
	if vn.Spec.Template == nil {
		vn.Spec.Template = &offloadingv1beta1.DeploymentTemplate{}
	}
	vkdep := VirtualKubeletDeployment(homeCluster, liqoNamespace, localPodCIDRs, vn, opts)
	vn.Spec.Template.ObjectMeta = *vkdep.ObjectMeta.DeepCopy()
	vn.Spec.Template.Spec = *vkdep.Spec.DeepCopy()
}

// MutateSpecInTemplate applies all the mutations to the virtual node template spec.
func MutateSpecInTemplate(vn *offloadingv1beta1.VirtualNode, vkOpts *offloadingv1beta1.VkOptionsTemplate) {
	MutateSecretArg(vn)
	MutateNodeCreate(vn)
	MutateNodeCheckNetwork(vn)
	MutateReplicas(vn, vkOpts)
}
