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

package wireguard

import (
	"context"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway"
)

func newTestClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	return fake.NewClientBuilder().WithScheme(scheme).WithObjects(objs...).Build()
}

func makeDeployment(name, namespace, remoteClusterID, templateName, templateNamespace, generation string, rolling bool) *appsv1.Deployment {
	dep := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       name,
			Namespace:  namespace,
			Generation: 1,
			Labels: map[string]string{
				consts.RemoteClusterID:        remoteClusterID,
				consts.NetworkingComponentKey: gateway.GatewayComponentGateway,
			},
		},
		Spec: appsv1.DeploymentSpec{
			Replicas: ptr.To(int32(1)),
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{
					Labels: map[string]string{"app": name},
				},
				Spec: corev1.PodSpec{
					Containers: []corev1.Container{
						{Name: "gateway", Image: "liqo/gateway:latest"},
					},
				},
			},
		},
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  1,
			Replicas:            1,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}
	if templateName != "" || templateNamespace != "" {
		if dep.Labels == nil {
			dep.Labels = map[string]string{}
		}
		if templateName != "" {
			dep.Labels[consts.TemplateNameLabelKey] = templateName
		}
		if templateNamespace != "" {
			dep.Labels[consts.TemplateNamespaceLabelKey] = templateNamespace
		}
	}
	if generation != "" {
		dep.Annotations = map[string]string{
			consts.TemplateGenerationAnnotationKey: generation,
		}
	}
	if rolling {
		dep.Status.ObservedGeneration = 0
	}
	return dep
}

var _ = Describe("Rollout helpers", func() {
	Context("IsDeploymentRollingOut", func() {
		DescribeTable("reports whether a Deployment is rolling out",
			func(dep *appsv1.Deployment, expected bool) {
				Expect(IsDeploymentRollingOut(dep)).To(Equal(expected))
			},
			Entry("nil deployment", nil, false),
			Entry("steady state", makeDeployment("gw", "ns", "remote", "template", "template-ns", "generation", false), false),
			Entry("unobserved generation", func() *appsv1.Deployment {
				d := makeDeployment("gw", "ns", "remote", "template", "template-ns", "generation", false)
				d.Generation = 2
				d.Status.ObservedGeneration = 1
				return d
			}(), true),
			Entry("updated replicas below desired", func() *appsv1.Deployment {
				d := makeDeployment("gw", "ns", "remote", "template", "template-ns", "generation", false)
				d.Status.UpdatedReplicas = 0
				return d
			}(), true),
			Entry("available replicas below desired", func() *appsv1.Deployment {
				d := makeDeployment("gw", "ns", "remote", "template", "template-ns", "generation", false)
				d.Status.AvailableReplicas = 0
				return d
			}(), true),
			Entry("unavailable replicas present", func() *appsv1.Deployment {
				d := makeDeployment("gw", "ns", "remote", "template", "template-ns", "generation", false)
				d.Status.UnavailableReplicas = 1
				return d
			}(), true),
		)
	})

	Context("ShouldDelayRollout", func() {
		const (
			remoteClusterID          = "remote"
			desiredTemplateName      = "template"
			desiredTemplateNamespace = "template-ns"
			desiredGeneration        = "desired-generation"
		)

		DescribeTable("decides whether a rollout should be delayed",
			func(self types.NamespacedName, peers []*appsv1.Deployment, expected bool) {
				objs := make([]client.Object, len(peers))
				for i := range peers {
					objs[i] = peers[i]
				}
				cl := newTestClient(objs...)
				delay, _, err := ShouldDelayRollout(context.Background(), cl, remoteClusterID, self,
					desiredTemplateName, desiredTemplateNamespace, desiredGeneration)
				Expect(err).NotTo(HaveOccurred())
				Expect(delay).To(Equal(expected))
			},
			Entry("no peers", types.NamespacedName{Namespace: "ns", Name: "gw-a"}, nil, false),
			Entry("self is excluded from peer list",
				types.NamespacedName{Namespace: "ns", Name: "gw-a"},
				[]*appsv1.Deployment{makeDeployment("gw-a", "ns", remoteClusterID, desiredTemplateName, desiredTemplateNamespace, desiredGeneration, false)},
				false,
			),
			Entry("lower-named peer with stale generation delays",
				types.NamespacedName{Namespace: "ns", Name: "gw-b"},
				[]*appsv1.Deployment{makeDeployment("gw-a", "ns", remoteClusterID, desiredTemplateName, desiredTemplateNamespace, "old-generation", false)},
				true,
			),
			Entry("lower-named peer with desired generation but rolling out delays",
				types.NamespacedName{Namespace: "ns", Name: "gw-b"},
				[]*appsv1.Deployment{makeDeployment("gw-a", "ns", remoteClusterID, desiredTemplateName, desiredTemplateNamespace, desiredGeneration, true)},
				true,
			),
			Entry("lower-named peer up to date does not delay",
				types.NamespacedName{Namespace: "ns", Name: "gw-b"},
				[]*appsv1.Deployment{makeDeployment("gw-a", "ns", remoteClusterID, desiredTemplateName, desiredTemplateNamespace, desiredGeneration, false)},
				false,
			),
			Entry("higher-named peer with stale generation does not delay",
				types.NamespacedName{Namespace: "ns", Name: "gw-a"},
				[]*appsv1.Deployment{makeDeployment("gw-b", "ns", remoteClusterID, desiredTemplateName, desiredTemplateNamespace, "old-generation", false)},
				false,
			),
			Entry("higher-named peer rolling out delays",
				types.NamespacedName{Namespace: "ns", Name: "gw-a"},
				[]*appsv1.Deployment{makeDeployment("gw-b", "ns", remoteClusterID, desiredTemplateName, desiredTemplateNamespace, desiredGeneration, true)},
				true,
			),
			Entry("peer with different remote cluster id is ignored",
				types.NamespacedName{Namespace: "ns", Name: "gw-a"},
				[]*appsv1.Deployment{makeDeployment("gw-b", "ns", "other-cluster", desiredTemplateName, desiredTemplateNamespace, "old-generation", true)},
				false,
			),
			Entry("peer with same remote cluster id but different template name is ignored",
				types.NamespacedName{Namespace: "ns", Name: "gw-b"},
				[]*appsv1.Deployment{makeDeployment("gw-a", "ns", remoteClusterID, "other-template", desiredTemplateNamespace, "old-generation", true)},
				false,
			),
			Entry("peer with same remote cluster id and name but different template namespace is ignored",
				types.NamespacedName{Namespace: "ns", Name: "gw-b"},
				[]*appsv1.Deployment{makeDeployment("gw-a", "ns", remoteClusterID, desiredTemplateName, "other-namespace", "old-generation", true)},
				false,
			),
		)
	})
})
