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
	"time"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	networkingutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/utils"
	"github.com/liqotech/liqo/pkg/utils/getters"
	"github.com/liqotech/liqo/pkg/utils/networkpolicy"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
)

const (
	// requeueAfter is the fallback resync for transient conditions and for readiness checks
	// waiting for garbage collection.
	requeueAfter = 30 * time.Second
)

// Reconciler reconciles a ShadowNetworkPolicy object into a provider NetworkPolicy.
type Reconciler struct {
	client.Client
	Scheme   *runtime.Scheme
	Recorder record.EventRecorder

	// Limits bounds the generated policy output.
	Limits networkpolicy.Limits
}

// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicies/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=namespacemaps,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations,verbs=get;list;watch
// +kubebuilder:rbac:groups="",resources=namespaces,verbs=get;list;watch

// Reconcile translates a ShadowNetworkPolicy into a provider NetworkPolicy.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var shadow offloadingv1beta1.ShadowNetworkPolicy
	if err := r.Get(ctx, req.NamespacedName, &shadow); err != nil {
		if errors.IsNotFound(err) {
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, err
	}

	status := shadow.Status
	status.ObservedGeneration = shadow.Generation
	status.ObservedInputRevision = shadow.Spec.InputRevision

	// The shadow is accepted only when the namespace is a provider-owned mapping for the claimed
	// consumer namespace. This prevents a tenant from inducing writes in another tenant namespace.
	if diag := r.validateNamespace(ctx, &shadow); diag != nil {
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionAccepted, false, string(diag.Reason), diag.Message, shadow.Generation)
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionResolved, false, string(diag.Reason), diag.Message, shadow.Generation)
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionApplied, false, string(diag.Reason), "no policy was written", shadow.Generation)
		return r.finish(ctx, &shadow, &status, diag, false)
	}
	setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionAccepted, true, "Accepted",
		"the shadow input is accepted for the authenticated tenant namespace", shadow.Generation)

	desired, resolved, diag, topology := r.compile(ctx, &shadow)
	status.DestinationTopologyDigest = topology

	if diag != nil {
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionResolved, false, string(diag.Reason), diag.Message, shadow.Generation)
	} else {
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionResolved, resolved, "Resolved",
			"every source peer has been translated", shadow.Generation)
	}

	ref, applyDiag := r.applyNetworkPolicy(ctx, &shadow, desired)
	if applyDiag != nil {
		diag = applyDiag
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionApplied, false, string(applyDiag.Reason),
			applyDiag.Message, shadow.Generation)
	} else {
		status.GeneratedPolicy = ref
		setCondition(&status, offloadingv1beta1.ShadowNetworkPolicyConditionApplied, true, "Applied",
			"the generated policy exists and matches the desired spec", shadow.Generation)
	}

	return r.finish(ctx, &shadow, &status, diag, applyDiag == nil)
}

func (r *Reconciler) compile(ctx context.Context, shadow *offloadingv1beta1.ShadowNetworkPolicy) (
	networkingv1.NetworkPolicySpec, bool, *networkpolicy.Diagnostic, string) {
	if shadow.Spec.InputError != nil {
		diag := &networkpolicy.Diagnostic{
			Reason:  networkpolicy.Reason(shadow.Spec.InputError.Reason),
			Field:   shadow.Spec.InputError.Field,
			Message: shadow.Spec.InputError.Message,
		}
		return networkpolicy.IsolationSpec(shadow.Spec.Template), false, diag, ""
	}
	return r.compileWithConfiguration(ctx, shadow)
}

func (r *Reconciler) compileWithConfiguration(ctx context.Context,
	shadow *offloadingv1beta1.ShadowNetworkPolicy) (networkingv1.NetworkPolicySpec, bool, *networkpolicy.Diagnostic, string) {
	cfg, err := getters.GetConfigurationByClusterID(ctx, r.Client, shadow.Spec.Source.ClusterID, corev1.NamespaceAll)
	if err != nil {
		if !errors.IsNotFound(err) {
			klog.Errorf("unable to get the Configuration for cluster %q: %v", shadow.Spec.Source.ClusterID, err)
		}
		cfg = nil
	}

	input, buildDiag, topology := BuildInput(shadow, cfg)
	if buildDiag != nil {
		return networkpolicy.IsolationSpec(shadow.Spec.Template), false, buildDiag, topology
	}
	result := networkpolicy.Compile(InputWithLimits(input, r.Limits))
	return result.Spec, result.Resolved, result.Diagnostic, topology
}

