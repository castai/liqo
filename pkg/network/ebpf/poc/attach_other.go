// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build !linux

package poc

import (
	"errors"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
)

// NetnsAction is a no-op on non-Linux platforms.
func NetnsAction(_ string, action func() error) error {
	return action()
}

// AttachTCProgram is not supported on non-Linux platforms.
func AttachTCProgram(*ebpf.Program, int) (link.Link, error) {
	return nil, errors.New("TC attachment is only supported on linux")
}
