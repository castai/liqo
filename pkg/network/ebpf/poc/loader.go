// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

// Package poc provides a minimal Go loader for the Liqo eBPF overlay PoC.
// It exposes helpers to create and pin the shared LPM-trie route map, to load
// the TC programs with per-attachment constants rewritten, and to attach a
// loaded program to a host-side interface using a clsact qdisc.
package poc

import (
	"fmt"
	"path/filepath"
	"sync"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
)

const (
	// DefaultRoutesMapPath is the pinned path for the shared LPM-trie map
	// used by the pod-side forward path.
	DefaultRoutesMapPath = "/sys/fs/bpf/liqo_routes_poc"
	// DefaultLocalRoutesMapPath is the pinned path for the gateway return
	// path map, which contains local pod CIDRs.
	DefaultLocalRoutesMapPath = "/sys/fs/bpf/liqo_local_routes_poc"
	// RoutesMapName is the BPF object name of the shared forward-path map.
	RoutesMapName = "liqo_routes_poc"
	// LocalRoutesMapName is the BPF object name of the gateway return-path map.
	LocalRoutesMapName = "liqo_local_routes_poc"
	// DefaultMaxEntries is the default number of prefix entries.
	DefaultMaxEntries = 1024
	// bpfMapFlagNoPrealloc mirrors BPF_F_NO_PREALLOC; the constant is not
	// exported by all versions of golang.org/x/sys/unix used in the tree.
	bpfMapFlagNoPrealloc uint32 = 1
)

var raiseMemlockOnce sync.Once

var raiseMemlockErr error

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
	return loadOrCreateLPMTrieMap(pinPath, RoutesMapName)
}

// LoadOrCreateLocalRoutesMap opens or creates the LPM-trie map used by the
// gateway return-path program. It contains local pod CIDRs.
func LoadOrCreateLocalRoutesMap(pinPath string) (*ebpf.Map, error) {
	if pinPath == "" {
		pinPath = DefaultLocalRoutesMapPath
	}
	return loadOrCreateLPMTrieMap(pinPath, LocalRoutesMapName)
}

func loadOrCreateLPMTrieMap(pinPath, name string) (*ebpf.Map, error) {
	raiseMemlockOnce.Do(func() {
		raiseMemlockErr = rlimit.RemoveMemlock()
	})
	if raiseMemlockErr != nil {
		return nil, fmt.Errorf("raising memlock rlimit: %w", raiseMemlockErr)
	}

	spec := &ebpf.MapSpec{
		Name:       name,
		Pinning:    ebpf.PinByName,
		Type:       ebpf.LPMTrie,
		KeySize:    8, // sizeof(struct lpm_key)
		ValueSize:  8, // sizeof(struct route_value)
		MaxEntries: DefaultMaxEntries,
		Flags:      bpfMapFlagNoPrealloc,
	}

	// PinByName uses PinPath as the parent directory and pins the map at
	// PinPath/name. Passing the full intended pin path as PinPath without
	// PinByName silently skips pinning in cilium/ebpf v0.22+.
	m, err := ebpf.NewMapWithOptions(spec, ebpf.MapOptions{
		PinPath: filepath.Dir(pinPath),
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
// the ifindex of gw-liqo-tun, and returns the loaded program. The local
// routes map is supplied via mapReplacements so the program can identify
// local pod destinations that must be re-encapsulated into Geneve.
func LoadGatewayReturnProgram(objectPath string, targetIfindex uint32, localRoutesMap *ebpf.Map) (*ebpf.Program, error) {
	return loadProgram(objectPath, "tc_gw_return", targetIfindex, localRoutesMap)
}

func loadProgram(objectPath, progName string, targetIfindex uint32, replacementMap *ebpf.Map) (*ebpf.Program, error) {
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
	if replacementMap != nil {
		var mapName string
		switch progName {
		case "tc_gw_return":
			mapName = LocalRoutesMapName
		default:
			mapName = RoutesMapName
		}
		opts.MapReplacements = map[string]*ebpf.Map{
			mapName: replacementMap,
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
