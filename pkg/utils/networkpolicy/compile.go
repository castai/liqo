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
	"fmt"
	"net/netip"

	networkingv1 "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Compile translates the source policy in the input into a provider NetworkPolicy spec.
//
// On any validation or resolution failure, the returned spec is the isolation-only derived from
// the source subject and effective policy types, while IsolationSpec is always populated. This
// prevents stale permissive rules from remaining active.
func Compile(in Input) Result {
	limits := in.Limits.withDefaults()

	normalized, diag := normalizePolicy(in.Template)
	if diag != nil {
		isolation := IsolationSpec(in.Template)
		return Result{Spec: isolation, IsolationSpec: isolation, Diagnostic: diag}
	}
	isolation := IsolationSpec(normalized)

	state, diag := newCompiler(normalized, in.Context, limits)
	if diag != nil {
		return Result{Spec: isolation, IsolationSpec: isolation, Diagnostic: diag}
	}

	out := *normalized.DeepCopy()
	if containsPolicyType(normalized.PolicyTypes, networkingv1.PolicyTypeIngress) {
		rules, diag := state.translateIngress(normalized.Ingress)
		if diag != nil {
			return Result{Spec: isolation, IsolationSpec: isolation, Diagnostic: diag}
		}
		out.Ingress = rules
	} else {
		out.Ingress = nil
	}
	if containsPolicyType(normalized.PolicyTypes, networkingv1.PolicyTypeEgress) {
		rules, diag := state.translateEgress(normalized.Egress)
		if diag != nil {
			return Result{Spec: isolation, IsolationSpec: isolation, Diagnostic: diag}
		}
		out.Egress = rules
	} else {
		out.Egress = nil
	}

	return Result{Spec: out, IsolationSpec: isolation, Resolved: true}
}

// IsolationSpec returns the isolation-only version of the given spec: the subject selector and the
// effective policy types are preserved, while every allow rule is removed.
func IsolationSpec(spec networkingv1.NetworkPolicySpec) networkingv1.NetworkPolicySpec {
	normalized, diag := normalizePolicy(spec)
	if diag != nil {
		normalized = *spec.DeepCopy()
		if len(normalized.PolicyTypes) == 0 {
			normalized.PolicyTypes = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
			if len(spec.Egress) > 0 {
				normalized.PolicyTypes = append(normalized.PolicyTypes, networkingv1.PolicyTypeEgress)
			}
		}
	}
	normalized.Ingress = nil
	normalized.Egress = nil
	return normalized
}

// EffectivePolicyTypes returns the policy types after Kubernetes defaulting.
func EffectivePolicyTypes(spec networkingv1.NetworkPolicySpec) []networkingv1.PolicyType {
	normalized, diag := normalizePolicy(spec)
	if diag != nil {
		return nil
	}
	return normalized.PolicyTypes
}

// NormalizeSpec returns a deep copy of the spec with explicit effective policy types.
func NormalizeSpec(spec networkingv1.NetworkPolicySpec) (networkingv1.NetworkPolicySpec, *Diagnostic) {
	return normalizePolicy(spec)
}

func normalizePolicy(spec networkingv1.NetworkPolicySpec) (networkingv1.NetworkPolicySpec, *Diagnostic) {
	out := *spec.DeepCopy()

	types := out.PolicyTypes
	if len(types) == 0 {
		types = []networkingv1.PolicyType{networkingv1.PolicyTypeIngress}
		if len(out.Egress) > 0 {
			types = append(types, networkingv1.PolicyTypeEgress)
		}
	}

	seen := make(map[networkingv1.PolicyType]struct{}, len(types))
	normalized := make([]networkingv1.PolicyType, 0, len(types))
	for _, policyType := range types {
		switch policyType {
		case networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress:
		default:
			return networkingv1.NetworkPolicySpec{}, &Diagnostic{
				Reason:  ReasonInvalidInput,
				Field:   "spec.policyTypes",
				Message: fmt.Sprintf("unsupported policy type %q", policyType),
			}
		}
		if _, found := seen[policyType]; found {
			continue
		}
		seen[policyType] = struct{}{}
		normalized = append(normalized, policyType)
	}
	out.PolicyTypes = normalized
	return out, nil
}

func containsPolicyType(types []networkingv1.PolicyType, wanted networkingv1.PolicyType) bool {
	for _, policyType := range types {
		if policyType == wanted {
			return true
		}
	}
	return false
}

