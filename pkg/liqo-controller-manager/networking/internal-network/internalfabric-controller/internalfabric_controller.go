// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internalfabriccontroller

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	"github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/internal-network/id"
)

// InternalFabricReconciler manage InternalFabric lifecycle.
type InternalFabricReconciler struct {
	client.Client
	Scheme                         *runtime.Scheme
	RouteConfigurationRulePriority int
}

// NewInternalFabricReconciler returns a new InternalFabricReconciler.
func NewInternalFabricReconciler(cl client.Client, s *runtime.Scheme, routeConfigurationRulePriority int) *InternalFabricReconciler {
	return &InternalFabricReconciler{
		Client:                         cl,
		Scheme:                         s,
		RouteConfigurationRulePriority: routeConfigurationRulePriority,
	}
}

// cluster-role
// +kubebuilder:rbac:groups=networking.liqo.io,resources=internalfabrics,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=internalfabrics/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=internalfabrics/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.liqo.io,resources=routeconfigurations,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=routeconfigurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.liqo.io,resources=genevetunnels,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=genevetunnels/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.liqo.io,resources=internalnodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=internalnodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=networking.liqo.io,resources=connections,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=firewallconfigurations,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=firewallconfigurations/finalizers,verbs=update

// Reconcile manage InternalFabric lifecycle.
func (r *InternalFabricReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	internalFabric := &networkingv1beta1.InternalFabric{}
	if err := r.Get(ctx, req.NamespacedName, internalFabric); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(6).Infof("InternalFabric %q not found", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("getting InternalFabric: %w", err)
	}

	if !internalFabric.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(internalFabric, consts.InternalFabricGeneveTunnelFinalizer) {
			if err := deleteGeneveTunnels(ctx, r.Client, internalFabric); err != nil {
				return ctrl.Result{}, fmt.Errorf("deleting Geneve tunnels: %w", err)
			}
		}

		// Remove the geneve tunnel finalizer and the old deprecated one from previous versions.
		updated := controllerutil.RemoveFinalizer(internalFabric, consts.InternalFabricGeneveTunnelFinalizer)
		updated = controllerutil.RemoveFinalizer(internalFabric, "internalfabric-controller.liqo.io/finalizer") || updated
		if updated {
			if err := r.Update(ctx, internalFabric); err != nil {
				return ctrl.Result{}, fmt.Errorf("removing finalizer: %w", err)
			}
		}
		return ctrl.Result{}, nil
	}

	// Ensure stable ECMP marks are allocated for this fabric before building route/firewall
	// configurations, so that mark values never change on reconnect.
	if err := r.ensureECMPMark(ctx, internalFabric); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring ECMP mark: %w", err)
	}

	// List all sibling InternalFabrics with the same remote-cluster-id to build ECMP routes.
	siblings, err := r.listSiblingInternalFabrics(ctx, internalFabric)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("listing sibling InternalFabrics: %w", err)
	}

	// route configuration

	if err := r.ensureRouteConfiguration(ctx, internalFabric, siblings); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring route configuration: %w", err)
	}

	// Per-gateway return-path route configuration:
	// - Only needed when ECMP is active (multiple connected gateways).
	// - Must be removed for disconnected gateways so that restored conntrack marks
	//   do not keep steering traffic to a dead tunnel.
	connected, currentConnected, err := r.connectedGateways(ctx, internalFabric, siblings)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("filtering connected gateways: %w", err)
	}

	if len(connected) > 1 && currentConnected {
		if err := r.ensurePerGatewayReturnRouteConfiguration(ctx, internalFabric); err != nil {
			return ctrl.Result{}, fmt.Errorf("ensuring per-gateway return route configuration: %w", err)
		}
	} else {
		if err := r.cleanupPerGatewayReturnRouteConfiguration(ctx, internalFabric); err != nil {
			return ctrl.Result{}, fmt.Errorf("cleaning up per-gateway return route configuration: %w", err)
		}
	}

	// firewall configuration (ECMP connection tracking marks)

	if err := r.ensureFirewallConfiguration(ctx, internalFabric, siblings); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring firewall configuration: %w", err)
	}

	// geneve tunnels

	var internalNodeList networkingv1beta1.InternalNodeList
	if err := r.List(ctx, &internalNodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("listing InternalNodes: %w", err)
	}

	if err := ensureGeneveTunnels(ctx, r.Client, r.Scheme, internalFabric, &internalNodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring GeneveTunnels: %w", err)
	}

	if err := cleanupGeneveTunnels(ctx, r.Client, internalFabric, &internalNodeList); err != nil {
		return ctrl.Result{}, fmt.Errorf("cleaning up GeneveTunnels: %w", err)
	}

	return ctrl.Result{}, nil
}

