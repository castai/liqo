// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build !linux

package returnpath

import "errors"

// Setup is not supported on non-Linux platforms.
func Setup(_ Options) (func() error, error) {
	return nil, errors.New("eBPF gateway return path is only supported on linux")
}
