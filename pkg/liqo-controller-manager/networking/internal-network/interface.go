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

	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/rand"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/utils/getters"
)

const (
	maxretries          = 20
	interfaceNameLength = 10
	// InterfaceNamePrefix is the prefix used for Liqo geneve interface names.
	InterfaceNamePrefix = "liqo."
)

func forgeInterfaceName() string {
	return InterfaceNamePrefix + rand.String(interfaceNameLength)
}

// FindFreeInterfaceName returns a free  interface name.
// If it cannot find a free name, it returns an error.
func FindFreeInterfaceName(ctx context.Context, cl client.Client, i interface{}) (string, error) {
	switch obj := i.(type) {
	case *networkingv1beta1.InternalNode:
		if obj.Spec.Interface.Gateway.Name != "" {
			return obj.Spec.Interface.Gateway.Name, nil
		}
		return findFreeInterfaceNameForInternalNode(ctx, cl)
	case *networkingv1beta1.InternalFabric:
		if obj.Spec.Interface != nil && obj.Spec.Interface.Node.Name != "" {
			return obj.Spec.Interface.Node.Name, nil
		}
		return findFreeInterfaceNameForInternalFabric(ctx, cl)
	default:
		return "", fmt.Errorf("type %T not supported", obj)
	}
}

func findFreeInterfaceNameForInternalFabric(ctx context.Context, cl client.Client) (string, error) {
	list, err := getters.ListInternalFabricsByLabels(ctx, cl, labels.Everything())
	if err != nil {
		return "", fmt.Errorf("cannot list internal nodes: %w", err)
	}

	ok := false
	retry := 0
	var name string
	for !ok && retry < maxretries {
		name = forgeInterfaceName()
		ok = true
		for i := range list.Items {
			if isNodeInterfaceNameUsed(&list.Items[i], name) {
				ok = false
				break
			}
		}
		retry++
	}
	if !ok {
		return "", fmt.Errorf("cannot find a free interface name")
	}
	return name, nil
}

// FindFreeReplicaInterfaceName returns a free interface name for the given internal fabric replica.
// If the replica already has an interface name, it returns it. Otherwise, it generates a new one
// that does not collide with any existing node interface name.
func FindFreeReplicaInterfaceName(ctx context.Context, cl client.Client,
	internalFabric *networkingv1beta1.InternalFabric, replicaID int32) (string, error) {
	if replica := GetInternalFabricReplica(internalFabric, replicaID); replica != nil && replica.Interface.Node.Name != "" {
		return replica.Interface.Node.Name, nil
	}

	list, err := getters.ListInternalFabricsByLabels(ctx, cl, labels.Everything())
	if err != nil {
		return "", fmt.Errorf("cannot list internal fabrics: %w", err)
	}

	ok := false
	retry := 0
	var name string
	for !ok && retry < maxretries {
		name = forgeInterfaceName()
		ok = true
		for i := range list.Items {
			if isNodeInterfaceNameUsed(&list.Items[i], name) {
				ok = false
				break
			}
		}
		retry++
	}
	if !ok {
		return "", fmt.Errorf("cannot find a free interface name")
	}
	return name, nil
}

func isNodeInterfaceNameUsed(internalFabric *networkingv1beta1.InternalFabric, name string) bool {
	if internalFabric.Spec.Interface != nil && internalFabric.Spec.Interface.Node.Name == name {
		return true
	}
	for i := range internalFabric.Spec.Replicas {
		if internalFabric.Spec.Replicas[i].Interface.Node.Name == name {
			return true
		}
	}
	return false
}

func findFreeInterfaceNameForInternalNode(ctx context.Context, cl client.Client) (string, error) {
	list, err := getters.ListInternalNodesByLabels(ctx, cl, labels.Everything())
	if err != nil {
		return "", fmt.Errorf("cannot list internal nodes: %w", err)
	}

	ok := false
	retry := 0
	var name string
	for !ok && retry < maxretries {
		name = forgeInterfaceName()
		ok = true
		for i := range list.Items {
			if list.Items[i].Spec.Interface.Gateway.Name == name {
				ok = false
				break
			}
		}
		retry++
	}
	if !ok {
		return "", fmt.Errorf("cannot find a free interface name")
	}
	return name, nil
}
