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
	"encoding/binary"
	"fmt"
	"math/bits"
	"net/netip"
	"slices"
)

// Range is an inclusive IPv4 address interval.
type Range struct {
	Lo uint32
	Hi uint32
}

// AddressSet is an immutable set of IPv4 addresses represented as a sorted list of
// disjoint, non-adjacent inclusive ranges.
//
// The zero value is a valid empty set.
type AddressSet struct {
	ranges []Range
}

// EmptySet returns an empty address set.
func EmptySet() AddressSet {
	return AddressSet{}
}

// PrefixSet returns the address set of the given prefix.
// IPv6 prefixes are rejected.
func PrefixSet(prefix netip.Prefix) (AddressSet, error) {
	rng, err := prefixRange(prefix)
	if err != nil {
		return AddressSet{}, err
	}
	return AddressSet{ranges: []Range{rng}}, nil
}

// MustPrefixSet is like PrefixSet but panics on invalid input.
// It is intended for tests and constant fixtures.
func MustPrefixSet(prefix string) AddressSet {
	p, err := netip.ParsePrefix(prefix)
	if err != nil {
		panic(err)
	}
	set, err := PrefixSet(p)
	if err != nil {
		panic(err)
	}
	return set
}

// prefixRange converts an IPv4 prefix into an inclusive range.
func prefixRange(prefix netip.Prefix) (Range, error) {
	if !prefix.IsValid() {
		return Range{}, fmt.Errorf("invalid prefix")
	}
	prefix = prefix.Masked()
	base, ok := addrToUint32(prefix.Addr())
	if !ok {
		return Range{}, fmt.Errorf("prefix %q is not IPv4", prefix)
	}
	hostBits := prefix.Addr().BitLen() - prefix.Bits()
	if hostBits > 32 {
		return Range{}, fmt.Errorf("prefix %q has an unsupported length", prefix)
	}
	size := uint64(1) << hostBits
	return Range{Lo: base, Hi: uint32(uint64(base) + size - 1)}, nil
}

// IsEmpty returns whether the set contains no address.
func (s AddressSet) IsEmpty() bool {
	return len(s.ranges) == 0
}

// Len returns the number of addresses in the set.
func (s AddressSet) Len() uint64 {
	var total uint64
	for _, r := range s.ranges {
		total += uint64(r.Hi) - uint64(r.Lo) + 1
	}
	return total
}

// Ranges returns a copy of the underlying ranges.
func (s AddressSet) Ranges() []Range {
	return slices.Clone(s.ranges)
}

// Union returns the union of the two sets.
func (s AddressSet) Union(other AddressSet) AddressSet {
	merged := make([]Range, 0, len(s.ranges)+len(other.ranges))
	merged = append(merged, s.ranges...)
	merged = append(merged, other.ranges...)
	return AddressSet{ranges: normalizeRanges(merged)}
}

// Subtract returns the addresses in s that are not in other.
func (s AddressSet) Subtract(other AddressSet) AddressSet {
	current := s.ranges
	for _, rem := range other.ranges {
		if len(current) == 0 {
			break
		}
		next := make([]Range, 0, len(current))
		for _, r := range current {
			next = append(next, subtractRange(r, rem)...)
		}
		current = next
	}
	return AddressSet{ranges: current}
}

// Intersect returns the intersection of the two sets.
func (s AddressSet) Intersect(other AddressSet) AddressSet {
	result := make([]Range, 0)
	for _, a := range s.ranges {
		for _, b := range other.ranges {
			lo := max(a.Lo, b.Lo)
			hi := min(a.Hi, b.Hi)
			if lo <= hi {
				result = append(result, Range{Lo: lo, Hi: hi})
			}
			if b.Hi > a.Hi {
				break
			}
		}
	}
	return AddressSet{ranges: normalizeRanges(result)}
}

