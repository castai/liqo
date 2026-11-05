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
	"fmt"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
)

// SetReconciler acknowledges a ShadowNetworkPolicySet inventory once every member policy is
// applied and every obsolete generated policy has disappeared.
type SetReconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder
}

// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicysets,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicysets/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicies,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch

// Reconcile evaluates the inventory readiness.
func (r *SetReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var set offloadingv1beta1.ShadowNetworkPolicySet
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	status := set.Status
	status.ObservedInventoryGeneration = set.Generation

	ready, reason, message := r.evaluate(ctx, &set)
	status.ObservedRevision = ""
	if ready {
		status.ObservedRevision = set.Spec.Revision
	}
	state := metav1.ConditionFalse
	if ready {
		state = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               string(offloadingv1beta1.ShadowNetworkPolicySetConditionReady),
		Status:             state,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: set.Generation,
	})

	if !equality.Semantic.DeepEqual(set.Status, status) {
		original := set.DeepCopy()
		set.Status = status
		if err := r.Status().Patch(ctx, &set, client.MergeFrom(original)); err != nil {
			klog.Errorf("unable to update the status of shadownetworkpolicyset %q: %v", klog.KObj(&set), err)
			return ctrl.Result{}, err
		}
	}

	if !ready {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

// evaluate returns whether the inventory is ready, with a reason and message.
func (r *SetReconciler) evaluate(ctx context.Context, set *offloadingv1beta1.ShadowNetworkPolicySet) (bool, string, string) {
	if set.Spec.Phase != offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted {
		return false, "StagingInventory", "the inventory is still being staged"
	}

	expected := make(map[string]offloadingv1beta1.ShadowNetworkPolicySetMember, len(set.Spec.Policies))
	for i := range set.Spec.Policies {
		expected[set.Spec.Policies[i].Name] = set.Spec.Policies[i]
	}

	for i := range set.Spec.Policies {
		member := set.Spec.Policies[i]
		var shadow offloadingv1beta1.ShadowNetworkPolicy
		if err := r.Get(ctx, types.NamespacedName{Namespace: set.Namespace, Name: member.Name}, &shadow); err != nil {
			if errors.IsNotFound(err) {
				return false, "MissingMember", fmt.Sprintf("the shadow policy %q does not exist", member.Name)
			}
			return false, "ReadError", fmt.Sprintf("unable to read the shadow policy %q: %v", member.Name, err)
		}
		if shadow.Spec.Source.UID != member.SourceUID {
			return false, "MemberReplaced", fmt.Sprintf("the shadow policy %q has a different source UID", member.Name)
		}
		if shadow.Spec.InputRevision != member.InputRevision {
			return false, "StaleInput", fmt.Sprintf("the shadow policy %q has input revision %q, expected %q",
				member.Name, shadow.Spec.InputRevision, member.InputRevision)
		}
		if shadow.Status.ObservedGeneration != shadow.Generation || shadow.Status.ObservedInputRevision != member.InputRevision {
			return false, "StaleStatus", fmt.Sprintf("the status of the shadow policy %q is not up to date", member.Name)
		}
		for _, conditionType := range []offloadingv1beta1.ShadowNetworkPolicyConditionType{
			offloadingv1beta1.ShadowNetworkPolicyConditionAccepted,
			offloadingv1beta1.ShadowNetworkPolicyConditionResolved,
			offloadingv1beta1.ShadowNetworkPolicyConditionApplied,
		} {
			condition := meta.FindStatusCondition(shadow.Status.Conditions, string(conditionType))
			if condition == nil || condition.Status != metav1.ConditionTrue || condition.ObservedGeneration != shadow.Generation {
				return false, "DegradedMember", fmt.Sprintf("the shadow policy %q has condition %s=%s",
					member.Name, conditionType, conditionStatus(condition))
			}
		}
		if shadow.Status.GeneratedPolicy == nil {
			return false, "MissingGeneratedPolicy", fmt.Sprintf("the shadow policy %q has not reported a generated policy", member.Name)
		}
		var generated networkingv1.NetworkPolicy
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: shadow.Status.GeneratedPolicy.Namespace,
			Name:      shadow.Status.GeneratedPolicy.Name,
		}, &generated); err != nil {
			if errors.IsNotFound(err) {
				return false, "MissingGeneratedPolicy", fmt.Sprintf("the generated policy for %q does not exist", member.Name)
			}
			return false, "ReadError", fmt.Sprintf("unable to read the generated policy for %q: %v", member.Name, err)
		}
		owner := metav1.GetControllerOf(&generated)
		if owner == nil || owner.UID != shadow.UID {
			return false, "OwnershipConflict", fmt.Sprintf("the generated policy for %q is not owned by its shadow", member.Name)
		}
	}

	if set.Spec.SourceNamespaceUID != "" {
		var policies networkingv1.NetworkPolicyList
		if err := r.List(ctx, &policies, client.InNamespace(set.Namespace), client.MatchingLabels{
			consts.ManagedByLabelKey:              consts.ManagedByShadowNetworkPolicyValue,
			consts.LiqoOriginNamespaceUIDLabelKey: string(set.Spec.SourceNamespaceUID),
		}); err != nil {
			return false, "ReadError", fmt.Sprintf("unable to list the generated policies: %v", err)
		}
		for i := range policies.Items {
			if _, found := expected[policies.Items[i].Name]; !found {
				return false, "ObsoletePolicyPresent", fmt.Sprintf("the obsolete generated policy %q is still present", policies.Items[i].Name)
			}
		}
	}

	return true, "InventoryReady", fmt.Sprintf("the inventory %q is acknowledged", set.Spec.Revision)
}

