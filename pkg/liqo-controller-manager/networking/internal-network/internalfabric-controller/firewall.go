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
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/apis/networking/v1beta1/firewall"
	"github.com/liqotech/liqo/pkg/fabric"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

func (r *InternalFabricReconciler) ensureFirewallConfiguration(ctx context.Context, internalFabric *networkingv1beta1.InternalFabric) error {
	replicas := internalFabric.Spec.Replicas
	deprecated := false
	if len(replicas) == 0 {
		if internalFabric.Spec.Interface == nil {
			// No replicas and no deprecated interface: nothing to mark.
			return nil
		}
		replicas = []networkingv1beta1.InternalFabricReplica{
			{Interface: *internalFabric.Spec.Interface},
		}
		deprecated = true
	}

	// Sort replicas by ID to produce a stable FirewallConfiguration.
	sort.Slice(replicas, func(i, j int) bool {
		return replicas[i].ReplicaID < replicas[j].ReplicaID
	})

	connectedReplicas := replicas
	if !deprecated {
		filtered, err := r.connectedReplicas(ctx, internalFabric, replicas)
		if err != nil {
			return fmt.Errorf("filtering connected replicas for internal fabric %q: %w",
				client.ObjectKeyFromObject(internalFabric), err)
		}
		connectedReplicas = filtered
	}

	fw := &networkingv1beta1.FirewallConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      GenerateFirewallConfigurationName(internalFabric),
			Namespace: internalFabric.Namespace,
		},
	}

	// Apply the connection-tracking mark firewall only when ECMP is actually used.
	if len(connectedReplicas) < 2 {
		if err := r.Delete(ctx, fw); err != nil && !k8serrors.IsNotFound(err) {
			klog.Errorf("Unable to delete FirewallConfiguration %q: %s", fw.Name, err)
			return err
		}
		return nil
	}

	_, err := resource.CreateOrUpdate(ctx, r.Client, fw, func() error {
		if fw.Labels == nil {
			fw.Labels = make(labels.Set)
		}
		fw.SetLabels(labels.Merge(fw.Labels, fabric.ForgeFirewallTargetLabels()))

		fw.Spec.Table.Name = ptr.To(fw.Name)
		fw.Spec.Table.Family = ptr.To(firewall.TableFamilyIPv4)
		fw.Spec.Table.Chains = forgeFirewallChains(internalFabric, connectedReplicas)

		return controllerutil.SetControllerReference(internalFabric, fw, r.Scheme)
	})
	if err != nil {
		klog.Errorf("Unable to create or update FirewallConfiguration %q: %s", fw.Name, err)
		return err
	}

	return nil
}

func forgeFirewallChains(internalFabric *networkingv1beta1.InternalFabric,
	replicas []networkingv1beta1.InternalFabricReplica) []firewall.Chain {
	if len(replicas) <= 1 {
		// No ECMP, no need for connection-tracking marks.
		return nil
	}

	remoteCIDRs := internalFabric.Spec.RemoteCIDRs
	// sort slice to prevent useless updates if CIDRs are in different order
	sort.Slice(remoteCIDRs, func(i, j int) bool {
		return remoteCIDRs[i] < remoteCIDRs[j]
	})

	if len(remoteCIDRs) == 0 {
		return nil
	}

	forwardRules := make([]firewall.FilterRule, 0, len(replicas)*len(remoteCIDRs))
	preroutingRules := make([]firewall.FilterRule, 0, len(remoteCIDRs))

	for _, remoteCIDR := range remoteCIDRs {
		for i := range replicas {
			replica := &replicas[i]
			// Mark traffic coming FROM the remote CIDR IN through this replica's node interface,
			// so the response traffic (same conntrack entry) is routed back through the same replica.
			forwardRules = append(forwardRules, firewall.FilterRule{
				Name:    ptr.To(fmt.Sprintf("mark-in-replica-%d-%s", replica.ReplicaID, sanitizeCIDR(remoteCIDR))),
				Action:  firewall.ActionCtMark,
				Value:   ptr.To(fmt.Sprintf("%d", replicaMark(replica.ReplicaID))),
				Counter: true,
				Match: []firewall.Match{
					{
						Op: firewall.MatchOperationEq,
						IP: &firewall.MatchIP{
							Position: firewall.MatchPositionSrc,
							Value:    remoteCIDR.String(),
						},
					},
					{
						Op: firewall.MatchOperationEq,
						Dev: &firewall.MatchDev{
							Position: firewall.MatchDevPositionIn,
							Value:    replica.Interface.Node.Name,
						},
					},
				},
			})
		}

		preroutingRules = append(preroutingRules, firewall.FilterRule{
			Name:    ptr.To(fmt.Sprintf("restore-mark-%s", sanitizeCIDR(remoteCIDR))),
			Action:  firewall.ActionSetMetaMarkFromCtMark,
			Counter: true,
			Match: []firewall.Match{
				{
					Op: firewall.MatchOperationEq,
					IP: &firewall.MatchIP{
						Position: firewall.MatchPositionDst,
						Value:    remoteCIDR.String(),
					},
				},
			},
		})
	}

	return []firewall.Chain{
		{
			Name:     ptr.To("ecmp-mark"),
			Type:     firewall.ChainTypeFilter,
			Policy:   ptr.To(firewall.ChainPolicyAccept),
			Hook:     ptr.To(firewall.ChainHookForward),
			Priority: ptr.To(firewall.ChainPriorityFilter),
			Rules: firewall.RulesSet{
				FilterRules: forwardRules,
			},
		},
		{
			Name:     ptr.To("ecmp-restore-mark"),
			Type:     firewall.ChainTypeFilter,
			Policy:   ptr.To(firewall.ChainPolicyAccept),
			Hook:     ptr.To(firewall.ChainHookPrerouting),
			Priority: ptr.To(firewall.ChainPriorityFilter),
			Rules: firewall.RulesSet{
				FilterRules: preroutingRules,
			},
		},
	}
}

// sanitizeCIDR returns a string that can be used inside a Kubernetes resource name.
func sanitizeCIDR(cidr networkingv1beta1.CIDR) string {
	return strings.NewReplacer(".", "-", "/", "-").Replace(string(cidr))
}

// GenerateFirewallConfigurationName returns the name of the FirewallConfiguration associated to the InternalFabric.
func GenerateFirewallConfigurationName(internalFabric *networkingv1beta1.InternalFabric) string {
	return fmt.Sprintf("%s-node-gw", internalFabric.Name)
}
