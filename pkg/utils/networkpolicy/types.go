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

// Package networkpolicy implements the pure translation of consumer NetworkPolicies into
// provider NetworkPolicies for Liqo offloaded workloads.
//
// The package does not perform any Kubernetes API operation: callers are responsible for
// gathering a consistent input snapshot and persisting the generated policy.
package networkpolicy

import (
	"net/netip"

	networkingv1 "k8s.io/api/networking/v1"
)

// Profile identifies the source CNI semantics profile.
type Profile string

const (
	// ProfileLiteral means that IPBlock rules match pod traffic by its literal pod address.
	ProfileLiteral Profile = "Literal"
)

// ExportPath identifies how an exported endpoint reaches the provider.
type ExportPath string

const (
	// ExportPathIndirect is the ordinary external-allocation export path.
	ExportPathIndirect ExportPath = "Indirect"
	// ExportPathDirect is an explicit direct provider-to-provider export path.
	ExportPathDirect ExportPath = "Direct"
)

// Reason is a stable machine-readable diagnostic reason.
type Reason string

// Diagnostic reasons, aligned with the ShadowNetworkPolicy status contract.
const (
	ReasonWaitingForNetworkMapping     Reason = "WaitingForNetworkMapping"
	ReasonWaitingForEndpointAllocation Reason = "WaitingForEndpointAllocation"
	ReasonUnsupportedPeerSelector      Reason = "UnsupportedPeerSelector"
	ReasonUnsupportedServiceAddress    Reason = "UnsupportedServiceAddress"
	ReasonUnsupportedSourceSemantics   Reason = "UnsupportedSourceSemantics"
	ReasonUnsupportedIPv6              Reason = "UnsupportedIPv6"
	ReasonUnsupportedNamedPort         Reason = "UnsupportedNamedPort"
	ReasonUnsupportedProtocol          Reason = "UnsupportedProtocol"
	ReasonUnsupportedEndPort           Reason = "UnsupportedEndPort"
	ReasonAmbiguousAddress             Reason = "AmbiguousAddress"
	ReasonUnresolvedAddress            Reason = "UnresolvedAddress"
	ReasonOutputLimitExceeded          Reason = "OutputLimitExceeded"
	ReasonInvalidInput                 Reason = "InvalidInput"
	ReasonOwnershipConflict            Reason = "OwnershipConflict"
	ReasonApplyFailed                  Reason = "ApplyFailed"
)

// Diagnostic describes why a policy could not be fully translated.
type Diagnostic struct {
	// Reason is a short machine-readable reason.
	Reason Reason
	// Field is the policy path associated with the failure.
	Field string
	// Message is a bounded human-readable explanation.
	Message string
	// Temporary reports whether a later change may resolve the condition.
	Temporary bool
}

// Error implements the error interface.
func (d Diagnostic) Error() string {
	if d.Field == "" {
		return string(d.Reason) + ": " + d.Message
	}
	return string(d.Reason) + " at " + d.Field + ": " + d.Message
}

// ExportRecord describes a validated exported endpoint relevant to the policy.
type ExportRecord struct {
	// SourceAddress is the endpoint address as seen by the consumer.
	SourceAddress netip.Addr
	// ExportedAddress is the consumer external CIDR address allocated for the endpoint.
	ExportedAddress netip.Addr
	// ProviderAddress is the provider-visible address for direct exports.
	ProviderAddress netip.Addr
	// Path identifies the export path.
	Path ExportPath
	// TargetClusterID is the cluster hosting the exported endpoint.
	TargetClusterID string
	// AllocationUID identifies the allocation that produced the record, for diagnostics.
	AllocationUID string
}

// SourceContext is the validated consumer network context required for translation.
type SourceContext struct {
	// Profile is the declared source semantics profile.
	Profile Profile
	// PodCIDRs are the consumer pod CIDRs.
	PodCIDRs []netip.Prefix
	// ServiceCIDRs are the consumer service CIDRs, used only to classify unsupported rules.
	ServiceCIDRs []netip.Prefix
	// ExternalCIDRs are the consumer external CIDRs holding the exported allocations.
	ExternalCIDRs []netip.Prefix
	// PodMappings map consumer pod CIDRs to the provider view.
	PodMappings []PrefixMapping
	// LocalPodMappings map the destination provider native pod CIDRs to the consumer view.
	LocalPodMappings []PrefixMapping
	// ExternalMappings map the consumer external CIDRs to the provider view.
	ExternalMappings []PrefixMapping
	// AliasedRanges are consumer-visible peer ranges reachable only through exports.
	AliasedRanges []netip.Prefix
	// DestinationManagedCIDRs are destination address spaces that must be subtracted from
	// unchanged literal rules to avoid authorizing an excluded identity through an alias.
	DestinationManagedCIDRs []netip.Prefix
	// Exports are the validated export records relevant to the target provider.
	Exports []ExportRecord
	// ExportsComplete states whether the export inventory is complete for the relevant scope.
	ExportsComplete bool
	// PassthroughExternal reports that consumer external addresses keep the same meaning in the
	// destination provider. When false, external literals are not preserved.
	PassthroughExternal bool
}

// Limits bounds the generated output.
type Limits struct {
	// MaxPeers is the maximum number of generated peers per policy. Zero means DefaultMaxPeers.
	MaxPeers int
	// AllowEndPort enables translation of port ranges.
	AllowEndPort bool
	// AllowSCTP enables SCTP rules. It must be validated against the deployed kube-router.
	AllowSCTP bool
}

// DefaultMaxPeers is the default maximum number of generated peers per policy.
const DefaultMaxPeers = 4096

// MaxSerializedInputBytes is the maximum serialized size of a ShadowNetworkPolicy payload.
const MaxSerializedInputBytes = 512 * 1024

func (l Limits) withDefaults() Limits {
	if l.MaxPeers <= 0 {
		l.MaxPeers = DefaultMaxPeers
	}
	return l
}

// Input is the immutable snapshot given to the compiler.
type Input struct {
	// Template is the source NetworkPolicy spec.
	Template networkingv1.NetworkPolicySpec
	// Context is the validated source network context.
	Context SourceContext
	// Limits bounds the generated output.
	Limits Limits
}

// Result is the compiler output.
type Result struct {
	// Spec is the generated policy spec. When Diagnostic is not nil and the input cannot be
	// translated, it is the isolation-only spec.
	Spec networkingv1.NetworkPolicySpec
	// IsolationSpec is the isolation-only spec preserving the source subject and policy types.
	IsolationSpec networkingv1.NetworkPolicySpec
	// Resolved reports whether every peer has been translated.
	Resolved bool
	// Diagnostic is set when the input cannot be fully resolved.
	Diagnostic *Diagnostic
}
