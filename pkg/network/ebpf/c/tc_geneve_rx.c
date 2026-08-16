/* SPDX-License-Identifier: Apache-2.0 */
/* Copyright 2019-2026 The Liqo Authors */

/*
 * tc_geneve_rx.c
 *
 * TC ingress hook attached to a pod's Geneve interface (liqo-tun).
 * The gateway's tc_gw_return program pushes an Ethernet header with
 * zero MAC addresses before encapsulating. On decapsulation, the kernel
 * checks the destination MAC against the interface MAC and drops frames
 * that don't match. This program forces the kernel to treat every
 * decapsulated packet as destined for the local host (PACKET_HOST),
 * regardless of the MAC address.
 *
 * This is safe for the PoC because liqo-tun is a single-tenant overlay
 * interface — all traffic arriving on it is cross-cluster overlay traffic
 * that should be delivered to the pod.
 */

#include "common.h"

SEC("classifier")
int tc_geneve_rx(struct __sk_buff *skb)
{
	/* Force the kernel to accept this packet as locally destined,
	 * overriding the zero-MAC mismatch from the gateway's L2 header push. */
	bpf_skb_change_type(skb, 0); /* PACKET_HOST = 0 */
	return TC_ACT_OK;
}

char _license[] __section("license") = LIQO_EBPF_LICENSE;
