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
	"net/netip"
	"testing"

	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func mustPrefix(t *testing.T, value string) netip.Prefix {
	t.Helper()
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		t.Fatalf("parsing prefix %q: %v", value, err)
	}
	return prefix
}

func mustAddr(t *testing.T, value string) netip.Addr {
	t.Helper()
	address, err := netip.ParseAddr(value)
	if err != nil {
		t.Fatalf("parsing address %q: %v", value, err)
	}
	return address
}

func mapping(t *testing.T, source, destination string) PrefixMapping {
	t.Helper()
	return PrefixMapping{Source: mustPrefix(t, source), Destination: mustPrefix(t, destination)}
}

func ipBlock(cidr string, except ...string) networkingv1.NetworkPolicyPeer {
	return networkingv1.NetworkPolicyPeer{IPBlock: &networkingv1.IPBlock{CIDR: cidr, Except: except}}
}

func port(protocol string, number int) networkingv1.NetworkPolicyPort {
	p := intstr.FromInt32(int32(number))
	proto := corev1.Protocol(protocol)
	return networkingv1.NetworkPolicyPort{Protocol: &proto, Port: &p}
}

func ingressPolicy(podSelector map[string]string, types []networkingv1.PolicyType,
	rules ...networkingv1.NetworkPolicyIngressRule) networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: podSelector},
		PolicyTypes: types,
		Ingress:     rules,
	}
}

