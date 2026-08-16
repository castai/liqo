/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright 2019-2026 The Liqo Authors */

/*
 * tc_gw_forward.c
 *
 * TC ingress hook attached to the gateway's Geneve interface (gw-liqo-tun).
 * It redirects ALL decapsulated traffic coming from remote pods to the
 * WireGuard tunnel interface (liqo-tunnel), replacing the host-level policy
 * routing (ip rule + ip route table 100).
 *
 * This ensures the CNI never sees traffic from remote pods — it only sees
 * pod-to-pod traffic within the local cluster. The eBPF redirect bypasses
 * the host root network namespace routing entirely.
 *
 * target_ifindex is rewritten by the Go loader to the ifindex of
 * liqo-tunnel (the WireGuard interface).
 */

#include "common.h"

struct {
	__uint(type, BPF_MAP_TYPE_ARRAY);
	__uint(max_entries, 4);
	__uint(key_size, sizeof(__u32));
	__uint(value_size, sizeof(__u64));
	__uint(pinning, LIBBPF_PIN_BY_NAME);
} liqo_gw_forward_stats_poc __section(".maps");

enum {
	FWD_STAT_ENTERED = 0,
	FWD_STAT_IPV4,
	FWD_STAT_REDIRECT,
	FWD_STAT_TRUNCATED,
};

static __always_inline void inc_fwd_stat(__u32 idx)
{
	__u64 *counter = bpf_map_lookup_elem(&liqo_gw_forward_stats_poc, &idx);
	if (counter)
		*counter += 1;
}

SEC("classifier")
int tc_gw_forward(struct __sk_buff *skb)
{
	void *data_end = (void *)(long)skb->data_end;
	void *data = (void *)(long)skb->data;
	struct ethhdr *eth;

	inc_fwd_stat(FWD_STAT_ENTERED);

	eth = data;
	if ((void *)(eth + 1) > data_end) {
		inc_fwd_stat(FWD_STAT_TRUNCATED);
		return TC_ACT_OK;
	}

	if (eth->h_proto == bpf_htons(ETH_P_IP))
		inc_fwd_stat(FWD_STAT_IPV4);

	/*
	 * Redirect ALL traffic to the WireGuard tunnel interface.
	 * No LPM lookup — we forward everything coming from the Geneve
	 * decapsulation path. Fine-tuning can be added later.
	 */
	inc_fwd_stat(FWD_STAT_REDIRECT);
	return bpf_redirect(target_ifindex, 0);
}

char _license[] __section("license") = LIQO_EBPF_LICENSE;
