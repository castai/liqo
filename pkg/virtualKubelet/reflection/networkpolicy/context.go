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

	discovery "k8s.io/api/discovery/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ipamapi "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	netapi "github.com/liqotech/liqo/apis/networking/v1beta1"
	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/utils/getters"
	"github.com/liqotech/liqo/pkg/utils/ipam"
	compiler "github.com/liqotech/liqo/pkg/utils/networkpolicy"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/options"
)

func (r *Reflector) sourceContext(ctx context.Context, mappings []*options.NamespacedOpts) (api.PolicySourceContext, error) {
	result := api.PolicySourceContext{LocalPodCIDRs: r.localPodCIDRs}
	var configs netapi.ConfigurationList
	if err := r.local.List(ctx, &configs); err != nil {
		return result, err
	}
	found := false
	for _, cfg := range configs.Items {
		if cfg.DeletionTimestamp.IsZero() && cfg.Labels[consts.RemoteClusterID] != "" {
			if cfg.Status.Remote == nil {
				return result, fmt.Errorf("peer network mapping %s is not ready", cfg.Name)
			}
			if cfg.Labels[consts.RemoteClusterID] == r.targetCluster {
				if found {
					return result, fmt.Errorf("multiple configurations for %s", r.targetCluster)
				}
				found = true
				pairs, err := compiler.PrefixPairs(cfg.Status.Remote.CIDR.Pod, cfg.Spec.Remote.CIDR.Pod)
				if err != nil {
					return result, err
				}
				for _, pair := range pairs {
					result.LocalProviderPairs = append(result.LocalProviderPairs, api.PolicyPrefixPair{Source: pair.Source.String(), Destination: pair.Destination.String()})
				}
			} else {
				for _, p := range cfg.Status.Remote.CIDR.Pod {
					result.IndirectCIDRs = append(result.IndirectCIDRs, p.String())
				}
			}
			for _, p := range cfg.Status.Remote.CIDR.External {
				result.IndirectCIDRs = append(result.IndirectCIDRs, p.String())
			}
		}
	}
	if !found {
		return result, fmt.Errorf("provider network mapping is not ready")
	}
	service, err := ipam.GetServiceCIDR(ctx, r.local, r.liqoNamespace)
	if err != nil {
		return result, err
	}
	result.ServiceCIDRs = []string{service}
	var local []netip.Prefix
	for _, value := range r.localPodCIDRs {
		p, err := netip.ParsePrefix(value)
		if err != nil {
			return result, err
		}
		local = append(local, p)
	}
	for _, mapping := range mappings {
		if err := r.exports(ctx, mapping, local, &result); err != nil {
			return result, err
		}
	}
	slices.Sort(result.IndirectCIDRs)
	slices.Sort(result.PendingExports)
	result.IndirectCIDRs = slices.Compact(result.IndirectCIDRs)
	result.PendingExports = slices.Compact(result.PendingExports)
	slices.SortFunc(result.Exports, func(a, b api.PolicyEndpointExport) int {
		return strings.Compare(a.Original+"/"+a.Allocated+"/"+a.Namespace+"/"+a.Slice, b.Original+"/"+b.Allocated+"/"+b.Namespace+"/"+b.Slice)
	})
	return result, nil
}

func (r *Reflector) exports(ctx context.Context, mapping *options.NamespacedOpts, local []netip.Prefix, result *api.PolicySourceContext) error {
	var endpoints discovery.EndpointSliceList
	if err := r.local.List(ctx, &endpoints, client.InNamespace(mapping.LocalNamespace)); err != nil {
		return err
	}
	ips, err := mapping.LocalLiqoClient.IpamV1alpha1().IPs(mapping.LocalNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return err
	}
	allocations := make([]*ipamapi.IP, 0, len(ips.Items))
	for i := range ips.Items {
		allocations = append(allocations, &ips.Items[i])
	}
	var shadows api.ShadowEndpointSliceList
	if err := r.remote.List(ctx, &shadows, client.InNamespace(mapping.RemoteNamespace)); err != nil {
		return err
	}
	byName := make(map[string]*api.ShadowEndpointSlice, len(shadows.Items))
	for i := range shadows.Items {
		s := &shadows.Items[i]
		if s.DeletionTimestamp.IsZero() && forge.IsEndpointSliceManagedByReflection(s) && s.Labels[forge.LiqoOriginClusterIDKey] == r.sourceCluster {
			byName[s.Name] = s
		}
	}
	directServices := make(map[string]bool)
	nodes := make(map[string]string)
	for _, slice := range endpoints.Items {
		if !slice.DeletionTimestamp.IsZero() || slice.AddressType != discovery.AddressTypeIPv4 {
			continue
		}
		serviceName := slice.Labels[discovery.LabelServiceName]
		direct, found := directServices[serviceName]
		if !found && serviceName != "" {
			service, err := mapping.LocalClient.CoreV1().Services(mapping.LocalNamespace).Get(ctx, serviceName, metav1.GetOptions{})
			if err != nil && !apierrors.IsNotFound(err) {
				return err
			}
			if err == nil {
				direct = service.Annotations[consts.UseDirectConnectionAnnotationKey] == "true"
			}
			directServices[serviceName] = direct
		}
		for _, endpoint := range slice.Endpoints {
			if endpoint.NodeName != nil {
				name := *endpoint.NodeName
				cluster, ok := nodes[name]
				if !ok {
					node, err := mapping.LocalClient.CoreV1().Nodes().Get(ctx, name, metav1.GetOptions{})
					if apierrors.IsNotFound(err) {
						continue
					}
					if err != nil {
						return err
					}
					id, err := getters.RetrieveRemoteClusterIDFromNode(node)
					if err != nil {
						return err
					}
					cluster = id
					nodes[name] = cluster
				}
				if cluster == r.targetCluster {
					continue
				}
			}
			for _, address := range endpoint.Addresses {
				original, err := netip.ParseAddr(address)
				if err != nil || !original.Is4() {
					return fmt.Errorf("invalid endpoint %s", address)
				}
				if slices.ContainsFunc(local, func(p netip.Prefix) bool { return p.Contains(original) }) {
					continue
				}
				shadow := byName[slice.Name]
				allocation, err := ipam.EndpointAllocation(allocations, address)
				if direct || err != nil || !exported(shadow, endpoint, allocation.Status.IP.String()) {
					result.PendingExports = append(result.PendingExports, address)
					continue
				}
				result.Exports = append(result.Exports, api.PolicyEndpointExport{
					Original: address, Allocated: allocation.Status.IP.String(), AllocationUID: string(allocation.UID),
					Namespace: mapping.RemoteNamespace, Slice: slice.Name,
				})
			}
		}
	}
	return nil
}

func exported(slice *api.ShadowEndpointSlice, source discovery.Endpoint, address string) bool {
	if slice == nil || slice.Annotations[consts.DirectConnectionDataAnnotationKey] != "" {
		return false
	}
	for _, ep := range slice.Spec.Template.Endpoints {
		if source.TargetRef != nil && (ep.TargetRef == nil || ep.TargetRef.UID != source.TargetRef.UID) {
			continue
		}
		if slices.Contains(ep.Addresses, address) {
			return true
		}
	}
	return false
}
