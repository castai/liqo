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
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	monitoringv1 "github.com/prometheus-operator/prometheus-operator/pkg/apis/monitoring/v1"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	"github.com/liqotech/liqo/pkg/gateway/leaderelection"
)

func newIntegrationClient(objs ...client.Object) client.Client {
	scheme := runtime.NewScheme()
	Expect(networkingv1beta1.AddToScheme(scheme)).To(Succeed())
	Expect(appsv1.AddToScheme(scheme)).To(Succeed())
	Expect(corev1.AddToScheme(scheme)).To(Succeed())
	Expect(rbacv1.AddToScheme(scheme)).To(Succeed())
	Expect(monitoringv1.AddToScheme(scheme)).To(Succeed())
	return fake.NewClientBuilder().
		WithScheme(scheme).
		WithObjects(objs...).
		WithStatusSubresource(&networkingv1beta1.WgGatewayServer{}, &appsv1.Deployment{}).
		Build()
}

func buildWgGatewayServer(name, namespace, remoteClusterID string) *networkingv1beta1.WgGatewayServer {
	return &networkingv1beta1.WgGatewayServer{
		ObjectMeta: metav1.ObjectMeta{
			Name:     name,
			Namespace: namespace,
			UID:      types.UID(name + "-uid"),
			Labels: map[string]string{
				consts.RemoteClusterID:             remoteClusterID,
				consts.TemplateNameLabelKey:        "server-template",
				consts.TemplateNamespaceLabelKey:   "server-template-ns",
			},
			Annotations: map[string]string{
				// Simulates the source template generation stamped by the server-operator.
				consts.TemplateGenerationAnnotationKey: "2",
			},
			Finalizers: []string{consts.ClusterRoleBindingFinalizer},
		},
		Spec: networkingv1beta1.WgGatewayServerSpec{
			SecretRef: corev1.LocalObjectReference{
				Name: name + "-keys",
			},
			Service: networkingv1beta1.ServiceTemplate{
				Metadata: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.RemoteClusterID:        remoteClusterID,
						consts.NetworkingComponentKey: gateway.GatewayComponentGateway,
					},
				},
				Spec: corev1.ServiceSpec{
					Type:       corev1.ServiceTypeClusterIP,
					ClusterIP:  "10.0.0.1",
					ClusterIPs: []string{"10.0.0.1"},
					Ports: []corev1.ServicePort{
						{Port: 51820, Protocol: corev1.ProtocolUDP},
					},
				},
			},
			Deployment: networkingv1beta1.DeploymentTemplate{
				Metadata: metav1.ObjectMeta{
					Labels: map[string]string{
						consts.RemoteClusterID:        remoteClusterID,
						consts.NetworkingComponentKey: gateway.GatewayComponentGateway,
					},
				},
				Spec: appsv1.DeploymentSpec{
					Replicas: ptr.To(int32(1)),
					Selector: &metav1.LabelSelector{
						MatchLabels: map[string]string{
							"app": forge.GatewayResourceName(name),
						},
					},
					Template: corev1.PodTemplateSpec{
						ObjectMeta: metav1.ObjectMeta{
							Labels: map[string]string{
								"app": forge.GatewayResourceName(name),
							},
						},
						Spec: corev1.PodSpec{
							Containers: []corev1.Container{
								{
									Name:  "wireguard",
									Image: "liqo/gateway-wireguard:latest",
									Env: []corev1.EnvVar{
										{Name: "VERSION", Value: "new"},
									},
								},
							},
						},
					},
				},
			},
		},
	}
}

func buildExistingDeployment(wgServer *networkingv1beta1.WgGatewayServer) *appsv1.Deployment {
	desired := wgServer.Spec.Deployment.Spec.DeepCopy()
	desired.Template.Spec.Containers[0].Env[0].Value = "old"
	depName := forge.GatewayResourceName(wgServer.Name)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:       depName,
			Namespace:  wgServer.Namespace,
			Generation: 1,
			Labels: map[string]string{
				consts.RemoteClusterID:           wgServer.Labels[consts.RemoteClusterID],
				consts.NetworkingComponentKey:    gateway.GatewayComponentGateway,
				consts.TemplateNameLabelKey:      wgServer.Labels[consts.TemplateNameLabelKey],
				consts.TemplateNamespaceLabelKey: wgServer.Labels[consts.TemplateNamespaceLabelKey],
			},
			Annotations: map[string]string{
				// The existing deployment is still at the previous template generation.
				consts.TemplateGenerationAnnotationKey: "1",
			},
		},
		Spec: *desired,
		Status: appsv1.DeploymentStatus{
			ObservedGeneration:  1,
			Replicas:            1,
			UpdatedReplicas:     1,
			AvailableReplicas:   1,
			UnavailableReplicas: 0,
		},
	}
}

func buildService(wgServer *networkingv1beta1.WgGatewayServer) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forge.GatewayResourceName(wgServer.Name),
			Namespace: wgServer.Namespace,
		},
		Spec: corev1.ServiceSpec{
			Type:       corev1.ServiceTypeClusterIP,
			ClusterIP:  "10.0.0.1",
			ClusterIPs: []string{"10.0.0.1"},
			Ports: []corev1.ServicePort{
				{Port: 51820, Protocol: corev1.ProtocolUDP},
			},
		},
	}
}

