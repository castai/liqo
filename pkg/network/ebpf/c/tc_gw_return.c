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
 * The shared LPM trie pinned at /sys/fs/bpf/liqo_local_routes_poc tells us
 * whether the destination belongs to the overlay and supplies the VNI.
 */

#include "common.h"

struct {
	__uint(type, BPF_MAP_TYPE_LPM_TRIE);
	__uint(max_entries, LIQO_ROUTES_MAX_ENTRIES);
	__uint(key_size, sizeof(struct lpm_key));
	__uint(value_size, sizeof(struct route_value));
	__uint(pinning, LIBBPF_PIN_BY_NAME);
	__uint(map_flags, BPF_F_NO_PREALLOC);
} liqo_local_routes_poc __section(".maps");

/*
 * Debug counters indexed by the constants below.  They are exposed as a
 * pinned map so userspace can dump them without relying on bpf_trace_printk,
 * which is rejected by some kernels/verifiers.
 */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 8);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, sizeof(__u64));
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} liqo_gw_return_stats_poc __section(".maps");

enum {
	STAT_ENTERED = 0,
	STAT_IPV4,
	STAT_LOOKUP_HIT,
	STAT_LOOKUP_MISS,
	STAT_TUNNEL_KEY_OK,
	STAT_REDIRECT,
	STAT_ETH_FALLBACK,
	STAT_TRUNCATED,
};

static __always_inline void inc_stat(__u32 idx)
{
	__u64 *counter = bpf_map_lookup_elem(&liqo_gw_return_stats_poc, &idx);
	if (counter)
		*counter += 1;
}

/*
 * Debug snapshot map: records what the program actually observes for the last
 * few packets so we can distinguish "wrong daddr" from "wrong hook state".
 * Index layout (each a __u32):
 *   0 = last skb->protocol (host order, e.g. 0x0800 for IPv4)
 *   1 = last skb->len
 *   2 = last ip->daddr (network order, raw bytes as in packet memory)
 *   3 = last ip->saddr (network order)
 *   4 = last data_end - data (linear bytes available)
 */
struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 8);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, sizeof(__u32));
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} liqo_gw_return_dbg_poc __section(".maps");

static __always_inline void dbg_set(__u32 idx, __u32 val)
{
	bpf_map_update_elem(&liqo_gw_return_dbg_poc, &idx, &val, BPF_ANY);
}

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

	inc_stat(STAT_ENTERED);

	/* Snapshot the raw skb state for every packet (debug). */
	dbg_set(0, bpf_ntohs(skb->protocol));
	dbg_set(1, skb->len);
	dbg_set(4, (__u32)(data_end - data));

	/*
	 * liqo-tunnel is a kernel WireGuard device (POINTOPOINT, NOARP, link/none):
	 * on TC ingress the skb starts directly at the decrypted inner IPv4 header,
	 * with no Ethernet header. Parse it as raw IP unconditionally.
	 *
	 * The previous heuristic sniffed eth->h_proto (bytes 12-13 of the packet),
	 * which for a raw IP packet are the top two octets of the source address,
	 * not an ethertype. That produced a wrong header offset and a garbage
	 * ip->daddr, so every LPM lookup missed. (void)eth keeps the unused local.
	 *
	 * On tunnel devices the decrypted skb is frequently non-linear: only a small
	 * head sits in the [data, data_end) window and the IP header lives in paged
	 * fragments. A direct deref then reads stale/adjacent bytes for ip->daddr
	 * (bytes 16-19), which is why every LPM lookup missed even after the raw-IP
	 * fix. Pull the first sizeof(iphdr) bytes into the linear area first, then
	 * re-read data/data_end (the pull may reallocate the buffer).
	 */
	(void)eth;
	if (bpf_skb_pull_data(skb, sizeof(struct iphdr)) < 0) {
		inc_stat(STAT_TRUNCATED);
		return TC_ACT_OK;
	}
	data = (void *)(long)skb->data;
	data_end = (void *)(long)skb->data_end;

	dbg_set(5, (__u32)(data_end - data)); /* linear bytes after pull (debug) */

	ip = (struct iphdr *)data;
	if ((void *)(ip + 1) > data_end) {
		inc_stat(STAT_TRUNCATED);
		return TC_ACT_OK;
	}

	if (ip->version != 4)
		return TC_ACT_OK;

	inc_stat(STAT_IPV4);

	/* Snapshot the parsed inner addresses (debug). */
	dbg_set(2, ip->daddr);
	dbg_set(3, ip->saddr);

	/*
	 * Check whether the destination belongs to the local overlay.  The local
	 * routes map contains local Pod CIDRs populated by the gateway loader.
	 */
	key.prefixlen = 32;
	key.addr = ip->daddr;

	rv = bpf_map_lookup_elem(&liqo_local_routes_poc, &key);
	if (!rv) {
		inc_stat(STAT_LOOKUP_MISS);
		return TC_ACT_OK;
	}

	inc_stat(STAT_LOOKUP_HIT);

	/*
	 * Save the destination address and tunnel ID in locals before
	 * bpf_skb_change_head — it may reallocate the buffer and invalidate
	 * all packet data pointers (ip, data).
	 */
	__u32 dst_addr = ip->daddr;
	__u32 vni = rv->tunnel_id;

	/*
	 * WireGuard delivers raw IP (no Ethernet header). The Geneve device
	 * (gw-liqo-tun) is an L2 device and expects an Ethernet frame.
	 * Use bpf_skb_change_head to grow the headroom by sizeof(struct ethhdr),
	 * then write an Ethernet header.
	 */
	if (bpf_skb_change_head(skb, sizeof(struct ethhdr), 0))
		return TC_ACT_OK;

	data = (void *)(long)skb->data;
	data_end = (void *)(long)skb->data_end;

	eth = data;
	if ((void *)(eth + 1) > data_end)
		return TC_ACT_OK;

	__builtin_memset(eth, 0, sizeof(*eth));
	eth->h_proto = bpf_htons(ETH_P_IP);

	/*
	 * Encapsulate toward the local pod.  The Geneve destination is the
	 * saved inner IPv4 destination address; the VNI is from the map value.
	 */
	__builtin_memset(&tkey, 0, sizeof(tkey));
	tkey.tunnel_id = vni;
	tkey.remote_ipv4 = bpf_ntohl(dst_addr);
	tkey.tunnel_ttl = 64;

	if (bpf_skb_set_tunnel_key(skb, &tkey, sizeof(tkey), BPF_F_ZERO_CSUM_TX)) {
		return TC_ACT_OK;
	}

	inc_stat(STAT_TUNNEL_KEY_OK);

	inc_stat(STAT_REDIRECT);
	return bpf_redirect(target_ifindex, 0);
}

char _license[] __section("license") = LIQO_EBPF_LICENSE;
