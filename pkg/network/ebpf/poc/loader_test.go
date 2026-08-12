// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package poc

import (
	"testing"
	"unsafe"

	"github.com/stretchr/testify/assert"
)

// The Go structs must match the C layout (struct lpm_key and struct route_value
// in pkg/network/ebpf/c/common.h) so that ebpf.Update/Delete use the correct
// key/value sizes.
func TestStructSizesMatchC(t *testing.T) {
	assert.Equal(t, 8, int(unsafe.Sizeof(LPMKey{})), "LPMKey size must match sizeof(struct lpm_key)")
	assert.Equal(t, 8, int(unsafe.Sizeof(RouteValue{})), "RouteValue size must match sizeof(struct route_value)")
}