type compiler struct {
	ctx    SourceContext
	limits Limits

	podSources         AddressSet
	podDestinations    AddressSet
	localSources       AddressSet
	localDestinations  AddressSet
	aliasSources       AddressSet
	destinationManaged AddressSet
	externalSources    AddressSet
	exportSources      AddressSet

	serviceSources AddressSet
}

func newCompiler(spec networkingv1.NetworkPolicySpec, ctx SourceContext, limits Limits) (*compiler, *Diagnostic) {
	_ = spec

	if ctx.Profile != ProfileLiteral {
		return nil, &Diagnostic{
			Reason:  ReasonUnsupportedSourceSemantics,
			Field:   "spec.sourceContext.profile",
			Message: fmt.Sprintf("source semantics profile %q is not supported", ctx.Profile),
		}
	}

	if err := validateMappings("podMappings", ctx.PodMappings); err != nil {
		return nil, invalidInput("spec.sourceContext.podMappings", err)
	}
	if err := validateMappings("localPodMappings", ctx.LocalPodMappings); err != nil {
		return nil, invalidInput("spec.sourceContext.localPodMappings", err)
	}
	if err := validateMappings("externalMappings", ctx.ExternalMappings); err != nil {
		return nil, invalidInput("spec.sourceContext.externalMappings", err)
	}

	podCIDRs, err := setFromPrefixes(ctx.PodCIDRs)
	if err != nil {
		return nil, invalidInput("spec.sourceContext.podCIDRs", err)
	}
	serviceCIDRs, err := setFromPrefixes(ctx.ServiceCIDRs)
	if err != nil {
		return nil, invalidInput("spec.sourceContext.serviceCIDRs", err)
	}
	aliased, err := setFromPrefixes(ctx.AliasedRanges)
	if err != nil {
		return nil, invalidInput("spec.sourceContext.aliasedRanges", err)
	}
	managed, err := setFromPrefixes(ctx.DestinationManagedCIDRs)
	if err != nil {
		return nil, invalidInput("spec.sourceContext.destinationManagedCIDRs", err)
	}
	externalSources, err := setFromPrefixes(ctx.ExternalCIDRs)
	if err != nil {
		return nil, invalidInput("spec.sourceContext.externalCIDRs", err)
	}
	externalSources = externalSources.Union(setFromMappings(ctx.ExternalMappings))

	// A source pod CIDR without a positive mapping cannot be translated: the provider must expose
	// the pair through its Configuration resources.
	if !podCIDRs.IsEmpty() {
		if len(ctx.PodMappings) == 0 {
			return nil, &Diagnostic{
				Reason:    ReasonWaitingForNetworkMapping,
				Field:     "spec.sourceContext.podMappings",
				Message:   "no pod CIDR mapping is available",
				Temporary: true,
			}
		}
		if !setFromMappings(ctx.PodMappings).Covers(podCIDRs) {
			return nil, &Diagnostic{
				Reason:    ReasonWaitingForNetworkMapping,
				Field:     "spec.sourceContext.podMappings",
				Message:   "the pod CIDR mappings do not cover every consumer pod CIDR",
				Temporary: true,
			}
		}
	}

	exportSources := EmptySet()
	for i := range ctx.Exports {
		if !ctx.Exports[i].SourceAddress.IsValid() {
			continue
		}
		set, err := PrefixSet(netip.PrefixFrom(ctx.Exports[i].SourceAddress, 32))
		if err != nil {
			continue
		}
		exportSources = exportSources.Union(set)
	}

	return &compiler{
		ctx:                ctx,
		limits:             limits,
		podSources:         setFromMappings(ctx.PodMappings),
		podDestinations:    setFromMappingDestinations(ctx.PodMappings),
		localSources:       setFromMappings(ctx.LocalPodMappings),
		localDestinations:  setFromMappingDestinations(ctx.LocalPodMappings),
		aliasSources:       aliased,
		destinationManaged: managed,
		externalSources:    externalSources,
		exportSources:      exportSources,
		serviceSources:     serviceCIDRs,
	}, nil
}

