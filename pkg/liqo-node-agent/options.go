// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"github.com/spf13/pflag"
)

// Options holds the command-line options for the Liqo node agent.
type Options struct {
	// NodeName is the Kubernetes node name the agent is running on.
	NodeName string
	// RouteMapPath is the pinned path for the shared LPM-trie route map
	// (forward path, used by pods).
	RouteMapPath string
	// LocalRoutesMapPath is the pinned path for the gateway return-path
	// LPM-trie map (local CIDRs).
	LocalRoutesMapPath string
	// PodEncapObject is the filesystem path to tc_pod_encap.o.
	PodEncapObject string
	// GwReturnObject is the filesystem path to tc_gw_return.o.
	GwReturnObject string
	// PodTunnelName is the name of the Geneve interface created inside pods.
	PodTunnelName string
	// GenevePort is the UDP port used for the Geneve overlay.
	GenevePort uint16
	// TunnelID is the default Geneve VNI used by the PoC.
	TunnelID uint32
}

// NewOptions returns a default Options value.
func NewOptions() *Options {
	return &Options{
		RouteMapPath:       "/sys/fs/bpf/liqo_routes_poc",
		LocalRoutesMapPath: "/sys/fs/bpf/liqo_local_routes_poc",
		PodEncapObject:     "/opt/liqo/ebpf/tc_pod_encap.o",
		GwReturnObject: "/opt/liqo/ebpf/tc_gw_return.o",
		PodTunnelName:  "liqo-tun",
		GenevePort:     6091,
		TunnelID:       1,
	}
}

// InitFlags registers the node agent flags on the supplied flag set.
func InitFlags(flags *pflag.FlagSet, o *Options) {
	flags.StringVar(&o.NodeName, "node-name", o.NodeName, "Kubernetes node name this agent runs on")
	flags.StringVar(&o.RouteMapPath, "route-map-path", o.RouteMapPath, "Pinned path for the shared eBPF route map (forward path)")
	flags.StringVar(&o.LocalRoutesMapPath, "local-routes-map-path", o.LocalRoutesMapPath, "Pinned path for the gateway return-path eBPF map")
	flags.StringVar(&o.PodEncapObject, "pod-encap-object", o.PodEncapObject, "Path to the tc_pod_encap.o eBPF object")
	flags.StringVar(&o.GwReturnObject, "gw-return-object", o.GwReturnObject, "Path to the tc_gw_return.o eBPF object")
	flags.StringVar(&o.PodTunnelName, "pod-tunnel-name", o.PodTunnelName, "Name of the Geneve interface created inside pods")
	flags.Uint16Var(&o.GenevePort, "geneve-port", o.GenevePort, "UDP port used for the Geneve overlay")
	flags.Uint32Var(&o.TunnelID, "tunnel-id", o.TunnelID, "Default Geneve VNI")
}
