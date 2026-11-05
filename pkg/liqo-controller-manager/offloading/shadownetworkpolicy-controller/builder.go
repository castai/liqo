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

package shadownetworkpolicyctrl

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"hash"
	"net/netip"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	networkingutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/utils"
	"github.com/liqotech/liqo/pkg/utils/networkpolicy"
)

// BuildInput assembles the compiler input from a shadow policy and the provider Configuration
// describing the consumer cluster.
//
// It returns the input, a diagnostic when the provider network state is not ready, and a digest of
// the destination topology used for the resolution.
func BuildInput(shadow *offloadingv1beta1.ShadowNetworkPolicy,
	cfg *networkingv1beta1.Configuration) (networkpolicy.Input, *networkpolicy.Diagnostic, string) {
	input := networkpolicy.Input{
		Template: shadow.Spec.Template,
		Context: networkpolicy.SourceContext{
			Profile:         networkpolicy.Profile(shadow.Spec.SourceContext.Profile),
			ExportsComplete: shadow.Spec.SourceContext.ExportsComplete,
		},
	}

	digest := sha256.New()
	fmt.Fprintf(digest, "shadow=%s/%d/%s", shadow.UID, shadow.Generation, shadow.Spec.InputRevision)

	podCIDRs, err := parsePrefixes(shadow.Spec.SourceContext.PodCIDRs)
	if err != nil {
		return input, invalidInput("spec.sourceContext.podCIDRs", err), topologyDigest(digest)
	}
	input.Context.PodCIDRs = podCIDRs

	serviceCIDRs, err := parsePrefixes(shadow.Spec.SourceContext.ServiceCIDRs)
	if err != nil {
		return input, invalidInput("spec.sourceContext.serviceCIDRs", err), topologyDigest(digest)
	}
	input.Context.ServiceCIDRs = serviceCIDRs

	externalCIDRs, err := parsePrefixes(shadow.Spec.SourceContext.ExternalCIDRs)
	if err != nil {
		return input, invalidInput("spec.sourceContext.externalCIDRs", err), topologyDigest(digest)
	}
	input.Context.ExternalCIDRs = externalCIDRs

	aliasedRanges, err := parsePrefixes(shadow.Spec.SourceContext.AliasedRanges)
	if err != nil {
		return input, invalidInput("spec.sourceContext.aliasedRanges", err), topologyDigest(digest)
	}
	input.Context.AliasedRanges = aliasedRanges

	localMappings, err := parseLocalMappings(shadow.Spec.SourceContext.LocalPodMappings)
	if err != nil {
		return input, invalidInput("spec.sourceContext.localPodMappings", err), topologyDigest(digest)
	}
	input.Context.LocalPodMappings = localMappings

	exports, diag := parseExports(shadow.Spec.SourceContext.Exports)
	if diag != nil {
		return input, diag, topologyDigest(digest)
	}
	input.Context.Exports = exports

	if cfg == nil {
		if len(podCIDRs) > 0 || len(exports) > 0 || len(externalCIDRs) > 0 {
			return input, &networkpolicy.Diagnostic{
				Reason:    networkpolicy.ReasonWaitingForNetworkMapping,
				Field:     "spec.sourceContext.podMappings",
				Message:   "the provider Configuration for the consumer cluster is not available yet",
				Temporary: true,
			}, topologyDigest(digest)
		}
		return input, nil, topologyDigest(digest)
	}

	fmt.Fprintf(digest, "cfg=%s/%d/%s", cfg.UID, cfg.Generation, cfg.ResourceVersion)

	podMappings, externalMappings, managed, diag := configurationMappings(cfg)
	if diag != nil {
		return input, diag, topologyDigest(digest)
	}
	input.Context.PodMappings = podMappings
	input.Context.ExternalMappings = externalMappings
	input.Context.DestinationManagedCIDRs = managed

	for _, mapping := range podMappings {
		fmt.Fprintf(digest, "pod=%s>%s;", mapping.Source, mapping.Destination)
	}
	for _, mapping := range externalMappings {
		fmt.Fprintf(digest, "ext=%s>%s;", mapping.Source, mapping.Destination)
	}

	return input, nil, topologyDigest(digest)
}

// configurationMappings derives the consumer-to-provider mappings from the provider Configuration.
func configurationMappings(cfg *networkingv1beta1.Configuration) ([]networkpolicy.PrefixMapping, []networkpolicy.PrefixMapping, []netip.Prefix, *networkpolicy.Diagnostic) {
	if cfg.Status.Remote == nil || !networkingutils.AreConfigurationNetworkCIDRsConfigured(cfg) {
		return nil, nil, nil, &networkpolicy.Diagnostic{
			Reason:    networkpolicy.ReasonWaitingForNetworkMapping,
			Field:     "status.remote",
			Message:   "the provider Configuration network CIDRs are not configured yet",
			Temporary: true,
		}
	}

	podMappings, diag := pairMappings("pod", cfg.Spec.Remote.CIDR.Pod, cfg.Status.Remote.CIDR.Pod)
	if diag != nil {
		return nil, nil, nil, diag
	}
	externalMappings, diag := pairMappings("external", cfg.Spec.Remote.CIDR.External, cfg.Status.Remote.CIDR.External)
	if diag != nil {
		return nil, nil, nil, diag
	}

	managed := make([]netip.Prefix, 0)
	if cfg.Spec.Local != nil {
		for _, field := range [][]networkingv1beta1.CIDR{cfg.Spec.Local.CIDR.Pod, cfg.Spec.Local.CIDR.External} {
			prefixes, err := parsePrefixes(field)
			if err != nil {
				return nil, nil, nil, invalidInput("spec.local.cidr", err)
			}
			managed = append(managed, prefixes...)
		}
	}

	return podMappings, externalMappings, managed, nil
}

