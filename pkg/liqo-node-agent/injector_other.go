// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build !linux

package nodeagent

import (
	"context"
	"errors"
)

// NewInjector returns a stub Injector on non-Linux platforms.
func NewInjector() Injector {
	return &stubInjector{}
}

type stubInjector struct{}

func (i *stubInjector) Inject(_ context.Context, _ InjectRequest) error {
	return errors.New("pod eBPF injection is only supported on linux")
}
