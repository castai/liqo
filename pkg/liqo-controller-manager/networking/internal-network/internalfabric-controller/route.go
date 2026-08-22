// Copyright 2019-2026 The Liqo Authors
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//      http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS, BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package internalfabriccontroller

import (
	"context"
	"fmt"
	"sort"
	"strings"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/fabric"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

func (r *InternalFabricReconciler) ensureRouteConfiguration(ctx context.Context, internalFabric *networkingv1beta1.InternalFabric) error {
	connectedReplicas, deprecated, err := r.internalFabricReplicas(ctx, internalFabric)
	if err != nil {
		return err
	}

	route := &networkingv1beta1.RouteConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GenerateRouteConfigurationName(internalFabric),
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

		var priority *int
		if r.RouteConfigurationRulePriority > 0 {
			priority = ptr.To(r.RouteConfigurationRulePriority)
		}

		// Add a link-scope route for each connected replica's gateway inner IP.
		for i := range connectedReplicas {
			replica := &connectedReplicas[i]
			rules = append(rules, networkingv1beta1.Rule{
				Dst:      ptr.To(networkingv1beta1.CIDR(fmt.Sprintf("%s/32", replica.Interface.Gateway.IP))),
				Priority: priority,
				Routes: []networkingv1beta1.Route{
					{
						Dst:   ptr.To(networkingv1beta1.CIDR(fmt.Sprintf("%s/32", replica.Interface.Gateway.IP))),
						Dev:   ptr.To(replica.Interface.Node.Name),
						Scope: ptr.To(networkingv1beta1.LinkScope),
					},
				},
			})
		}

		remoteCIDRs := internalFabric.Spec.RemoteCIDRs
		// sort slice to prevent useless updates if CIDRs are in different order
		sort.Slice(remoteCIDRs, func(i, j int) bool {
			return remoteCIDRs[i] < remoteCIDRs[j]
		})

		// Add one rule per remote CIDR.
		// Use a classic single gateway when there is only one connected replica, otherwise use ECMP next-hops.
		for _, remoteCIDR := range remoteCIDRs {
			var route networkingv1beta1.Route
			route.Dst = ptr.To(remoteCIDR)

			switch len(connectedReplicas) {
			case 0:
				// No connected replicas: no routes to remote CIDRs.
				continue
			case 1:
				route.Gw = ptr.To(connectedReplicas[0].Interface.Gateway.IP)
			default:
				nextHops := make([]networkingv1beta1.NextHop, 0, len(connectedReplicas))
				for i := range connectedReplicas {
					replica := &connectedReplicas[i]
					nextHops = append(nextHops, networkingv1beta1.NextHop{
						Gw:  replica.Interface.Gateway.IP,
						Dev: replica.Interface.Node.Name,
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

		return controllerutil.SetControllerReference(internalFabric, route, r.Scheme)
	})
	if err != nil {
		klog.Errorf("Unable to create or update RouteConfiguration %q: %s", route.Name, err)
		return err
	}

	// Ensure per-replica policy-routing tables exist only when ECMP is actually used.
	if deprecated || len(connectedReplicas) < 2 {
		return r.cleanupReplicaRouteConfigurations(ctx, internalFabric)
	}
	return r.ensureReplicaRouteConfigurations(ctx, internalFabric, connectedReplicas)
}

func (r *InternalFabricReconciler) ensureReplicaRouteConfigurations(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric, replicas []networkingv1beta1.InternalFabricReplica) error {
	remoteCIDRs := internalFabric.Spec.RemoteCIDRs
	sort.Slice(remoteCIDRs, func(i, j int) bool {
		return remoteCIDRs[i] < remoteCIDRs[j]
	})

	var markPriority *int
	if r.RouteConfigurationRulePriority > 0 {
		markPriority = ptr.To(r.RouteConfigurationRulePriority)
	}

	desiredNames := make(map[string]struct{}, len(replicas))
	for i := range replicas {
		replica := &replicas[i]
		name := replicaRouteConfigurationName(internalFabric, replica.GatewayName)
		desiredNames[name] = struct{}{}

		route := &networkingv1beta1.RouteConfiguration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      name,
				Namespace: internalFabric.Namespace,
			},
		}
		if _, err := resource.CreateOrUpdate(ctx, r.Client, route, func() error {
			if route.Labels == nil {
				route.Labels = make(labels.Set)
			}
			route.SetLabels(labels.Merge(route.Labels, fabric.ForgeRouteTargetLabels()))
			route.Labels[consts.InternalFabricName] = internalFabric.Name

			var rules []networkingv1beta1.Rule
			for _, remoteCIDR := range remoteCIDRs {
				rules = append(rules, networkingv1beta1.Rule{
					FwMark:   ptr.To(replicaMark(i)),
					Dst:      ptr.To(remoteCIDR),
					Priority: markPriority,
					Routes: []networkingv1beta1.Route{
						{
							Dst: ptr.To(remoteCIDR),
							Gw:  ptr.To(replica.Interface.Gateway.IP),
							Dev: ptr.To(replica.Interface.Node.Name),
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
		}); err != nil {
			klog.Errorf("Unable to create or update RouteConfiguration %q: %s", name, err)
			return err
		}
	}

	// Delete stale per-replica route configurations owned by this InternalFabric.
	var routeList networkingv1beta1.RouteConfigurationList
	if err := r.List(ctx, &routeList, client.InNamespace(internalFabric.Namespace)); err != nil {
		return fmt.Errorf("listing RouteConfigurations: %w", err)
	}
	for i := range routeList.Items {
		rc := &routeList.Items[i]
		if !isReplicaRouteConfiguration(internalFabric, rc.Name) {
			continue
		}
		if _, ok := desiredNames[rc.Name]; ok {
			continue
		}
		if !metav1.IsControlledBy(rc, internalFabric) {
			continue
		}
		if err := r.Delete(ctx, rc); err != nil {
			klog.Errorf("Unable to delete stale RouteConfiguration %q: %s", rc.Name, err)
			return err
		}
	}

	return nil
}

func (r *InternalFabricReconciler) cleanupReplicaRouteConfigurations(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric) error {
	var routeList networkingv1beta1.RouteConfigurationList
	if err := r.List(ctx, &routeList, client.InNamespace(internalFabric.Namespace)); err != nil {
		return fmt.Errorf("listing RouteConfigurations: %w", err)
	}
	for i := range routeList.Items {
		rc := &routeList.Items[i]
		if !isReplicaRouteConfiguration(internalFabric, rc.Name) {
			continue
		}
		if !metav1.IsControlledBy(rc, internalFabric) {
			continue
		}
		if err := r.Delete(ctx, rc); err != nil {
			klog.Errorf("Unable to delete stale RouteConfiguration %q: %s", rc.Name, err)
			return err
		}
	}
	return nil
}

func (r *InternalFabricReconciler) internalFabricReplicas(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric) (
	[]networkingv1beta1.InternalFabricReplica, bool, error) {
	replicas := internalFabric.Spec.Replicas
	deprecated := false
	if len(replicas) == 0 {
		if internalFabric.Spec.Interface == nil {
			return nil, false, fmt.Errorf("internal fabric %q has no replicas and no deprecated interface",
				client.ObjectKeyFromObject(internalFabric))
		}
		replicas = []networkingv1beta1.InternalFabricReplica{
			{Interface: *internalFabric.Spec.Interface, GatewayIP: internalFabric.Spec.GatewayIP},
		}
		deprecated = true
	}

	for i := range replicas {
		if replicas[i].Interface.Node.Name == "" {
			return nil, false, fmt.Errorf("internal fabric %q has node interface name empty for replica %q",
				client.ObjectKeyFromObject(internalFabric), replicas[i].GatewayName)
		}
	}

	// Sort replicas by gateway name to produce a stable RouteConfiguration and stable mark allocation.
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].GatewayName < replicas[j].GatewayName
	})

	// Filter out replicas whose Connection is not in Connected state.
	// For the deprecated single-interface path we cannot map to a Connection,
	// so we keep the existing behavior.
	if deprecated {
		return replicas, true, nil
	}

	connected, err := r.connectedReplicas(ctx, internalFabric, replicas)
	if err != nil {
		return nil, false, fmt.Errorf("filtering connected replicas for internal fabric %q: %w",
			client.ObjectKeyFromObject(internalFabric), err)
	}
	return connected, false, nil
}

// connectedReplicas returns the subset of replicas whose related Connection resource is in Connected state.
func (r *InternalFabricReconciler) connectedReplicas(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric, replicas []networkingv1beta1.InternalFabricReplica) (
	[]networkingv1beta1.InternalFabricReplica, error) {
	if _, ok := gatewayNameFromOwnerRef(internalFabric); !ok {
		klog.V(4).Infof("InternalFabric %q has no gateway owner reference, cannot filter by Connection state",
			client.ObjectKeyFromObject(internalFabric))
		return replicas, nil
	}

	connected := make([]networkingv1beta1.InternalFabricReplica, 0, len(replicas))
	for i := range replicas {
		replica := &replicas[i]
		connectionName := forge.GatewayResourceName(replica.GatewayName)
		connection := &networkingv1beta1.Connection{}
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: internalFabric.Namespace,
			Name:      connectionName,
		}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("Connection %q/%q not found, skipping replica %q",
					internalFabric.Namespace, connectionName, replica.GatewayName)
				continue
			}
			return nil, fmt.Errorf("getting Connection %q/%q: %w",
				internalFabric.Namespace, connectionName, err)
		}

		if connection.Status.Value == networkingv1beta1.Connected {
			connected = append(connected, *replica)
		} else {
			klog.V(4).Infof("Connection %q/%q status is %q, skipping replica %q",
				internalFabric.Namespace, connectionName, connection.Status.Value, replica.GatewayName)
		}
	}

	return connected, nil
}

// gatewayNameFromOwnerRef returns the name of the gateway owner reference of the InternalFabric.
func gatewayNameFromOwnerRef(internalFabric *networkingv1beta1.InternalFabric) (string, bool) {
	owner := metav1.GetControllerOf(internalFabric)
	if owner == nil {
		return "", false
	}
	return owner.Name, true
}

// replicaMark returns the policy-routing mark associated to a gateway replica at the given index.
func replicaMark(index int) int {
	return consts.ECMPReplicaMarkBase + index
}

// isReplicaRouteConfiguration reports whether the given RouteConfiguration name belongs to a per-replica table.
func isReplicaRouteConfiguration(internalFabric *networkingv1beta1.InternalFabric, name string) bool {
	return strings.HasPrefix(name, fmt.Sprintf("%s-node-gw-replica-", internalFabric.Name))
}

// replicaRouteConfigurationName returns the name of the per-replica RouteConfiguration.
func replicaRouteConfigurationName(internalFabric *networkingv1beta1.InternalFabric, gatewayName string) string {
	return fmt.Sprintf("%s-node-gw-replica-%s", internalFabric.Name, gatewayName)
}

// GenerateRouteConfigurationName returns the name of the RouteConfiguration associated to the InternalFabric.
func GenerateRouteConfigurationName(internalFabric *networkingv1beta1.InternalFabric) string {
	return fmt.Sprintf("%s-node-gw", internalFabric.Name)
}
