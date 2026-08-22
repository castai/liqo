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

package servercontroller

import (
	"context"
	"fmt"
	"slices"
	"sort"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	internalnetwork "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/internal-network"
	"github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/internal-network/fabricipam"
	netutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/utils"
	"github.com/liqotech/liqo/pkg/utils"
	"github.com/liqotech/liqo/pkg/utils/getters"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// ServerReconciler manage GatewayServer lifecycle.
type ServerReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

// NewServerReconciler returns a new ServerReconciler.
func NewServerReconciler(cl client.Client, s *runtime.Scheme) *ServerReconciler {
	return &ServerReconciler{
		Client: cl,
		Scheme: s,
	}
}

// cluster-role
// +kubebuilder:rbac:groups=networking.liqo.io,resources=gatewayservers,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=gatewayservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=internalfabrics,verbs=get;list;watch;delete;create;update;patch

// Reconcile manage GatewayServer lifecycle.
func (r *ServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	gwServer := &networkingv1beta1.GatewayServer{}
	if err = r.Get(ctx, req.NamespacedName, gwServer); err != nil {
		if apierrors.IsNotFound(err) {
			klog.Infof("Gateway server %q not found", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.Errorf("Unable to get the gateway server %q: %s", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	ipam, err := fabricipam.Get(ctx, r.Client)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("unable to initialize the IPAM: %w", err)
	}

	remoteClusterID, ok := utils.GetClusterIDFromLabels(gwServer.Labels)
	if !ok {
		err = fmt.Errorf("remote cluster ID not found in the gateway server %q", req.NamespacedName)
		klog.Error(err)
		return ctrl.Result{}, err
	}

	configuration, err := getters.GetConfigurationByClusterID(ctx, r.Client, remoteClusterID, corev1.NamespaceAll)
	if err != nil {
		klog.Errorf("Unable to get the configuration for the remote cluster %q: %s", remoteClusterID, err)
		return ctrl.Result{}, err
	}

	if err = r.ensureInternalFabric(ctx, gwServer, configuration, remoteClusterID, ipam); err != nil {
		klog.Errorf("Unable to ensure the internal fabric for the gateway server %q: %s", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	return ctrl.Result{}, nil
}

// ensureInternalFabric ensures the shared InternalFabric for the remote cluster is correctly configured.
// It lists all GatewayServers for the same remote cluster and aggregates their internal endpoints
// into a single InternalFabric with one replica per gateway.
func (r *ServerReconciler) ensureInternalFabric(ctx context.Context, gwServer *networkingv1beta1.GatewayServer,
	configuration *networkingv1beta1.Configuration, remoteClusterID liqov1beta1.ClusterID, ipam *fabricipam.IPAM) error {
	if configuration.Status.Remote == nil {
		return fmt.Errorf("remote configuration not found for the gateway server %q", gwServer.Name)
	}

	// List all GatewayServers for the same remote cluster.
	var gwServerList networkingv1beta1.GatewayServerList
	if err := r.List(ctx, &gwServerList, client.MatchingLabels{
		consts.RemoteClusterID: string(remoteClusterID),
	}); err != nil {
		return fmt.Errorf("listing GatewayServers for remote cluster %q: %w", remoteClusterID, err)
	}

	// Collect gateway endpoints from all non-deleting GatewayServers that have an internal endpoint.
	gateways := make([]internalnetwork.GatewayEndpoint, 0, len(gwServerList.Items))
	for i := range gwServerList.Items {
		gw := &gwServerList.Items[i]
		if !gw.DeletionTimestamp.IsZero() {
			continue
		}
		if gw.Status.InternalEndpoint == nil || gw.Status.InternalEndpoint.IP == nil {
			continue
		}
		gateways = append(gateways, internalnetwork.GatewayEndpoint{
			Name:     gw.Name,
			Endpoint: *gw.Status.InternalEndpoint,
		})
	}

	// If no gateways have internal endpoints, nothing to do.
	if len(gateways) == 0 {
		klog.V(4).Infof("No gateway servers with internal endpoints for remote cluster %q", remoteClusterID)
		return nil
	}

	// Sort for stable ordering.
	sort.Slice(gateways, func(i, j int) bool {
		return gateways[i].Name < gateways[j].Name
	})

	internalFabricName := string(remoteClusterID)

	internalFabric := &networkingv1beta1.InternalFabric{
		ObjectMeta: metav1.ObjectMeta{
			Name:      internalFabricName,
			Namespace: gwServer.Namespace,
		},
	}

	// If the GatewayServer is being deleted and there are no remaining gateways,
	// delete the InternalFabric.
	if !gwServer.DeletionTimestamp.IsZero() && len(gateways) == 0 {
		if err := r.Delete(ctx, internalFabric); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting InternalFabric %q: %w", internalFabricName, err)
		}
		return nil
	}

	if _, err := resource.CreateOrUpdate(ctx, r.Client, internalFabric, func() error {
		if internalFabric.Labels == nil {
			internalFabric.Labels = make(map[string]string)
		}
		internalFabric.Labels[consts.RemoteClusterID] = string(remoteClusterID)

		internalFabric.Spec.MTU = gwServer.Spec.MTU

		var err error
		internalFabric.Spec.Replicas, err = internalnetwork.BuildInternalFabricReplicas(ctx, r.Client, internalFabric, gateways, ipam)
		if err != nil {
			return err
		}

		internalFabric.Spec.RemoteCIDRs = slices.Concat(
			configuration.Status.Remote.CIDR.Pod,
			configuration.Status.Remote.CIDR.External,
		)

		// Set non-controller owner reference so each GatewayServer is tracked as an owner.
		return controllerutil.SetOwnerReference(gwServer, internalFabric, r.Scheme)
	}); err != nil {
		return err
	}

	return nil
}

// SetupWithManager register the ServerReconciler to the manager.
func (r *ServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlGatewayServerInternal).
		For(&networkingv1beta1.GatewayServer{}).
		Watches(
			&networkingv1beta1.Configuration{},
			handler.EnqueueRequestsFromMapFunc(r.gatewayServerEnqueuerByRemoteID()),
			builder.WithPredicates(netutils.AreConfigurationNetworkCIDRsConfiguredPredicate()),
		).
		Complete(r)
}

func (r *ServerReconciler) gatewayServerEnqueuerByRemoteID() handler.MapFunc {
	return func(ctx context.Context, obj client.Object) []reconcile.Request {
		remoteClusterID, ok := utils.GetClusterIDFromLabels(obj.GetLabels())
		if !ok {
			klog.Errorf("unable to get the remote cluster ID from the labels of configuration %s", obj.GetName())
			return nil
		}

		var gwServerList networkingv1beta1.GatewayServerList
		if err := r.List(ctx, &gwServerList, client.MatchingLabels{
			consts.RemoteClusterID: string(remoteClusterID),
		}); err != nil {
			klog.Errorf("unable to list gateway servers for cluster %s: %s", remoteClusterID, err)
			return nil
		}

		var requests []reconcile.Request
		for i := range gwServerList.Items {
			requests = append(requests, reconcile.Request{
				NamespacedName: client.ObjectKeyFromObject(&gwServerList.Items[i]),
			})
		}
		return requests
	}
}
