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
)

// PrefixMapping is a host-bit-preserving, equally-sized CIDR mapping between the consumer and
// provider address spaces.
//
// Every address in Source is translated to Destination by preserving the host bits, i.e. the
// offset inside the source prefix is added to the destination network address.
type PrefixMapping struct {
	Source      netip.Prefix
	Destination netip.Prefix
}

// Validate checks that the mapping can be applied with affine prefix arithmetic.
func (m PrefixMapping) Validate() error {
	if !m.Source.IsValid() || !m.Destination.IsValid() {
		return fmt.Errorf("invalid mapping prefixes")
	}
	if m.Source != m.Source.Masked() || m.Destination != m.Destination.Masked() {
		return fmt.Errorf("mapping prefixes must not contain host bits")
	}
	if m.Source.Addr().BitLen() != 32 || m.Destination.Addr().BitLen() != 32 {
		return fmt.Errorf("IPv6 mappings are not supported")
	}
	if m.Source.Bits() != m.Destination.Bits() {
		return fmt.Errorf("mapping %q -> %q has mismatched prefix lengths",
			m.Source, m.Destination)
	}
	return nil
}

// MapAddress translates an address using the mapping, reporting whether it belongs to Source.
func (m PrefixMapping) MapAddress(addr netip.Addr) (netip.Addr, bool) {
	if m.Validate() != nil || !addr.IsValid() {
		return netip.Addr{}, false
	}
	addr = addr.Unmap()
	if !m.Source.Contains(addr) {
		return netip.Addr{}, false
	}
	sourceBase, ok := addrToUint32(m.Source.Addr())
	if !ok {
		return netip.Addr{}, false
	}
	destBase, ok := addrToUint32(m.Destination.Addr())
	if !ok {
		return netip.Addr{}, false
	}
	value, ok := addrToUint32(addr)
	if !ok {
		return netip.Addr{}, false
	}
	return uint32ToAddr(destBase + (value - sourceBase)), true
}

// Invert returns the inverse mapping.
func (m PrefixMapping) Invert() PrefixMapping {
	return PrefixMapping{Source: m.Destination, Destination: m.Source}
}

// MapSet translates every address of the set that belongs to the mapping source.
func (m PrefixMapping) MapSet(set AddressSet) AddressSet {
	if m.Validate() != nil {
		return EmptySet()
	}
	sourceRange, err := prefixRange(m.Source)
	if err != nil {
		return EmptySet()
	}
	sourceBase, ok := addrToUint32(m.Source.Addr())
	if !ok {
		return EmptySet()
	}
	destBase, ok := addrToUint32(m.Destination.Addr())
	if !ok {
		return EmptySet()
	}

	intersection := set.Intersect(AddressSet{ranges: []Range{sourceRange}})
	translated := make([]Range, 0, len(intersection.ranges))
	for _, r := range intersection.ranges {
		translated = append(translated, Range{
			Lo: destBase + (r.Lo - sourceBase),
			Hi: destBase + (r.Hi - sourceBase),
		})
	}
	return AddressSet{ranges: normalizeRanges(translated)}
}

// validateMappings validates a mapping list and checks for ambiguous overlaps, both on the source
// and on the destination side.
func validateMappings(name string, mappings []PrefixMapping) error {
	sourceSets := make([]AddressSet, 0, len(mappings))
	destSets := make([]AddressSet, 0, len(mappings))

	for i := range mappings {
		if err := mappings[i].Validate(); err != nil {
			return fmt.Errorf("%s[%d]: %w", name, i, err)
		}
		sourceSet, err := PrefixSet(mappings[i].Source)
		if err != nil {
			return fmt.Errorf("%s[%d]: %w", name, i, err)
		}
		destSet, err := PrefixSet(mappings[i].Destination)
		if err != nil {
			return fmt.Errorf("%s[%d]: %w", name, i, err)
		}
		for j := range sourceSets {
			if !sourceSets[j].Intersect(sourceSet).IsEmpty() {
				return fmt.Errorf("%s: source prefixes %q and %q overlap",
					name, mappings[j].Source, mappings[i].Source)
			}
			if !destSets[j].Intersect(destSet).IsEmpty() {
				return fmt.Errorf("%s: destination prefixes %q and %q overlap",
					name, mappings[j].Destination, mappings[i].Destination)
			}
		}
		sourceSets = append(sourceSets, sourceSet)
		destSets = append(destSets, destSet)
	}
	return nil
}

// setFromPrefixes builds an address set from a list of CIDRs.
func setFromPrefixes(prefixes []netip.Prefix) (AddressSet, error) {
	result := EmptySet()
	for i := range prefixes {
		set, err := PrefixSet(prefixes[i])
		if err != nil {
			return EmptySet(), fmt.Errorf("prefix %q: %w", prefixes[i], err)
		}
		result = result.Union(set)
	}
	return result, nil
}

// setFromMappings returns the union of the mapping source prefixes.
func setFromMappings(mappings []PrefixMapping) AddressSet {
	result := EmptySet()
	for i := range mappings {
		set, err := PrefixSet(mappings[i].Source)
		if err != nil {
			continue
		}
		result = result.Union(set)
	}
	return result
}

// setFromMappingDestinations returns the union of the mapping destination prefixes.
func setFromMappingDestinations(mappings []PrefixMapping) AddressSet {
	result := EmptySet()
	for i := range mappings {
		set, err := PrefixSet(mappings[i].Destination)
		if err != nil {
			continue
		}
		result = result.Union(set)
	}
	return result
}
