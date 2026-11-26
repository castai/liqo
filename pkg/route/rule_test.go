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

package route

import (
	"net"
	"testing"

	"github.com/vishvananda/netlink"
	"k8s.io/utils/ptr"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
)

func mustCIDR(s string) *networkingv1beta1.CIDR {
	return ptr.To(networkingv1beta1.CIDR(s))
}

func mustIPNet(s string) *net.IPNet {
	_, parsed, err := net.ParseCIDR(s)
	if err != nil {
		panic(err)
	}
	return parsed
}

func TestIsDefaultPrefix(t *testing.T) {
	testCases := []struct {
		name   string
		prefix string
		expect bool
	}{
		{name: "nil prefix", prefix: "", expect: false},
		{name: "ipv4 all addresses", prefix: "0.0.0.0/0", expect: true},
		{name: "ipv6 all addresses", prefix: "::/0", expect: true},
		{name: "ipv4 specific", prefix: "10.0.0.0/8", expect: false},
		{name: "ipv4 host address", prefix: "0.0.0.0/32", expect: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var prefix *net.IPNet
			if tc.prefix != "" {
				prefix = mustIPNet(tc.prefix)
			}
			if got := isDefaultPrefix(prefix); got != tc.expect {
				t.Errorf("isDefaultPrefix(%v) = %v, expected %v", tc.prefix, got, tc.expect)
			}
		})
	}
}

func TestEqualRuleAddress(t *testing.T) {
	testCases := []struct {
		name   string
		spec   *networkingv1beta1.CIDR
		kernel string // "" means nil
		expect bool
	}{
		// Both sides absent: equal.
		{name: "both nil", spec: nil, kernel: "", expect: true},
		// A nil spec selector means "any": it must be equivalent to an
		// unspecified kernel prefix, however the kernel represents it.
		// Before the fix these compared unequal, causing the catch-all rule
		// to be deleted and re-created on every reconcile.
		{name: "nil spec vs ipv4 all addresses", spec: nil, kernel: "0.0.0.0/0", expect: true},
		{name: "nil spec vs ipv6 all addresses", spec: nil, kernel: "::/0", expect: true},
		{name: "nil spec vs real prefix", spec: nil, kernel: "10.0.0.0/8", expect: false},
		// A set spec selector must not match a kernel rule without the
		// corresponding filter. Before the fix this compared equal, so a
		// diverging kernel rule was never corrected nor removed.
		{name: "set spec vs nil kernel", spec: mustCIDR("10.0.0.0/8"), kernel: "", expect: false},
		{name: "all addresses spec vs nil kernel", spec: mustCIDR("0.0.0.0/0"), kernel: "", expect: true},
		// Regular comparisons.
		{name: "equal prefixes", spec: mustCIDR("10.0.0.0/8"), kernel: "10.0.0.0/8", expect: true},
		{name: "different prefixes", spec: mustCIDR("10.0.0.0/8"), kernel: "192.168.0.0/16", expect: false},
		{name: "different prefix length", spec: mustCIDR("10.0.0.0/8"), kernel: "10.0.0.0/16", expect: false},
		{name: "ipv6 equal prefixes", spec: mustCIDR("2001:db8::/32"), kernel: "2001:db8::/32", expect: true},
		{name: "ipv6 different prefixes", spec: mustCIDR("2001:db8::/32"), kernel: "2001:db8:1::/48", expect: false},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			var kernel *net.IPNet
			if tc.kernel != "" {
				kernel = mustIPNet(tc.kernel)
			}
			if got := equalRuleAddress(tc.spec, kernel); got != tc.expect {
				t.Errorf("equalRuleAddress(%v, %v) = %v, expected %v", tc.spec, tc.kernel, got, tc.expect)
			}
		})
	}
}

