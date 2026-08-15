/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright 2019-2026 The Liqo Authors */

/*
 * tc_pod_encap.c
 *
 * TC egress hook attached to a pod's eth0 inside the pod network namespace.
 * It inspects outgoing IPv4 packets and, if the destination matches a remote
 * remapped Pod CIDR, sets a Geneve tunnel key and redirects the packet to the
 * pod-local Geneve LWT interface (liqo-tun).
 *
 * The LPM trie map is pinned at /sys/fs/bpf/liqo_routes_poc by the node agent
 * and shared with every pod program instance.
 */

#include "common.h"

struct {
	__uint(type, BPF_MAP_TYPE_LPM_TRIE);
	__uint(max_entries, LIQO_ROUTES_MAX_ENTRIES);
	__uint(key_size, sizeof(struct lpm_key));
	__uint(value_size, sizeof(struct route_value));
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	/* LPM-trie requires BPF_F_NO_PREALLOC in current kernels. */
	__uint(map_flags, BPF_F_NO_PREALLOC);
} liqo_routes_poc __section(".maps");

SEC("classifier")
int tc_pod_encap(struct __sk_buff *skb)
{
	void *data_end = (void *)(long)skb->data_end;
	void *data = (void *)(long)skb->data;
	struct ethhdr *eth;
	struct iphdr *ip;
	struct lpm_key key;
	struct route_value *rv;
	struct bpf_tunnel_key tkey;

	/* Basic sanity check: need at least an Ethernet header. */
	eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;

	/* We only handle IPv4 in this PoC. */
	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	ip = (struct iphdr *)((void *)eth + sizeof(*eth));
	if ((void *)(ip + 1) > data_end)
		return TC_ACT_OK;

	/* IPv4 only. */
	if (ip->version != 4)
		return TC_ACT_OK;

	/* Look up the destination address in the shared LPM trie. */
	key.prefixlen = 32;
	key.addr = ip->daddr;

	rv = bpf_map_lookup_elem(&liqo_routes_poc, &key);
	if (!rv)
		return TC_ACT_OK;

	/*
	 * Populate the tunnel metadata.  The Geneve interface in the pod netns
	 * is configured in external (metadata) mode, so the kernel will use the
	 * key we provide here to build the outer UDP/Geneve header.
	 */
	__builtin_memset(&tkey, 0, sizeof(tkey));
	tkey.tunnel_id = rv->tunnel_id;
	tkey.remote_ipv4 = rv->gateway_ip;
	tkey.tunnel_ttl = 64;

	if (bpf_skb_set_tunnel_key(skb, &tkey, sizeof(tkey), BPF_F_ZERO_CSUM_TX))
		return TC_ACT_OK;

	/*
	 * Redirect to the pod-local Geneve interface egress path so the kernel
	 * performs Geneve encapsulation using the tunnel metadata set above.
	 */
	return bpf_redirect(target_ifindex, 0);
}

char _license[] __section("license") = LIQO_EBPF_LICENSE;