func (c *compiler) translateIngress(rules []networkingv1.NetworkPolicyIngressRule) ([]networkingv1.NetworkPolicyIngressRule, *Diagnostic) {
	if rules == nil {
		return nil, nil
	}
	out := make([]networkingv1.NetworkPolicyIngressRule, 0, len(rules))
	for i := range rules {
		path := fmt.Sprintf("spec.ingress[%d]", i)
		if diag := validatePorts(rules[i].Ports, path+".ports", c.limits); diag != nil {
			return nil, diag
		}
		peers, diag := c.translatePeers(rules[i].From, path+".from")
		if diag != nil {
			return nil, diag
		}
		if len(rules[i].From) > 0 && len(peers) == 0 {
			// The originally nonempty peer list translates to no peer: remove the whole allow rule.
			continue
		}
		translated := rules[i].DeepCopy()
		translated.From = peers
		out = append(out, *translated)
	}
	return out, nil
}

func (c *compiler) translateEgress(rules []networkingv1.NetworkPolicyEgressRule) ([]networkingv1.NetworkPolicyEgressRule, *Diagnostic) {
	if rules == nil {
		return nil, nil
	}
	out := make([]networkingv1.NetworkPolicyEgressRule, 0, len(rules))
	for i := range rules {
		path := fmt.Sprintf("spec.egress[%d]", i)
		if diag := validatePorts(rules[i].Ports, path+".ports", c.limits); diag != nil {
			return nil, diag
		}
		peers, diag := c.translatePeers(rules[i].To, path+".to")
		if diag != nil {
			return nil, diag
		}
		if len(rules[i].To) > 0 && len(peers) == 0 {
			continue
		}
		translated := rules[i].DeepCopy()
		translated.To = peers
		out = append(out, *translated)
	}
	return out, nil
}

func (c *compiler) translatePeers(peers []networkingv1.NetworkPolicyPeer, path string) ([]networkingv1.NetworkPolicyPeer, *Diagnostic) {
	if len(peers) == 0 {
		return nil, nil
	}

	allowed := EmptySet()
	unrestricted := false
	for i := range peers {
		field := fmt.Sprintf("%s[%d]", path, i)
		peer := peers[i]
		if peer.PodSelector != nil || peer.NamespaceSelector != nil {
			return nil, &Diagnostic{
				Reason:  ReasonUnsupportedPeerSelector,
				Field:   field,
				Message: "pod and namespace peer selectors are not supported",
			}
		}
		if peer.IPBlock == nil {
			// An empty peer object matches every peer.
			unrestricted = true
			continue
		}
		block, diag := c.parseIPBlock(peer.IPBlock, field+".ipBlock")
		if diag != nil {
			return nil, diag
		}
		translated, diag := c.translateSet(block, field+".ipBlock")
		if diag != nil {
			return nil, diag
		}
		allowed = allowed.Union(translated)
	}

	if unrestricted {
		return []networkingv1.NetworkPolicyPeer{{}}, nil
	}

	prefixes := allowed.Prefixes()
	if len(prefixes) > c.limits.MaxPeers {
		return nil, &Diagnostic{
			Reason:  ReasonOutputLimitExceeded,
			Field:   path,
			Message: fmt.Sprintf("the translated peer set requires %d peers, exceeding the limit of %d", len(prefixes), c.limits.MaxPeers),
		}
	}

	out := make([]networkingv1.NetworkPolicyPeer, 0, len(prefixes))
	for _, prefix := range prefixes {
		// The canonical cover is composed of plain IPBlocks without exceptions.
		out = append(out, networkingv1.NetworkPolicyPeer{
			IPBlock: &networkingv1.IPBlock{CIDR: prefix.String()},
		})
	}
	return out, nil
}