func egressRule(ports []networkingv1.NetworkPolicyPort, peers ...networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicyEgressRule {
	return networkingv1.NetworkPolicyEgressRule{Ports: ports, To: peers}
}

func ingressRule(ports []networkingv1.NetworkPolicyPort, peers ...networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicyIngressRule {
	return networkingv1.NetworkPolicyIngressRule{Ports: ports, From: peers}
}

// peerPrefixes returns the CIDRs of a peer list.
func peerPrefixes(peers []networkingv1.NetworkPolicyPeer) []string {
	result := make([]string, 0, len(peers))
	for i := range peers {
		if peers[i].IPBlock != nil {
			result = append(result, peers[i].IPBlock.CIDR)
		}
	}
	return result
}

// allowedSet builds the address set allowed by a peer list.
func allowedSet(t *testing.T, peers []networkingv1.NetworkPolicyPeer) AddressSet {
	t.Helper()
	result := EmptySet()
	for i := range peers {
		if peers[i].IPBlock == nil {
			continue
		}
		set, err := PrefixSet(mustPrefix(t, peers[i].IPBlock.CIDR))
		if err != nil {
			t.Fatalf("building set for %q: %v", peers[i].IPBlock.CIDR, err)
		}
		result = result.Union(set)
	}
	return result
}

func requireAllowed(t *testing.T, peers []networkingv1.NetworkPolicyPeer, address string) {
	t.Helper()
	if !allowedSet(t, peers).Contains(mustAddr(t, address)) {
		t.Fatalf("expected %s to be allowed, got %v", address, peerPrefixes(peers))
	}
}

func requireDenied(t *testing.T, peers []networkingv1.NetworkPolicyPeer, address string) {
	t.Helper()
	if allowedSet(t, peers).Contains(mustAddr(t, address)) {
		t.Fatalf("expected %s to be denied, got %v", address, peerPrefixes(peers))
	}
}

func resolve(t *testing.T, spec networkingv1.NetworkPolicySpec, ctx SourceContext, limits Limits) Result {
	t.Helper()
	return Compile(Input{Template: spec, Context: ctx, Limits: limits})
}

func TestConsumerPodCIDRRemapping(t *testing.T) {
	ctx := SourceContext{
		Profile:     ProfileLiteral,
		PodCIDRs:    []netip.Prefix{mustPrefix(t, "10.20.0.0/16")},
		PodMappings: []PrefixMapping{mapping(t, "10.20.0.0/16", "172.20.0.0/16")},
	}
	spec := ingressPolicy(map[string]string{"app": "foo"}, nil,
		ingressRule(nil, ipBlock("10.20.7.9/32")))
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	got := peerPrefixes(result.Spec.Ingress[0].From)
	if len(got) != 1 || got[0] != "172.20.7.9/32" {
		t.Fatalf("expected [172.20.7.9/32], got %v", got)
	}
	if len(result.Spec.PolicyTypes) != 1 || result.Spec.PolicyTypes[0] != networkingv1.PolicyTypeIngress {
		t.Fatalf("expected defaulted ingress policy types, got %v", result.Spec.PolicyTypes)
	}
}

func TestDestinationLocalPodInversion(t *testing.T) {
	ctx := SourceContext{
		Profile:          ProfileLiteral,
		LocalPodMappings: []PrefixMapping{mapping(t, "10.80.0.0/16", "172.28.0.0/16")},
	}
	spec := ingressPolicy(nil, []networkingv1.PolicyType{networkingv1.PolicyTypeIngress},
		ingressRule(nil, ipBlock("172.28.4.7/32")))
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	got := peerPrefixes(result.Spec.Ingress[0].From)
	if len(got) != 1 || got[0] != "10.80.4.7/32" {
		t.Fatalf("expected [10.80.4.7/32], got %v", got)
	}
}

func indirectContext(t *testing.T, destination string) SourceContext {
	t.Helper()
	aliased := "172.26.0.0/16"
	if destination != "" {
		aliased = destination
	}
	return SourceContext{
		Profile:       ProfileLiteral,
		ExternalCIDRs: []netip.Prefix{mustPrefix(t, "10.70.0.0/16")},
		ExternalMappings: []PrefixMapping{
			mapping(t, "10.70.0.0/16", "172.31.0.0/16"),
		},
		AliasedRanges:   []netip.Prefix{mustPrefix(t, aliased)},
		ExportsComplete: true,
		Exports: []ExportRecord{
			{
				SourceAddress:   mustAddr(t, "172.26.4.7"),
				ExportedAddress: mustAddr(t, "10.70.0.42"),
				Path:            ExportPathIndirect,
				TargetClusterID: "P1",
				AllocationUID:   "alloc-a",
			},
			{
				SourceAddress:   mustAddr(t, "172.26.4.8"),
				ExportedAddress: mustAddr(t, "10.70.0.90"),
				Path:            ExportPathIndirect,
				TargetClusterID: "P1",
				AllocationUID:   "alloc-b",
			},
			{
				SourceAddress:   mustAddr(t, "172.27.4.7"),
				ExportedAddress: mustAddr(t, "10.70.0.43"),
				Path:            ExportPathIndirect,
				TargetClusterID: "P1",
				AllocationUID:   "alloc-unrelated",
			},
		},
	}
}

func TestIndirectEndpointA(t *testing.T) {
	ctx := indirectContext(t, "")
	spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.7/32")))
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	if len(result.Spec.Egress) != 1 {
		t.Fatalf("expected one egress rule, got %d", len(result.Spec.Egress))
	}
	requireAllowed(t, result.Spec.Egress[0].To, "172.31.0.42")
	requireDenied(t, result.Spec.Egress[0].To, "172.31.0.90")
	requireDenied(t, result.Spec.Egress[0].To, "172.31.0.43")
	got := peerPrefixes(result.Spec.Egress[0].To)
	if len(got) != 1 || got[0] != "172.31.0.42/32" {
		t.Fatalf("expected [172.31.0.42/32], got %v", got)
	}
}

func egressPolicy(types []networkingv1.PolicyType, rules ...networkingv1.NetworkPolicyEgressRule) networkingv1.NetworkPolicySpec {
	if types == nil {
		types = []networkingv1.PolicyType{networkingv1.PolicyTypeEgress}
	}
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
		PolicyTypes: types,
		Egress:      rules,
	}
}

func TestIndirectEndpointsABAndException(t *testing.T) {
	ctx := indirectContext(t, "")

	t.Run("whole prefix yields both hosts", func(t *testing.T) {
		spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.0/24")))
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		requireAllowed(t, result.Spec.Egress[0].To, "172.31.0.42")
		requireAllowed(t, result.Spec.Egress[0].To, "172.31.0.90")
		requireDenied(t, result.Spec.Egress[0].To, "172.31.0.43")
	})

	t.Run("exception leaves only A", func(t *testing.T) {
		spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.0/24", "172.26.4.8/32")))
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		got := peerPrefixes(result.Spec.Egress[0].To)
		if len(got) != 1 || got[0] != "172.31.0.42/32" {
			t.Fatalf("expected [172.31.0.42/32], got %v", got)
		}
	})

	t.Run("unrelated allocation is never granted", func(t *testing.T) {
		spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.0/24")))
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		requireDenied(t, result.Spec.Egress[0].To, "172.31.0.43")
	})
}

func TestUnchangedExternalCIDR(t *testing.T) {
	ctx := SourceContext{
		Profile:          ProfileLiteral,
		ExternalCIDRs:    []netip.Prefix{mustPrefix(t, "10.70.0.0/16")},
		ExternalMappings: []PrefixMapping{mapping(t, "10.70.0.0/16", "10.70.0.0/16")},
		AliasedRanges:    []netip.Prefix{mustPrefix(t, "172.26.0.0/16")},
		ExportsComplete:  true,
		Exports: []ExportRecord{{
			SourceAddress:   mustAddr(t, "172.26.4.7"),
			ExportedAddress: mustAddr(t, "10.70.0.42"),
			Path:            ExportPathIndirect,
			TargetClusterID: "P1",
		}},
	}
	spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.7/32")))
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	got := peerPrefixes(result.Spec.Egress[0].To)
	if len(got) != 1 || got[0] != "10.70.0.42/32" {
		t.Fatalf("expected [10.70.0.42/32], got %v", got)
	}
}

func TestNoExpansionForConsumerPodCIDROnly(t *testing.T) {
	ctx := indirectContext(t, "")
	ctx.PodCIDRs = []netip.Prefix{mustPrefix(t, "10.20.0.0/16")}
	ctx.PodMappings = []PrefixMapping{mapping(t, "10.20.0.0/16", "172.20.0.0/16")}

	spec := egressPolicy(nil, egressRule(nil, ipBlock("10.20.0.0/16")))
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	requireAllowed(t, result.Spec.Egress[0].To, "172.20.1.2")
	requireDenied(t, result.Spec.Egress[0].To, "172.31.0.42")
	requireDenied(t, result.Spec.Egress[0].To, "172.31.0.43")
}

func TestAliasCollisionWithRemappedExclusion(t *testing.T) {
	ctx := SourceContext{
		Profile:     ProfileLiteral,
		PodCIDRs:    []netip.Prefix{mustPrefix(t, "10.20.0.0/16")},
		PodMappings: []PrefixMapping{mapping(t, "10.20.0.0/16", "172.20.0.0/16")},
	}
	// Broad allow excluding a consumer subnet. The literal passthrough must not reauthorize the
	// excluded pod through the provider view of the consumer pod CIDR.
	spec := egressPolicy(nil, egressRule(nil, ipBlock("0.0.0.0/0", "10.20.7.0/24")))
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	peers := result.Spec.Egress[0].To
	requireAllowed(t, peers, "172.20.6.9")
	requireDenied(t, peers, "172.20.7.9")
	requireAllowed(t, peers, "8.8.8.8")
}

func TestDefaultDenyAndPolicyTypes(t *testing.T) {
	ctx := SourceContext{Profile: ProfileLiteral}

	t.Run("ingress and egress default deny", func(t *testing.T) {
		spec := networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		}
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		if result.Spec.Ingress != nil || result.Spec.Egress != nil {
			t.Fatalf("expected a default deny policy, got %#v", result.Spec)
		}
		if len(result.Spec.PolicyTypes) != 2 {
			t.Fatalf("expected two policy types, got %v", result.Spec.PolicyTypes)
		}
	})

	t.Run("omitted types with egress rules imply ingress and egress", func(t *testing.T) {
		spec := networkingv1.NetworkPolicySpec{
			PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
			Egress:      []networkingv1.NetworkPolicyEgressRule{egressRule(nil, ipBlock("8.8.8.8/32"))},
		}
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		if len(result.Spec.PolicyTypes) != 2 {
			t.Fatalf("expected defaulted [Ingress Egress], got %v", result.Spec.PolicyTypes)
		}
		if result.Spec.Ingress != nil {
			t.Fatalf("expected default deny ingress")
		}
		requireAllowed(t, result.Spec.Egress[0].To, "8.8.8.8")
	})

	t.Run("unrestricted rule and empty peer are preserved", func(t *testing.T) {
		spec := egressPolicy(nil,
			egressRule(nil),
			egressRule(nil, networkingv1.NetworkPolicyPeer{}))
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		if len(result.Spec.Egress) != 2 {
			t.Fatalf("expected two egress rules, got %d", len(result.Spec.Egress))
		}
		if result.Spec.Egress[0].To != nil {
			t.Fatalf("expected the empty rule to keep a nil peer list, got %v", result.Spec.Egress[0].To)
		}
		if len(result.Spec.Egress[1].To) != 1 {
			t.Fatalf("expected the empty peer object to be preserved, got %v", result.Spec.Egress[1].To)
		}
	})
}

