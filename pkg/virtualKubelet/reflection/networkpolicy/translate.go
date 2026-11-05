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

package networkpolicy

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/netip"
	"sort"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
	ipamv1alpha1 "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	networkingutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/utils"
	compiler "github.com/liqotech/liqo/pkg/utils/networkpolicy"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
)

// desiredShadow builds the ShadowNetworkPolicy for the given source policy.
func (r *NetworkPolicyReflector) desiredShadow(state *namespacedState,
	policy *networkingv1.NetworkPolicy) (*offloadingv1beta1.ShadowNetworkPolicy, string, error) {
	source := offloadingv1beta1.ShadowNetworkPolicySource{
		ClusterID:       r.localClusterID,
		Namespace:       state.localNamespace,
		NamespaceUID:    state.namespaceUID,
		Name:            policy.Name,
		UID:             policy.UID,
		ResourceVersion: policy.ResourceVersion,
	}

	normalized, normalizeDiag := compiler.NormalizeSpec(policy.Spec)
	template := normalized
	sourceContext := offloadingv1beta1.ShadowNetworkPolicySourceContext{
		Profile: offloadingv1beta1.SourceSemanticsProfile(r.sourceProfile),
	}
	var inputError *offloadingv1beta1.ShadowNetworkPolicyInputError

	switch {
	case normalizeDiag != nil:
		template = compiler.IsolationSpec(policy.Spec)
		inputError = apiInputError(normalizeDiag)
	default:
		builtContext, contextDiag := r.buildSourceContext(state)
		if contextDiag != nil {
			template = compiler.IsolationSpec(policy.Spec)
			inputError = apiInputError(contextDiag)
		} else {
			sourceContext = builtContext
		}
	}

	shadowSpec := offloadingv1beta1.ShadowNetworkPolicySpec{
		Source:        source,
		Template:      template,
		SourceContext: sourceContext,
		InputError:    inputError,
	}
	if encoded, err := json.Marshal(shadowSpec); err == nil && len(encoded) > compiler.MaxSerializedInputBytes {
		// Fail closed and bound the payload: publish an isolation-only shadow without the network
		// context that caused the overflow.
		shadowSpec.Template = compiler.IsolationSpec(policy.Spec)
		shadowSpec.SourceContext = offloadingv1beta1.ShadowNetworkPolicySourceContext{
			Profile: offloadingv1beta1.SourceSemanticsProfile(r.sourceProfile),
		}
		shadowSpec.InputError = &offloadingv1beta1.ShadowNetworkPolicyInputError{
			Reason:  string(compiler.ReasonOutputLimitExceeded),
			Field:   "spec.sourceContext",
			Message: fmt.Sprintf("the serialized shadow input exceeds the %d byte limit", compiler.MaxSerializedInputBytes),
		}
		sourceContext = shadowSpec.SourceContext
		template = shadowSpec.Template
	}

	revision := inputRevision(source, template, sourceContext)
	name := shadowNameFor(source)
	return &offloadingv1beta1.ShadowNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: state.remoteNamespace,
			Labels:    r.shadowLabels(state.localNamespace, state.namespaceUID),
		},
		Spec: offloadingv1beta1.ShadowNetworkPolicySpec{
			Source:        shadowSpec.Source,
			Template:      shadowSpec.Template,
			InputRevision: revision,
			SourceContext: shadowSpec.SourceContext,
			InputError:    shadowSpec.InputError,
		},
	}, revision, nil
}

func apiInputError(diag *compiler.Diagnostic) *offloadingv1beta1.ShadowNetworkPolicyInputError {
	return &offloadingv1beta1.ShadowNetworkPolicyInputError{
		Reason:  string(diag.Reason),
		Field:   diag.Field,
		Message: diag.Message,
	}
}

func shadowNameFor(source offloadingv1beta1.ShadowNetworkPolicySource) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s|%s",
		source.ClusterID, source.NamespaceUID, source.Namespace, source.UID)))
	return "np-" + hex.EncodeToString(digest[:])[:24]
}

func inventoryNameFor(clusterID liqov1beta1.ClusterID, namespace string, namespaceUID types.UID) string {
	digest := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%s", clusterID, namespaceUID, namespace)))
	return "np-set-" + hex.EncodeToString(digest[:])[:24]
}

