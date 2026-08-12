/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright 2019-2026 The Liqo Authors */

#ifndef __LIQO_EBPF_POC_COMMON_H__
#define __LIQO_EBPF_POC_COMMON_H__

#include <linux/bpf.h>
#include <linux/if_ether.h>
#include <linux/ip.h>
#include <linux/ipv6.h>
#include <linux/pkt_cls.h>
#include <linux/tcp.h>
#include <linux/udp.h>
#include <bpf/bpf_helpers.h>
#include <bpf/bpf_endian.h>

/* Pinned LPM-trie map path exposed to user-space (forward path, used by pods). */
#define LIQO_ROUTES_MAP_PATH "/sys/fs/bpf/liqo_routes_poc"
/* Pinned LPM-trie map path for the gateway return path (local pod CIDRs). */
#define LIQO_LOCAL_ROUTES_MAP_PATH "/sys/fs/bpf/liqo_local_routes_poc"

/* Default Geneve port used by the PoC. */
#define LIQO_GENEVE_PORT 6091

/* Maximum number of remote CIDR entries the PoC map can hold. */
#define LIQO_ROUTES_MAX_ENTRIES 1024

/*
 * LPM trie key for IPv4 prefixes.
 * The prefixlen field is interpreted by the kernel as the number of valid bits
 * in the address (network order is fine because the lookup is bitwise).
 */
struct lpm_key {
	__u32 prefixlen;
	__u32 addr;
};

/*
 * Value stored for each remote prefix.
 * gateway_ip is the IPv4 address of the Gateway Pod that terminates the
 * WireGuard tunnel for this remote cluster.
 * tunnel_id is the Geneve VNI used for this overlay segment.
 */
struct route_value {
	__u32 gateway_ip;
	__u32 tunnel_id;
};

/*
 * Volatile constant rewritten by the Go loader per pod/attachment point.
 * It holds the ifindex of the local Geneve tunnel interface (liqo-tun for
 * pods, gw-liqo-tun for the gateway) used as the redirect target.
 */
volatile const __u32 target_ifindex = 0;

/* License accepted by the eBPF verifier for helper calls. */
#ifndef LIQO_EBPF_LICENSE
#define LIQO_EBPF_LICENSE "Dual BSD/GPL"
#endif

#endif /* __LIQO_EBPF_POC_COMMON_H__ */
