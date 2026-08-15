// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

//go:build linux

package poc

import (
	"errors"
	"fmt"
	"io"

	"github.com/cilium/ebpf"
	"github.com/vishvananda/netlink"
	gnetns "github.com/vishvananda/netns"
	"golang.org/x/sys/unix"
)

type tcDirection uint32

const (
	tcDirectionIngress tcDirection = netlink.HANDLE_MIN_INGRESS
	tcDirectionEgress  tcDirection = netlink.HANDLE_MIN_EGRESS
)

// NetnsAction runs action inside the network namespace identified by nsPath
// (e.g. "/proc/<pid>/ns/net") and restores the original namespace on return.
func NetnsAction(nsPath string, action func() error) error {
	origNs, err := gnetns.Get()
	if err != nil {
		return fmt.Errorf("getting current netns: %w", err)
	}
	defer origNs.Close()

	targetNs, err := gnetns.GetFromPath(nsPath)
	if err != nil {
		return fmt.Errorf("getting netns %s: %w", nsPath, err)
	}
	defer targetNs.Close()

	if err := gnetns.Set(targetNs); err != nil {
		return fmt.Errorf("entering netns %s: %w", nsPath, err)
	}
	if err := action(); err != nil {
		_ = gnetns.Set(origNs)
		return err
	}
	if err := gnetns.Set(origNs); err != nil {
		return fmt.Errorf("restoring original netns: %w", err)
	}
	return nil
}

// AttachTCProgram attaches prog to the egress side of ifindex inside the
// current network namespace using a clsact qdisc. It returns an io.Closer
// that detaches the program and removes the qdisc when closed.
func AttachTCProgram(prog *ebpf.Program, ifindex int) (io.Closer, error) {
	return attachTCProgram(prog, ifindex, tcDirectionEgress)
}

// AttachTCProgramIngress attaches prog to the ingress side of ifindex inside
// the current network namespace using a clsact qdisc.
func AttachTCProgramIngress(prog *ebpf.Program, ifindex int) (io.Closer, error) {
	return attachTCProgram(prog, ifindex, tcDirectionIngress)
}

func attachTCProgram(prog *ebpf.Program, ifindex int, direction tcDirection) (io.Closer, error) {
	if prog == nil {
		return nil, errors.New("program is nil")
	}

	attrs := netlink.QdiscAttrs{
		LinkIndex: ifindex,
		Handle:    netlink.MakeHandle(0xffff, 0),
		Parent:    netlink.HANDLE_CLSACT,
	}
	qdisc := &netlink.Clsact{QdiscAttrs: attrs}
	if err := netlink.QdiscAdd(qdisc); err != nil && !errors.Is(err, unix.EEXIST) {
		return nil, fmt.Errorf("adding clsact qdisc for ifindex %d: %w", ifindex, err)
	}

	filter := &netlink.BpfFilter{
		FilterAttrs: netlink.FilterAttrs{
			LinkIndex: ifindex,
			Parent:    uint32(direction),
			Handle:    1,
			Protocol:  unix.ETH_P_ALL,
			Priority:  1,
		},
		Fd:           prog.FD(),
		Name:         prog.String(),
		DirectAction: true,
	}
	if err := netlink.FilterAdd(filter); err != nil {
		if !errors.Is(err, unix.EEXIST) {
			return nil, fmt.Errorf("adding bpf filter on ifindex %d: %w", ifindex, err)
		}
		// A previous injection left a filter attached. Replace it so the
		// pod picks up the current program and route map.
		if err := netlink.FilterReplace(filter); err != nil {
			return nil, fmt.Errorf("replacing bpf filter on ifindex %d: %w", ifindex, err)
		}
	}

	return &tcLink{
		prog:    prog,
		ifindex: ifindex,
		direction: direction,
		filter:  filter,
		qdisc:   qdisc,
	}, nil
}

// tcLink is a minimal link.Link implementation for classic TC clsact filters.
type tcLink struct {
	prog    *ebpf.Program
	ifindex int
	direction tcDirection
	filter  *netlink.BpfFilter
	qdisc   *netlink.Clsact
}

func (l *tcLink) Close() error {
	var closeErr error
	if err := netlink.FilterDel(l.filter); err != nil {
		closeErr = err
	}
	if err := netlink.QdiscDel(l.qdisc); err != nil {
		if closeErr == nil {
			closeErr = err
		}
	}
	l.prog.Close()
	return closeErr
}

func (l *tcLink) Update(prog *ebpf.Program) error {
	if prog == nil {
		return errors.New("program is nil")
	}
	l.filter.Fd = prog.FD()
	l.filter.Name = prog.String()
	if err := netlink.FilterReplace(l.filter); err != nil {
		return fmt.Errorf("replacing bpf filter on ifindex %d: %w", l.ifindex, err)
	}
	l.prog.Close()
	l.prog = prog
	return nil
}
