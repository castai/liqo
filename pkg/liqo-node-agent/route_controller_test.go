// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestIPv4ToU32(t *testing.T) {
	cases := []struct {
		ip   string
		want uint32
	}{
		{"0.0.0.0", 0},
		{"10.0.0.1", 0x0a000001},
		{"192.168.1.1", 0xc0a80101},
		{"255.255.255.255", 0xffffffff},
	}

	for _, tc := range cases {
		t.Run(tc.ip, func(t *testing.T) {
			ip := net.ParseIP(tc.ip)
			assert.Equal(t, tc.want, ipv4ToU32(ip))
		})
	}
}

func TestIPv4ToU32Nil(t *testing.T) {
	assert.Equal(t, uint32(0), ipv4ToU32(nil))
}
