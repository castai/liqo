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
// remote CIDRs into the forward-path route map and local CIDRs into the
// gateway return-path local-routes map.
type RouteReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RouteMap       *ebpf.Map
	LocalRoutesMap *ebpf.Map
	TunnelID       uint32
	GatewayIP      net.IP
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
	// Forward path: remote remapped CIDRs are reached via the local gateway pod.
	if conf.Status.Remote != nil {
		cidrs := make([]networkingv1beta1.CIDR, 0, len(conf.Status.Remote.CIDR.Pod)+len(conf.Status.Remote.CIDR.External))
		cidrs = append(cidrs, conf.Status.Remote.CIDR.Pod...)
		cidrs = append(cidrs, conf.Status.Remote.CIDR.External...)

		for _, cidr := range cidrs {
			if err := r.updateRoute(r.RouteMap, cidr, r.GatewayIP, r.TunnelID); err != nil {
				return fmt.Errorf("updating remote route for %s: %w", cidr, err)
			}
		}
	}

	// Return path: local pod CIDRs are local destinations that the gateway must
	// re-encapsulate into Geneve and deliver to the receiving pod.  gateway_ip
	// is unused by the return-path eBPF program, but we store the gateway IP
	// for consistency.
	if r.LocalRoutesMap != nil && conf.Spec.Local != nil {
		for _, cidr := range conf.Spec.Local.CIDR.Pod {
			if err := r.updateRoute(r.LocalRoutesMap, cidr, r.GatewayIP, r.TunnelID); err != nil {
				return fmt.Errorf("updating local route for %s: %w", cidr, err)
			}
		}
	}

	return nil
}

func (r *RouteReconciler) updateRoute(m *ebpf.Map, cidr networkingv1beta1.CIDR, gatewayIP net.IP, tunnelID uint32) error {
	_, ipNet, err := net.ParseCIDR(string(cidr))
	if err != nil {
		return fmt.Errorf("parsing cidr %q: %w", cidr, err)
	}

	ones, _ := ipNet.Mask.Size()
	key := poc.LPMKey{
		PrefixLen: uint32(ones),
		Addr:      ipv4ToU32(ipNet.IP),
	}
	val := poc.RouteValue{
		GatewayIP: ipv4ToU32(gatewayIP),
		TunnelID:  tunnelID,
	}

	if err := m.Update(key, val, ebpf.UpdateAny); err != nil {
		return fmt.Errorf("updating route for %s: %w", cidr, err)
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
