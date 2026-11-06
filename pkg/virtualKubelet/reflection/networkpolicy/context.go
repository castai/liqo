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

package networkpolicy

import (
	"context"
	"fmt"
	"net/netip"
	"slices"
	"strings"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	netapi "github.com/liqotech/liqo/apis/networking/v1beta1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/utils/ipam"
	compiler "github.com/liqotech/liqo/pkg/utils/networkpolicy"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/options"
)

func (r *Reflector) sourceContext(ctx context.Context, mappings []*options.NamespacedOpts) (api.PolicySourceContext, error) {
	srcContext := api.PolicySourceContext{LocalPodCIDRs: r.localPodCIDRs}
	var configs netapi.ConfigurationList
	if err := r.local.List(ctx, &configs); err != nil {
		return srcContext, err
	}
	found := false
	for _, cfg := range configs.Items {
		if cfg.DeletionTimestamp.IsZero() && cfg.Labels[consts.RemoteClusterID] != "" {
			if cfg.Status.Remote == nil {
				return srcContext, fmt.Errorf("peer network mapping %s is not ready", cfg.Name)
			}
			if cfg.Labels[consts.RemoteClusterID] == r.targetCluster {
				if found {
					return srcContext, fmt.Errorf("multiple configurations for %s", r.targetCluster)
				}
				found = true
				pairs, err := compiler.PrefixPairs(cfg.Status.Remote.CIDR.Pod, cfg.Spec.Remote.CIDR.Pod)
				if err != nil {
					return srcContext, err
				}
				for _, pair := range pairs {
					srcContext.LocalProviderPairs = append(srcContext.LocalProviderPairs, api.PolicyPrefixPair{Source: pair.Source.String(), Destination: pair.Destination.String()})
				}
			} else {
				for _, p := range cfg.Status.Remote.CIDR.Pod {
					srcContext.IndirectCIDRs = append(srcContext.IndirectCIDRs, p.String())
				}
			}
			for _, p := range cfg.Status.Remote.CIDR.External {
				srcContext.IndirectCIDRs = append(srcContext.IndirectCIDRs, p.String())
			}
		}
	}
	if !found {
		return srcContext, fmt.Errorf("provider network mapping is not ready")
	}
	service, err := ipam.GetServiceCIDR(ctx, r.local, r.liqoNamespace)
	if err != nil {
		return srcContext, err
	}
	srcContext.ServiceCIDRs = []string{service}
	var local []netip.Prefix
	for _, value := range r.localPodCIDRs {
		p, err := compiler.ParsePrefix(value)
		if err != nil {
			return srcContext, err
		}
		local = append(local, p)
	}
	// Destination-provider pods use the inverse pod-prefix mapping, not an external allocation.
	for _, pair := range srcContext.LocalProviderPairs {
		p, err := compiler.ParsePrefix(pair.Source)
		if err != nil {
			return srcContext, err
		}
		local = append(local, p)
	}
	for _, mapping := range mappings {
		if err := r.exports(ctx, mapping, local, &srcContext); err != nil {
			return srcContext, err
		}
	}
	slices.Sort(srcContext.IndirectCIDRs)
	slices.Sort(srcContext.PendingExports)
	srcContext.IndirectCIDRs = slices.Compact(srcContext.IndirectCIDRs)
	srcContext.PendingExports = slices.Compact(srcContext.PendingExports)
	slices.SortFunc(srcContext.Exports, func(a, b api.PolicyEndpointExport) int {
		return strings.Compare(a.Original+"/"+a.Allocated+"/"+a.AllocationUID, b.Original+"/"+b.Allocated+"/"+b.AllocationUID)
	})
	return srcContext, nil
}

// exports records allocated addresses independently of Service publication.
// Prefix-mapped pod addresses are excluded by the caller through local.
func (r *Reflector) exports(ctx context.Context, mapping *options.NamespacedOpts, local []netip.Prefix, result *api.PolicySourceContext) error {
	ips, err := mapping.LocalLiqoClient.IpamV1alpha1().IPs(mapping.LocalNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	for _, allocation := range ips.Items {
		if !allocation.DeletionTimestamp.IsZero() {
			continue
		}
		original, err := netip.ParseAddr(allocation.Spec.IP.String())
		if err != nil || !original.Is4() {
			return fmt.Errorf("invalid original address in IP %s/%s", allocation.Namespace, allocation.Name)
		}
		if slices.ContainsFunc(local, func(p netip.Prefix) bool { return p.Contains(original) }) {
			continue
		}
		allocated, addrErr := netip.ParseAddr(allocation.Status.IP.String())
		cidr, cidrErr := compiler.ParsePrefix(allocation.Status.CIDR.String())
		if addrErr != nil || !allocated.Is4() || cidrErr != nil || !cidr.Contains(allocated) || allocation.UID == "" ||
			allocation.Labels[consts.IPHostUnreachableKey] == consts.IPHostUnreachableValue {
			result.PendingExports = append(result.PendingExports, original.String())
			continue
		}
		result.Exports = append(result.Exports, api.PolicyEndpointExport{
			Original: original.String(), Allocated: allocated.String(), AllocationUID: string(allocation.UID),
		})
	}
	return nil
}