func conditionStatus(condition *metav1.Condition) string {
	if condition == nil {
		return "Missing"
	}
	return string(condition.Status)
}

// SetupWithManager sets up the inventory controller.
func (r *SetReconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, workers int) error {
	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlShadowNetworkPolicySet).
		For(&offloadingv1beta1.ShadowNetworkPolicySet{}).
		Watches(&offloadingv1beta1.ShadowNetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(r.setsInNamespace)).
		Watches(&networkingv1.NetworkPolicy{}, handler.EnqueueRequestsFromMapFunc(r.setsInNamespace)).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Complete(r)
}

func (r *SetReconciler) setsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	var sets offloadingv1beta1.ShadowNetworkPolicySetList
	if err := r.List(ctx, &sets, client.InNamespace(obj.GetNamespace())); err != nil {
		klog.Errorf("unable to list ShadowNetworkPolicySets: %v", err)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(sets.Items))
	for i := range sets.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Namespace: sets.Items[i].Namespace, Name: sets.Items[i].Name},
		})
	}
	return requests
}

// Gate checks whether a source namespace inventory has been acknowledged by the provider.
type Gate struct {
	client.Client
}

// Check returns whether new ShadowPods of the given source namespace may be materialized.
func (g *Gate) Check(ctx context.Context, namespace string, sourceClusterID string, sourceNamespace string) (bool, string, error) {
	if sourceClusterID == "" || sourceNamespace == "" {
		return false, "MissingSourceIdentity", nil
	}

	var sets offloadingv1beta1.ShadowNetworkPolicySetList
	if err := g.List(ctx, &sets, client.InNamespace(namespace), client.MatchingLabels{
		forge.LiqoOriginClusterIDKey: sourceClusterID,
	}); err != nil {
		return false, "ReadError", err
	}

	var matched []*offloadingv1beta1.ShadowNetworkPolicySet
	for i := range sets.Items {
		set := &sets.Items[i]
		if string(set.Spec.SourceClusterID) != sourceClusterID {
			continue
		}
		if set.Spec.SourceNamespace != sourceNamespace {
			continue
		}
		matched = append(matched, set)
	}

	switch len(matched) {
	case 0:
		return false, "MissingInventory", nil
	case 1:
	default:
		return false, "AmbiguousInventory", nil
	}

	set := matched[0]
	if set.Spec.Phase != offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted {
		return false, "StagingInventory", nil
	}
	ready := meta.FindStatusCondition(set.Status.Conditions, string(offloadingv1beta1.ShadowNetworkPolicySetConditionReady))
	if ready == nil || ready.Status != metav1.ConditionTrue {
		return false, "InventoryNotReady", nil
	}
	if set.Status.ObservedInventoryGeneration != set.Generation || set.Status.ObservedRevision != set.Spec.Revision {
		return false, "StaleInventory", nil
	}
	return true, "InventoryReady", nil
}
