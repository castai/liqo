// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"context"
)

// InjectRequest carries the parameters needed to inject the eBPF datapath
// into a single pod network namespace.
type InjectRequest struct {
	PodNamespace   string
	PodName        string
	PodIP          string
	PID            int
	RouteMapPath   string
	PodEncapObject string
	GeneveRxObject string
	TunnelName     string
	GenevePort     uint16
}

// Injector abstracts the pod namespace injection logic.
type Injector interface {
	Inject(ctx context.Context, req InjectRequest) error
}
