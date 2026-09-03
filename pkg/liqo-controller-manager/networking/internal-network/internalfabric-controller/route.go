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
	"context"
	"fmt"
	"slices"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/fabric"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

const defaultMarkRoutePriority = 10001
const defaultECMPRoutePriority = 10000

// GenerateRouteConfigurationName returns the name of the RouteConfiguration associated to the InternalFabric.
// It uses the remote-cluster-id from the InternalFabric labels so all siblings share the same name.
func generateRouteConfigurationName(remoteClusterID string) string {
	return fmt.Sprintf("%s-node-gw", remoteClusterID)
}

func (r *InternalFabricReconciler) ensureRouteConfiguration(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric, siblings []networkingv1beta1.InternalFabric) error {
	if internalFabric.Spec.Interface.Node.Name == "" {
		return fmt.Errorf("internal fabric %q has node interface name empty", client.ObjectKeyFromObject(internalFabric))
	}

	remoteClusterID, ok := internalFabric.Labels[consts.RemoteClusterID]
	if !ok || remoteClusterID == "" {
		return fmt.Errorf("InternalFabric %q does not have a remote-cluster-id label", internalFabric.Name)
	}

	// Filter by Connection status: only connected gateways participate in ECMP next-hops.
	connected, _, err := r.connectedGateways(ctx, internalFabric, siblings)
	if err != nil {
		return fmt.Errorf("filtering connected gateways: %w", err)
	}

	route := &networkingv1beta1.RouteConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateRouteConfigurationName(remoteClusterID),
			Namespace: internalFabric.Namespace,
		},
	}
	_, err = resource.CreateOrUpdate(ctx, r.Client, route, func() error {
		// Forge metadata
		if route.Labels == nil {
			route.Labels = make(labels.Set)
		}
		route.SetLabels(labels.Merge(route.Labels, fabric.ForgeRouteTargetLabels()))

		var rules []networkingv1beta1.Rule

		priority := ptr.To(defaultMarkRoutePriority)
		if r.RouteConfigurationRulePriority > 0 {
			priority = ptr.To(r.RouteConfigurationRulePriority)
		}

		// Add a link-scope route for each configured gateway's inner IP.
		for i := range siblings {
			fabric := &siblings[i]
			rules = append(rules, networkingv1beta1.Rule{
				Dst:      ptr.To(networkingv1beta1.CIDR(fmt.Sprintf("%s/32", fabric.Spec.Interface.Gateway.IP))),
				Priority: priority,
				Routes: []networkingv1beta1.Route{
					{
						Dst:   ptr.To(networkingv1beta1.CIDR(fmt.Sprintf("%s/32", fabric.Spec.Interface.Gateway.IP))),
						Dev:   ptr.To(fabric.Spec.Interface.Node.Name),
						Scope: ptr.To(networkingv1beta1.LinkScope),
					},
				},
			})
		}

		remoteCIDRs := slices.Clone(internalFabric.Spec.RemoteCIDRs) // clone to avoid modifying the original slice and invalidating the cache
		// sort slice to prevent useless updates if CIDRs are in different order
		slices.Sort(remoteCIDRs)

		// Add one rule per remote CIDR.
		// Use a classic single gateway when there is only one connected gateway, otherwise use ECMP next-hops.
		for _, remoteCIDR := range remoteCIDRs {
			var route networkingv1beta1.Route
			route.Dst = ptr.To(remoteCIDR)

			switch len(connected) {
			case 0:
				continue
			case 1:
				route.Gw = ptr.To(connected[0].Spec.Interface.Gateway.IP)
			default:
				nextHops := make([]networkingv1beta1.NextHop, 0, len(connected))
				for i := range connected {
					fab := &connected[i]
					nextHops = append(nextHops, networkingv1beta1.NextHop{
						Gw:  fab.Spec.Interface.Gateway.IP,
						Dev: fab.Spec.Interface.Node.Name,
					})
				}
				route.NextHops = nextHops
			}

			rules = append(rules, networkingv1beta1.Rule{
				Routes:   []networkingv1beta1.Route{route},
				Dst:      ptr.To(remoteCIDR),
				Priority: priority,
			})
		}

		route.Spec = networkingv1beta1.RouteConfigurationSpec{
			Table: networkingv1beta1.Table{
				Name:  route.Name,
				Rules: rules,
			},
		}

		return controllerutil.SetOwnerReference(internalFabric, route, r.Scheme)
	})
	if err != nil {
		klog.Errorf("Unable to create or update RouteConfiguration %q: %s", route.Name, err)
		return err
	}

	return nil
}