func pairMappings(kind string, consumer, provider []networkingv1beta1.CIDR) ([]networkpolicy.PrefixMapping, *networkpolicy.Diagnostic) {
	if len(consumer) != len(provider) {
		return nil, &networkpolicy.Diagnostic{
			Reason:    networkpolicy.ReasonWaitingForNetworkMapping,
			Field:     "status.remote.cidr." + kind,
			Message:   fmt.Sprintf("the %s CIDR lists are not index-aligned (%d local, %d remapped)", kind, len(consumer), len(provider)),
			Temporary: true,
		}
	}
	mappings := make([]networkpolicy.PrefixMapping, 0, len(consumer))
	for i := range consumer {
		source, err := parsePrefix(consumer[i])
		if err != nil {
			return nil, invalidInput(fmt.Sprintf("spec.remote.cidr.%s[%d]", kind, i), err)
		}
		destination, err := parsePrefix(provider[i])
		if err != nil {
			return nil, invalidInput(fmt.Sprintf("status.remote.cidr.%s[%d]", kind, i), err)
		}
		mapping := networkpolicy.PrefixMapping{Source: source, Destination: destination}
		if err := mapping.Validate(); err != nil {
			return nil, &networkpolicy.Diagnostic{
				Reason:  networkpolicy.ReasonWaitingForNetworkMapping,
				Field:   fmt.Sprintf("status.remote.cidr.%s[%d]", kind, i),
				Message: err.Error(),
			}
		}
		mappings = append(mappings, mapping)
	}
	return mappings, nil
}

func parseExports(records []offloadingv1beta1.NetworkPolicyExportRecord) ([]networkpolicy.ExportRecord, *networkpolicy.Diagnostic) {
	out := make([]networkpolicy.ExportRecord, 0, len(records))
	for i := range records {
		record := records[i]
		source, err := netip.ParseAddr(record.SourceAddress)
		if err != nil {
			return nil, invalidInput(fmt.Sprintf("spec.sourceContext.exports[%d].sourceAddress", i), err)
		}

		converted := networkpolicy.ExportRecord{
			SourceAddress:   source.Unmap(),
			Path:            networkpolicy.ExportPath(record.Path),
			TargetClusterID: string(record.TargetClusterID),
		}
		if record.Provenance != nil && record.Provenance.IPUID != "" {
			converted.AllocationUID = string(record.Provenance.IPUID)
		}
		switch record.Path {
		case offloadingv1beta1.ExportPathDirect:
			address := record.DirectAddress
			if address == "" {
				address = record.ExportedAddress
			}
			if address != "" {
				providerAddress, err := netip.ParseAddr(address)
				if err != nil {
					return nil, invalidInput(fmt.Sprintf("spec.sourceContext.exports[%d].directAddress", i), err)
				}
				converted.ProviderAddress = providerAddress.Unmap()
			}
		default:
			if record.ExportedAddress != "" {
				exported, err := netip.ParseAddr(record.ExportedAddress)
				if err != nil {
					return nil, invalidInput(fmt.Sprintf("spec.sourceContext.exports[%d].exportedAddress", i), err)
				}
				converted.ExportedAddress = exported.Unmap()
			}
		}
		out = append(out, converted)
	}
	return out, nil
}

func parseLocalMappings(mappings []offloadingv1beta1.CIDRMapping) ([]networkpolicy.PrefixMapping, error) {
	out := make([]networkpolicy.PrefixMapping, 0, len(mappings))
	for i := range mappings {
		consumer, err := parsePrefix(mappings[i].Consumer)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		provider, err := parsePrefix(mappings[i].Provider)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		// The compiler expresses local mappings as provider native -> consumer view.
		out = append(out, networkpolicy.PrefixMapping{Source: provider, Destination: consumer})
	}
	return out, nil
}

func parseMappings(mappings []offloadingv1beta1.CIDRMapping) ([]networkpolicy.PrefixMapping, error) {
	out := make([]networkpolicy.PrefixMapping, 0, len(mappings))
	for i := range mappings {
		source, err := parsePrefix(mappings[i].Consumer)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		destination, err := parsePrefix(mappings[i].Provider)
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, networkpolicy.PrefixMapping{Source: source, Destination: destination})
	}
	return out, nil
}

func parsePrefixes(cidrs []networkingv1beta1.CIDR) ([]netip.Prefix, error) {
	out := make([]netip.Prefix, 0, len(cidrs))
	for i := range cidrs {
		prefix, err := parsePrefix(cidrs[i])
		if err != nil {
			return nil, fmt.Errorf("[%d]: %w", i, err)
		}
		out = append(out, prefix)
	}
	return out, nil
}

func parsePrefix(cidr networkingv1beta1.CIDR) (netip.Prefix, error) {
	prefix, err := netip.ParsePrefix(string(cidr))
	if err != nil {
		return netip.Prefix{}, fmt.Errorf("invalid CIDR %q: %w", cidr, err)
	}
	if !prefix.Addr().Is4() {
		return netip.Prefix{}, fmt.Errorf("CIDR %q is not IPv4", cidr)
	}
	return prefix.Masked(), nil
}

func invalidInput(field string, err error) *networkpolicy.Diagnostic {
	return &networkpolicy.Diagnostic{
		Reason:  networkpolicy.ReasonInvalidInput,
		Field:   field,
		Message: err.Error(),
	}
}

func topologyDigest(digest hash.Hash) string {
	return hex.EncodeToString(digest.Sum(nil))
}