func TestRuleGroupingPortsAndAlternatives(t *testing.T) {
	ctx := SourceContext{Profile: ProfileLiteral}

	t.Run("independent peer alternatives keep ports", func(t *testing.T) {
		spec := egressPolicy(nil,
			egressRule([]networkingv1.NetworkPolicyPort{port("TCP", 80)}, ipBlock("8.8.8.8/32")),
			egressRule([]networkingv1.NetworkPolicyPort{port("UDP", 53)}, ipBlock("1.1.1.1/32")),
		)
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		if len(result.Spec.Egress) != 2 {
			t.Fatalf("expected two rules, got %d", len(result.Spec.Egress))
		}
		if result.Spec.Egress[0].Ports[0].Port.IntVal != 80 {
			t.Fatalf("expected the first rule to keep port 80")
		}
		if result.Spec.Egress[1].Ports[0].Port.IntVal != 53 {
			t.Fatalf("expected the second rule to keep port 53")
		}
	})

	t.Run("nonempty peer list translating to nothing removes the rule", func(t *testing.T) {
		ctx := SourceContext{
			Profile:         ProfileLiteral,
			AliasedRanges:   []netip.Prefix{mustPrefix(t, "172.26.0.0/16")},
			ExportsComplete: true,
		}
		spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.0/24")))
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic != nil {
			t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
		}
		if len(result.Spec.Egress) != 0 {
			t.Fatalf("expected the allow rule to be removed, got %v", result.Spec.Egress)
		}
		if len(result.Spec.PolicyTypes) != 1 || result.Spec.PolicyTypes[0] != networkingv1.PolicyTypeEgress {
			t.Fatalf("expected the isolation policy types to be preserved, got %v", result.Spec.PolicyTypes)
		}
	})
}