// ensureECMPMark ensures that the reconciled InternalFabric has a stable ECMP policy-routing
// mark allocated and persisted in its status. It must be called before ensureRouteConfiguration
// and ensureFirewallConfiguration, which rely on mark values being present.
// Sibling marks are allocated by their own reconciliations; route/firewall functions will return
// an error (and requeue) if a sibling mark is not yet present.
func (r *InternalFabricReconciler) ensureECMPMark(ctx context.Context, internalFabric *networkingv1beta1.InternalFabric) error {
	if internalFabric.Status.ECMPMark != nil {
		return nil
	}

	manager := id.GetECMPMarkManager(ctx, r.Client)
	mark, err := manager.Allocate(client.ObjectKeyFromObject(internalFabric).String())
	if err != nil {
		return fmt.Errorf("allocating ECMP mark for InternalFabric %q: %w",
			client.ObjectKeyFromObject(internalFabric), err)
	}
	internalFabric.Status.ECMPMark = ptr.To(int(mark))
	if err := r.Status().Update(ctx, internalFabric); err != nil {
		return fmt.Errorf("updating ECMP mark status for InternalFabric %q: %w",
			client.ObjectKeyFromObject(internalFabric), err)
	}

	return nil
}

// SetupWithManager register the InternalFabricReconciler to the manager.
func (r *InternalFabricReconciler) SetupWithManager(mgr ctrl.Manager) error {
	internalNodeEnqueuer := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, _ client.Object) []reconcile.Request {
			var requests []reconcile.Request

			var internalFabricList networkingv1beta1.InternalFabricList
			if err := r.List(ctx, &internalFabricList); err != nil {
				klog.Errorf("Unable to list InternalFabrics: %s", err)
				return nil
			}

			for i := range internalFabricList.Items {
				fabric := &internalFabricList.Items[i]

				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(fabric),
				})
			}

			return requests
		},
	)

	connectionEnqueuer := handler.EnqueueRequestsFromMapFunc(
		func(ctx context.Context, obj client.Object) []reconcile.Request {
			conn, ok := obj.(*networkingv1beta1.Connection)
			if !ok {
				return nil
			}

			gatewayNamespace := conn.Spec.GatewayRef.Namespace
			if gatewayNamespace == "" {
				gatewayNamespace = conn.Namespace
			}

			var internalFabricList networkingv1beta1.InternalFabricList
			if err := r.List(ctx, &internalFabricList, client.InNamespace(gatewayNamespace)); err != nil {
				klog.Errorf("Unable to list InternalFabrics: %s", err)
				return nil
			}

			var requests []reconcile.Request
			for i := range internalFabricList.Items {
				internalFabric := &internalFabricList.Items[i]
				owner := metav1.GetControllerOf(internalFabric)
				if owner == nil || owner.Name != conn.Spec.GatewayRef.Name {
					continue
				}

				requests = append(requests, reconcile.Request{
					NamespacedName: client.ObjectKeyFromObject(internalFabric),
				})
			}

			return requests
		},
	)

	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlInternalFabricCM).
		For(&networkingv1beta1.InternalFabric{}).
		Watches(&networkingv1beta1.InternalNode{}, internalNodeEnqueuer).
		Watches(&networkingv1beta1.Connection{}, connectionEnqueuer).
		Owns(&networkingv1beta1.RouteConfiguration{}).
		Owns(&networkingv1beta1.GeneveTunnel{}).
		Complete(r)
}

