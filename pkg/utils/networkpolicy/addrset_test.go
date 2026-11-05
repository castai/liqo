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
	"math/rand"
	"net/netip"
	"testing"
)

func baseAddr(value uint32) netip.Addr {
	return netip.AddrFrom4([4]byte{
		byte(value >> 24), byte(value >> 16), byte(value >> 8), byte(value),
	})
}

func TestAddressSetPrefixRoundTrip(t *testing.T) {
	cases := []string{
		"0.0.0.0/0",
		"10.0.0.0/8",
		"10.20.0.0/16",
		"192.168.1.0/24",
		"8.8.8.8/32",
		"255.255.255.255/32",
	}
	for _, c := range cases {
		set := MustPrefixSet(c)
		prefixes := set.Prefixes()
		if len(prefixes) != 1 || prefixes[0].String() != c {
			t.Fatalf("round trip of %q returned %v", c, prefixes)
		}
	}
}

func TestAddressSetSubtractProducesCanonicalCover(t *testing.T) {
	all := MustPrefixSet("10.0.0.0/8")
	remainder := all.Subtract(MustPrefixSet("10.20.7.0/24"))
	prefixes := remainder.Prefixes()
	if len(prefixes) == 0 {
		t.Fatalf("expected a nonempty remainder")
	}
	if remainder.Contains(netip.MustParseAddr("10.20.7.9")) {
		t.Fatalf("expected the excluded address to be removed")
	}
	if !remainder.Contains(netip.MustParseAddr("10.20.8.9")) {
		t.Fatalf("expected the surrounding addresses to be preserved")
	}
	// Every prefix must be a subset of the original range.
	for _, prefix := range prefixes {
		if !MustPrefixSet("10.0.0.0/8").Covers(MustPrefixSet(prefix.String())) {
			t.Fatalf("prefix %q is not contained in the original set", prefix)
		}
	}
}

// TestAddressSetOperationsAgainstEnumeration cross-checks the interval implementation against
// brute force on a small domain.
func TestAddressSetOperationsAgainstEnumeration(t *testing.T) {
	domain := MustPrefixSet("10.20.0.0/24")
	_ = domain
	random := rand.New(rand.NewSource(42))
	for i := 0; i < 64; i++ {
		a := randomPrefix(domain, random)
		b := randomPrefix(domain, random)
		for value := uint32(0x0a140000); value < 0x0a140100; value++ {
			address := baseAddr(value)
			inA := a.Contains(address)
			inB := b.Contains(address)
			if got := a.Union(b).Contains(address); got != (inA || inB) {
				t.Fatalf("union mismatch for %s", address)
			}
			if got := a.Intersect(b).Contains(address); got != (inA && inB) {
				t.Fatalf("intersection mismatch for %s", address)
			}
			if got := a.Subtract(b).Contains(address); got != (inA && !inB) {
				t.Fatalf("subtraction mismatch for %s", address)
			}
		}
	}
}

func randomPrefix(domain AddressSet, random *rand.Rand) AddressSet {
	base := uint32(0x0a140000) + uint32(random.Intn(256))
	bits := random.Intn(9) // /24 through /32
	prefix := netip.PrefixFrom(baseAddr(base), 24+bits).Masked()
	set, err := PrefixSet(prefix)
	if err != nil || !domain.Covers(set) {
		return EmptySet()
	}
	return set
}

func TestPrefixMappingMembership(t *testing.T) {
	mappings := []PrefixMapping{
		{Source: netip.MustParsePrefix("10.20.0.0/16"), Destination: netip.MustParsePrefix("172.20.0.0/16")},
		{Source: netip.MustParsePrefix("10.70.0.0/16"), Destination: netip.MustParsePrefix("172.31.0.0/16")},
	}
	for _, m := range mappings {
		if err := m.Validate(); err != nil {
			t.Fatalf("unexpected validation error: %v", err)
		}
		for hostBits := 0; hostBits < 16; hostBits++ {
			source := netip.PrefixFrom(
				netip.MustParseAddr("10.20.0.1").Next(),
				m.Source.Bits()+hostBits).Masked()
			if !m.Source.Contains(source.Addr()) {
				break
			}
			set, _ := PrefixSet(source)
			mapped := m.MapSet(set)
			inverse := m.Invert().MapSet(mapped)
			if !inverse.Covers(set) || !set.Covers(inverse) {
				t.Fatalf("mapping %q is not bijective for %q: %v vs %v",
					m.Source, source, inverse.Prefixes(), set.Prefixes())
			}
		}
	}
}