func TestUnsupportedInputsFallBackToIsolation(t *testing.T) {
	ctx := SourceContext{Profile: ProfileLiteral}

	tests := []struct {
		name    string
		spec    networkingv1.NetworkPolicySpec
		context SourceContext
		limits  Limits
		reason  Reason
	}{
		{
			name: "named port",
			spec: egressPolicy(nil, egressRule([]networkingv1.NetworkPolicyPort{{
				Protocol: ptrProto("TCP"),
				Port:     ptrIntStr(intstr.FromString("http")),
			}}, ipBlock("8.8.8.8/32"))),
			context: ctx,
			reason:  ReasonUnsupportedNamedPort,
		},
		{
			name:    "ipv6",
			spec:    egressPolicy(nil, egressRule(nil, ipBlock("2001:db8::/32"))),
			context: ctx,
			reason:  ReasonUnsupportedIPv6,
		},
		{
			name: "peer selector",
			spec: egressPolicy(nil, egressRule(nil, networkingv1.NetworkPolicyPeer{
				PodSelector: &metav1.LabelSelector{},
			})),
			context: ctx,
			reason:  ReasonUnsupportedPeerSelector,
		},
		{
			name:    "source semantics profile",
			spec:    egressPolicy(nil, egressRule(nil, ipBlock("8.8.8.8/32"))),
			context: SourceContext{Profile: "CloudPodIPExcluded"},
			reason:  ReasonUnsupportedSourceSemantics,
		},
		{
			name: "service address",
			spec: egressPolicy(nil, egressRule(nil, ipBlock("10.96.0.10/32"))),
			context: SourceContext{
				Profile:      ProfileLiteral,
				ServiceCIDRs: []netip.Prefix{mustPrefix(t, "10.96.0.0/12")},
			},
			reason: ReasonUnsupportedServiceAddress,
		},
		{
			name: "output limit",
			spec: egressPolicy(nil, egressRule(nil, ipBlock("0.0.0.0/0", "10.0.0.0/8", "172.16.0.0/12", "192.168.0.0/16"))),
			context: SourceContext{
				Profile:                 ProfileLiteral,
				DestinationManagedCIDRs: []netip.Prefix{mustPrefix(t, "10.20.0.0/16"), mustPrefix(t, "10.30.0.0/16")},
			},
			limits: Limits{MaxPeers: 4},
			reason: ReasonOutputLimitExceeded,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			result := resolve(t, test.spec, test.context, test.limits)
			if result.Diagnostic == nil {
				t.Fatalf("expected a diagnostic")
			}
			if result.Diagnostic.Reason != test.reason {
				t.Fatalf("expected reason %q, got %q (%v)", test.reason, result.Diagnostic.Reason, result.Diagnostic)
			}
			if result.Resolved {
				t.Fatalf("expected the policy not to be resolved")
			}
			if result.Spec.Ingress != nil || result.Spec.Egress != nil {
				t.Fatalf("expected an isolation-only spec, got %#v", result.Spec)
			}
			if len(result.Spec.PodSelector.MatchLabels) != 1 || result.Spec.PodSelector.MatchLabels["app"] != "foo" {
				t.Fatalf("expected the subject selector to be preserved, got %v", result.Spec.PodSelector)
			}
			if len(result.Spec.PolicyTypes) == 0 {
				t.Fatalf("expected the policy types to be preserved")
			}
		})
	}
}