// InputWithLimits overrides the compiler limits.
func InputWithLimits(input networkpolicy.Input, limits networkpolicy.Limits) networkpolicy.Input {
	input.Limits = limits
	return input
}

// finish persists the status and reports events and requeue policy.
func (r *Reconciler) finish(ctx context.Context, shadow *offloadingv1beta1.ShadowNetworkPolicy,
	status *offloadingv1beta1.ShadowNetworkPolicyStatus, diag *networkpolicy.Diagnostic, applied bool) (ctrl.Result, error) {
	if err := r.persistStatus(ctx, shadow, status); err != nil {
		return ctrl.Result{}, err
	}

	if diag == nil {
		return ctrl.Result{}, nil
	}

	if r.Recorder != nil {
		eventType := corev1.EventTypeWarning
		if !applied {
			eventType = corev1.EventTypeWarning
		}
		r.Recorder.Event(shadow, eventType, string(diag.Reason), diag.Message)
	}
	if diag.Temporary {
		return ctrl.Result{RequeueAfter: requeueAfter}, nil
	}
	return ctrl.Result{}, nil
}

func (r *Reconciler) persistStatus(ctx context.Context, shadow *offloadingv1beta1.ShadowNetworkPolicy,
	status *offloadingv1beta1.ShadowNetworkPolicyStatus) error {
	status.ObservedGeneration = shadow.Generation
	if equality.Semantic.DeepEqual(shadow.Status, *status) {
		return nil
	}
	original := shadow.DeepCopy()
	shadow.Status = *status
	if err := r.Status().Patch(ctx, shadow, client.MergeFrom(original)); err != nil {
		klog.Errorf("unable to update the status of shadownetworkpolicy %q: %v", klog.KObj(shadow), err)
		return err
	}
	return nil
}

func (r *Reconciler) validateNamespace(ctx context.Context, shadow *offloadingv1beta1.ShadowNetworkPolicy) *networkpolicy.Diagnostic {
	if shadow.Spec.Source.Namespace == "" || shadow.Spec.Source.Name == "" {
		return &networkpolicy.Diagnostic{
			Reason:  networkpolicy.ReasonInvalidInput,
			Field:   "spec.source",
			Message: "the source namespace and policy name must be set",
		}
	}

	var namespace corev1.Namespace
	if err := r.Get(ctx, types.NamespacedName{Name: shadow.Namespace}, &namespace); err != nil {
		if errors.IsNotFound(err) {
			return &networkpolicy.Diagnostic{
				Reason:    networkpolicy.Reason("WaitingForNamespaceMapping"),
				Field:     "metadata.namespace",
				Message:   "the destination namespace does not exist",
				Temporary: true,
			}
		}
		return &networkpolicy.Diagnostic{
			Reason:    networkpolicy.ReasonApplyFailed,
			Field:     "metadata.namespace",
			Message:   fmt.Sprintf("unable to read the destination namespace: %v", err),
			Temporary: true,
		}
	}

	nmID, ok := namespace.Annotations[consts.RemoteNamespaceManagedByAnnotationKey]
	if !ok {
		return &networkpolicy.Diagnostic{
			Reason:    networkpolicy.Reason("WaitingForNamespaceMapping"),
			Field:     "metadata.namespace",
			Message:   "the destination namespace is not managed by a Liqo NamespaceMap",
			Temporary: true,
		}
	}
	originalName, ok := namespace.Annotations[consts.RemoteNamespaceOriginalNameAnnotationKey]
	if !ok || originalName != shadow.Spec.Source.Namespace {
		return &networkpolicy.Diagnostic{
			Reason:  networkpolicy.Reason("UnauthorizedNamespace"),
			Field:   "spec.source.namespace",
			Message: "the source namespace does not match the destination namespace mapping",
		}
	}
	if namespace.Labels[consts.RemoteClusterID] != string(shadow.Spec.Source.ClusterID) {
		return &networkpolicy.Diagnostic{
			Reason:  networkpolicy.Reason("UnauthorizedNamespace"),
			Field:   "spec.source.clusterID",
			Message: "the source cluster does not match the destination namespace mapping",
		}
	}

	nmNamespace, nmName, err := cache.SplitMetaNamespaceKey(nmID)
	if err != nil {
		return &networkpolicy.Diagnostic{
			Reason:  networkpolicy.Reason("UnauthorizedNamespace"),
			Field:   "metadata.namespace",
			Message: fmt.Sprintf("invalid NamespaceMap reference %q", nmID),
		}
	}
	var namespaceMap offloadingv1beta1.NamespaceMap
	if err := r.Get(ctx, types.NamespacedName{Namespace: nmNamespace, Name: nmName}, &namespaceMap); err != nil {
		return &networkpolicy.Diagnostic{
			Reason:    networkpolicy.Reason("WaitingForNamespaceMapping"),
			Field:     "metadata.namespace",
			Message:   fmt.Sprintf("the managing NamespaceMap is not available: %v", err),
			Temporary: true,
		}
	}
	mapping, ok := namespaceMap.Status.CurrentMapping[originalName]
	if !ok || mapping.Phase != offloadingv1beta1.MappingAccepted || mapping.RemoteNamespace != shadow.Namespace {
		return &networkpolicy.Diagnostic{
			Reason:    networkpolicy.Reason("WaitingForNamespaceMapping"),
			Field:     "metadata.namespace",
			Message:   "the namespace mapping is not accepted yet",
			Temporary: true,
		}
	}

	return nil
}