type revisionPayload struct {
	ClusterID    liqov1beta1.ClusterID                              `json:"clusterID"`
	Namespace    string                                             `json:"namespace"`
	NamespaceUID types.UID                                          `json:"namespaceUID"`
	Name         string                                             `json:"name"`
	UID          types.UID                                          `json:"uid"`
	Template     networkingv1.NetworkPolicySpec                     `json:"template"`
	Context      offloadingv1beta1.ShadowNetworkPolicySourceContext `json:"context"`
}

func inputRevision(source offloadingv1beta1.ShadowNetworkPolicySource, template networkingv1.NetworkPolicySpec,
	context offloadingv1beta1.ShadowNetworkPolicySourceContext) string {
	payload := revisionPayload{
		ClusterID:    source.ClusterID,
		Namespace:    source.Namespace,
		NamespaceUID: source.NamespaceUID,
		Name:         source.Name,
		UID:          source.UID,
		Template:     template,
		Context:      context,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

// buildSourceContext reads the shared network caches and returns the consumer network context.
func (r *NetworkPolicyReflector) buildSourceContext(state *namespacedState) (
	offloadingv1beta1.ShadowNetworkPolicySourceContext, *compiler.Diagnostic) {
	context := offloadingv1beta1.ShadowNetworkPolicySourceContext{
		Profile:         offloadingv1beta1.SourceSemanticsProfile(r.sourceProfile),
		ExportsComplete: true,
	}

	for _, cidr := range r.localPodCIDRs {
		prefix, err := netip.ParsePrefix(cidr)
		if err != nil {
			return context, invalidSourceContext("podCIDRs", err)
		}
		context.PodCIDRs = append(context.PodCIDRs, networkingv1beta1.CIDR(prefix.Masked().String()))
	}

	for _, object := range r.networkInformer.GetStore().List() {
		var network ipamv1alpha1.Network
		if err := objectToTyped(object, &network); err != nil {
			return context, invalidSourceContext("networks", err)
		}
		if network.Labels[consts.NetworkTypeLabelKey] != string(consts.NetworkTypeServiceCIDR) ||
			network.Status.CIDR == "" {
			continue
		}
		prefix, err := netip.ParsePrefix(string(network.Status.CIDR))
		if err != nil {
			return context, invalidSourceContext("serviceCIDRs", err)
		}
		context.ServiceCIDRs = append(context.ServiceCIDRs, networkingv1beta1.CIDR(prefix.Masked().String()))
	}

	configurations := make([]*networkingv1beta1.Configuration, 0)
	for _, object := range r.configInformer.GetStore().List() {
		var configuration networkingv1beta1.Configuration
		if err := objectToTyped(object, &configuration); err != nil {
			return context, invalidSourceContext("configurations", err)
		}
		configurations = append(configurations, &configuration)
	}
	sort.Slice(configurations, func(i, j int) bool { return configurations[i].Name < configurations[j].Name })

	externalCIDRs := make(map[string]struct{})
	aliasedRanges := make(map[string]struct{})
	for _, configuration := range configurations {
		peer := liqov1beta1.ClusterID(configuration.Labels[consts.RemoteClusterID])
		if configuration.Status.Remote == nil || !networkingutils.AreConfigurationNetworkCIDRsConfigured(configuration) {
			return context, &compiler.Diagnostic{
				Reason:    compiler.ReasonWaitingForNetworkMapping,
				Field:     fmt.Sprintf("configuration/%s.status.remote", configuration.Name),
				Message:   "a consumer network configuration is not ready yet",
				Temporary: true,
			}
		}

		if configuration.Spec.Local != nil {
			for _, cidr := range configuration.Spec.Local.CIDR.External {
				prefix, err := netip.ParsePrefix(string(cidr))
				if err != nil {
					return context, invalidSourceContext("externalCIDRs", err)
				}
				externalCIDRs[prefix.Masked().String()] = struct{}{}
			}
		}

		if peer == r.remoteClusterID {
			localMappings, diag := localMappingsFromConfiguration(configuration)
			if diag != nil {
				return context, diag
			}
			context.LocalPodMappings = append(context.LocalPodMappings, localMappings...)
			continue
		}

		for _, cidr := range configuration.Status.Remote.CIDR.Pod {
			prefix, err := netip.ParsePrefix(string(cidr))
			if err != nil {
				return context, invalidSourceContext("aliasedRanges", err)
			}
			aliasedRanges[prefix.Masked().String()] = struct{}{}
		}
	}

	for cidr := range externalCIDRs {
		context.ExternalCIDRs = append(context.ExternalCIDRs, networkingv1beta1.CIDR(cidr))
	}
	sort.Slice(context.ExternalCIDRs, func(i, j int) bool { return context.ExternalCIDRs[i] < context.ExternalCIDRs[j] })
	for cidr := range aliasedRanges {
		context.AliasedRanges = append(context.AliasedRanges, networkingv1beta1.CIDR(cidr))
	}
	sort.Slice(context.AliasedRanges, func(i, j int) bool { return context.AliasedRanges[i] < context.AliasedRanges[j] })

	exports, diag := r.buildExports(configurations)
	if diag != nil {
		return context, diag
	}
	context.Exports = exports

	return context, nil
}

func localMappingsFromConfiguration(configuration *networkingv1beta1.Configuration) ([]offloadingv1beta1.CIDRMapping, *compiler.Diagnostic) {
	if len(configuration.Spec.Remote.CIDR.Pod) != len(configuration.Status.Remote.CIDR.Pod) {
		return nil, &compiler.Diagnostic{
			Reason:    compiler.ReasonWaitingForNetworkMapping,
			Field:     fmt.Sprintf("configuration/%s.status.remote.cidr.pod", configuration.Name),
			Message:   "the local pod CIDR mappings are not index-aligned",
			Temporary: true,
		}
	}
	out := make([]offloadingv1beta1.CIDRMapping, 0, len(configuration.Spec.Remote.CIDR.Pod))
	for i := range configuration.Spec.Remote.CIDR.Pod {
		out = append(out, offloadingv1beta1.CIDRMapping{
			// The consumer-visible range of the provider pods maps back to the provider native range.
			Consumer: configuration.Status.Remote.CIDR.Pod[i],
			Provider: configuration.Spec.Remote.CIDR.Pod[i],
		})
	}
	return out, nil
}

func (r *NetworkPolicyReflector) buildExports(configurations []*networkingv1beta1.Configuration) (
	[]offloadingv1beta1.NetworkPolicyExportRecord, *compiler.Diagnostic) {
	exports := make([]offloadingv1beta1.NetworkPolicyExportRecord, 0)
	for _, object := range r.ipInformer.GetStore().List() {
		var ip ipamv1alpha1.IP
		if err := objectToTyped(object, &ip); err != nil {
			return nil, invalidSourceContext("ips", err)
		}
		if ip.Labels[consts.IPTypeLabelKey] == consts.IPTypeAPIServer ||
			ip.Labels[consts.IPTypeLabelKey] == consts.IPTypeAPIServerProxy {
			continue
		}
		sourceAddress, err := netip.ParseAddr(string(ip.Spec.IP))
		if err != nil {
			continue
		}
		peer, found := peerForAddress(configurations, sourceAddress.Unmap())
		if !found || peer == r.remoteClusterID {
			continue
		}

		record := offloadingv1beta1.NetworkPolicyExportRecord{
			SourceAddress:   sourceAddress.Unmap().String(),
			Path:            offloadingv1beta1.ExportPathIndirect,
			TargetClusterID: peer,
			Provenance: &offloadingv1beta1.NetworkPolicyExportProvenance{
				IPName: ip.Name,
				IPUID:  ip.UID,
			},
		}
		if ip.Status.IP != "" {
			if exported, err := netip.ParseAddr(string(ip.Status.IP)); err == nil {
				record.ExportedAddress = exported.Unmap().String()
			}
		}
		exports = append(exports, record)
	}
	sort.Slice(exports, func(i, j int) bool { return exports[i].SourceAddress < exports[j].SourceAddress })
	return exports, nil
}

func peerForAddress(configurations []*networkingv1beta1.Configuration, address netip.Addr) (liqov1beta1.ClusterID, bool) {
	for _, configuration := range configurations {
		if configuration.Status.Remote == nil {
			continue
		}
		peer := liqov1beta1.ClusterID(configuration.Labels[consts.RemoteClusterID])
		for _, cidr := range configuration.Status.Remote.CIDR.Pod {
			prefix, err := netip.ParsePrefix(string(cidr))
			if err != nil {
				continue
			}
			if prefix.Contains(address) {
				return peer, true
			}
		}
	}
	return "", false
}

func invalidSourceContext(field string, err error) *compiler.Diagnostic {
	return &compiler.Diagnostic{
		Reason:  compiler.ReasonInvalidInput,
		Field:   "spec.sourceContext." + field,
		Message: err.Error(),
	}
}

// reconcileInventory writes the ShadowNetworkPolicySet inventory and enforces the member shadows.
func (r *NetworkPolicyReflector) reconcileInventory(state *namespacedState) error {
	policies, err := state.policyInformer.Lister().NetworkPolicies(state.localNamespace).List(labels.Everything())
	if err != nil {
		return err
	}

	shadows := make([]*offloadingv1beta1.ShadowNetworkPolicy, 0, len(policies))
	members := make([]offloadingv1beta1.ShadowNetworkPolicySetMember, 0, len(policies))
	for i := range policies {
		shadow, revision, err := r.desiredShadow(state, policies[i])
		if err != nil {
			return err
		}
		shadows = append(shadows, shadow)
		members = append(members, offloadingv1beta1.ShadowNetworkPolicySetMember{
			Name:          shadow.Name,
			SourceUID:     policies[i].UID,
			InputRevision: revision,
		})
	}
	sort.Slice(shadows, func(i, j int) bool { return shadows[i].Name < shadows[j].Name })
	sort.Slice(members, func(i, j int) bool { return members[i].Name < members[j].Name })

	digest := membersDigest(members)
	setName := inventoryNameFor(r.localClusterID, state.localNamespace, state.namespaceUID)
	set := &offloadingv1beta1.ShadowNetworkPolicySet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      setName,
			Namespace: state.remoteNamespace,
		},
		Spec: offloadingv1beta1.ShadowNetworkPolicySetSpec{
			SourceClusterID:    r.localClusterID,
			SourceNamespace:    state.localNamespace,
			SourceNamespaceUID: state.namespaceUID,
			Revision:           digest,
			Policies:           members,
		},
	}
	set.Labels = inventoryLabels(set)

	committed, err := r.currentCommittedInventory(state, setName, digest)
	if err != nil {
		return err
	}

	if !committed {
		if err := r.writeInventory(r.ctx, state.remoteNamespace, set,
			offloadingv1beta1.ShadowNetworkPolicySetPhaseStaging); err != nil {
			return err
		}
	}

	expected := make(map[string]struct{}, len(shadows))
	for _, shadow := range shadows {
		if err := r.applyShadow(r.ctx, state.remoteNamespace, shadow); err != nil {
			return err
		}
		expected[shadow.Name] = struct{}{}
	}

	if err := r.deleteObsoleteShadows(state, expected); err != nil {
		return err
	}

	if !committed {
		return r.writeInventory(r.ctx, state.remoteNamespace, set,
			offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted)
	}
	return nil
}

