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

// FindFreeReplicaInterfaceName returns a free interface name for a gateway replica.
// It avoids collisions with interface names already used by other InternalFabric replicas.
func FindFreeReplicaInterfaceName(ctx context.Context, cl client.Client,
	internalFabric *networkingv1beta1.InternalFabric, gatewayName string) (string, error) {
	// Collect all interface names already in use across all InternalFabrics.
	var fabricList networkingv1beta1.InternalFabricList
	if err := cl.List(ctx, &fabricList); err != nil {
		return "", fmt.Errorf("listing InternalFabrics: %w", err)
	}

	used := make(map[string]struct{})
	for i := range fabricList.Items {
		f := &fabricList.Items[i]
		if f.Spec.Interface != nil {
			used[f.Spec.Interface.Node.Name] = struct{}{}
		}
		for j := range f.Spec.Replicas {
			used[f.Spec.Replicas[j].Interface.Node.Name] = struct{}{}
		}
	}

	for retry := 0; retry < maxretries; retry++ {
		name := forgeInterfaceName()
		if _, ok := used[name]; !ok {
			return name, nil
		}
	}
	return "", fmt.Errorf("cannot find a free interface name for gateway %q", gatewayName)
}

// BuildInternalFabricReplicas builds the per-gateway replica list for an InternalFabric
// from the given gateway internal endpoints. Each gateway contributes one replica entry.
// It preserves existing interface names and gateway IPs for stable gateway names and
// allocates new ones for new gateways.
func BuildInternalFabricReplicas(ctx context.Context, cl client.Client,
	internalFabric *networkingv1beta1.InternalFabric,
	gateways []GatewayEndpoint, ipam *fabricipam.IPAM) ([]networkingv1beta1.InternalFabricReplica, error) {
	existing := make(map[string]*networkingv1beta1.InternalFabricReplica)
	for i := range internalFabric.Spec.Replicas {
		replica := &internalFabric.Spec.Replicas[i]
		existing[replica.GatewayName] = replica
	}

	// Sort gateways by name for stable ordering.
	sort.Slice(gateways, func(i, j int) bool {
		return gateways[i].Name < gateways[j].Name
	})

	replicas := make([]networkingv1beta1.InternalFabricReplica, 0, len(gateways))
	for i := range gateways {
		gw := &gateways[i]
		if gw.Endpoint.IP == nil {
			continue
		}

		var replica *networkingv1beta1.InternalFabricReplica
		if existing[gw.Name] != nil {
			replica = existing[gw.Name]
		} else {
			replica = &networkingv1beta1.InternalFabricReplica{GatewayName: gw.Name}
		}

		replica.GatewayIP = *gw.Endpoint.IP

		if replica.Interface.Node.Name == "" {
			name, err := FindFreeReplicaInterfaceName(ctx, cl, internalFabric, gw.Name)
			if err != nil {
				return nil, fmt.Errorf("cannot find free interface name for gateway %q: %w", gw.Name, err)
			}
			replica.Interface.Node.Name = name
		}

		if replica.Interface.Gateway.IP == "" {
			ip, err := ipam.Allocate(fmt.Sprintf("%s-%s", internalFabric.GetName(), gw.Name))
			if err != nil {
				return nil, fmt.Errorf("cannot allocate IP for gateway %q: %w", gw.Name, err)
			}
			replica.Interface.Gateway.IP = networkingv1beta1.IP(ip.String())
		}

		replicas = append(replicas, *replica)
	}

	return replicas, nil
}

// GatewayEndpoint pairs a gateway CR name with its internal endpoint.
type GatewayEndpoint struct {
	Name     string
	Endpoint networkingv1beta1.InternalGatewayEndpoint
}