func TestPrefixMappingRejectsMismatchedLengths(t *testing.T) {
	bad := PrefixMapping{
		Source:      netip.MustParsePrefix("10.0.0.0/8"),
		Destination: netip.MustParsePrefix("172.16.0.0/12"),
	}
	if err := bad.Validate(); err == nil {
		t.Fatalf("expected a validation error")
	}
}

func TestValidateMappingsRejectsOverlaps(t *testing.T) {
	overlap := []PrefixMapping{
		{Source: netip.MustParsePrefix("10.0.0.0/8"), Destination: netip.MustParsePrefix("172.16.0.0/8")},
		{Source: netip.MustParsePrefix("10.1.0.0/16"), Destination: netip.MustParsePrefix("172.17.0.0/16")},
	}
	if err := validateMappings("test", overlap); err == nil {
		t.Fatalf("expected an overlap error")
	}
}

func FuzzAddressSetCanonicalCover(f *testing.F) {
	f.Add(uint32(0x0a140000), uint32(0x0a1400ff), uint32(0x0a140080), uint32(0x0a1400c0))
	f.Add(uint32(0), uint32(0xffffffff), uint32(0), uint32(0))
	f.Fuzz(func(t *testing.T, lo, hi, elo, ehi uint32) {
		if lo > hi {
			lo, hi = hi, lo
		}
		if elo > ehi {
			elo, ehi = ehi, elo
		}
		set := AddressSet{ranges: []Range{{Lo: lo, Hi: hi}}}
		excluded := AddressSet{ranges: []Range{{Lo: elo, Hi: ehi}}}
		result := set.Subtract(excluded)

		// The canonical cover must represent exactly the same set.
		rebuilt := EmptySet()
		for _, prefix := range result.Prefixes() {
			prefixSet, err := PrefixSet(prefix)
			if err != nil {
				t.Fatalf("invalid prefix %q: %v", prefix, err)
			}
			if !prefixSet.Subtract(result).IsEmpty() {
				t.Fatalf("prefix %q is not a subset of the result", prefix)
			}
			rebuilt = rebuilt.Union(prefixSet)
		}
		if rebuilt.Len() != result.Len() {
			t.Fatalf("canonical cover does not preserve the set size: %d != %d", rebuilt.Len(), result.Len())
		}
		if !rebuilt.Covers(result) || !result.Covers(rebuilt) {
			t.Fatalf("canonical cover mismatch: %v vs %s", rebuilt.Prefixes(), result.String())
		}

		// Every exclusion must be honored, and every retained address must not be excluded.
		if !result.Intersect(excluded).IsEmpty() {
			t.Fatalf("result contains excluded addresses")
		}
		if hi-lo+1 < 1<<20 {
			for value := lo; value <= hi; value++ {
				expected := excluded.Contains(baseAddr(value)) == false
				if result.Contains(baseAddr(value)) != expected {
					t.Fatalf("membership mismatch at %s", baseAddr(value))
				}
				if value == ^uint32(0) {
					break
				}
			}
		}
	})
}

func TestRangeToPrefixesEdgeCases(t *testing.T) {
	for _, c := range [][2]uint32{
		{0, 0xffffffff},
		{0xffffffff, 0xffffffff},
		{0, 0},
		{0x0a140000, 0x0a1400ff},
	} {
		set := AddressSet{ranges: []Range{{Lo: c[0], Hi: c[1]}}}
		rebuilt := EmptySet()
		for _, prefix := range set.Prefixes() {
			rebuilt = rebuilt.Union(MustPrefixSet(prefix.String()))
		}
		if !rebuilt.Covers(set) || !set.Covers(rebuilt) {
			t.Fatalf("range %v did not round trip: %v", c, set.Prefixes())
		}
	}
}

func ExampleAddressSet() {
	set := MustPrefixSet("10.20.0.0/16").Subtract(MustPrefixSet("10.20.7.0/24"))
	fmt.Println(len(set.Prefixes()) > 0)
	// Output: true
}