func (c *compiler) parseIPBlock(block *networkingv1.IPBlock, path string) (AddressSet, *Diagnostic) {
	prefix, err := netip.ParsePrefix(block.CIDR)
	if err != nil {
		return EmptySet(), invalidInput(path+".cidr", fmt.Errorf("invalid CIDR %q: %w", block.CIDR, err))
	}
	if !prefix.Addr().Is4() {
		return EmptySet(), &Diagnostic{
			Reason:  ReasonUnsupportedIPv6,
			Field:   path + ".cidr",
			Message: fmt.Sprintf("address family of %q is not supported", block.CIDR),
		}
	}
	if prefix != prefix.Masked() {
		return EmptySet(), invalidInput(path+".cidr", fmt.Errorf("CIDR %q must not contain host bits", block.CIDR))
	}
	set, err := PrefixSet(prefix)
	if err != nil {
		return EmptySet(), invalidInput(path+".cidr", err)
	}

	// An explicit Service address or Service-address exception cannot be preserved without
	// Service address translation.
	if !c.serviceSources.IsEmpty() && c.serviceSources.Covers(set) {
		return EmptySet(), &Diagnostic{
			Reason:  ReasonUnsupportedServiceAddress,
			Field:   path + ".cidr",
			Message: fmt.Sprintf("CIDR %q is contained in the consumer Service range", block.CIDR),
		}
	}

	excepts := EmptySet()
	for i := range block.Except {
		except, err := netip.ParsePrefix(block.Except[i])
		if err != nil {
			return EmptySet(), invalidInput(fmt.Sprintf("%s.except[%d]", path, i), fmt.Errorf("invalid CIDR %q: %w", block.Except[i], err))
		}
		if !except.Addr().Is4() {
			return EmptySet(), &Diagnostic{
				Reason:  ReasonUnsupportedIPv6,
				Field:   fmt.Sprintf("%s.except[%d]", path, i),
				Message: fmt.Sprintf("address family of %q is not supported", block.Except[i]),
			}
		}
		if except != except.Masked() {
			return EmptySet(), invalidInput(fmt.Sprintf("%s.except[%d]", path, i),
				fmt.Errorf("CIDR %q must not contain host bits", block.Except[i]))
		}
		exceptSet, err := PrefixSet(except)
		if err != nil {
			return EmptySet(), invalidInput(fmt.Sprintf("%s.except[%d]", path, i), err)
		}
		if !set.Covers(exceptSet) {
			return EmptySet(), invalidInput(fmt.Sprintf("%s.except[%d]", path, i),
				fmt.Errorf("exception %q is not contained in %q", block.Except[i], block.CIDR))
		}
		if !c.serviceSources.IsEmpty() && c.serviceSources.Covers(exceptSet) {
			return EmptySet(), &Diagnostic{
				Reason:  ReasonUnsupportedServiceAddress,
				Field:   fmt.Sprintf("%s.except[%d]", path, i),
				Message: fmt.Sprintf("exception %q is contained in the consumer Service range", block.Except[i]),
			}
		}
		excepts = excepts.Union(exceptSet)
	}

	return set.Subtract(excepts), nil
}

// translateSet resolves every address of the allowed source set into the provider address space.
func (c *compiler) translateSet(allowed AddressSet, path string) (AddressSet, *Diagnostic) {
	out := EmptySet()

	// 1. Consumer pod traffic, remapped through the provider view of the consumer pod CIDRs.
	for _, mapping := range c.ctx.PodMappings {
		out = out.Union(mapping.MapSet(allowed))
	}

	// 2. Pods local to the destination provider: the consumer-visible range is inverted to the
	// provider native range.
	for _, mapping := range c.ctx.LocalPodMappings {
		consumerView, err := PrefixSet(mapping.Destination)
		if err != nil {
			continue
		}
		intersection := allowed.Intersect(consumerView)
		if intersection.IsEmpty() {
			continue
		}
		out = out.Union(mapping.Invert().MapSet(intersection))
	}

	// 3. Export records: translate the explicitly authorized endpoints to their provider-visible alias.
	matchedSource := EmptySet()
	for i := range c.ctx.Exports {
		record := c.ctx.Exports[i]
		if !record.SourceAddress.IsValid() || !allowed.Contains(record.SourceAddress) {
			continue
		}
		address, diag := c.resolveExport(record, fmt.Sprintf("%s.export[%d]", path, i))
		if diag != nil {
			return EmptySet(), diag
		}
		hostSet, err := PrefixSet(netip.PrefixFrom(address, 32))
		if err != nil {
			return EmptySet(), invalidInput(fmt.Sprintf("%s.export[%d]", path, i), err)
		}
		out = out.Union(hostSet)
		sourceSet, err := PrefixSet(netip.PrefixFrom(record.SourceAddress, 32))
		if err != nil {
			return EmptySet(), invalidInput(fmt.Sprintf("%s.export[%d]", path, i), err)
		}
		matchedSource = matchedSource.Union(sourceSet)
	}

	// Addresses belonging to aliased ranges can only be represented through validated exports.
	unresolvedAlias := allowed.Intersect(c.aliasSources).Subtract(matchedSource)
	if !unresolvedAlias.IsEmpty() {
		if !c.ctx.ExportsComplete {
			return EmptySet(), &Diagnostic{
				Reason:    ReasonWaitingForEndpointAllocation,
				Field:     path,
				Message:   fmt.Sprintf("the export inventory is not complete for %s", unresolvedAlias.String()),
				Temporary: true,
			}
		}
		if unresolvedAlias.Len() == 1 {
			return EmptySet(), &Diagnostic{
				Reason:  ReasonUnresolvedAddress,
				Field:   path,
				Message: fmt.Sprintf("no export record matches the explicitly authorized peer %s", unresolvedAlias.String()),
			}
		}
	}

	// 4. Unchanged literals. Subtract every destination-managed space, so that a copied prefix
	// cannot authorize an excluded identity through an alias or through the provider view of the
	// consumer pod CIDR.
	excluded := c.podSources.
		Union(c.podDestinations).
		Union(c.localSources).
		Union(c.localDestinations).
		Union(c.aliasSources).
		Union(c.externalSources).
		Union(c.exportSources).
		Union(c.destinationManaged)
	out = out.Union(allowed.Subtract(excluded))

	return out, nil
}