// listSiblingInternalFabrics returns all InternalFabrics with the same remote-cluster-id label,
// including the one being reconciled.
func (r *InternalFabricReconciler) listSiblingInternalFabrics(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric) ([]networkingv1beta1.InternalFabric, error) {
	remoteClusterID, ok := internalFabric.Labels[consts.RemoteClusterID]
	if !ok {
		return nil, fmt.Errorf("InternalFabric %q has no %q label", client.ObjectKeyFromObject(internalFabric), consts.RemoteClusterID)
	}

	var list networkingv1beta1.InternalFabricList
	if err := r.List(ctx, &list, client.InNamespace(internalFabric.Namespace), client.MatchingLabels{
		consts.RemoteClusterID: remoteClusterID,
	}); err != nil {
		return nil, err
	}

	result := make([]networkingv1beta1.InternalFabric, 0, len(list.Items))
	for i := range list.Items {
		if list.Items[i].DeletionTimestamp.IsZero() && list.Items[i].Spec.Interface.Node.Name != "" {
			result = append(result, list.Items[i])
		}
	}

	// Sort the result by gateway name so that the order is stable across reconciliations.
	slices.SortFunc(result, func(a, b networkingv1beta1.InternalFabric) int {
		return cmp.Compare(gatewayNameFromInternalFabric(&a), gatewayNameFromInternalFabric(&b))
	})

	return result, nil
}

// connectedGateways returns the subset of sibling InternalFabrics whose related Connection resource is in Connected state,
// along with a boolean indicating whether the provided InternalFabric itself is connected.
func (r *InternalFabricReconciler) connectedGateways(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric, fabrics []networkingv1beta1.InternalFabric) ([]networkingv1beta1.InternalFabric, bool, error) {
	if _, ok := gatewayNameFromOwnerRef(internalFabric); !ok {
		klog.V(4).Infof("InternalFabric %q has no gateway owner reference, cannot filter by Connection state",
			client.ObjectKeyFromObject(internalFabric))
		return fabrics, true, nil
	}

	connected := make([]networkingv1beta1.InternalFabric, 0, len(fabrics))
	currentConnected := false
	currentName := internalFabric.Name
	for i := range fabrics {
		fabric := &fabrics[i]
		gwName := gatewayNameFromInternalFabric(fabric)

		connectionName := forge.GatewayResourceName(gwName)
		connection := &networkingv1beta1.Connection{}
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: internalFabric.Namespace,
			Name:      connectionName,
		}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("Connection %q/%q not found, skipping fabric %q",
					internalFabric.Namespace, connectionName, gwName)
				continue
			}
			return nil, false, fmt.Errorf("getting Connection %q/%q: %w",
				internalFabric.Namespace, connectionName, err)
		}

		if connection.Status.Value == networkingv1beta1.Connected {
			connected = append(connected, *fabric)
			if fabric.Name == currentName {
				currentConnected = true
			}
		} else {
			klog.V(4).Infof("Connection %q/%q status is %q, skipping fabric %q",
				internalFabric.Namespace, connectionName, connection.Status.Value, gwName)
		}
	}

	return connected, currentConnected, nil
}

// gatewayNameFromInternalFabric returns the gateway name from the InternalFabric's controller owner reference,
// falling back to the InternalFabric name.
func gatewayNameFromInternalFabric(internalFabric *networkingv1beta1.InternalFabric) string {
	if owner := metav1.GetControllerOf(internalFabric); owner != nil {
		return owner.Name
	}
	return internalFabric.Name
}

// gatewayNameFromOwnerRef returns the name of the gateway owner reference of the InternalFabric.
func gatewayNameFromOwnerRef(internalFabric *networkingv1beta1.InternalFabric) (string, bool) {
	owner := metav1.GetControllerOf(internalFabric)
	if owner == nil {
		return "", false
	}
	return owner.Name, true
}

// fabricECMPMark returns the stable ECMP policy-routing mark assigned to the given InternalFabric.
// It returns an error if the mark has not been allocated yet (ensureECMPMarks should be called first).
func fabricECMPMark(internalFabric *networkingv1beta1.InternalFabric) (int, error) {
	if internalFabric.Status.ECMPMark == nil {
		return 0, fmt.Errorf("InternalFabric %q has no ECMP mark allocated", client.ObjectKeyFromObject(internalFabric))
	}
	return *internalFabric.Status.ECMPMark, nil
}