func buildActivePod(wgServer *networkingv1beta1.WgGatewayServer) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forge.GatewayResourceName(wgServer.Name) + "-abc",
			Namespace: wgServer.Namespace,
			Labels: map[string]string{
				"app":                         forge.GatewayResourceName(wgServer.Name),
				leaderelection.LeaderLabelKey: leaderelection.LeaderLabelValue,
			},
		},
		Spec: corev1.PodSpec{
			Containers: []corev1.Container{
				{Name: "wireguard", Image: "liqo/gateway-wireguard:latest"},
			},
		},
		Status: corev1.PodStatus{
			Phase: corev1.PodRunning,
			PodIP: "10.244.1.10",
		},
	}
}

func buildSecret(wgServer *networkingv1beta1.WgGatewayServer) *corev1.Secret {
	return &corev1.Secret{
		ObjectMeta: metav1.ObjectMeta{
			Name:      wgServer.Spec.SecretRef.Name,
			Namespace: wgServer.Namespace,
			Labels: map[string]string{
				consts.RemoteClusterID:        wgServer.Labels[consts.RemoteClusterID],
				consts.GatewayResourceLabel:   consts.GatewayResourceLabelValue,
				consts.NetworkingComponentKey: gateway.GatewayComponentGateway,
			},
		},
		Data: map[string][]byte{
			consts.PrivateKeyField: []byte("private-key"),
			consts.PublicKeyField:  []byte("public-key"),
		},
	}
}

var _ = Describe("WgGatewayServer controller serialization gate", Ordered, func() {
	const remoteClusterID = "remote"

	var (
		ctx      context.Context
		cl       client.Client
		r        *WgGatewayServerReconciler
		wgA      *networkingv1beta1.WgGatewayServer
		wgB      *networkingv1beta1.WgGatewayServer
		depA     *appsv1.Deployment
		depB     *appsv1.Deployment
		updatedA appsv1.Deployment
	)

	BeforeAll(func() {
		ctx = context.Background()

		wgA = buildWgGatewayServer("a", "test-a", remoteClusterID)
		wgB = buildWgGatewayServer("b", "test-b", remoteClusterID)
		depA = buildExistingDeployment(wgA)
		depB = buildExistingDeployment(wgB)

		cl = newIntegrationClient(
			wgA, wgB,
			depA, depB,
			buildService(wgA), buildService(wgB),
			buildActivePod(wgA), buildActivePod(wgB),
			buildSecret(wgA), buildSecret(wgB),
		)

		recorder := record.NewFakeRecorder(100)
		r = NewWgGatewayServerReconciler(cl, cl.Scheme(), recorder, "liqo-gateway")
	})

	It("allows the lowest-named peer to proceed and update its Deployment", func() {
		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wgA.Namespace, Name: wgA.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		Expect(cl.Get(ctx, client.ObjectKeyFromObject(depA), &updatedA)).To(Succeed())
		Expect(updatedA.Spec.Template.Spec.Containers[0].Env[0].Value).To(Equal("new"))
		Expect(updatedA.Labels).To(HaveKey(consts.TemplateNameLabelKey))
		Expect(updatedA.Labels[consts.TemplateNameLabelKey]).To(Equal("server-template"))
		Expect(updatedA.Labels).To(HaveKey(consts.TemplateNamespaceLabelKey))
		Expect(updatedA.Labels[consts.TemplateNamespaceLabelKey]).To(Equal("server-template-ns"))
		Expect(updatedA.Annotations).To(HaveKey(consts.TemplateGenerationAnnotationKey))
		Expect(updatedA.Annotations[consts.TemplateGenerationAnnotationKey]).To(Equal("2"))
	})

	It("delays the higher-named peer while the lower-named peer is rolling out", func() {
		// Simulate what a real apiserver would do: a spec update increments Generation,
		// while ObservedGeneration stays at 1 until the Deployment controller observes
		// the new spec. This makes gw-a appear to be rolling out, so gw-b must delay.
		updatedA.Generation = 2
		Expect(cl.Update(ctx, &updatedA)).To(Succeed())

		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wgB.Namespace, Name: wgB.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeNumerically(">", 0))

		var unchangedB appsv1.Deployment
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(depB), &unchangedB)).To(Succeed())
		Expect(unchangedB.Spec.Template.Spec.Containers[0].Env[0].Value).To(Equal("old"))
	})

	It("allows the higher-named peer to roll after the lower-named peer finishes", func() {
		updatedA.Status.ObservedGeneration = updatedA.Generation
		updatedA.Status.UpdatedReplicas = 1
		updatedA.Status.AvailableReplicas = 1
		updatedA.Status.UnavailableReplicas = 0
		Expect(cl.Status().Update(ctx, &updatedA)).To(Succeed())

		res, err := r.Reconcile(ctx, ctrl.Request{NamespacedName: types.NamespacedName{Namespace: wgB.Namespace, Name: wgB.Name}})
		Expect(err).NotTo(HaveOccurred())
		Expect(res.RequeueAfter).To(BeZero())

		var updatedB appsv1.Deployment
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(depB), &updatedB)).To(Succeed())
		Expect(updatedB.Spec.Template.Spec.Containers[0].Env[0].Value).To(Equal("new"))
	})
})
