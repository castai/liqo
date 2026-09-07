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

package wireguard

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"

	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway"
)

const (
	// DefaultRolloutRequeueInterval is the delay before a controller retries when
	// its rollout is gated by a peer.
	DefaultRolloutRequeueInterval = 5 * time.Second
)

// IsDeploymentRollingOut reports whether the Deployment is currently rolling out.
// A Deployment is considered rolling out if the controller has not yet observed the
// latest generation, has not yet updated all desired replicas, or has any unavailable
// replicas.
func IsDeploymentRollingOut(dep *appsv1.Deployment) bool {
	if dep == nil {
		return false
	}
	desired := dep.Status.Replicas
	if dep.Spec.Replicas != nil {
		desired = *dep.Spec.Replicas
	}
	if dep.Generation > dep.Status.ObservedGeneration {
		return true
	}
	return dep.Status.UpdatedReplicas < desired ||
		dep.Status.AvailableReplicas < desired ||
		dep.Status.UnavailableReplicas > 0
}

// ShouldDelayRollout checks whether the requested rollout of the gateway Deployment
// identified by self should be delayed because a peer Deployment for the same remote
// cluster is still stale or rolling out.
func ShouldDelayRollout(ctx context.Context, cl client.Client, remoteClusterID string,
	self types.NamespacedName, selfTemplateName, selfTemplateNamespace, desiredGeneration string) (bool, string, error) {
	// If the source template is not known (e.g., manually created resources), we
	// cannot compare peers, so we proceed without gating.
	if selfTemplateName == "" || selfTemplateNamespace == "" || desiredGeneration == "" {
		return false, "", nil
	}

	// The list is already filtered by template identity, so every returned peer is
	// a candidate for serialization.
	selector := labels.SelectorFromSet(labels.Set{
		consts.RemoteClusterID:           remoteClusterID,
		consts.NetworkingComponentKey:    gateway.GatewayComponentGateway,
		consts.TemplateNameLabelKey:      selfTemplateName,
		consts.TemplateNamespaceLabelKey: selfTemplateNamespace,
	})

	var deps appsv1.DeploymentList
	if err := cl.List(ctx, &deps, &client.ListOptions{LabelSelector: selector}); err != nil {
		return false, "", fmt.Errorf("listing peer gateway deployments: %w", err)
	}

	selfKey := self.String()
	anyRollingOut := false
	for i := range deps.Items {
		dep := &deps.Items[i]
		if dep.Namespace == self.Namespace && dep.Name == self.Name {
			continue
		}

		peerKey := client.ObjectKeyFromObject(dep).String()
		peerGeneration := dep.GetAnnotations()[consts.TemplateGenerationAnnotationKey]

		// If a lower-named peer with a different generation is still waiting to roll. We must wait for it before we can proceed.
		if peerKey < selfKey && peerGeneration != desiredGeneration {
			return true, fmt.Sprintf("lower-named peer %s has stale generation %q (want %q)", peerKey, peerGeneration, desiredGeneration), nil
		}

		// Wait while the peer that comes before us is still rolling out.
		if IsDeploymentRollingOut(dep) {
			anyRollingOut = true
		}
	}

	if anyRollingOut {
		return true, "a peer gateway deployment is still rolling out", nil
	}

	return false, "", nil
}

// isDeploymentUpdateNeeded reports whether the desired Deployment differs from
// the existing one in spec, labels, or annotations.
func isDeploymentUpdateNeeded(existing, desired *appsv1.Deployment) bool {
	if existing == nil || desired == nil {
		return existing != desired
	}
	return !equality.Semantic.DeepEqual(existing.Spec, desired.Spec) ||
		!equality.Semantic.DeepEqual(existing.Labels, desired.Labels) ||
		!equality.Semantic.DeepEqual(existing.Annotations, desired.Annotations)
}

func getRemoteClusterID(obj client.Object) (string, bool) {
	if obj == nil || obj.GetLabels() == nil {
		return "", false
	}
	val, ok := obj.GetLabels()[consts.RemoteClusterID]
	return val, ok && val != ""
}
