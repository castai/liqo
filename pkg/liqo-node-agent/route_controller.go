// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"context"
	"encoding/binary"
	"fmt"
	"net"
	"time"

	"github.com/cilium/ebpf"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/network/ebpf/poc"
)

const (
	// gatewayNotReadyRequeue is the delay used when the gateway for a
	// Configuration does not exist yet or its internal endpoint is not set.
	gatewayNotReadyRequeue = 10 * time.Second
)

// RouteReconciler watches Configuration CRDs and reconciles their remapped
// remote CIDRs into the forward-path route map and local CIDRs into the
// gateway return-path local-routes map. The gateway IP is resolved dynamically
// from the GatewayClient/GatewayServer associated with the Configuration's
// remote cluster ID.
type RouteReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	RouteMap       *ebpf.Map
	LocalRoutesMap *ebpf.Map
	TunnelID       uint32
}

// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=gatewayclients,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=gatewayservers,verbs=get;list;watch

// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *RouteReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&networkingv1beta1.Configuration{}).
		Watches(
			&networkingv1beta1.GatewayClient{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayEnqueuer),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()[consts.RemoteClusterID] != ""
			})),
		).
		Watches(
			&networkingv1beta1.GatewayServer{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayEnqueuer),
			builder.WithPredicates(predicate.NewPredicateFuncs(func(obj client.Object) bool {
				return obj.GetLabels()[consts.RemoteClusterID] != ""
			})),
		).
		Complete(r)
}

// gatewayEnqueuer maps a GatewayClient or GatewayServer to the Configuration
// resources that have the same remote cluster ID label.
func (r *RouteReconciler) gatewayEnqueuer(ctx context.Context, obj client.Object) []reconcile.Request {
	clusterID := obj.GetLabels()[consts.RemoteClusterID]
	if clusterID == "" {
		return nil
	}

	var confs networkingv1beta1.ConfigurationList
	if err := r.List(ctx, &confs, client.InNamespace(""),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{consts.RemoteClusterID: clusterID})}); err != nil {
		klog.Errorf("failed to list configurations for cluster %s: %v", clusterID, err)
		return nil
	}

	requests := make([]reconcile.Request, 0, len(confs.Items))
	for i := range confs.Items {
		requests = append(requests, reconcile.Request{
			NamespacedName: types.NamespacedName{
				Name:      confs.Items[i].Name,
				Namespace: confs.Items[i].Namespace,
			},
		})
	}
	return requests
}

func (r *RouteReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var conf networkingv1beta1.Configuration
	if err := r.Get(ctx, req.NamespacedName, &conf); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if !controllerutil.ContainsFinalizer(&conf, "ebpf-overlay.liqo.io/route") {
		controllerutil.AddFinalizer(&conf, "ebpf-overlay.liqo.io/route")
		if err := r.Update(ctx, &conf); err != nil {
			return ctrl.Result{}, err
		}
	}

	gatewayIP, err := r.resolveGatewayIP(ctx, &conf)
	if err != nil {
		klog.Infof("gateway for configuration %s/%s not ready: %v", conf.Namespace, conf.Name, err)
		return ctrl.Result{RequeueAfter: gatewayNotReadyRequeue}, nil
	}

	if err := r.syncRoutes(ctx, &conf, gatewayIP); err != nil {
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

func (r *RouteReconciler) resolveGatewayIP(ctx context.Context, conf *networkingv1beta1.Configuration) (net.IP, error) {
	clusterID := conf.Labels[consts.RemoteClusterID]
	if clusterID == "" {
		return nil, fmt.Errorf("configuration %s/%s has no %s label", conf.Namespace, conf.Name, consts.RemoteClusterID)
	}

	var clients networkingv1beta1.GatewayClientList
	if err := r.List(ctx, &clients, client.InNamespace(""),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{consts.RemoteClusterID: clusterID})}); err != nil {
		return nil, fmt.Errorf("listing gateway clients for cluster %s: %w", clusterID, err)
	}
	for i := range clients.Items {
		if ip := internalEndpointIP(&clients.Items[i]); ip != nil {
			return ip, nil
		}
	}

	var servers networkingv1beta1.GatewayServerList
	if err := r.List(ctx, &servers, client.InNamespace(""),
		client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(labels.Set{consts.RemoteClusterID: clusterID})}); err != nil {
		return nil, fmt.Errorf("listing gateway servers for cluster %s: %w", clusterID, err)
	}
	for i := range servers.Items {
		if ip := internalEndpointIP(&servers.Items[i]); ip != nil {
			return ip, nil
		}
	}

	return nil, fmt.Errorf("no gateway with populated internalEndpoint found for cluster %s", clusterID)
}

func internalEndpointIP(obj client.Object) net.IP {
	switch gw := obj.(type) {
	case *networkingv1beta1.GatewayClient:
		if gw.Status.InternalEndpoint != nil && gw.Status.InternalEndpoint.IP != nil {
			return net.ParseIP(string(*gw.Status.InternalEndpoint.IP))
		}
	case *networkingv1beta1.GatewayServer:
		if gw.Status.InternalEndpoint != nil && gw.Status.InternalEndpoint.IP != nil {
			return net.ParseIP(string(*gw.Status.InternalEndpoint.IP))
		}
	}
	return nil
}

func (r *RouteReconciler) syncRoutes(ctx context.Context, conf *networkingv1beta1.Configuration, gatewayIP net.IP) error {
	// Forward path: remote remapped CIDRs are reached via the local gateway pod.
	if conf.Status.Remote != nil {
		cidrs := make([]networkingv1beta1.CIDR, 0, len(conf.Status.Remote.CIDR.Pod)+len(conf.Status.Remote.CIDR.External))
		cidrs = append(cidrs, conf.Status.Remote.CIDR.Pod...)
		cidrs = append(cidrs, conf.Status.Remote.CIDR.External...)

		for _, cidr := range cidrs {
			if err := r.updateRoute(r.RouteMap, cidr, gatewayIP, r.TunnelID); err != nil {
				return fmt.Errorf("updating remote route for %s: %w", cidr, err)
			}
		}
	}

	// Return path: local pod CIDRs are local destinations that the gateway must
	// re-encapsulate into Geneve and deliver to the receiving pod.
	if r.LocalRoutesMap != nil && conf.Spec.Local != nil {
		for _, cidr := range conf.Spec.Local.CIDR.Pod {
			if err := r.updateRoute(r.LocalRoutesMap, cidr, gatewayIP, r.TunnelID); err != nil {
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
	// Preserve the raw IPv4 byte order as it appears in packet memory.
	// The eBPF programs read/write __u32 IPv4 values directly from packet and
	// map memory, so the user-space encoding must match the host native endian.
	return binary.NativeEndian.Uint32(ip)
}
