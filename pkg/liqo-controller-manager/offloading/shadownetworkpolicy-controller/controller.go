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

// Package shadownetworkpolicy reconciles reflected policy inventories.
package shadownetworkpolicy

import (
	"context"
	"fmt"
	"net/netip"
	"strings"
	"time"

	netutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/utils"
	"k8s.io/apimachinery/pkg/api/equality"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	ipamapi "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	netapi "github.com/liqotech/liqo/apis/networking/v1beta1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/utils/networkpolicy"
)

const (
	managedLabel = "offloading.liqo.io/reflected-networkpolicy"
)

// Reconciler translates the namespace inventory using current provider mappings.
type Reconciler struct {
	client.Client
	APIReader     client.Reader
	Scheme        *runtime.Scheme
	PodCIDRs      []string
	LiqoNamespace string
}

// +kubebuilder:rbac:groups=networking.k8s.io,resources=networkpolicies,verbs=get;list;watch;create;update;patch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicies;shadownetworkpolicysets,verbs=get;list;watch
// +kubebuilder:rbac:groups=offloading.liqo.io,resources=shadownetworkpolicies/status;shadownetworkpolicysets/status,verbs=get;patch;update

// SetupWithManager registers the controller.
func (r *Reconciler) SetupWithManager(mgr ctrl.Manager) error {
	enqueue := handler.EnqueueRequestsFromMapFunc(func(_ context.Context, obj client.Object) []ctrl.Request {
		return []ctrl.Request{{NamespacedName: client.ObjectKey{Namespace: obj.GetNamespace(), Name: networkpolicy.InventoryName}}}
	})
	return ctrl.NewControllerManagedBy(mgr).Named("shadow-network-policy-set").
		For(&api.ShadowNetworkPolicySet{}).
		Watches(&api.ShadowNetworkPolicy{}, enqueue).
		Watches(&networking.NetworkPolicy{}, enqueue).
		Complete(r)
}

// Reconcile applies current member policies and acknowledges the exact inventory.
func (r *Reconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var set api.ShadowNetworkPolicySet
	if req.Name != networkpolicy.InventoryName {
		return ctrl.Result{}, nil
	}
	if err := r.Get(ctx, req.NamespacedName, &set); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if !set.DeletionTimestamp.IsZero() {
		return ctrl.Result{}, nil
	}
	ready, err := r.reconcileInventory(ctx, &set)
	status := api.ShadowNetworkPolicySetStatus{ObservedGeneration: set.Generation, Revision: set.Spec.Revision, Ready: ready && err == nil}
	if err != nil {
		status.Message = err.Error()
	}
	if status != set.Status {
		set.Status = status
		if updateErr := r.Status().Update(ctx, &set); updateErr != nil {
			return ctrl.Result{}, updateErr
		}
	}
	if err != nil {
		return ctrl.Result{}, err
	}
	return ctrl.Result{RequeueAfter: 3 * time.Second}, nil
}

func (r *Reconciler) reconcileInventory(ctx context.Context, set *api.ShadowNetworkPolicySet) (bool, error) {
	if err := ValidateMapping(ctx, r.Client, set.Namespace, set.Spec.Source); err != nil {
		return false, fmt.Errorf("validating mapping: %w", err)
	}

	// Changes are not ready to be applied.
	if !set.Spec.Committed {
		return false, nil
	}

	ready := true
	expected := make(map[string]types.UID, len(set.Spec.Members))
	for _, member := range set.Spec.Members {
		var shadow api.ShadowNetworkPolicy
		if err := r.Get(ctx, client.ObjectKey{Namespace: set.Namespace, Name: member.Name}, &shadow); err != nil {
			if apierrors.IsNotFound(err) {
				ready = false
				continue
			}
			return false, fmt.Errorf("getting network policy %s/%s: %w", set.Namespace, member.Name, err)
		}
		if shadow.Spec.Source != set.Spec.Source || string(shadow.UID) != member.UID || shadow.Generation != member.Generation || !shadow.DeletionTimestamp.IsZero() {
			ready = false
			continue
		}
		expected[shadow.Name] = shadow.UID
		resolved, err := r.apply(ctx, &shadow)
		if err != nil {
			return false, err
		}
		ready = ready && resolved
	}
	var policies networking.NetworkPolicyList
	if err := r.List(ctx, &policies, client.InNamespace(set.Namespace)); err != nil {
		return false, err
	}
	for i := range policies.Items {
		policy := &policies.Items[i]
		owner := metav1.GetControllerOf(policy)
		if !managed(policy, owner) {
			continue
		}
		if owner == nil || expected[policy.Name] != owner.UID {
			ready = false
		}
	}
	return ready, nil
}

