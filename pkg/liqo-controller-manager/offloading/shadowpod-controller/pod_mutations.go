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

package shadowpodctrl

import (
	"context"

	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
	ipamips "github.com/liqotech/liqo/pkg/utils/ipam/mapping"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
)

// PodMutator is a function that mutates a PodSpec.
type PodMutator func(ctx context.Context, c client.Client, podSpec *corev1.PodSpec, remoteClusterID liqov1beta1.ClusterID) error

// PodMutations lists all PodSpec mutations to be applied.
var PodMutations = []PodMutator{
	remapKubernetesServiceIP,
	remapContainerdSocket,
}

// GetPodMutations returns the list of all PodSpec mutations.
func GetPodMutations() []PodMutator {
	return PodMutations
}

// remapKubernetesServiceIP remaps the Kubernetes service IP in HostAliases.
func remapKubernetesServiceIP(ctx context.Context, c client.Client,
	podSpec *corev1.PodSpec, remoteClusterID liqov1beta1.ClusterID) error {
	if len(podSpec.HostAliases) == 0 {
		return nil
	}

	for i := range podSpec.HostAliases {
		if !Contains(podSpec.HostAliases[i].Hostnames, forge.KubernetesAPIService) {
			continue
		}

		// If the HostAliases contains the kubernetes service hostname, it must be replaced with the remapped IP.
		ip := podSpec.HostAliases[i].IP

		// Get the remapped IP for the Kubernetes service.
		rIP, err := ipamips.MapAddress(ctx, c, remoteClusterID, ip)
		if err != nil {
			return err
		}

		// Update the HostAliases with the remapped IP.
		podSpec.HostAliases[i].IP = rIP

		return nil
	}

	return nil
}

// remapContainerdSocket remaps the containerd socket path from /run/containerd/containerd.sock to /run/k0s/containerd.sock.
func remapContainerdSocket(_ context.Context, _ client.Client,
	podSpec *corev1.PodSpec, _ liqov1beta1.ClusterID) error {
	const containerdSocketPath = "/run/containerd/containerd.sock"
	const k0sContainerdSocketPath = "/run/k0s/containerd.sock"

	for i := range podSpec.Volumes {
		if podSpec.Volumes[i].HostPath == nil {
			continue
		}

		if podSpec.Volumes[i].HostPath.Path == containerdSocketPath {
			podSpec.Volumes[i].HostPath.Path = k0sContainerdSocketPath
		}
	}

	return nil
}

// Contains returns true if the slice contains the value.
func Contains[T comparable](slice []T, value T) bool {
	for _, v := range slice {
		if v == value {
			return true
		}
	}
	return false
}
