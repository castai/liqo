// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build linux

package nodeagent

import (
	"context"
	"fmt"
	"os"

	"github.com/vishvananda/netlink"

	"github.com/liqotech/liqo/pkg/network/ebpf/poc"
)

// ebpfInjector implements Injector on Linux nodes.
type ebpfInjector struct{}

// NewInjector returns a Linux-capable Injector.
func NewInjector() Injector {
	return &ebpfInjector{}
}

// Inject creates a Geneve LWT interface inside the pod network namespace and
// attaches the pod encapsulation eBPF program to the pod's eth0 egress.
func (i *ebpfInjector) Inject(ctx context.Context, req InjectRequest) error {
	nsPath := fmt.Sprintf("/proc/%d/ns/net", req.PID)
	if _, err := os.Stat(nsPath); err != nil {
		return fmt.Errorf("pod netns %s not available: %w", nsPath, err)
	}

	return poc.NetnsAction(nsPath, func() error {
		// Find the pod's eth0 and its ifindex.
		eth0, err := netlink.LinkByName("eth0")
		if err != nil {
			return fmt.Errorf("finding eth0 in pod netns: %w", err)
		}

		if err := disableTXChecksumOffload("eth0"); err != nil {
			return err
		}

		// Create the pod-local Geneve LWT interface in external metadata mode.
		// FlowBased tells the kernel that the eBPF program will provide the
		// tunnel destination and VNI via bpf_skb_set_tunnel_key.
		geneve := &netlink.Geneve{
			LinkAttrs: netlink.LinkAttrs{
				Name: req.TunnelName,
			},
			FlowBased: true,
			Dport:     req.GenevePort,
		}
		if err := netlink.LinkAdd(geneve); err != nil && !os.IsExist(err) {
			return fmt.Errorf("creating Geneve interface %s: %w", req.TunnelName, err)
		}
		if err := netlink.LinkSetUp(geneve); err != nil {
			return fmt.Errorf("bringing up %s: %w", req.TunnelName, err)
		}

		// Refresh to obtain the kernel-assigned ifindex.
		link, err := netlink.LinkByName(req.TunnelName)
		if err != nil {
			return fmt.Errorf("fetching ifindex of %s: %w", req.TunnelName, err)
		}
		tun, ok := link.(*netlink.Geneve)
		if !ok {
			return fmt.Errorf("interface %s is not a Geneve device", req.TunnelName)
		}

		// Load the route map and the pod encap program, rewriting the
		// target_ifindex constant to point at liqo-tun.
		routeMap, err := poc.LoadOrCreateRouteMap(req.RouteMapPath)
		if err != nil {
			return fmt.Errorf("loading route map: %w", err)
		}
		defer routeMap.Close()

		prog, err := poc.LoadPodProgram(req.PodEncapObject, uint32(tun.Attrs().Index), routeMap)
		if err != nil {
			return fmt.Errorf("loading pod encap program: %w", err)
		}
		defer prog.Close()

		if _, err := poc.AttachTCProgram(prog, eth0.Attrs().Index); err != nil {
			return fmt.Errorf("attaching eBPF to eth0: %w", err)
		}

		// Attach tc_geneve_rx to liqo-tun ingress to force the kernel to
		// accept decapsulated Geneve packets as locally destined (PACKET_HOST),
		// overriding the zero-MAC Ethernet header pushed by the gateway.
		rxProg, err := poc.LoadGeneveRxProgram(req.GeneveRxObject)
		if err != nil {
			return fmt.Errorf("loading geneve rx program: %w", err)
		}
		defer rxProg.Close()

		if _, err := poc.AttachTCProgramIngress(rxProg, tun.Attrs().Index); err != nil {
			return fmt.Errorf("attaching geneve rx to %s: %w", req.TunnelName, err)
		}

		return nil
	})
}
