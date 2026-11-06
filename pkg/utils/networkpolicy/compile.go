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

// Package networkpolicy translates literal IPv4 NetworkPolicy peers between clusters.
package networkpolicy

import (
	"fmt"
	"net/netip"
	"slices"
	"strings"

	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

// Pair maps equal-length prefixes while preserving host bits.
type Pair struct {
	Source, Destination netip.Prefix
}

// Export records a consumer-visible IP and its resolved egress alias.
type Export struct {
	Original, Alias netip.Addr
}

// AddressSpace describes the current address mappings for a destination.
type AddressSpace struct {
	Pending []netip.Addr
	Pairs   []Pair
	// Indirect contains source ranges that require individual export records.
	Indirect []netip.Prefix
	// Reserved contains destination ranges where identity passthrough is unsafe.
	Reserved []netip.Prefix
	Services []netip.Prefix
	Exports  []Export
}

// Isolation preserves the subject and effective policy types with no allow rules.
func Isolation(spec *networking.NetworkPolicySpec) *networking.NetworkPolicySpec {
	out := &networking.NetworkPolicySpec{PodSelector: *spec.PodSelector.DeepCopy(), PolicyTypes: slices.Clone(spec.PolicyTypes)}
	if len(out.PolicyTypes) == 0 {
		out.PolicyTypes = []networking.PolicyType{networking.PolicyTypeIngress}
		if len(spec.Egress) > 0 {
			out.PolicyTypes = append(out.PolicyTypes, networking.PolicyTypeEgress)
		}
	}
	return out
}

// Compile returns isolation-only output on any unsupported or unresolved input.
func Compile(spec *networking.NetworkPolicySpec, space AddressSpace) (*networking.NetworkPolicySpec, error) {
	isolation := Isolation(spec)
	for i, pair := range space.Pairs {
		for _, other := range space.Pairs[:i] {
			if pair != other && (pair.Source.Overlaps(other.Source) || pair.Destination.Overlaps(other.Destination)) {
				return isolation, fmt.Errorf("ambiguous prefix mapping for %s", pair.Source)
			}
		}
		for _, export := range space.Exports {
			if pair.Destination.Contains(export.Alias) {
				return isolation, fmt.Errorf("external alias %s overlaps a pod mapping", export.Alias)
			}
		}
	}
	out := isolation.DeepCopy()
	peers := 0
	for i, rule := range spec.Ingress {
		translated, err := translate(rule.From, rule.Ports, space, false)
		if err != nil {
			return isolation, fmt.Errorf("ingress[%d]: %w", i, err)
		}
		peers += len(translated)
		if peers > 4096 {
			return isolation, fmt.Errorf("translated policy exceeds peer limit")
		}
		if len(rule.From) == 0 || len(translated) > 0 {
			out.Ingress = append(out.Ingress, networking.NetworkPolicyIngressRule{From: translated, Ports: rule.DeepCopy().Ports})
		}
	}
	for i, rule := range spec.Egress {
		translated, err := translate(rule.To, rule.Ports, space, true)
		if err != nil {
			return isolation, fmt.Errorf("egress[%d]: %w", i, err)
		}
		peers += len(translated)
		if peers > 4096 {
			return isolation, fmt.Errorf("translated policy exceeds peer limit")
		}
		if len(rule.To) == 0 || len(translated) > 0 {
			out.Egress = append(out.Egress, networking.NetworkPolicyEgressRule{To: translated, Ports: rule.DeepCopy().Ports})
		}
	}
	return out, nil
}

func translate(peers []networking.NetworkPolicyPeer, ports []networking.NetworkPolicyPort, space AddressSpace, egress bool) ([]networking.NetworkPolicyPeer, error) {
	for _, port := range ports {
		if port.EndPort != nil || (port.Port != nil && port.Port.Type != intstr.Int) ||
			(port.Protocol != nil && *port.Protocol != core.ProtocolTCP && *port.Protocol != core.ProtocolUDP) {
			return nil, fmt.Errorf("unsupported port or protocol")
		}
	}
	var out []networking.NetworkPolicyPeer
	for _, peer := range peers {
		if peer.PodSelector != nil || peer.NamespaceSelector != nil {
			return nil, fmt.Errorf("unsupported peer selector")
		}
		if peer.IPBlock == nil {
			out = append(out, networking.NetworkPolicyPeer{})
			continue
		}
		ranges, err := translateBlock(peer.IPBlock, space, egress)
		if err != nil {
			return nil, err
		}
		for _, p := range ranges {
			out = append(out, networking.NetworkPolicyPeer{IPBlock: &networking.IPBlock{CIDR: p.String()}})
		}
		if len(out) > 4096 {
			return nil, fmt.Errorf("translated rule exceeds peer limit")
		}
	}
	return out, nil
}

func parsePrefix(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("expected IPv4 CIDR: %q", value)
	}
	return p.Masked(), nil
}

