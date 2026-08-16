// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build linux

// Package returnpath configures the eBPF-based gateway return path for the
// Liqo overlay PoC. When enabled it creates a gw-liqo-tun Geneve interface in
// external-metadata mode and attaches tc_gw_return.o to the ingress of the
// gateway's liqo-tunnel interface.
package returnpath

import (
	"fmt"

	"github.com/vishvananda/netlink"
	"k8s.io/klog/v2"

	"github.com/liqotech/liqo/pkg/gateway/tunnel"
	"github.com/liqotech/liqo/pkg/network/ebpf/poc"
)

const (
	// DefaultInterfaceName is the name of the Geneve interface used for the
	// eBPF overlay return path.
	DefaultInterfaceName = "gw-liqo-tun"
	// DefaultObjectPath is the default path to the tc_gw_return.o object.
	DefaultObjectPath = "/opt/liqo/ebpf/tc_gw_return.o"
	// DefaultForwardObjectPath is the default path to the tc_gw_forward.o object.
	DefaultForwardObjectPath = "/opt/liqo/ebpf/tc_gw_forward.o"
)

// Setup creates the gw-liqo-tun interface and attaches the eBPF return-path
// program to liqo-tunnel ingress. It returns a closer that can be used to tear
// down the eBPF attachment. The Geneve interface is left in place to avoid
// disrupting ongoing traffic.
func Setup(opts Options) (func() error, error) {
	if opts.RouteMapPath == "" {
		opts.RouteMapPath = poc.DefaultRoutesMapPath
	}
	if opts.LocalRoutesMapPath == "" {
		opts.LocalRoutesMapPath = poc.DefaultLocalRoutesMapPath
	}
	if opts.ObjectPath == "" {
		opts.ObjectPath = DefaultObjectPath
	}
	if opts.GenevePort == 0 {
		opts.GenevePort = 6091
	}

	// The return-path program only needs the local-routes map; the shared
	// route map is loaded here as well to make sure it exists and is pinned
	// for the pod-side programs on this node.
	routeMap, err := poc.LoadOrCreateRouteMap(opts.RouteMapPath)
	if err != nil {
		return nil, fmt.Errorf("loading route map: %w", err)
	}
	defer routeMap.Close()

	localRoutesMap, err := poc.LoadOrCreateLocalRoutesMap(opts.LocalRoutesMapPath)
	if err != nil {
		return nil, fmt.Errorf("loading local routes map: %w", err)
	}
	defer localRoutesMap.Close()

	link, err := createGeneveInterface(opts.GenevePort)
	if err != nil {
		return nil, fmt.Errorf("creating return-path Geneve interface: %w", err)
	}

	tunnelLink, err := netlink.LinkByName(tunnel.TunnelInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("finding %s: %w", tunnel.TunnelInterfaceName, err)
	}

	prog, err := poc.LoadGatewayReturnProgram(opts.ObjectPath, uint32(link.Attrs().Index), localRoutesMap)
	if err != nil {
		return nil, fmt.Errorf("loading gateway return program: %w", err)
	}

	attachment, err := poc.AttachTCProgramIngress(prog, tunnelLink.Attrs().Index)
	if err != nil {
		prog.Close()
		return nil, fmt.Errorf("attaching return program to %s ingress: %w", tunnel.TunnelInterfaceName, err)
	}

	klog.InfoS("eBPF return path configured",
		"interface", DefaultInterfaceName,
		"tunnel", tunnel.TunnelInterfaceName,
		"ifindex", tunnelLink.Attrs().Index)

	// Attach tc_gw_forward to gw-liqo-tun ingress to redirect ALL Geneve-decapsulated
	// traffic directly to the WireGuard tunnel interface, replacing the host-level
	// policy routing (ip rule + ip route table 100). This ensures the CNI never
	// sees remote pod traffic — it only sees local pod-to-pod traffic.
	forwardProg, err := poc.LoadGatewayForwardProgram(DefaultForwardObjectPath, uint32(tunnelLink.Attrs().Index))
	if err != nil {
		_ = attachment.Close()
		return nil, fmt.Errorf("loading gateway forward program: %w", err)
	}

	forwardAttachment, err := poc.AttachTCProgramIngress(forwardProg, link.Attrs().Index)
	if err != nil {
		forwardProg.Close()
		_ = attachment.Close()
		return nil, fmt.Errorf("attaching forward program to %s ingress: %w", DefaultInterfaceName, err)
	}

	klog.InfoS("eBPF forward path configured",
		"interface", DefaultInterfaceName,
		"tunnel", tunnel.TunnelInterfaceName,
		"geneve_ifindex", link.Attrs().Index,
		"tunnel_ifindex", tunnelLink.Attrs().Index)

	return func() error {
		var closeErr error
		if err := forwardAttachment.Close(); err != nil {
			closeErr = err
		}
		if err := attachment.Close(); err != nil {
			if closeErr == nil {
				closeErr = err
			}
		}
		return closeErr
	}, nil
}

func createGeneveInterface(port uint16) (netlink.Link, error) {
	// If the interface already exists, reuse it. This makes the gateway
	// return-path setup idempotent across pod restarts.
	link, err := netlink.LinkByName(DefaultInterfaceName)
	if err == nil {
		klog.InfoS("Geneve interface already exists, reusing", "interface", DefaultInterfaceName)
		if err := netlink.LinkSetUp(link); err != nil {
			return nil, fmt.Errorf("bringing up existing %s: %w", DefaultInterfaceName, err)
		}
		return link, nil
	}

	geneve := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: DefaultInterfaceName,
		},
		FlowBased: true,
		Dport:     port,
	}

	if err := netlink.LinkAdd(geneve); err != nil {
		return nil, fmt.Errorf("adding Geneve interface %s: %w", DefaultInterfaceName, err)
	}

	link, err = netlink.LinkByName(DefaultInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", DefaultInterfaceName, err)
	}

	if err := netlink.LinkSetUp(link); err != nil {
		return nil, fmt.Errorf("bringing up %s: %w", DefaultInterfaceName, err)
	}
	return link, nil
}