func (r *Reconciler) apply(ctx context.Context, shadow *api.ShadowNetworkPolicy) (bool, error) {
	spec, compileErr := r.compile(ctx, r.Client, shadow)
	policy := &networking.NetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: shadow.Name, Namespace: shadow.Namespace}}
	_, applyErr := controllerutil.CreateOrUpdate(ctx, r.Client, policy, func() error {
		if policy.UID != "" && !metav1.IsControlledBy(policy, shadow) {
			return fmt.Errorf("NetworkPolicy %s has a different owner", policy.Name)
		}
		if err := controllerutil.SetControllerReference(shadow, policy, r.Scheme); err != nil {
			return err
		}
		if policy.Labels == nil {
			policy.Labels = make(map[string]string)
		}
		policy.Labels[managedLabel] = "true"
		policy.Spec = *spec
		return nil
	})
	previous := shadow.DeepCopy()
	for _, condition := range []struct {
		name string
		err  error
	}{
		{"Accepted", nil}, {"Resolved", compileErr}, {"Applied", applyErr},
	} {
		value := metav1.Condition{Type: condition.name, Status: metav1.ConditionTrue, Reason: "Succeeded", ObservedGeneration: shadow.Generation}
		if condition.err != nil {
			value.Status = metav1.ConditionFalse
			value.Reason = "Failed"
			value.Message = condition.err.Error()
		}
		meta.SetStatusCondition(&shadow.Status.Conditions, value)
	}
	if !equality.Semantic.DeepEqual(previous.Status, shadow.Status) {
		if err := r.Status().Patch(ctx, shadow, client.MergeFromWithOptions(previous, client.MergeFromWithOptimisticLock{})); err != nil {
			return false, err
		}
	}
	return compileErr == nil && applyErr == nil, applyErr
}

func (r *Reconciler) compile(ctx context.Context, reader client.Reader, shadow *api.ShadowNetworkPolicy) (*networking.NetworkPolicySpec, error) {
	spec := networkpolicy.Isolation(&shadow.Spec.Template)
	var compileErr error
	switch {
	case shadow.Spec.InputError != "":
		compileErr = fmt.Errorf("%s", shadow.Spec.InputError)
	case len(shadow.Spec.Template.Ingress) > 0 || len(shadow.Spec.Template.Egress) > 0:
		var space networkpolicy.AddressSpace
		space, compileErr = r.addressSpace(ctx, reader, shadow)
		if compileErr == nil {
			spec, compileErr = networkpolicy.Compile(&shadow.Spec.Template, space)
		}
	}
	return spec, compileErr
}

