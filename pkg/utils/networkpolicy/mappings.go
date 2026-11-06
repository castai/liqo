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

	netapi "github.com/liqotech/liqo/apis/networking/v1beta1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
)

// InventoryName is the single inventory for a dedicated mapped namespace.
const InventoryName = "liqo-network-policies"

// ParsePrefix parses a canonical IPv4 CIDR.
func ParsePrefix(value string) (netip.Prefix, error) {
	p, err := netip.ParsePrefix(value)
	if err != nil || !p.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("invalid IPv4 network %q", value)
	}
	return p.Masked(), nil
}

// AppendPrefixes parses and appends a list of IPv4 CIDRs.
func AppendPrefixes(out *[]netip.Prefix, in []string) error {
	for _, value := range in {
		p, err := ParsePrefix(value)
		if err != nil {
			return fmt.Errorf("parsing prefix %s: %w", in, err)
		}
		*out = append(*out, p)
	}
	return nil
}

// PrefixPairs validates aligned Liqo Configuration prefixes.
func PrefixPairs(from, to []netapi.CIDR) ([]Pair, error) {
	if len(from) != len(to) {
		return nil, fmt.Errorf("network mapping is incomplete")
	}
	out := make([]Pair, 0, len(from))
	for i := range from {
		a, err := ParsePrefix(from[i].String())
		if err != nil {
			return nil, err
		}
		b, err := ParsePrefix(to[i].String())
		if err != nil {
			return nil, err
		}
		if a.Bits() != b.Bits() {
			return nil, fmt.Errorf("mapping prefixes %s and %s have different lengths", a, b)
		}
		out = append(out, Pair{Source: a, Destination: b})
	}
	return out, nil
}

// Ready reports whether an inventory acknowledgement matches its current specification.
func Ready(set *api.ShadowNetworkPolicySet) bool {
	return set.DeletionTimestamp.IsZero() && set.Spec.Committed && set.Spec.Revision != "" && set.Status.Ready &&
		set.Status.ObservedGeneration == set.Generation && set.Status.Revision == set.Spec.Revision
}