func (r *Reconciler) applyNetworkPolicy(ctx context.Context, shadow *offloadingv1beta1.ShadowNetworkPolicy,
	desired networkingv1.NetworkPolicySpec) (*corev1.ObjectReference, *networkpolicy.Diagnostic) {
	key := types.NamespacedName{Namespace: shadow.Namespace, Name: shadow.Name}

	var existing networkingv1.NetworkPolicy
	err := r.Get(ctx, key, &existing)
	switch {
	case err == nil:
		owner := metav1.GetControllerOf(&existing)
		if owner == nil || owner.UID != shadow.UID {
			return nil, &networkpolicy.Diagnostic{
				Reason:  networkpolicy.ReasonOwnershipConflict,
				Field:   "metadata.ownerReferences",
				Message: fmt.Sprintf("the existing NetworkPolicy %q is not owned by this shadow", key.Name),
			}
		}
		// Replace the whole spec with a resource-version-checked update, so that field removals
		// (e.g. an allow list that became empty) are applied deterministically.
		updated := existing.DeepCopy()
		updated.Spec = desired
		updated.Labels = policyLabels(shadow)
		if err := r.Update(ctx, updated); err != nil {
			return nil, &networkpolicy.Diagnostic{
				Reason:    networkpolicy.ReasonApplyFailed,
				Message:   fmt.Sprintf("unable to update the generated policy; stale rules may remain active: %v", err),
				Temporary: true,
			}
		}
	case errors.IsNotFound(err):
		created := &networkingv1.NetworkPolicy{
			ObjectMeta: metav1.ObjectMeta{
				Name:      shadow.Name,
				Namespace: shadow.Namespace,
				Labels:    policyLabels(shadow),
			},
			Spec: desired,
		}
		if err := ctrl.SetControllerReference(shadow, created, r.Scheme); err != nil {
			return nil, &networkpolicy.Diagnostic{
				Reason:  networkpolicy.ReasonApplyFailed,
				Message: fmt.Sprintf("unable to set the owner reference: %v", err),
			}
		}
		if err := r.Create(ctx, created); err != nil {
			return nil, &networkpolicy.Diagnostic{
				Reason:    networkpolicy.ReasonApplyFailed,
				Message:   fmt.Sprintf("unable to create the generated policy: %v", err),
				Temporary: true,
			}
		}
	default:
		return nil, &networkpolicy.Diagnostic{
			Reason:    networkpolicy.ReasonApplyFailed,
			Message:   fmt.Sprintf("unable to read the existing policy: %v", err),
			Temporary: true,
		}
	}

	var persisted networkingv1.NetworkPolicy
	if err := r.Get(ctx, key, &persisted); err != nil {
		return nil, &networkpolicy.Diagnostic{
			Reason:    networkpolicy.ReasonApplyFailed,
			Message:   fmt.Sprintf("unable to verify the generated policy; stale rules may remain active: %v", err),
			Temporary: true,
		}
	}
	owner := metav1.GetControllerOf(&persisted)
	if owner == nil || owner.UID != shadow.UID {
		return nil, &networkpolicy.Diagnostic{
			Reason:  networkpolicy.ReasonOwnershipConflict,
			Message: "the generated policy is not owned by this shadow",
		}
	}
	if !equality.Semantic.DeepEqual(persisted.Spec, desired) {
		return nil, &networkpolicy.Diagnostic{
			Reason:    networkpolicy.ReasonApplyFailed,
			Message:   "the persisted policy spec does not match the desired spec; stale rules may remain active",
			Temporary: true,
		}
	}

	return &corev1.ObjectReference{
		APIVersion: networkingv1.SchemeGroupVersion.String(),
		Kind:       "NetworkPolicy",
		Name:       persisted.Name,
		Namespace:  persisted.Namespace,
		UID:        persisted.UID,
	}, nil
}

