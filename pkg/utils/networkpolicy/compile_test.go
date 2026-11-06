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

	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func TestIndirectExportAndExceptions(t *testing.T) {
	addr := netip.MustParseAddr
	prefix := netip.MustParsePrefix
	space := AddressSpace{
		Pairs:    []Pair{{prefix("10.20.0.0/16"), prefix("172.20.0.0/16")}},
		Indirect: []netip.Prefix{prefix("172.26.0.0/16"), prefix("172.27.0.0/16")},
		Reserved: []netip.Prefix{prefix("172.31.0.0/16")},
		Exports: []Export{
			{addr("172.26.4.7"), addr("172.31.0.42")},
			{addr("172.26.4.8"), addr("172.31.0.90")},
			{addr("172.27.4.7"), addr("172.31.0.43")},
		},
	}
	for _, tt := range []struct {
		name, cidr          string
		except, allow, deny []string
	}{
		{"exact", "172.26.4.7/32", nil, []string{"172.31.0.42"}, []string{"172.31.0.43", "172.31.0.90", "172.26.4.7"}},
		{"subnet", "172.26.4.0/24", nil, []string{"172.31.0.42", "172.31.0.90"}, []string{"172.31.0.43"}},
		{"except", "172.26.4.0/24", []string{"172.26.4.8/32"}, []string{"172.31.0.42"}, []string{"172.31.0.90", "172.31.0.43"}},
		{"consumer only", "10.20.0.0/16", nil, []string{"172.20.7.9"}, []string{"172.31.0.42", "10.20.7.9"}},
		{"broad excluded original", "0.0.0.0/0", []string{"10.20.0.0/16", "172.26.4.7/32"}, []string{"192.0.2.1", "172.31.0.90"}, []string{"172.20.7.9", "172.31.0.42", "172.31.0.99"}},
	} {
		t.Run(tt.name, func(t *testing.T) {
			port := intstr.FromInt32(8080)
			in := &networking.NetworkPolicySpec{Egress: []networking.NetworkPolicyEgressRule{{
				To:    []networking.NetworkPolicyPeer{{IPBlock: &networking.IPBlock{CIDR: tt.cidr, Except: tt.except}}},
				Ports: []networking.NetworkPolicyPort{{Port: &port}},
			}}}
			got, err := Compile(in, space)
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Egress) != 1 || len(got.Egress[0].Ports) != 1 || got.Egress[0].Ports[0].Port.IntVal != 8080 {
				t.Fatalf("lost original port: %+v", got)
			}
			var ranges []netip.Prefix
			for _, peer := range got.Egress[0].To {
				ranges = append(ranges, prefix(peer.IPBlock.CIDR))
			}
			for _, ip := range tt.allow {
				if !contains(ranges, addr(ip)) {
					t.Errorf("%s was denied", ip)
				}
			}
			for _, ip := range tt.deny {
				if contains(ranges, addr(ip)) {
					t.Errorf("%s was allowed", ip)
				}
			}
		})
	}
}

func TestEmptyOrUnresolvedPeerNeverBecomesUnrestricted(t *testing.T) {
	space := AddressSpace{Indirect: []netip.Prefix{netip.MustParsePrefix("172.26.0.0/16")}}
	spec := &networking.NetworkPolicySpec{
		PolicyTypes: []networking.PolicyType{networking.PolicyTypeEgress},
		Egress:      []networking.NetworkPolicyEgressRule{{To: []networking.NetworkPolicyPeer{{IPBlock: &networking.IPBlock{CIDR: "172.26.4.0/24"}}}}},
	}
	got, err := Compile(spec, space)
	if err != nil || len(got.Egress) != 0 || len(got.PolicyTypes) != 1 || got.PolicyTypes[0] != networking.PolicyTypeEgress {
		t.Fatalf("empty export set should retain isolation: %+v %v", got, err)
	}
	spec.Egress[0].To[0].IPBlock.CIDR = "172.26.4.7/32"
	got, err = Compile(spec, space)
	if err == nil || len(got.Egress) != 0 {
		t.Fatalf("missing endpoint should retain isolation: %+v %v", got, err)
	}
	spec.Egress[0].To = nil
	got, err = Compile(spec, space)
	if err != nil || len(got.Egress) != 1 || len(got.Egress[0].To) != 0 {
		t.Fatalf("unrestricted source rule should survive: %+v %v", got, err)
	}
}
