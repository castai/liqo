// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build linux

// Package returnpath configures the eBPF-based gateway return path for the
// Liqo overlay PoC. When enabled it creates a gw-liqo-tun Geneve interface in
// external-metadata mode and attaches tc_gw_return.o to the egress of the
// gateway's liqo-tunnel interface.
package returnpath

import (
	"errors"
	"fmt"
	"os"

	"github.com/vishvananda/netlink"
	"golang.org/x/sys/unix"
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
	// PolicyRouteTable is the routing table used for traffic decapsulated by
	// gw-liqo-tun.  Any packet coming from that interface is routed to the
	// WireGuard tunnel (liqo-tunnel) via this table.
	PolicyRouteTable = 100
	// PolicyRoutePriority is the priority of the ip rule matching iif gw-liqo-tun.
	PolicyRoutePriority = 100
)

// Setup creates the gw-liqo-tun interface and attaches the eBPF return-path
// program to liqo-tunnel egress. It returns a closer that can be used to tear
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

	attachment, err := poc.AttachTCProgram(prog, tunnelLink.Attrs().Index)
	if err != nil {
		prog.Close()
		return nil, fmt.Errorf("attaching return program to %s: %w", tunnel.TunnelInterfaceName, err)
	}

	klog.InfoS("eBPF return path configured",
		"interface", DefaultInterfaceName,
		"tunnel", tunnel.TunnelInterfaceName,
		"ifindex", tunnelLink.Attrs().Index)

	if err := installPolicyRoute(tunnelLink.Attrs().Index); err != nil {
		_ = attachment.Close()
		return nil, fmt.Errorf("installing policy route: %w", err)
	}

	return func() error {
		var closeErr error
		if err := cleanupPolicyRoute(); err != nil {
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

func installPolicyRoute(tunnelIfindex int) error {
	// Route any packet decapsulated from gw-liqo-tun through the dedicated
	// routing table, where the only/default route points at the WireGuard
	// tunnel interface.
	rule := netlink.NewRule()
	rule.IifName = DefaultInterfaceName
	rule.Table = PolicyRouteTable
	rule.Priority = PolicyRoutePriority
	if err := netlink.RuleAdd(rule); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("adding ip rule iif %s table %d: %w", DefaultInterfaceName, PolicyRouteTable, err)
	}

	route := &netlink.Route{
		LinkIndex: tunnelIfindex,
		Table:     PolicyRouteTable,
	}
	if err := netlink.RouteAdd(route); err != nil && !errors.Is(err, unix.EEXIST) {
		return fmt.Errorf("adding default route in table %d: %w", PolicyRouteTable, err)
	}

	return nil
}

func cleanupPolicyRoute() error {
	var closeErr error

	rule := netlink.NewRule()
	rule.IifName = DefaultInterfaceName
	rule.Table = PolicyRouteTable
	rule.Priority = PolicyRoutePriority
	if err := netlink.RuleDel(rule); err != nil {
		closeErr = fmt.Errorf("deleting ip rule iif %s table %d: %w", DefaultInterfaceName, PolicyRouteTable, err)
	}

	route := &netlink.Route{
		Table: PolicyRouteTable,
	}
	if err := netlink.RouteDel(route); err != nil {
		if closeErr == nil {
			closeErr = fmt.Errorf("deleting default route in table %d: %w", PolicyRouteTable, err)
		}
	}

	return closeErr
}

func createGeneveInterface(port uint16) (netlink.Link, error) {
	geneve := &netlink.Geneve{
		LinkAttrs: netlink.LinkAttrs{
			Name: DefaultInterfaceName,
		},
		FlowBased: true,
		Dport:     port,
	}

	if err := netlink.LinkAdd(geneve); err != nil && !os.IsExist(err) {
		return nil, fmt.Errorf("adding Geneve interface %s: %w", DefaultInterfaceName, err)
	}
	if err := netlink.LinkSetUp(geneve); err != nil {
		return nil, fmt.Errorf("bringing up %s: %w", DefaultInterfaceName, err)
	}

	link, err := netlink.LinkByName(DefaultInterfaceName)
	if err != nil {
		return nil, fmt.Errorf("fetching %s: %w", DefaultInterfaceName, err)
	}
	return link, nil
}
