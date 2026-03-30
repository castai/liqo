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

package fabric

import (
	"context"
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
)

var _ manager.Runnable = &NodeReadyRunnable{}

// NodeReadyRunnable waits until the node where the fabric pod is scheduled becomes Ready.
type NodeReadyRunnable struct {
	Client   client.Client
	NodeName string
}

// NewNodeReadyRunnable creates a new NodeReadyRunnable.
func NewNodeReadyRunnable(cl client.Client, nodeName string) *NodeReadyRunnable {
	return &NodeReadyRunnable{
		Client:   cl,
		NodeName: nodeName,
	}
}

// Start implements manager.Runnable. It blocks until the node is Ready or the context is cancelled.
func (r *NodeReadyRunnable) Start(ctx context.Context) error {
	klog.Infof("Waiting for node %q to become Ready", r.NodeName)

	if err := wait.PollUntilContextCancel(ctx, 5*time.Second, true, func(ctx context.Context) (bool, error) {
		node := &corev1.Node{}
		if err := r.Client.Get(ctx, types.NamespacedName{Name: r.NodeName}, node); err != nil {
			klog.Errorf("Unable to get node %q: %v", r.NodeName, err)
			return false, nil
		}

		for i := range node.Status.Conditions {
			c := &node.Status.Conditions[i]
			if c.Type == corev1.NodeReady && c.Status == corev1.ConditionTrue {
				klog.Infof("Node %q is Ready", r.NodeName)
				return true, nil
			}
		}

		klog.V(4).Infof("Node %q is not Ready yet", r.NodeName)
		return false, nil
	}); err != nil {
		return fmt.Errorf("waiting for node %q to become Ready: %w", r.NodeName, err)
	}

	return nil
}
