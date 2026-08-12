// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

// Package poc provides a minimal Go loader for the Liqo eBPF overlay PoC.
// It exposes helpers to create and pin the shared LPM-trie route map, to load
// the TC programs with per-attachment constants rewritten, and to attach a
// loaded program to a host-side interface using a clsact qdisc.
package poc

import (
	"errors"
	"fmt"
	"os"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"
)

const (
	// DefaultRoutesMapPath is the pinned path for the shared LPM-trie map.
	DefaultRoutesMapPath = "/sys/fs/bpf/liqo_routes_poc"
	// RoutesMapName is the BPF object name of the shared map.
	RoutesMapName = "liqo_routes_poc"
	// DefaultMaxEntries is the default number of remote prefix entries.
	DefaultMaxEntries = 1024
	// bpfMapFlagNoPrealloc mirrors BPF_F_NO_PREALLOC; the constant is not
	// exported by all versions of golang.org/x/sys/unix used in the tree.
	bpfMapFlagNoPrealloc uint32 = 1
)

// LPMKey mirrors struct lpm_key in common.h.
type LPMKey struct {
	PrefixLen uint32
	Addr      uint32
}

// RouteValue mirrors struct route_value in common.h.
type RouteValue struct {
	GatewayIP uint32
	TunnelID  uint32
}

// LoadOrCreateRouteMap opens an existing pinned LPM-trie map or creates and
// pins a new one at the given path. The map is shared between the node agent,
// every pod program instance, and the gateway return-path program.
func LoadOrCreateRouteMap(pinPath string) (*ebpf.Map, error) {
	if pinPath == "" {
		pinPath = DefaultRoutesMapPath
	}

	// Prefer an already-pinned map so multiple loaders share the same instance.
	m, err := ebpf.LoadPinnedMap(pinPath, nil)
	if err == nil {
		return m, nil
	}
	if !os.IsNotExist(err) && !errors.Is(err, unix.ENOENT) {
		return nil, fmt.Errorf("loading pinned map %s: %w", pinPath, err)
	}

	spec := &ebpf.MapSpec{
		Name:       RoutesMapName,
		Type:       ebpf.LPMTrie,
		KeySize:    8, // sizeof(struct lpm_key)
		ValueSize:  8, // sizeof(struct route_value)
		MaxEntries: DefaultMaxEntries,
		Flags:      bpfMapFlagNoPrealloc,
	}

	m, err = ebpf.NewMapWithOptions(spec, ebpf.MapOptions{
		PinPath: pinPath,
	})
	if err != nil {
		return nil, fmt.Errorf("creating pinned route map %s: %w", pinPath, err)
	}
	return m, nil
}

// LoadPodProgram loads tc_pod_encap.o, rewrites the volatile constant
// target_ifindex to the ifindex of the pod-local liqo-tun interface, and
// returns the loaded program. The shared route map is supplied via
// mapReplacements so the program shares it with the rest of the node.
func LoadPodProgram(objectPath string, targetIfindex uint32, routeMap *ebpf.Map) (*ebpf.Program, error) {
	return loadProgram(objectPath, "tc_pod_encap", targetIfindex, routeMap)
}

// LoadGatewayReturnProgram loads tc_gw_return.o, rewrites target_ifindex to
// the ifindex of gw-liqo-tun, and returns the loaded program. The shared
// route map is supplied via mapReplacements.
func LoadGatewayReturnProgram(objectPath string, targetIfindex uint32, routeMap *ebpf.Map) (*ebpf.Program, error) {
	return loadProgram(objectPath, "tc_gw_return", targetIfindex, routeMap)
}

func loadProgram(objectPath, progName string, targetIfindex uint32, routeMap *ebpf.Map) (*ebpf.Program, error) {
	spec, err := ebpf.LoadCollectionSpec(objectPath)
	if err != nil {
		return nil, fmt.Errorf("loading collection spec %s: %w", objectPath, err)
	}

	varSpec, ok := spec.Variables["target_ifindex"]
	if !ok {
		return nil, fmt.Errorf("variable target_ifindex not found in %s", objectPath)
	}
	if err := varSpec.Set(targetIfindex); err != nil {
		return nil, fmt.Errorf("rewriting target_ifindex: %w", err)
	}

	opts := ebpf.CollectionOptions{}
	if routeMap != nil {
		opts.MapReplacements = map[string]*ebpf.Map{
			RoutesMapName: routeMap,
		}
	}

	coll, err := ebpf.NewCollectionWithOptions(spec, opts)
	if err != nil {
		return nil, fmt.Errorf("loading collection: %w", err)
	}
	defer coll.Close()

	prog := coll.Programs[progName]
	if prog == nil {
		return nil, fmt.Errorf("program %s not found in %s", progName, objectPath)
	}
	return prog.Clone()
}
