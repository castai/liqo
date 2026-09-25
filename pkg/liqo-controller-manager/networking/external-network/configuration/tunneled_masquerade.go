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

package configurationcontroller

import (
	"context"
	"fmt"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/apis/networking/v1beta1/firewall"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway/tunnel"
	"github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/external-network/remapping"
	cidrutils "github.com/liqotech/liqo/pkg/utils/cidr"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// tunneledMasqueradeChainName is the name of the postrouting NAT chain that masquerades
// traffic directed to the tunneled CIDRs when it egresses the Wireguard tunnel.
const tunneledMasqueradeChainName = "postrouting-tunneled-snat"

// generateTunneledMasqueradeFirewallConfigurationName returns the name of the FirewallConfiguration
// that masquerades traffic directed to the tunneled CIDRs through the gateway tunnel.
func generateTunneledMasqueradeFirewallConfigurationName(cfg *networkingv1beta1.Configuration) string {
	return fmt.Sprintf("%s-tunneled-masquerade", cfg.Name)
}

// ensureTunneledMasquerade ensures the FirewallConfiguration that masquerades traffic directed to the
// tunneled CIDRs on the gateway's Wireguard interface egress is correctly configured.
//
// The MASQUERADE rule lets the kernel pick the egress interface address (i.e. the tunnel interface IP of
// the specific gateway replica), so the rule is correct with multiple gateway replicas and without the
// controller having to know their tunnel IPs.
func (r *ConfigurationReconciler) ensureTunneledMasquerade(ctx context.Context, cfg *networkingv1beta1.Configuration) error {
	fwcfg := &networkingv1beta1.FirewallConfiguration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      generateTunneledMasqueradeFirewallConfigurationName(cfg),
			Namespace: cfg.Namespace,
		},
	}

	// If there are no reserved tunneled CIDRs, there is nothing to masquerade: clean up any leftover.
	if len(cfg.Status.TunneledCIDRs) == 0 {
		if err := r.Delete(ctx, fwcfg); err != nil && !apierrors.IsNotFound(err) {
			return fmt.Errorf("deleting firewall configuration %q: %w", fwcfg.Name, err)
		}
		return nil
	}

	remoteClusterID, ok := cfg.Labels[consts.RemoteClusterID]
	if !ok {
		return fmt.Errorf("configuration %q has no %q label", client.ObjectKeyFromObject(cfg).String(), consts.RemoteClusterID)
	}

	if _, err := resource.CreateOrUpdate(ctx, r.Client, fwcfg, func() error {
		if fwcfg.Labels == nil {
			fwcfg.Labels = make(map[string]string)
		}
		fwcfg.SetLabels(labels.Merge(fwcfg.Labels, remapping.ForgeFirewallTargetLabels(remoteClusterID)))

		fwcfg.Spec.Table.Name = ptr.To(fwcfg.Name)
		fwcfg.Spec.Table.Family = ptr.To(firewall.TableFamilyIPv4)
		fwcfg.Spec.Table.Chains = []firewall.Chain{forgeTunneledMasqueradeChain(cfg)}

		return controllerutil.SetControllerReference(cfg, fwcfg, r.Scheme)
	}); err != nil {
		return fmt.Errorf("creating or updating firewall configuration %q: %w", fwcfg.Name, err)
	}

	return nil
}

// forgeTunneledMasqueradeChain forges the postrouting NAT chain that masquerades traffic directed to the
// tunneled CIDRs when it egresses the Wireguard tunnel.
func forgeTunneledMasqueradeChain(cfg *networkingv1beta1.Configuration) firewall.Chain {
	tunneled := cfg.Status.TunneledCIDRs
	rules := make([]firewall.NatRule, 0, len(tunneled))
	for i := range tunneled {
		cidr := tunneled[i]
		rules = append(rules, firewall.NatRule{
			Name:    ptr.To(fmt.Sprintf("tunneled-snat-%s", cidrutils.EscapeForName(cidr))),
			NatType: firewall.NatTypeMasquerade,
			Match: []firewall.Match{
				{
					Op: firewall.MatchOperationEq,
					IP: &firewall.MatchIP{
						Position: firewall.MatchPositionDst,
						Value:    cidr.String(),
					},
				},
				{
					Op: firewall.MatchOperationEq,
					Dev: &firewall.MatchDev{
						Position: firewall.MatchDevPositionOut,
						Value:    tunnel.TunnelInterfaceName,
					},
				},
			},
		})
	}

	return firewall.Chain{
		Name:     ptr.To(tunneledMasqueradeChainName),
		Type:     firewall.ChainTypeNAT,
		Policy:   ptr.To(firewall.ChainPolicyAccept),
		Hook:     ptr.To(firewall.ChainHookPostrouting),
		Priority: ptr.To(firewall.ChainPriorityNATSource),
		Rules: firewall.RulesSet{
			NatRules: rules,
		},
	}
}
