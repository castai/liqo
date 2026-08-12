// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"context"
	"fmt"
	"net"

	"github.com/cilium/ebpf"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/network/ebpf/poc"
)

// RouteReconciler watches Configuration CRDs and reconciles their remapped
// remote Pod CIDRs into the shared eBPF LPM-trie route map.
type RouteReconciler struct {
	client.Client
	Scheme    *runtime.Scheme
	RouteMap  *ebpf.Map
	TunnelID  uint32
	GatewayIP net.IP
}

// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations,verbs=get;list;watch

// Reconcile implements sigs.k8s.io/controller-runtime.Reconciler.
// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *RouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1beta1.Configuration{}).
		Complete(r)
}

func (r *RouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var conf networkingv1beta1.Configuration
	if err := r.Get(ctx, req.NamespacedName, &conf); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		// Object deleted: nothing to do; the map is left as-is for simplicity.
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&conf, "ebpf-overlay.liqo.io/route") {
		controllerutil.AddFinalizer(&conf, "ebpf-overlay.liqo.io/route")
		if err := r.Update(ctx, &conf); err != nil {
			return ctrl.Result{}, err
		}
	}

	if err := r.syncRoutes(ctx, &conf); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RouteReconciler) syncRoutes(ctx context.Context, conf *networkingv1beta1.Configuration) error {
	if conf.Status.Remote == nil {
		return nil
	}

	cidrs := make([]networkingv1beta1.CIDR, 0, len(conf.Status.Remote.CIDR.Pod)+len(conf.Status.Remote.CIDR.External))
	cidrs = append(cidrs, conf.Status.Remote.CIDR.Pod...)
	cidrs = append(cidrs, conf.Status.Remote.CIDR.External...)

	for _, cidr := range cidrs {
		_, ipNet, err := net.ParseCIDR(string(cidr))
		if err != nil {
			return fmt.Errorf("parsing remote pod cidr %q: %w", cidr, err)
		}

		ones, _ := ipNet.Mask.Size()
		key := poc.LPMKey{
			PrefixLen: uint32(ones),
			Addr:      ipv4ToU32(ipNet.IP),
		}
		val := poc.RouteValue{
			GatewayIP: ipv4ToU32(r.GatewayIP),
			TunnelID:  r.TunnelID,
		}

		if err := r.RouteMap.Update(key, val, ebpf.UpdateAny); err != nil {
			return fmt.Errorf("updating route for %s: %w", cidr, err)
		}
	}

	return nil
}

func ipv4ToU32(ip net.IP) uint32 {
	ip = ip.To4()
	if ip == nil {
		return 0
	}
	return uint32(ip[0])<<24 | uint32(ip[1])<<16 | uint32(ip[2])<<8 | uint32(ip[3])
}
