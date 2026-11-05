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

package shadownetworkpolicyctrl

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/utils/networkpolicy"
)

func newScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	for _, add := range []func(*runtime.Scheme) error{
		corev1.AddToScheme,
		networkingv1.AddToScheme,
		offloadingv1beta1.AddToScheme,
		networkingv1beta1.AddToScheme,
	} {
		if err := add(scheme); err != nil {
			t.Fatalf("adding scheme: %v", err)
		}
	}
	return scheme
}

func destinationNamespace() *corev1.Namespace {
	return &corev1.Namespace{
		ObjectMeta: metav1.ObjectMeta{
			Name: "dest",
			Labels: map[string]string{
				consts.RemoteClusterID: "consumer",
			},
			Annotations: map[string]string{
				consts.RemoteNamespaceManagedByAnnotationKey:    "tenant-ns/nsmap",
				consts.RemoteNamespaceOriginalNameAnnotationKey: "source",
			},
		},
	}
}

func acceptedNamespaceMap() *offloadingv1beta1.NamespaceMap {
	return &offloadingv1beta1.NamespaceMap{
		ObjectMeta: metav1.ObjectMeta{Name: "nsmap", Namespace: "tenant-ns"},
		Status: offloadingv1beta1.NamespaceMapStatus{
			CurrentMapping: map[string]offloadingv1beta1.RemoteNamespaceStatus{
				"source": {RemoteNamespace: "dest", Phase: offloadingv1beta1.MappingAccepted},
			},
		},
	}
}

func TestReconcileAppliesTranslatedPolicy(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "10.20.7.9/32"},
	}))

	reconciler := &Reconciler{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithStatusSubresource(&offloadingv1beta1.ShadowNetworkPolicy{}).
			WithObjects(shadow, destinationNamespace(), acceptedNamespaceMap(), readyConfiguration()).
			Build(),
		Scheme: newScheme(t),
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "dest", Name: "shadow"},
	}); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	var generated networkingv1.NetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &generated); err != nil {
		t.Fatalf("reading the generated policy: %v", err)
	}
	if len(generated.Spec.Egress) != 1 || len(generated.Spec.Egress[0].To) != 1 {
		t.Fatalf("unexpected generated policy: %#v", generated.Spec)
	}
	if generated.Spec.Egress[0].To[0].IPBlock.CIDR != "172.20.7.9/32" {
		t.Fatalf("expected the remapped peer, got %v", generated.Spec.Egress[0].To)
	}
	owner := metav1.GetControllerOf(&generated)
	if owner == nil || owner.UID != shadow.UID {
		t.Fatalf("expected the generated policy to be owned by the shadow")
	}

	var persisted offloadingv1beta1.ShadowNetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &persisted); err != nil {
		t.Fatalf("reading the shadow: %v", err)
	}
	for _, conditionType := range []offloadingv1beta1.ShadowNetworkPolicyConditionType{
		offloadingv1beta1.ShadowNetworkPolicyConditionAccepted,
		offloadingv1beta1.ShadowNetworkPolicyConditionResolved,
		offloadingv1beta1.ShadowNetworkPolicyConditionApplied,
	} {
		condition := meta.FindStatusCondition(persisted.Status.Conditions, string(conditionType))
		if condition == nil || condition.Status != metav1.ConditionTrue {
			t.Fatalf("expected condition %s to be true, got %v", conditionType, condition)
		}
	}
}