func (r *Reconciler) addressSpace(ctx context.Context, reader client.Reader, shadow *api.ShadowNetworkPolicy) (networkpolicy.AddressSpace, error) {
	input := shadow.Spec.Context
	var addrSpace networkpolicy.AddressSpace
	for _, address := range input.PendingExports {
		ip, err := netip.ParseAddr(address)
		if err != nil || !ip.Is4() {
			return addrSpace, fmt.Errorf("invalid pending export %q", address)
		}
		addrSpace.Pending = append(addrSpace.Pending, ip)
	}
	var configs netapi.ConfigurationList

	if err := reader.List(ctx, &configs); err != nil {
		return addrSpace, err
	}
	var cfg *netapi.Configuration
	for i := range configs.Items {
		peer := &configs.Items[i]
		if peer.Labels[consts.RemoteClusterID] == shadow.Spec.Source.Cluster {
			if cfg != nil {
				return addrSpace, fmt.Errorf("multiple consumer Configurations")
			}
			cfg = peer
			continue
		}
		if peer.Status.Remote == nil {
			continue
		}
		for _, ranges := range [][]netapi.CIDR{peer.Status.Remote.CIDR.Pod, peer.Status.Remote.CIDR.External, peer.Status.TunneledCIDRs} {
			for _, cidr := range ranges {
				p, err := networkpolicy.ParsePrefix(cidr.String())
				if err != nil {
					return addrSpace, err
				}
				addrSpace.Reserved = append(addrSpace.Reserved, p)
			}
		}
	}
	if cfg == nil || !cfg.DeletionTimestamp.IsZero() || cfg.Status.Remote == nil || !netutils.AreConfigurationNetworkCIDRsConfigured(cfg) {
		return addrSpace, fmt.Errorf("consumer Configuration is not ready")
	}
	if r.LiqoNamespace != "" {
		var networks ipamapi.NetworkList
		if err := reader.List(ctx, &networks, client.InNamespace(r.LiqoNamespace)); err != nil {
			return addrSpace, err
		}
		for _, nw := range networks.Items {
			if nw.Status.CIDR == "" {
				continue
			}
			p, err := networkpolicy.ParsePrefix(nw.Status.CIDR.String())
			if err != nil {
				return addrSpace, err
			}
			addrSpace.Reserved = append(addrSpace.Reserved, p)
		}
	}
	if err := networkpolicy.AppendPrefixes(&addrSpace.Indirect, input.IndirectCIDRs); err != nil {
		return addrSpace, fmt.Errorf("appending indirect prefixes: %w", err)
	}
	if err := networkpolicy.AppendPrefixes(&addrSpace.Services, input.ServiceCIDRs); err != nil {
		return addrSpace, fmt.Errorf("appending service CIDR prefixes: %w", err)
	}
	if err := networkpolicy.AppendPrefixes(&addrSpace.Reserved, r.PodCIDRs); err != nil {
		return addrSpace, fmt.Errorf("appending reserved prefixes: %w", err)
	}
	pods, err := networkpolicy.PrefixPairs(cfg.Spec.Remote.CIDR.Pod, cfg.Status.Remote.CIDR.Pod)
	if err != nil {
		return addrSpace, err
	}
	addrSpace.Pairs = append(addrSpace.Pairs, pods...)
	for _, source := range input.LocalPodCIDRs {
		p, err := networkpolicy.ParsePrefix(source)
		if err != nil {
			return addrSpace, err
		}
		matched := false
		for _, pair := range pods {
			matched = matched || p == pair.Source
		}
		if !matched {
			return addrSpace, fmt.Errorf("consumer pod range %s is missing from Configuration", p)
		}
	}
	for _, local := range input.LocalProviderPairs {
		from, err := networkpolicy.ParsePrefix(local.Source)
		if err != nil {
			return addrSpace, err
		}
		to, err := networkpolicy.ParsePrefix(local.Destination)
		if err != nil {
			return addrSpace, err
		}
		if !containsPrefix(addrSpace.Reserved, to) {
			return addrSpace, fmt.Errorf("unrecognized destination-local pod range %s", to)
		}
		addrSpace.Pairs = append(addrSpace.Pairs, networkpolicy.Pair{Source: from, Destination: to})
	}
	external, err := networkpolicy.PrefixPairs(cfg.Spec.Remote.CIDR.External, cfg.Status.Remote.CIDR.External)
	if err != nil {
		return addrSpace, err
	}
	for _, pair := range external {
		addrSpace.Reserved = append(addrSpace.Reserved, pair.Destination)
	}
	aliases := make(map[netip.Addr]netip.Addr)
	originals := make(map[netip.Addr]netip.Addr)
	for _, export := range input.Exports {
		from, fromErr := netip.ParseAddr(export.Original)
		alloc, allocErr := netip.ParseAddr(export.Allocated)
		if fromErr != nil || allocErr != nil || !from.Is4() || !alloc.Is4() || export.AllocationUID == "" {
			return addrSpace, fmt.Errorf("invalid external export %s", export.Original)
		}
		if old, ok := originals[from]; ok && old != alloc {
			return addrSpace, fmt.Errorf("ambiguous external allocation for %s", from)
		}
		originals[from] = alloc
		matched := false
		for _, pair := range external {
			if !pair.Source.Contains(alloc) {
				continue
			}
			alias := networkpolicy.MapAddress(alloc, pair.Destination)
			if old, ok := aliases[alias]; ok && old != from {
				return addrSpace, fmt.Errorf("ambiguous external alias %s", alias)
			}
			aliases[alias] = from
			addrSpace.Exports = append(addrSpace.Exports, networkpolicy.Export{Original: from, Alias: alias})
			matched = true
		}
		if !matched {
			return addrSpace, fmt.Errorf("allocation %s is outside the consumer external ranges", alloc)
		}
	}
	return addrSpace, nil
}

