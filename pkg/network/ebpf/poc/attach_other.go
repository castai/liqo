// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build !linux

package poc

import (
	"errors"
	"io"

	"github.com/cilium/ebpf"
)

// NetnsAction is a no-op on non-Linux platforms.
func NetnsAction(_ string, action func() error) error {
	return action()
}

// AttachTCProgram is not supported on non-Linux platforms.
func AttachTCProgram(*ebpf.Program, int) (io.Closer, error) {
	return nil, errors.New("TC attachment is only supported on linux")
}

// AttachTCProgramIngress is not supported on non-Linux platforms.
func AttachTCProgramIngress(*ebpf.Program, int) (io.Closer, error) {
	return nil, errors.New("TC attachment is only supported on linux")
}
