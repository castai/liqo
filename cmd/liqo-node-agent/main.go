// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

// Package main implements the Liqo eBPF overlay node agent. It runs as a
// privileged DaemonSet on every worker node, maintains the shared eBPF route
// map, and injects the pod-namespace datapath into local pods.
package main

import (
	"fmt"
	"net"
	"os"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	nodeagent "github.com/liqotech/liqo/pkg/liqo-node-agent"
	"github.com/liqotech/liqo/pkg/network/ebpf/poc"
	flagsutils "github.com/liqotech/liqo/pkg/utils/flags"
	"github.com/liqotech/liqo/pkg/utils/restcfg"
)

var (
	options = nodeagent.NewOptions()
	scheme  = runtime.NewScheme()
)

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(networkingv1beta1.AddToScheme(scheme))
}

func main() {
	defer klog.Flush()

	var cmd = cobra.Command{
		Use:  "liqo-node-agent",
		RunE: run,
	}

	flagsutils.InitKlogFlags(cmd.Flags())
	restcfg.InitFlags(cmd.Flags())
	nodeagent.InitFlags(cmd.Flags(), options)

	if err := cmd.Execute(); err != nil {
		klog.Error(err)
		os.Exit(1)
	}
}

func run(cmd *cobra.Command, _ []string) error {
	ctx := ctrl.SetupSignalHandler()

	cfg, err := config.GetConfigWithContext("")
	if err != nil {
		return fmt.Errorf("getting kubernetes config: %w", err)
	}

	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		Scheme: scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{},
		},
		Metrics: server.Options{
			BindAddress: "0",
		},
		HealthProbeBindAddress: "0",
		LivenessEndpointName:   "/healthz",
		ReadinessEndpointName:  "/readyz",
	})
	if err != nil {
		return fmt.Errorf("creating manager: %w", err)
	}

	// Ensure the shared LPM-trie route maps are pinned before any program
	// loads them.
	routeMap, err := poc.LoadOrCreateRouteMap(options.RouteMapPath)
	if err != nil {
		return fmt.Errorf("loading route map: %w", err)
	}
	defer routeMap.Close()
	klog.InfoS("eBPF route map ready", "path", options.RouteMapPath)

	localRoutesMap, err := poc.LoadOrCreateLocalRoutesMap(options.LocalRoutesMapPath)
	if err != nil {
		return fmt.Errorf("loading local routes map: %w", err)
	}
	defer localRoutesMap.Close()
	klog.InfoS("eBPF local routes map ready", "path", options.LocalRoutesMapPath)

	// Discover the gateway IP from the environment. In a real implementation
	// this would be sourced from the Gateway CRD/Service; for the PoC we use
	// a flag or env var. Default to a placeholder if unset.
	gatewayIP := net.ParseIP(os.Getenv("LIQO_EBPF_GATEWAY_IP"))
	if gatewayIP == nil {
		gatewayIP = net.ParseIP("10.0.0.1")
		klog.InfoS("LIQO_EBPF_GATEWAY_IP not set, using placeholder", "ip", gatewayIP)
	}

	if err := (&nodeagent.RouteReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		RouteMap:       routeMap,
		LocalRoutesMap: localRoutesMap,
		TunnelID:       options.TunnelID,
		GatewayIP:      gatewayIP,
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up route reconciler: %w", err)
	}

	if err := (&nodeagent.PodReconciler{
		Client:         mgr.GetClient(),
		Scheme:         mgr.GetScheme(),
		NodeName:       options.NodeName,
		RouteMapPath:   options.RouteMapPath,
		PodEncapObject: options.PodEncapObject,
		PodTunnelName:  options.PodTunnelName,
		GenevePort:     options.GenevePort,
		Injector:       nodeagent.NewInjector(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("setting up pod reconciler: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("adding healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("adding readyz check: %w", err)
	}

	log.SetLogger(klog.NewKlogr())

	klog.InfoS("starting liqo-node-agent", "node", options.NodeName)
	return mgr.Start(ctx)
}
