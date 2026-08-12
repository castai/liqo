// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package returnpath

// Options holds the parameters needed to set up the gateway return path.
type Options struct {
	// RouteMapPath is the pinned path of the shared LPM-trie route map used by
	// the pod-side program.
	RouteMapPath string
	// LocalRoutesMapPath is the pinned path of the LPM-trie map used by the
	// gateway return-path program.
	LocalRoutesMapPath string
	// ObjectPath is the filesystem path to tc_gw_return.o.
	ObjectPath string
	// GenevePort is the UDP destination port used by the Geneve overlay.
	GenevePort uint16
}