func translateBlock(block *networking.IPBlock, space AddressSpace, egress bool) ([]netip.Prefix, error) {
	base, err := parsePrefix(block.CIDR)
	if err != nil {
		return nil, err
	}
	for _, service := range space.Services {
		if service.Bits() <= base.Bits() && service.Contains(base.Addr()) {
			return nil, fmt.Errorf("unsupported Service CIDR %s", base)
		}
	}
	allowed := []netip.Prefix{base}
	for _, value := range block.Except {
		p, err := parsePrefix(value)
		if err != nil {
			return nil, err
		}
		if p.Bits() <= base.Bits() || !base.Contains(p.Addr()) {
			return nil, fmt.Errorf("invalid exception %s for %s", p, base)
		}
		for _, service := range space.Services {
			if p.Overlaps(service) {
				return nil, fmt.Errorf("unsupported Service CIDR exception %s", p)
			}
		}
		allowed = subtract(allowed, p)
	}
	remaining := slices.Clone(allowed)
	var output []netip.Prefix
	for _, pair := range space.Pairs {
		if !pair.Source.Addr().Is4() || !pair.Destination.Addr().Is4() || pair.Source.Bits() != pair.Destination.Bits() {
			return nil, fmt.Errorf("invalid prefix mapping %s to %s", pair.Source, pair.Destination)
		}
		for _, p := range allowed {
			if p.Overlaps(pair.Source) {
				if p.Bits() < pair.Source.Bits() {
					p = pair.Source
				}
				output = append(output, netip.PrefixFrom(MapAddress(p.Addr(), pair.Destination), p.Bits()))
			}
		}
		remaining = subtract(remaining, pair.Source)
	}
	for _, p := range space.Indirect {
		if !egress && slices.ContainsFunc(allowed, p.Overlaps) {
			return nil, fmt.Errorf("indirect ingress source in %s is unresolved", p)
		}
		if base.Bits() == 32 && contains(allowed, base.Addr()) && p.Contains(base.Addr()) &&
			!slices.ContainsFunc(space.Exports, func(e Export) bool { return e.Original == base.Addr() }) {
			return nil, fmt.Errorf("no export for %s", base)
		}
		remaining = subtract(remaining, p)
	}
	for _, p := range space.Reserved {
		remaining = subtract(remaining, p)
	}
	for _, p := range space.Services {
		remaining = subtract(remaining, p)
	}
	for _, pair := range space.Pairs {
		remaining = subtract(remaining, pair.Destination)
	}
	if egress {
		for _, ip := range space.Pending {
			if contains(allowed, ip) {
				return nil, fmt.Errorf("no ready allocation for %s", ip)
			}
		}
		for _, e := range space.Exports {
			if contains(allowed, e.Original) {
				output = append(output, netip.PrefixFrom(e.Alias, 32))
			}
			remaining = subtract(remaining, netip.PrefixFrom(e.Alias, 32))
		}
	}
	output = append(output, remaining...)
	slices.SortFunc(output, func(a, b netip.Prefix) int { return strings.Compare(a.String(), b.String()) })
	return slices.Compact(output), nil
}

func contains(prefixes []netip.Prefix, addr netip.Addr) bool {
	return slices.ContainsFunc(prefixes, func(p netip.Prefix) bool { return p.Contains(addr) })
}

// MapAddress preserves host bits from an IPv4 address in a validated prefix pair.
func MapAddress(ip netip.Addr, destination netip.Prefix) netip.Addr {
	out := destination.Masked().Addr().As4()
	src := ip.As4()
	for bit := destination.Bits(); bit < 32; bit++ {
		mask := byte(0x80 >> (bit % 8))
		out[bit/8] |= src[bit/8] & mask
	}
	return netip.AddrFrom4(out)
}

func subtract(prefixes []netip.Prefix, cut netip.Prefix) []netip.Prefix {
	var out []netip.Prefix
	for _, p := range prefixes {
		for p.Overlaps(cut) && p.Bits() < cut.Bits() {
			bit := p.Bits()
			right := p.Addr().As4()
			right[bit/8] |= 0x80 >> (bit % 8)
			leftPrefix := netip.PrefixFrom(p.Addr(), bit+1)
			rightPrefix := netip.PrefixFrom(netip.AddrFrom4(right), bit+1)
			if leftPrefix.Overlaps(cut) {
				out = append(out, rightPrefix)
				p = leftPrefix
			} else {
				out = append(out, leftPrefix)
				p = rightPrefix
			}
		}
		if !p.Overlaps(cut) {
			out = append(out, p)
		}
	}
	return out
}