func TestIncompleteInventoryAndUnresolvedAddress(t *testing.T) {
	spec := egressPolicy(nil, egressRule(nil, ipBlock("172.26.4.7/32")))

	t.Run("incomplete inventory is pending", func(t *testing.T) {
		ctx := SourceContext{
			Profile:       ProfileLiteral,
			AliasedRanges: []netip.Prefix{mustPrefix(t, "172.26.0.0/16")},
		}
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic == nil || result.Diagnostic.Reason != ReasonWaitingForEndpointAllocation {
			t.Fatalf("expected a pending diagnostic, got %v", result.Diagnostic)
		}
		if !result.Diagnostic.Temporary {
			t.Fatalf("expected the diagnostic to be temporary")
		}
	})

	t.Run("complete inventory without a match is unresolved", func(t *testing.T) {
		ctx := SourceContext{
			Profile:         ProfileLiteral,
			AliasedRanges:   []netip.Prefix{mustPrefix(t, "172.26.0.0/16")},
			ExportsComplete: true,
		}
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic == nil || result.Diagnostic.Reason != ReasonUnresolvedAddress {
			t.Fatalf("expected an unresolved diagnostic, got %v", result.Diagnostic)
		}
	})

	t.Run("awaiting allocation is pending", func(t *testing.T) {
		ctx := SourceContext{
			Profile:         ProfileLiteral,
			AliasedRanges:   []netip.Prefix{mustPrefix(t, "172.26.0.0/16")},
			ExportsComplete: true,
			Exports: []ExportRecord{{
				SourceAddress:   mustAddr(t, "172.26.4.7"),
				Path:            ExportPathIndirect,
				TargetClusterID: "P1",
			}},
		}
		result := resolve(t, spec, ctx, Limits{})
		if result.Diagnostic == nil || result.Diagnostic.Reason != ReasonWaitingForEndpointAllocation {
			t.Fatalf("expected a pending diagnostic, got %v", result.Diagnostic)
		}
	})
}

func TestDirectionalIsolationAndIndependentDirections(t *testing.T) {
	ctx := SourceContext{
		Profile:     ProfileLiteral,
		PodCIDRs:    []netip.Prefix{mustPrefix(t, "10.20.0.0/16")},
		PodMappings: []PrefixMapping{mapping(t, "10.20.0.0/16", "172.20.0.0/16")},
	}
	spec := networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeIngress, networkingv1.PolicyTypeEgress},
		Ingress:     []networkingv1.NetworkPolicyIngressRule{ingressRule(nil, ipBlock("10.20.0.0/16"))},
		Egress:      []networkingv1.NetworkPolicyEgressRule{egressRule(nil, ipBlock("10.0.0.0/8"))},
	}
	result := resolve(t, spec, ctx, Limits{})
	if result.Diagnostic != nil {
		t.Fatalf("unexpected diagnostic: %v", result.Diagnostic)
	}
	requireAllowed(t, result.Spec.Ingress[0].From, "172.20.1.2")
	requireAllowed(t, result.Spec.Egress[0].To, "10.1.2.3")
	requireDenied(t, result.Spec.Ingress[0].From, "10.1.2.3")
}

func TestDeterministicOutput(t *testing.T) {
	ctx := SourceContext{
		Profile:     ProfileLiteral,
		PodCIDRs:    []netip.Prefix{mustPrefix(t, "10.20.0.0/16")},
		PodMappings: []PrefixMapping{mapping(t, "10.20.0.0/16", "172.20.0.0/16")},
	}
	spec := egressPolicy(nil, egressRule(nil,
		ipBlock("10.20.0.0/16"), ipBlock("8.8.8.8/32"), ipBlock("1.1.1.1/32")))
	first := resolve(t, spec, ctx, Limits{})
	second := resolve(t, spec, ctx, Limits{})
	if len(first.Spec.Egress[0].To) != len(second.Spec.Egress[0].To) {
		t.Fatalf("output length is not deterministic")
	}
	for i := range first.Spec.Egress[0].To {
		if first.Spec.Egress[0].To[i].IPBlock.CIDR != second.Spec.Egress[0].To[i].IPBlock.CIDR {
			t.Fatalf("output is not deterministic at index %d", i)
		}
	}
}

func ptrProto(value string) *corev1.Protocol {
	protocol := corev1.Protocol(value)
	return &protocol
}

func ptrIntStr(value intstr.IntOrString) *intstr.IntOrString {
	return &value
}