func TestReconcileUnsupportedPeerPublishesIsolation(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		PodSelector: &metav1.LabelSelector{MatchLabels: map[string]string{"app": "other"}},
	}))

	reconciler := &Reconciler{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithStatusSubresource(&offloadingv1beta1.ShadowNetworkPolicy{}).
			WithObjects(shadow, destinationNamespace(), acceptedNamespaceMap(), readyConfiguration()).
			Build(),
		Scheme: newScheme(t),
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "dest", Name: "shadow"},
	}); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	var generated networkingv1.NetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &generated); err != nil {
		t.Fatalf("reading the generated policy: %v", err)
	}
	if generated.Spec.Egress != nil {
		t.Fatalf("expected an isolation-only policy, got %v", generated.Spec.Egress)
	}
	if len(generated.Spec.PolicyTypes) != 1 || generated.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
		t.Fatalf("expected the policy types to be preserved, got %v", generated.Spec.PolicyTypes)
	}

	var persisted offloadingv1beta1.ShadowNetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &persisted); err != nil {
		t.Fatalf("reading the shadow: %v", err)
	}
	applied := meta.FindStatusCondition(persisted.Status.Conditions, string(offloadingv1beta1.ShadowNetworkPolicyConditionApplied))
	if applied == nil || applied.Status != metav1.ConditionTrue {
		t.Fatalf("expected the isolation policy to be applied, got %v", applied)
	}
	resolved := meta.FindStatusCondition(persisted.Status.Conditions, string(offloadingv1beta1.ShadowNetworkPolicyConditionResolved))
	if resolved == nil || resolved.Status != metav1.ConditionFalse {
		t.Fatalf("expected the policy not to be resolved, got %v", resolved)
	}
}

func TestReconcileRejectsForeignNamespace(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "10.20.7.9/32"},
	}))
	shadow.Spec.Source.Namespace = "another-source"

	reconciler := &Reconciler{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithStatusSubresource(&offloadingv1beta1.ShadowNetworkPolicy{}).
			WithObjects(shadow, destinationNamespace(), acceptedNamespaceMap(), readyConfiguration()).
			Build(),
		Scheme: newScheme(t),
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "dest", Name: "shadow"},
	}); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	var generated networkingv1.NetworkPolicy
	err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &generated)
	if err == nil {
		t.Fatalf("expected no policy to be written for a foreign namespace")
	}
	if !errors.IsNotFound(err) {
		t.Fatalf("unexpected error reading the generated policy: %v", err)
	}

	var persisted offloadingv1beta1.ShadowNetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &persisted); err != nil {
		t.Fatalf("reading the shadow: %v", err)
	}
	accepted := meta.FindStatusCondition(persisted.Status.Conditions, string(offloadingv1beta1.ShadowNetworkPolicyConditionAccepted))
	if accepted == nil || accepted.Status != metav1.ConditionFalse {
		t.Fatalf("expected the shadow to be rejected, got %v", accepted)
	}
}

func TestReconcileRefusesToAdoptForeignPolicy(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "10.20.7.9/32"},
	}))
	foreign := &networkingv1.NetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "shadow",
			Namespace: "dest",
			OwnerReferences: []metav1.OwnerReference{{
				APIVersion: "offloading.liqo.io/v1beta1",
				Kind:       "ShadowNetworkPolicy",
				Name:       "other",
				UID:        types.UID("other-uid"),
			}},
		},
		Spec: networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{},
		},
	}

	reconciler := &Reconciler{
		Client: fake.NewClientBuilder().
			WithScheme(newScheme(t)).
			WithStatusSubresource(&offloadingv1beta1.ShadowNetworkPolicy{}).
			WithObjects(shadow, destinationNamespace(), acceptedNamespaceMap(), readyConfiguration(), foreign).
			Build(),
		Scheme: newScheme(t),
	}

	if _, err := reconciler.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: "dest", Name: "shadow"},
	}); err != nil {
		t.Fatalf("unexpected reconcile error: %v", err)
	}

	var persisted networkingv1.NetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &persisted); err != nil {
		t.Fatalf("reading the policy: %v", err)
	}
	if len(persisted.Spec.Egress) != 0 {
		t.Fatalf("expected the foreign policy to be untouched")
	}

	var shadowState offloadingv1beta1.ShadowNetworkPolicy
	if err := reconciler.Client.Get(context.Background(),
		types.NamespacedName{Namespace: "dest", Name: "shadow"}, &shadowState); err != nil {
		t.Fatalf("reading the shadow: %v", err)
	}
	applied := meta.FindStatusCondition(shadowState.Status.Conditions, string(offloadingv1beta1.ShadowNetworkPolicyConditionApplied))
	if applied == nil || applied.Status != metav1.ConditionFalse || applied.Reason != string(networkpolicy.ReasonOwnershipConflict) {
		t.Fatalf("expected an ownership conflict, got %v", applied)
	}
}