// Contains returns whether the set contains the given address.
func (s AddressSet) Contains(addr netip.Addr) bool {
	value, ok := addrToUint32(addr)
	if !ok {
		return false
	}
	i, found := slices.BinarySearchFunc(s.ranges, value, func(r Range, v uint32) int {
		switch {
		case v < r.Lo:
			return 1
		case v > r.Hi:
			return -1
		default:
			return 0
		}
	})
	_ = i
	return found
}

// Covers returns whether the set contains every address of the given set.
func (s AddressSet) Covers(other AddressSet) bool {
	return other.Subtract(s).IsEmpty()
}

// Prefixes returns the canonical, minimal CIDR cover of the set, in ascending order.
func (s AddressSet) Prefixes() []netip.Prefix {
	result := make([]netip.Prefix, 0, len(s.ranges))
	for _, r := range s.ranges {
		result = append(result, rangeToPrefixes(r)...)
	}
	return result
}

// String returns a compact representation of the set, for diagnostics.
func (s AddressSet) String() string {
	prefixes := s.Prefixes()
	strs := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		strs = append(strs, p.String())
	}
	return fmt.Sprint(strs)
}

func normalizeRanges(ranges []Range) []Range {
	if len(ranges) == 0 {
		return nil
	}
	sorted := slices.Clone(ranges)
	slices.SortFunc(sorted, func(a, b Range) int {
		switch {
		case a.Lo < b.Lo:
			return -1
		case a.Lo > b.Lo:
			return 1
		default:
			return 0
		}
	})

	merged := make([]Range, 0, len(sorted))
	merged = append(merged, sorted[0])
	for _, r := range sorted[1:] {
		last := &merged[len(merged)-1]
		if uint64(r.Lo) <= uint64(last.Hi)+1 {
			if r.Hi > last.Hi {
				last.Hi = r.Hi
			}
			continue
		}
		merged = append(merged, r)
	}
	return merged
}

func subtractRange(a, b Range) []Range {
	// No overlap.
	if b.Hi < a.Lo || b.Lo > a.Hi {
		return []Range{a}
	}
	result := make([]Range, 0, 2)
	if b.Lo > a.Lo {
		result = append(result, Range{Lo: a.Lo, Hi: b.Lo - 1})
	}
	if b.Hi < a.Hi {
		result = append(result, Range{Lo: b.Hi + 1, Hi: a.Hi})
	}
	return result
}

func rangeToPrefixes(r Range) []netip.Prefix {
	var result []netip.Prefix
	lo, hi := uint64(r.Lo), uint64(r.Hi)
	for lo <= hi {
		// The largest power-of-two block aligned at lo that fits in the range.
		maxAlign := 32
		if lo != 0 {
			maxAlign = bits.TrailingZeros32(uint32(lo))
		}
		maxSizeExp := 0
		for maxSizeExp < 32 && (uint64(1)<<(maxSizeExp+1)) <= hi-lo+1 {
			maxSizeExp++
		}
		sizeExp := min(maxAlign, maxSizeExp)
		prefix, err := netip.ParsePrefix(fmt.Sprintf("%s/%d",
			uint32ToAddr(uint32(lo)).String(), 32-sizeExp))
		if err != nil {
			// Unreachable: the synthesized prefix is always valid.
			return result
		}
		result = append(result, prefix)
		lo += uint64(1) << sizeExp
	}
	return result
}

// addrToUint32 converts an IPv4 address (including an IPv4-mapped IPv6 one) to its numeric value.
func addrToUint32(addr netip.Addr) (uint32, bool) {
	if !addr.IsValid() {
		return 0, false
	}
	addr = addr.Unmap()
	if !addr.Is4() {
		return 0, false
	}
	bytes := addr.As4()
	return binary.BigEndian.Uint32(bytes[:]), true
}

func uint32ToAddr(value uint32) netip.Addr {
	var bytes [4]byte
	binary.BigEndian.PutUint32(bytes[:], value)
	return netip.AddrFrom4(bytes)
}