func containsPrefix(list []netip.Prefix, target netip.Prefix) bool {
	for _, candidate := range list {
		if candidate == target {
			return true
		}
	}
	return false
}

// ValidateMapping verifies the provider's ownership metadata and current desired mapping.
func ValidateMapping(ctx context.Context, reader client.Reader, namespace string, source api.PolicySource) error {
	var ns core.Namespace
	if err := reader.Get(ctx, client.ObjectKey{Name: namespace}, &ns); err != nil {
		return err
	}
	tenant, name, ok := strings.Cut(ns.Annotations[consts.RemoteNamespaceManagedByAnnotationKey], "/")
	if !ok || tenant == "" || name == "" || source.Cluster == "" || source.NamespaceUID == "" ||
		ns.Labels[consts.RemoteClusterID] != source.Cluster ||
		ns.Annotations[consts.RemoteNamespaceOriginalNameAnnotationKey] != source.Namespace || !ns.DeletionTimestamp.IsZero() {
		return fmt.Errorf("invalid policy source for namespace %s", namespace)
	}
	var nm api.NamespaceMap
	if err := reader.Get(ctx, client.ObjectKey{Namespace: tenant, Name: name}, &nm); err != nil {
		return err
	}
	if nm.Spec.DesiredMapping[source.Namespace] != namespace || nm.Status.CurrentMapping[source.Namespace].Phase != api.MappingAccepted || !nm.DeletionTimestamp.IsZero() {
		return fmt.Errorf("namespace mapping is not accepted")
	}
	return nil
}

// CheckReady verifies the live inventory and generated policies before creating a pod.
func (r *Reconciler) CheckReady(ctx context.Context, namespace, cluster string) error {
	reader := r.APIReader
	if reader == nil {
		reader = r.Client
	}
	var set api.ShadowNetworkPolicySet
	if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: networkpolicy.InventoryName}, &set); err != nil {
		return err
	}
	if !networkpolicy.Ready(&set) || set.Spec.Source.Cluster != cluster {
		return fmt.Errorf("policy inventory is not ready")
	}
	if err := ValidateMapping(ctx, reader, namespace, set.Spec.Source); err != nil {
		return err
	}
	expected := make(map[string]types.UID, len(set.Spec.Members))
	for _, member := range set.Spec.Members {
		var shadow api.ShadowNetworkPolicy
		if err := reader.Get(ctx, client.ObjectKey{Namespace: namespace, Name: member.Name}, &shadow); err != nil {
			return err
		}
		if shadow.Spec.Source != set.Spec.Source || string(shadow.UID) != member.UID || shadow.Generation != member.Generation || !shadow.DeletionTimestamp.IsZero() {
			return fmt.Errorf("stale policy inventory member %s", member.Name)
		}
		spec, err := r.compile(ctx, reader, &shadow)
		if err != nil {
			return err
		}
		var policy networking.NetworkPolicy
		if err := reader.Get(ctx, client.ObjectKeyFromObject(&shadow), &policy); err != nil {
			return err
		}
		if !policy.DeletionTimestamp.IsZero() || !metav1.IsControlledBy(&policy, &shadow) || !equality.Semantic.DeepEqual(policy.Spec, *spec) {
			return fmt.Errorf("generated NetworkPolicy %s differs from desired state", member.Name)
		}
		expected[policy.Name] = shadow.UID
	}
	var policies networking.NetworkPolicyList
	if err := reader.List(ctx, &policies, client.InNamespace(namespace)); err != nil {
		return err
	}
	for _, policy := range policies.Items {
		owner := metav1.GetControllerOf(&policy)
		if !managed(&policy, owner) {
			continue
		}
		if owner == nil || expected[policy.Name] != owner.UID {
			return fmt.Errorf("obsolete NetworkPolicy %s", policy.Name)
		}
	}
	return nil
}

func managed(policy *networking.NetworkPolicy, owner *metav1.OwnerReference) bool {
	return policy.Labels[managedLabel] == "true" || (owner != nil && owner.APIVersion == api.SchemeGroupVersion.String() && owner.Kind == "ShadowNetworkPolicy")
}
