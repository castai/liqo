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

package internalnetwork

import (
	"context"
	"fmt"
	"sort"

	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/internal-network/fabricipam"
)

// FirstInternalEndpoint returns the first available internal endpoint.
// It prefers the slice field and falls back to the deprecated single InternalEndpoint.
func FirstInternalEndpoint(endpoints []networkingv1beta1.InternalGatewayEndpoint,
	legacy *networkingv1beta1.InternalGatewayEndpoint) *networkingv1beta1.InternalGatewayEndpoint {
	if len(endpoints) > 0 {
		return &endpoints[0]
	}
	return legacy
}

// GetInternalEndpointByReplicaID returns the internal endpoint matching the given replica ID.
func GetInternalEndpointByReplicaID(endpoints []networkingv1beta1.InternalGatewayEndpoint,
	replicaID int32) *networkingv1beta1.InternalGatewayEndpoint {
	for i := range endpoints {
		if endpoints[i].ReplicaID == replicaID {
			return &endpoints[i]
		}
	}
	return nil
}

// SortInternalEndpointsByReplicaID sorts the internal endpoints by replica ID in place.
func SortInternalEndpointsByReplicaID(endpoints []networkingv1beta1.InternalGatewayEndpoint) {
	sort.Slice(endpoints, func(i, j int) bool {
		return endpoints[i].ReplicaID < endpoints[j].ReplicaID
	})
}

// GetInternalFabricReplica returns the replica entry matching the given replica ID.
func GetInternalFabricReplica(internalFabric *networkingv1beta1.InternalFabric,
	replicaID int32) *networkingv1beta1.InternalFabricReplica {
	for i := range internalFabric.Spec.Replicas {
		if internalFabric.Spec.Replicas[i].ReplicaID == replicaID {
			return &internalFabric.Spec.Replicas[i]
		}
	}
	return nil
}

// FirstInternalFabricReplica returns the first replica of the internal fabric, or nil if none exist.
func FirstInternalFabricReplica(internalFabric *networkingv1beta1.InternalFabric) *networkingv1beta1.InternalFabricReplica {
	if len(internalFabric.Spec.Replicas) == 0 {
		return nil
	}
	return &internalFabric.Spec.Replicas[0]
}

// BuildInternalFabricReplicas builds the per-replica list for an InternalFabric from the gateway internal endpoints.
// It preserves existing interface names and gateway IPs for stable replica IDs and allocates new ones for new replicas.
func BuildInternalFabricReplicas(ctx context.Context, cl client.Client,
	internalFabric *networkingv1beta1.InternalFabric,
	endpoints []networkingv1beta1.InternalGatewayEndpoint,
	ipam *fabricipam.IPAM) ([]networkingv1beta1.InternalFabricReplica, error) {
	existing := make(map[int32]*networkingv1beta1.InternalFabricReplica)
	for i := range internalFabric.Spec.Replicas {
		replica := &internalFabric.Spec.Replicas[i]
		existing[replica.ReplicaID] = replica
	}

	SortInternalEndpointsByReplicaID(endpoints)

	replicas := make([]networkingv1beta1.InternalFabricReplica, 0, len(endpoints))
	for i := range endpoints {
		endpoint := &endpoints[i]
		if endpoint.IP == nil {
			continue
		}

		var replica *networkingv1beta1.InternalFabricReplica
		if existing[endpoint.ReplicaID] != nil {
			replica = existing[endpoint.ReplicaID]
		} else {
			replica = &networkingv1beta1.InternalFabricReplica{ReplicaID: endpoint.ReplicaID}
		}

		replica.GatewayIP = *endpoint.IP

		if replica.Interface.Node.Name == "" {
			name, err := FindFreeReplicaInterfaceName(ctx, cl, internalFabric, endpoint.ReplicaID)
			if err != nil {
				return nil, fmt.Errorf("cannot find free interface name for replica %d: %w", endpoint.ReplicaID, err)
			}
			replica.Interface.Node.Name = name
		}

		if replica.Interface.Gateway.IP == "" {
			ip, err := ipam.Allocate(fmt.Sprintf("%s-%d", internalFabric.GetName(), endpoint.ReplicaID))
			if err != nil {
				return nil, fmt.Errorf("cannot allocate IP for replica %d: %w", endpoint.ReplicaID, err)
			}
			replica.Interface.Gateway.IP = networkingv1beta1.IP(ip.String())
		}

		replicas = append(replicas, *replica)
	}

	return replicas, nil
}