func (r *InternalFabricReconciler) ensurePerGatewayReturnRouteConfiguration(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric) error {
	remoteCIDRs := slices.Clone(internalFabric.Spec.RemoteCIDRs) // clone to avoid modifying the original slice and invalidating the cache
	slices.Sort(remoteCIDRs)

	markPriority := ptr.To(defaultECMPRoutePriority)
	if r.RouteConfigurationRulePriority > 0 {
		// We make sure marks always have priority slightly lower than the main route configuration rules.
		// to avoid to do ECMP on return traffic.
		markPriority = ptr.To(r.RouteConfigurationRulePriority - 1)
	}

	remoteClusterID, ok := internalFabric.Labels[consts.RemoteClusterID]
	if !ok {
		return fmt.Errorf("InternalFabric %q has no %q label", client.ObjectKeyFromObject(internalFabric), consts.RemoteClusterID)
	}

	gwName := gatewayNameFromInternalFabric(internalFabric)
	name := replicaRouteConfigurationName(remoteClusterID, gwName)

	mark, err := fabricECMPMark(internalFabric)
	if err != nil {
		return fmt.Errorf("ensuring route configuration: %w", err)
	}

	route := &networkingv1beta1.RouteConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: internalFabric.Namespace,
		},
	}
	_, err = resource.CreateOrUpdate(ctx, r.Client, route, func() error {
		if route.Labels == nil {
			route.Labels = make(labels.Set)
		}
		route.SetLabels(labels.Merge(route.Labels, fabric.ForgeRouteTargetLabels()))
		route.Labels[consts.InternalFabricName] = internalFabric.Name

		var rules []networkingv1beta1.Rule
		for _, remoteCIDR := range remoteCIDRs {
			rules = append(rules, networkingv1beta1.Rule{
				FwMark:   ptr.To(mark),
				Dst:      ptr.To(remoteCIDR),
				Priority: markPriority,
				Routes: []networkingv1beta1.Route{
					{
						Dst: ptr.To(remoteCIDR),
						Gw:  ptr.To(internalFabric.Spec.Interface.Gateway.IP),
						Dev: ptr.To(internalFabric.Spec.Interface.Node.Name),
					},
				},
			})
		}

		route.Spec = networkingv1beta1.RouteConfigurationSpec{
			Table: networkingv1beta1.Table{
				Name:  route.Name,
				Rules: rules,
			},
		}

		return controllerutil.SetControllerReference(internalFabric, route, r.Scheme)
	})

	if err != nil {
		klog.Errorf("Unable to create or update RouteConfiguration %q: %s", name, err)
		return err
	}

	return nil
}

func (r *InternalFabricReconciler) cleanupPerGatewayReturnRouteConfiguration(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric) error {
	remoteClusterID, ok := internalFabric.Labels[consts.RemoteClusterID]
	if !ok {
		return nil
	}

	gwName := gatewayNameFromInternalFabric(internalFabric)
	name := replicaRouteConfigurationName(remoteClusterID, gwName)

	route := &networkingv1beta1.RouteConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      name,
			Namespace: internalFabric.Namespace,
		},
	}
	err := r.Delete(ctx, route)
	if client.IgnoreNotFound(err) != nil {
		return fmt.Errorf("deleting per-gateway return path RouteConfiguration %q: %w", name, err)
	}
	return nil
}

// replicaRouteConfigurationName returns the name of the per-gateway RouteConfiguration.
// The name is unique per gateway within a remote cluster, not per owner fabric, so that all sibling fabrics
// reconcile the same replica table for a given gateway.
func replicaRouteConfigurationName(remoteClusterID, gatewayName string) string {
	return fmt.Sprintf("%s-node-gw-replica-%s", remoteClusterID, gatewayName)
}
