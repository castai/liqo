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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	corev1 "k8s.io/api/core/v1"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
)

var _ = Describe("PodMutations", func() {
	const remoteClusterID liqov1beta1.ClusterID = "test-cluster"

	Describe("remapContainerdSocket", func() {
		var podSpec *corev1.PodSpec

		BeforeEach(func() {
			podSpec = &corev1.PodSpec{}
		})

		When("volume has containerd socket path", func() {
			It("should remap to k0s containerd socket path", func() {
				podSpec.Volumes = []corev1.Volume{
					{
						Name: "containerd-socket",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: containerdSocketPath,
							},
						},
					},
				}

				err := remapContainerdSocket(context.Background(), nil, podSpec, remoteClusterID)
				Expect(err).NotTo(HaveOccurred())
				Expect(podSpec.Volumes[0].HostPath.Path).To(Equal(k0sContainerdSocketPath))
			})
		})

		When("volume has a different host path", func() {
			It("should not modify the volume", func() {
				podSpec.Volumes = []corev1.Volume{
					{
						Name: "other-socket",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/run/docker.sock", // nolint:goconst
							},
						},
					},
				}

				err := remapContainerdSocket(context.Background(), nil, podSpec, remoteClusterID)
				Expect(err).NotTo(HaveOccurred())
				Expect(podSpec.Volumes[0].HostPath.Path).To(Equal("/run/docker.sock")) // nolint:goconst
			})
		})

		When("volume has no host path", func() {
			It("should not modify the volume", func() {
				podSpec.Volumes = []corev1.Volume{
					{
						Name: "config-map-volume",
						VolumeSource: corev1.VolumeSource{
							ConfigMap: &corev1.ConfigMapVolumeSource{},
						},
					},
				}

				err := remapContainerdSocket(context.Background(), nil, podSpec, remoteClusterID)
				Expect(err).NotTo(HaveOccurred())
				Expect(podSpec.Volumes[0].ConfigMap).NotTo(BeNil())
			})
		})

		When("pod has multiple volumes", func() {
			It("should only remap the containerd socket volume", func() {
				podSpec.Volumes = []corev1.Volume{
					{
						Name: "docker-socket",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: "/var/run/docker.sock", // nolint:goconst
							},
						},
					},
					{
						Name: "containerd-socket",
						VolumeSource: corev1.VolumeSource{
							HostPath: &corev1.HostPathVolumeSource{
								Path: containerdSocketPath,
							},
						},
					},
					{
						Name: "empty-dir-volume",
						VolumeSource: corev1.VolumeSource{
							EmptyDir: &corev1.EmptyDirVolumeSource{},
						},
					},
				}

				err := remapContainerdSocket(context.Background(), nil, podSpec, remoteClusterID)
				Expect(err).NotTo(HaveOccurred())
				Expect(podSpec.Volumes[0].HostPath.Path).To(Equal("/var/run/docker.sock")) // nolint:goconst
				Expect(podSpec.Volumes[1].HostPath.Path).To(Equal(k0sContainerdSocketPath))
				Expect(podSpec.Volumes[2].EmptyDir).NotTo(BeNil())
			})
		})

		When("pod has no volumes", func() {
			It("should not error", func() {
				err := remapContainerdSocket(context.Background(), nil, podSpec, remoteClusterID)
				Expect(err).NotTo(HaveOccurred())
				Expect(podSpec.Volumes).To(BeEmpty())
			})
		})
	})

})

var _ = Describe("PodMutations Integration", func() {
	Describe("PodMutators slice", func() {
		It("should contain both mutations", func() {
			mutations := GetPodMutations()
			Expect(mutations).To(HaveLen(2))
		})
	})
})