func policyLabels(shadow *offloadingv1beta1.ShadowNetworkPolicy) map[string]string {
	out := map[string]string{
		consts.ManagedByLabelKey:           consts.ManagedByShadowNetworkPolicyValue,
		forge.LiqoOriginClusterIDKey:       string(shadow.Spec.Source.ClusterID),
		consts.LiqoOriginNamespaceLabelKey: shadow.Spec.Source.Namespace,
	}
	if shadow.Spec.Source.NamespaceUID != "" {
		out[consts.LiqoOriginNamespaceUIDLabelKey] = string(shadow.Spec.Source.NamespaceUID)
	}
	return out
}

func setCondition(status *offloadingv1beta1.ShadowNetworkPolicyStatus, conditionType offloadingv1beta1.ShadowNetworkPolicyConditionType,
	conditionStatus bool, reason, message string, generation int64) {
	state := metav1.ConditionFalse
	if conditionStatus {
		state = metav1.ConditionTrue
	}
	meta.SetStatusCondition(&status.Conditions, metav1.Condition{
		Type:               string(conditionType),
		Status:             state,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: generation,
	})
}

// SetupWithManager sets up the controller with the Manager.
func (r *Reconciler) SetupWithManager(ctx context.Context, mgr ctrl.Manager, workers int) error {
	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlShadowNetworkPolicy).
		For(&offloadingv1beta1.ShadowNetworkPolicy{}).
		Owns(&networkingv1.NetworkPolicy{}).
		Watches(&networkingv1beta1.Configuration{},
			handler.EnqueueRequestsFromMapFunc(r.shadowsForConfiguration),
			builder.WithPredicates(networkingutils.AreConfigurationNetworkCIDRsConfiguredPredicate())).
		Watches(&corev1.Namespace{}, handler.EnqueueRequestsFromMapFunc(r.shadowsInNamespace)).
		WithOptions(controller.Options{MaxConcurrentReconciles: workers}).
		Complete(r)
}

func (r *Reconciler) shadowsForConfiguration(ctx context.Context, obj client.Object) []reconcile.Request {
	cfg, ok := obj.(*networkingv1beta1.Configuration)
	if !ok {
		return nil
	}
	clusterID, ok := cfg.Labels[consts.RemoteClusterID]
	if !ok {
		return nil
	}
	return r.listShadows(ctx, client.MatchingLabels{forge.LiqoOriginClusterIDKey: clusterID})
}

func (r *Reconciler) shadowsInNamespace(ctx context.Context, obj client.Object) []reconcile.Request {
	return r.listShadows(ctx, client.InNamespace(obj.GetNamespace()))
}

func (r *Reconciler) listShadows(ctx context.Context, opts ...client.ListOption) []reconcile.Request {
	var list offloadingv1beta1.ShadowNetworkPolicyList
	if err := r.List(ctx, &list, opts...); err != nil {
		klog.Errorf("unable to list ShadowNetworkPolicies: %v", err)
		return nil
	}
	requests := make([]reconcile.Request, 0, len(list.Items))
	for i := range list.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{Name: list.Items[i].Name, Namespace: list.Items[i].Namespace},
		})
	}
	return requests
}
