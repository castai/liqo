/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright 2019-2026 The Liqo Authors */

/*
 * tc_gw_return.c
 *
 * TC egress hook attached to the gateway's WireGuard interface (liqo-tunnel).
 * It handles the return path of the PoC overlay: traffic coming from the
 * remote cluster and destined to a local pod is encapsulated into Geneve and
 * redirected to the gateway-local Geneve interface (gw-liqo-tun).
 *
 * The destination IPv4 address is used directly as the Geneve remote endpoint,
 * because in the return direction the inner destination IS the local pod that
 * owns the receiving Geneve tunnel.
 *
 * The shared LPM trie pinned at /sys/fs/bpf/liqo_routes_poc tells us whether
 * the destination belongs to the overlay and supplies the VNI.
 */

#include "common.h"

struct {
	__uint(type, BPF_MAP_TYPE_LPM_TRIE);
	__uint(max_entries, LIQO_ROUTES_MAX_ENTRIES);
	__uint(key_size, sizeof(struct lpm_key));
	__uint(value_size, sizeof(struct route_value));
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__uint(map_flags, BPF_F_NO_PREALLOC);
} liqo_routes_poc __section(".maps");

SEC("classifier")
int tc_gw_return(struct __sk_buff *skb)
{
	void *data_end = (void *)(long)skb->data_end;
	void *data = (void *)(long)skb->data;
	struct ethhdr *eth;
	struct iphdr *ip;
	struct lpm_key key;
	struct route_value *rv;
	struct bpf_tunnel_key tkey;

	/* Parse Ethernet header. */
	eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;

	if (eth->h_proto != bpf_htons(ETH_P_IP))
		return TC_ACT_OK;

	/* Parse IPv4 header. */
	ip = (struct iphdr *)((void *)eth + sizeof(*eth));
	if ((void *)(ip + 1) > data_end)
		return TC_ACT_OK;

	if (ip->version != 4)
		return TC_ACT_OK;

	/*
	 * Check whether the destination belongs to the overlay.  The map is
	 * shared with the pod-side program, but on the gateway the matching
	 * entries are local Pod CIDRs populated by the gateway loader.
	 */
	key.prefixlen = 32;
	key.addr = ip->daddr;

	rv = bpf_map_lookup_elem(&liqo_routes_poc, &key);
	if (!rv)
		return TC_ACT_OK;

	/*
	 * Encapsulate toward the local pod.  The Geneve destination is the inner
	 * IPv4 destination address; the VNI is taken from the map value.
	 */
	__builtin_memset(&tkey, 0, sizeof(tkey));
	tkey.tunnel_id = rv->tunnel_id;
	tkey.remote_ipv4 = ip->daddr;
	tkey.tunnel_ttl = 64;

	if (bpf_skb_set_tunnel_key(skb, &tkey, sizeof(tkey), BPF_F_ZERO_CSUM_TX))
		return TC_ACT_OK;

	/* Redirect to gw-liqo-tun. */
	return bpf_redirect(target_ifindex, BPF_F_INGRESS);
}

char _license[] __section("license") = LIQO_EBPF_LICENSE;
