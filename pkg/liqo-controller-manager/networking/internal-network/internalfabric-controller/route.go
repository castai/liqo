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
	"sort"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/fabric"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

func (r *InternalFabricReconciler) ensureRouteConfiguration(ctx context.Context, internalFabric *networkingv1beta1.InternalFabric) error {
	replicas := internalFabric.Spec.Replicas
	deprecated := false
	if len(replicas) == 0 {
		if internalFabric.Spec.Interface == nil {
			return fmt.Errorf("internal fabric %q has no replicas and no deprecated interface", client.ObjectKeyFromObject(internalFabric))
		}
		replicas = []networkingv1beta1.InternalFabricReplica{
			{Interface: *internalFabric.Spec.Interface},
		}
		deprecated = true
	}

	for i := range replicas {
		if replicas[i].Interface.Node.Name == "" {
			return fmt.Errorf("internal fabric %q has node interface name empty for replica %d",
				client.ObjectKeyFromObject(internalFabric), replicas[i].ReplicaID)
		}
	}

	// Sort replicas by ID to produce a stable RouteConfiguration.
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].ReplicaID < replicas[j].ReplicaID
	})

	// Filter out replicas whose Connection is not in Connected state.
	// For the deprecated single-interface path we cannot map to a Connection,
	// so we keep the existing behavior.
	connectedReplicas := replicas
	if !deprecated {
		filtered, err := r.connectedReplicas(ctx, internalFabric, replicas)
		if err != nil {
			return fmt.Errorf("filtering connected replicas for internal fabric %q: %w",
				client.ObjectKeyFromObject(internalFabric), err)
		}
		connectedReplicas = filtered
	}

	route := &networkingv1beta1.RouteConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GenerateRouteConfigurationName(internalFabric),
			Namespace: internalFabric.Namespace,
		},
	}
	_, err := resource.CreateOrUpdate(ctx, r.Client, route, func() error {
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

			if len(connectedReplicas) == 1 {
				route.Gw = ptr.To(connectedReplicas[0].Interface.Gateway.IP)
			} else {
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

	return nil
}

// connectedReplicas returns the subset of replicas whose related Connection resource is in Connected state.
func (r *InternalFabricReconciler) connectedReplicas(ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric, replicas []networkingv1beta1.InternalFabricReplica) (
	[]networkingv1beta1.InternalFabricReplica, error) {
	gatewayName, ok := gatewayNameFromOwnerRef(internalFabric)
	if !ok {
		klog.V(4).Infof("InternalFabric %q has no gateway owner reference, cannot filter by Connection state",
			client.ObjectKeyFromObject(internalFabric))
		return replicas, nil
	}

	connected := make([]networkingv1beta1.InternalFabricReplica, 0, len(replicas))
	for i := range replicas {
		replica := &replicas[i]
		connectionName := forge.ReplicaResourceName(gatewayName, replica.ReplicaID)
		connection := &networkingv1beta1.Connection{}
		if err := r.Get(ctx, types.NamespacedName{
			Namespace: internalFabric.Namespace,
			Name:      connectionName,
		}, connection); err != nil {
			if apierrors.IsNotFound(err) {
				klog.V(4).Infof("Connection %q/%q not found, skipping replica %d",
					internalFabric.Namespace, connectionName, replica.ReplicaID)
				continue
			}
			return nil, fmt.Errorf("getting Connection %q/%q: %w",
				internalFabric.Namespace, connectionName, err)
		}

		if connection.Status.Value == networkingv1beta1.Connected {
			connected = append(connected, *replica)
		} else {
			klog.V(4).Infof("Connection %q/%q status is %q, skipping replica %d",
				internalFabric.Namespace, connectionName, connection.Status.Value, replica.ReplicaID)
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

// GenerateRouteConfigurationName returns the name of the RouteConfiguration associated to the InternalFabric.
func GenerateRouteConfigurationName(internalFabric *networkingv1beta1.InternalFabric) string {
	return fmt.Sprintf("%s-node-gw", internalFabric.Name)
}