func (c *compiler) resolveExport(record ExportRecord, path string) (netip.Addr, *Diagnostic) {
	if record.Path == ExportPathDirect {
		if record.ProviderAddress.IsValid() {
			return record.ProviderAddress.Unmap(), nil
		}
		return netip.Addr{}, &Diagnostic{
			Reason:    ReasonWaitingForEndpointAllocation,
			Field:     path,
			Message:   "the direct export has no provider-visible address yet",
			Temporary: true,
		}
	}

	if !record.ExportedAddress.IsValid() {
		return netip.Addr{}, &Diagnostic{
			Reason:    ReasonWaitingForEndpointAllocation,
			Field:     path,
			Message:   "the endpoint export is awaiting an external address allocation",
			Temporary: true,
		}
	}
	exported := record.ExportedAddress.Unmap()
	if !c.externalSources.IsEmpty() && !c.externalSources.Contains(exported) {
		return netip.Addr{}, &Diagnostic{
			Reason:    ReasonWaitingForEndpointAllocation,
			Field:     path,
			Message:   fmt.Sprintf("the allocated address %s is outside the expected external ranges", exported),
			Temporary: true,
		}
	}
	for _, mapping := range c.ctx.ExternalMappings {
		if mapped, ok := mapping.MapAddress(exported); ok {
			return mapped, nil
		}
	}
	if len(c.ctx.ExternalMappings) == 0 {
		// No mapping means that the external address keeps the same meaning in the destination.
		return exported, nil
	}
	return netip.Addr{}, &Diagnostic{
		Reason:    ReasonWaitingForEndpointAllocation,
		Field:     path,
		Message:   fmt.Sprintf("the allocated address %s is not covered by any external CIDR mapping", exported),
		Temporary: true,
	}
}

func validatePorts(ports []networkingv1.NetworkPolicyPort, path string, limits Limits) *Diagnostic {
	for i := range ports {
		field := fmt.Sprintf("%s[%d]", path, i)
		port := ports[i]

		protocol := "TCP"
		if port.Protocol != nil {
			protocol = string(*port.Protocol)
		}
		switch protocol {
		case "TCP", "UDP":
		case "SCTP":
			if !limits.AllowSCTP {
				return &Diagnostic{
					Reason:  ReasonUnsupportedProtocol,
					Field:   field + ".protocol",
					Message: "SCTP is not enabled for the target provider dataplane",
				}
			}
		default:
			return &Diagnostic{
				Reason:  ReasonUnsupportedProtocol,
				Field:   field + ".protocol",
				Message: fmt.Sprintf("protocol %q is not supported", protocol),
			}
		}

		if port.Port == nil {
			if port.EndPort != nil {
				return invalidInput(field+".endPort", fmt.Errorf("endPort requires a numeric port"))
			}
			continue
		}
		if port.Port.Type == intstr.String {
			return &Diagnostic{
				Reason:  ReasonUnsupportedNamedPort,
				Field:   field + ".port",
				Message: "named ports are not supported",
			}
		}
		if port.EndPort != nil {
			if !limits.AllowEndPort {
				return &Diagnostic{
					Reason:  ReasonUnsupportedEndPort,
					Field:   field + ".endPort",
					Message: "port ranges are not enabled for the target provider dataplane",
				}
			}
			if *port.EndPort < port.Port.IntVal {
				return invalidInput(field+".endPort", fmt.Errorf("endPort must not be lower than port"))
			}
		}
	}
	return nil
}

func invalidInput(field string, err error) *Diagnostic {
	return &Diagnostic{
		Reason:  ReasonInvalidInput,
		Field:   field,
		Message: err.Error(),
	}
}
