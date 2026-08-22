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
	"strings"

	k8serrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/apis/networking/v1beta1/firewall"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/fabric"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// GenerateFirewallConfigurationName returns the name of the FirewallConfiguration associated to the InternalFabric.
// It uses the remote-cluster-id from the InternalFabric labels so all siblings share the same name.
func generateFirewallConfigurationName(remoteClusterID string) string {
	return fmt.Sprintf("%s-node-gw", remoteClusterID)
}

func (r *InternalFabricReconciler) ensureFirewallConfiguration(
	ctx context.Context,
	internalFabric *networkingv1beta1.InternalFabric,
	siblings []networkingv1beta1.InternalFabric,
) error {
	remoteClusterID, ok := internalFabric.Labels[consts.RemoteClusterID]
	if !ok || remoteClusterID == "" {
		return fmt.Errorf("InternalFabric %q does not have a remote-cluster-id label", internalFabric.Name)
	}

	fw := &networkingv1beta1.FirewallConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateFirewallConfigurationName(remoteClusterID),
			Namespace: internalFabric.Namespace,
		},
	}

	// Apply the connection-tracking mark firewall only when multiple gateways are configured.
	if len(siblings) < 2 {
		if err := r.Delete(ctx, fw); err != nil && !k8serrors.IsNotFound(err) {
			return fmt.Errorf("deleting FirewallConfiguration %q: %w", fw.Name, err)
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
		chains, err := forgeFirewallChains(internalFabric, siblings)
		if err != nil {
			return err
		}
		fw.Spec.Table.Chains = chains

		return controllerutil.SetOwnerReference(internalFabric, fw, r.Scheme)
	})
	if err != nil {
		return fmt.Errorf("creating or updating FirewallConfiguration %q: %w", fw.Name, err)
	}

	return nil
}

func forgeFirewallChains(internalFabric *networkingv1beta1.InternalFabric, fabrics []networkingv1beta1.InternalFabric) ([]firewall.Chain, error) {
	if len(fabrics) <= 1 {
		return nil, nil
	}

	remoteCIDRs := slices.Clone(internalFabric.Spec.RemoteCIDRs)
	slices.Sort(remoteCIDRs)

	if len(remoteCIDRs) == 0 {
		return nil, nil
	}

	forwardRules := make([]firewall.FilterRule, 0, len(fabrics)*len(remoteCIDRs))
	preroutingRules := make([]firewall.FilterRule, 0, len(remoteCIDRs))

	for _, remoteCIDR := range remoteCIDRs {
		for i := range fabrics {
			fab := &fabrics[i]
			mark, err := fabricECMPMark(fab)
			if err != nil {
				return nil, err
			}
			forwardRules = append(forwardRules, firewall.FilterRule{
				Name:    ptr.To(fmt.Sprintf("mark-in-gw-%d-%s", i, sanitizeCIDR(remoteCIDR))),
				Action:  firewall.ActionCtMark,
				Value:   ptr.To(fmt.Sprintf("%d", mark)),
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
							Value:    fab.Spec.Interface.Node.Name,
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
	}, nil
}

// sanitizeCIDR returns a string that can be used inside a Kubernetes resource name.
func sanitizeCIDR(cidr networkingv1beta1.CIDR) string {
	return strings.NewReplacer(".", "-", "/", "-").Replace(string(cidr))
}