func TestRuleIsEqual(t *testing.T) {
	dummyIif := "liqo-tun"
	dummyOif := "eth0"

	testCases := []struct {
		name       string
		rule       networkingv1beta1.Rule
		netlinkArg netlink.Rule
		expect     bool
	}{
		{
			name: "equal catch-all rules (no selectors)",
			rule: networkingv1beta1.Rule{
				Priority: ptr.To(10001),
			},
			netlinkArg: netlink.Rule{Priority: 10001},
			expect:     true,
		},
		{
			name: "catch-all rule with nil priority matches any kernel priority",
			rule: networkingv1beta1.Rule{},
			netlinkArg: netlink.Rule{
				Priority: 32766,
			},
			expect: true,
		},
		{
			name: "equal rules with dst",
			rule: networkingv1beta1.Rule{
				Dst:      mustCIDR("10.0.0.0/8"),
				Priority: ptr.To(10001),
			},
			netlinkArg: netlink.Rule{
				Dst:      mustIPNet("10.0.0.0/8"),
				Priority: 10001,
			},
			expect: true,
		},
		{
			name: "different dst",
			rule: networkingv1beta1.Rule{
				Dst: mustCIDR("10.0.0.0/8"),
			},
			netlinkArg: netlink.Rule{
				Dst: mustIPNet("192.168.0.0/16"),
			},
			expect: false,
		},
		{
			name: "different src",
			rule: networkingv1beta1.Rule{
				Src: mustCIDR("10.0.0.0/8"),
			},
			netlinkArg: netlink.Rule{
				Src: mustIPNet("192.168.0.0/16"),
			},
			expect: false,
		},
		{
			name: "different iif",
			rule: networkingv1beta1.Rule{
				Iif: ptr.To(dummyIif),
			},
			netlinkArg: netlink.Rule{
				IifName: "other0",
			},
			expect: false,
		},
		{
			name: "equal rules with iif and oif",
			rule: networkingv1beta1.Rule{
				Iif: ptr.To(dummyIif),
				Oif: ptr.To(dummyOif),
			},
			netlinkArg: netlink.Rule{
				IifName: dummyIif,
				OifName: dummyOif,
			},
			expect: true,
		},
		{
			name: "fwmark spec set, kernel unmarked",
			rule: networkingv1beta1.Rule{
				FwMark: ptr.To(30590),
			},
			netlinkArg: netlink.Rule{},
			expect:     false,
		},
		{
			name: "fwmark spec nil, kernel marked",
			rule: networkingv1beta1.Rule{},
			netlinkArg: netlink.Rule{
				Mark: 30590,
			},
			expect: false,
		},
		{
			name: "fwmark equal",
			rule: networkingv1beta1.Rule{
				FwMark: ptr.To(30590),
			},
			netlinkArg: netlink.Rule{
				Mark: 30590,
			},
			expect: true,
		},
		{
			name: "fwmark different",
			rule: networkingv1beta1.Rule{
				FwMark: ptr.To(30590),
			},
			netlinkArg: netlink.Rule{
				Mark: 30591,
			},
			expect: false,
		},
		{
			name: "priority different",
			rule: networkingv1beta1.Rule{
				Priority: ptr.To(10000),
			},
			netlinkArg: netlink.Rule{
				Priority: 10001,
			},
			expect: false,
		},
	}

	for _, tc := range testCases {
		t.Run(tc.name, func(t *testing.T) {
			netlinkRule := tc.netlinkArg
			if got := RuleIsEqual(&tc.rule, &netlinkRule); got != tc.expect {
				t.Errorf("RuleIsEqual(%v, %v) = %v, expected %v", tc.rule, tc.netlinkArg, got, tc.expect)
			}
		})
	}
}

func TestRuleIsEqualNilArguments(t *testing.T) {
	rule := networkingv1beta1.Rule{}
	netlinkRule := netlink.Rule{}

	if RuleIsEqual(nil, &netlinkRule) {
		t.Error("RuleIsEqual(nil, rule) should be false")
	}
	if RuleIsEqual(&rule, nil) {
		t.Error("RuleIsEqual(rule, nil) should be false")
	}
	if !RuleIsEqual(&rule, &netlinkRule) {
		t.Error("RuleIsEqual(rule, rule) should be true")
	}
}

func TestExistsRule(t *testing.T) {
	desired := networkingv1beta1.Rule{
		Dst:      mustCIDR("10.0.0.0/8"),
		Priority: ptr.To(10001),
	}
	existing := []netlink.Rule{
		{Dst: mustIPNet("192.168.0.0/16"), Priority: 10000},
		{Dst: mustIPNet("10.0.0.0/8"), Priority: 10001},
	}

	_, exists, err := ExistsRule(&desired, existing)
	if err != nil {
		t.Fatalf("ExistsRule returned error: %v", err)
	}
	if !exists {
		t.Error("ExistsRule should find the desired rule")
	}

	other := networkingv1beta1.Rule{Dst: mustCIDR("172.16.0.0/12")}
	_, exists, err = ExistsRule(&other, existing)
	if err != nil {
		t.Fatalf("ExistsRule returned error: %v", err)
	}
	if exists {
		t.Error("ExistsRule should not find the undesired rule")
	}
}
