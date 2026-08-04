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

package gateway

import (
	"time"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// FlagName is the type for the name of the flags.
type FlagName string

func (fn FlagName) String() string {
	return string(fn)
}

const (
	// FlagNameName is the name of the Gateway resource.
	FlagNameName FlagName = "name"
	// FlagNameNamespace is the namespace Gateway resource.
	FlagNameNamespace FlagName = "namespace"
	// FlagNameRemoteClusterID is the clusterID of the remote cluster.
	FlagNameRemoteClusterID FlagName = "remote-cluster-id"
	// FlagNameNodeName is the name of the node.
	FlagNameNodeName FlagName = "node-name"
	// FlagNamePodName is the name of the pod.
	FlagNamePodName FlagName = "pod-name"

	// FlagNameGatewayUID is the UID of the Gateway resource.
	FlagNameGatewayUID FlagName = "gateway-uid"

	// FlagNameMode is the mode in which the gateway is configured.
	FlagNameMode FlagName = "mode"

	// FlagNameReconcileTimeout is the reconciliation timeout.
	FlagNameReconcileTimeout FlagName = "reconcile-timeout"

	// FlagNameMetricsAddress is the address for the metrics endpoint.
	FlagNameMetricsAddress FlagName = "metrics-address"
	// FlagNameProbeAddr is the address for the health probe endpoint.
	FlagNameProbeAddr FlagName = "health-probe-bind-address"

	// FlagNamePprofAddr is the address for the pprof endpoint. Empty disables it.
	FlagNamePprofAddr FlagName = "pprof-bind-address"

	// FlagNameEnableNftMonitor is the flag to enable the nftables monitor.
	FlagNameEnableNftMonitor FlagName = "enable-nft-monitor"
	// FlagNameEnableRouteMonitor is the flag to enable the route monitor.
	FlagNameEnableRouteMonitor FlagName = "enable-route-monitor"

	// FlagNameDisableKernelVersionCheck is the flag to enable the kernel version check.
	FlagNameDisableKernelVersionCheck FlagName = "disable-kernel-version-check"
	// FlagNameMinimumKernelVersion is the minimum kernel version required by Liqo.
	FlagNameMinimumKernelVersion FlagName = "minimum-kernel-version"
	// FlagNameEnableMultipathHashPolicy enables 5-tuple hashing to improve ECMP load balancing.
	// When set to true, the gateway writes 1 to /proc/sys/net/ipv4/fib_multipath_hash_policy.
	// This allows the kernel to distribute traffic across ECMP routes based on the
	// 5-tuple (src/dst IP, src/dst port, protocol) instead of the default L3-only hashing.
	// Without this setting, traffic between the same IP pair always uses the same tunnel
	// regardless of the port. Notes: (1) this setting is namespaced and only affects the
	// gateway pod's network namespace; (2) this is meaningful only if the gateway uses multiple
	// parallel tunnels to exchange date with its remote endpoint.
	FlagNameEnableMultipathHashPolicy FlagName = "enable-multipath-hash-policy"
)

// RequiredFlags contains the list of the mandatory flags.
var RequiredFlags = []FlagName{
	FlagNameName,
	FlagNameNamespace,
	FlagNameRemoteClusterID,
	FlagNameMode,
	FlagNameGatewayUID,
	FlagNameNodeName,
	FlagNamePodName,
}

// InitFlags initializes the flags for the gateway.
func InitFlags(flagset *pflag.FlagSet, opts *Options) {
	flagset.StringVar(&opts.Name, FlagNameName.String(), "", "Parent gateway name")
	flagset.StringVar(&opts.Namespace, FlagNameNamespace.String(), "", "Parent gateway namespace")
	flagset.StringVar(&opts.RemoteClusterID, FlagNameRemoteClusterID.String(), "", "ClusterID of the remote cluster")
	flagset.StringVar(&opts.NodeName, FlagNameNodeName.String(), "", "Node name")
	flagset.StringVar(&opts.PodName, FlagNamePodName.String(), "", "Pod name")

	flagset.StringVar(&opts.GatewayUID, FlagNameGatewayUID.String(), "", "Parent gateway resource UID")

	flagset.Var(&opts.Mode, FlagNameMode.String(), "Parent gateway mode")

	flagset.DurationVar(&opts.ReconcileTimeout, FlagNameReconcileTimeout.String(), 10*time.Second, "Reconciliation timeout")

	flagset.StringVar(&opts.MetricsAddress, FlagNameMetricsAddress.String(), "0", "Address for the metrics endpoint")
	flagset.StringVar(&opts.ProbeAddr, FlagNameProbeAddr.String(), "0", "Address for the health probe endpoint")
	flagset.StringVar(&opts.PprofAddr, FlagNamePprofAddr.String(), "", "Address for the pprof endpoint. Empty disables it")

	flagset.BoolVar(&opts.EnableNftMonitor, FlagNameEnableNftMonitor.String(), true, "Enable nftables monitor")
	flagset.BoolVar(&opts.EnableRouteMonitor, FlagNameEnableRouteMonitor.String(), true, "Enable route monitor")

	flagset.BoolVar(&opts.DisableKernelVersionCheck, FlagNameDisableKernelVersionCheck.String(), false, "Disable the kernel version check")
	flagset.Var(&opts.MinimumKernelVersion, FlagNameMinimumKernelVersion.String(), "Minimum kernel version required by Liqo")
	flagset.BoolVar(&opts.EnableMultipathHashPolicy, FlagNameEnableMultipathHashPolicy.String(), false,
		"Set fib_multipath_hash_policy=1 to use 5-tuple hashing for multipath routing")
}

// MarkFlagsRequired marks the flags as required.
func MarkFlagsRequired(cmd *cobra.Command) error {
	for _, flag := range RequiredFlags {
		if err := cmd.MarkFlagRequired(flag.String()); err != nil {
			return err
		}
	}

	return nil
}