func (r *NetworkPolicyReflector) currentCommittedInventory(state *namespacedState, name, revision string) (bool, error) {
	object, err := r.remoteDynamic.Resource(shadowNetworkPolicySetGVR).Namespace(state.remoteNamespace).
		Get(r.ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return false, nil
		}
		return false, err
	}
	var current offloadingv1beta1.ShadowNetworkPolicySet
	if err := objectToTyped(object, &current); err != nil {
		return false, err
	}
	return current.Spec.Phase == offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted &&
		current.Spec.Revision == revision, nil
}

func (r *NetworkPolicyReflector) deleteObsoleteShadows(state *namespacedState, expected map[string]struct{}) error {
	shadows, err := r.listShadows(r.ctx, state.remoteNamespace)
	if err != nil {
		return err
	}
	for i := range shadows {
		shadow := &shadows[i]
		if _, found := expected[shadow.Name]; found {
			continue
		}
		if shadow.Labels[consts.ManagedByLabelKey] != consts.ManagedByShadowNetworkPolicyValue {
			continue
		}
		if shadow.Labels[forge.LiqoOriginClusterIDKey] != string(r.localClusterID) {
			continue
		}
		if err := r.deleteShadow(r.ctx, state.remoteNamespace, shadow.Name); err != nil {
			return err
		}
	}
	return nil
}

func membersDigest(members []offloadingv1beta1.ShadowNetworkPolicySetMember) string {
	encoded, err := json.Marshal(members)
	if err != nil {
		return ""
	}
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}
