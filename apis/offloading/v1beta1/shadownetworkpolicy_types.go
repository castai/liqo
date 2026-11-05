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

package v1beta1

import (
	corev1 "k8s.io/api/core/v1"
	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
)

// ShadowNetworkPolicyConditionType represents the type of a ShadowNetworkPolicy condition.
type ShadowNetworkPolicyConditionType string

const (
	// ShadowNetworkPolicyConditionAccepted indicates whether the shadow input is well-formed and accepted
	// by the provider for the authenticated tenant and namespace.
	ShadowNetworkPolicyConditionAccepted ShadowNetworkPolicyConditionType = "Accepted"
	// ShadowNetworkPolicyConditionResolved indicates whether every source peer has been fully translated
	// using the provider network state.
	ShadowNetworkPolicyConditionResolved ShadowNetworkPolicyConditionType = "Resolved"
	// ShadowNetworkPolicyConditionApplied indicates whether the generated NetworkPolicy object has been
	// written successfully for the observed input revision.
	ShadowNetworkPolicyConditionApplied ShadowNetworkPolicyConditionType = "Applied"
)

// SourceSemanticsProfile identifies the semantics of the source CNI with respect to IPBlock matching.
type SourceSemanticsProfile string

const (
	// SourceSemanticsLiteral means that IPBlock rules match pod traffic by its literal pod address,
	// i.e. pod traffic is not excluded from IPBlock matching.
	SourceSemanticsLiteral SourceSemanticsProfile = "Literal"
)

// ExportPathKind identifies how a consumer-visible endpoint address is exported to the provider.
type ExportPathKind string

const (
	// ExportPathIndirect is the ordinary Liqo export path, where a non-local endpoint is reached through
	// an individual external CIDR allocation of the consumer and then remapped into the provider view.
	ExportPathIndirect ExportPathKind = "Indirect"
	// ExportPathDirect is a direct provider-to-provider connection authorized with explicit export metadata.
	ExportPathDirect ExportPathKind = "Direct"
)

// ShadowNetworkPolicySource identifies the source NetworkPolicy.
type ShadowNetworkPolicySource struct {
	// ClusterID is the ID of the cluster where the source policy lives.
	ClusterID liqov1beta1.ClusterID `json:"clusterID"`
	// Namespace is the name of the source namespace.
	Namespace string `json:"namespace"`
	// NamespaceUID is the UID of the source namespace.
	// +kubebuilder:validation:Optional
	NamespaceUID types.UID `json:"namespaceUID,omitempty"`
	// Name is the name of the source NetworkPolicy.
	Name string `json:"name"`
	// UID is the UID of the source NetworkPolicy.
	UID types.UID `json:"uid"`
	// ResourceVersion is the source resource version, carried for diagnostics only.
	// +kubebuilder:validation:Optional
	ResourceVersion string `json:"resourceVersion,omitempty"`
}

// CIDRMapping maps a consumer-visible CIDR to the corresponding CIDR in the provider address space.
type CIDRMapping struct {
	// Consumer is the CIDR as seen by the consumer.
	Consumer networkingv1beta1.CIDR `json:"consumer"`
	// Provider is the corresponding CIDR in the provider address space.
	Provider networkingv1beta1.CIDR `json:"provider"`
}

// NetworkPolicyExportProvenance identifies the objects that produced an export record.
type NetworkPolicyExportProvenance struct {
	// IPName is the name of the consumer IP allocation resource.
	// +kubebuilder:validation:Optional
	IPName string `json:"ipName,omitempty"`
	// IPUID is the UID of the consumer IP allocation resource.
	// +kubebuilder:validation:Optional
	IPUID types.UID `json:"ipUID,omitempty"`
	// Namespace is the namespace of the exported endpoint.
	// +kubebuilder:validation:Optional
	Namespace string `json:"namespace,omitempty"`
	// EndpointSliceName is the name of the source EndpointSlice that references the endpoint.
	// +kubebuilder:validation:Optional
	EndpointSliceName string `json:"endpointSliceName,omitempty"`
	// EndpointSliceUID is the UID of the source EndpointSlice that references the endpoint.
	// +kubebuilder:validation:Optional
	EndpointSliceUID types.UID `json:"endpointSliceUID,omitempty"`
}

// NetworkPolicyExportRecord describes one validated exported endpoint address relevant to the policy.
type NetworkPolicyExportRecord struct {
	// SourceAddress is the endpoint address as seen by the consumer.
	SourceAddress string `json:"sourceAddress"`
	// ExportedAddress is the consumer external CIDR address allocated for the endpoint.
	// +kubebuilder:validation:Optional
	ExportedAddress string `json:"exportedAddress,omitempty"`
	// Path identifies the export path kind.
	Path ExportPathKind `json:"path"`
	// TargetClusterID is the cluster hosting the exported endpoint.
	TargetClusterID liqov1beta1.ClusterID `json:"targetClusterID"`
	// DirectProviderID is the provider that terminates a direct connection, if Path is Direct.
	// +kubebuilder:validation:Optional
	DirectProviderID liqov1beta1.ClusterID `json:"directProviderID,omitempty"`
	// DirectAddress is the address passed to the direct mapper, if Path is Direct.
	// +kubebuilder:validation:Optional
	DirectAddress string `json:"directAddress,omitempty"`
	// Provenance identifies the source objects used to build this record.
	// +kubebuilder:validation:Optional
	Provenance *NetworkPolicyExportProvenance `json:"provenance,omitempty"`
}

// ShadowNetworkPolicySourceContext carries the consumer network context needed to interpret policy peers.
type ShadowNetworkPolicySourceContext struct {
	// Profile is the declared source CNI semantics profile.
	Profile SourceSemanticsProfile `json:"profile"`
	// PodCIDRs are the consumer pod CIDRs, in the consumer address space.
	// +kubebuilder:validation:Optional
	PodCIDRs []networkingv1beta1.CIDR `json:"podCIDRs,omitempty"`
	// ServiceCIDRs are the consumer service CIDRs, used to classify unsupported service-address rules.
	// +kubebuilder:validation:Optional
	ServiceCIDRs []networkingv1beta1.CIDR `json:"serviceCIDRs,omitempty"`
	// PodMappings map the consumer pod CIDRs to the provider view.
	// +kubebuilder:validation:Optional
	PodMappings []CIDRMapping `json:"podMappings,omitempty"`
	// LocalPodMappings map the provider native pod CIDRs to the consumer view.
	// +kubebuilder:validation:Optional
	LocalPodMappings []CIDRMapping `json:"localPodMappings,omitempty"`
	// ExternalMappings map the consumer external CIDRs to the provider view.
	// +kubebuilder:validation:Optional
	ExternalMappings []CIDRMapping `json:"externalMappings,omitempty"`
	// ExternalCIDRs are the consumer external CIDRs holding the exported allocations.
	// +kubebuilder:validation:Optional
	ExternalCIDRs []networkingv1beta1.CIDR `json:"externalCIDRs,omitempty"`
	// AliasedRanges are consumer-visible peer ranges whose addresses are only reachable through exports.
	// +kubebuilder:validation:Optional
	AliasedRanges []networkingv1beta1.CIDR `json:"aliasedRanges,omitempty"`
	// DestinationManagedCIDRs are destination address spaces to subtract from unchanged literal rules.
	// +kubebuilder:validation:Optional
	DestinationManagedCIDRs []networkingv1beta1.CIDR `json:"destinationManagedCIDRs,omitempty"`
	// Exports are the validated export records relevant to the target provider.
	// +kubebuilder:validation:Optional
	Exports []NetworkPolicyExportRecord `json:"exports,omitempty"`
	// ExportsComplete states whether the export inventory is known to be complete for the relevant scope.
	// +kubebuilder:validation:Optional
	ExportsComplete bool `json:"exportsComplete,omitempty"`
}

// ShadowNetworkPolicyInputError is a stable, bounded description of an input failure.
type ShadowNetworkPolicyInputError struct {
	// Reason is a short machine-readable reason.
	Reason string `json:"reason"`
	// Field is the policy rule path associated with the failure.
	// +kubebuilder:validation:Optional
	Field string `json:"field,omitempty"`
	// Message is a bounded human-readable message.
	Message string `json:"message"`
}

// ShadowNetworkPolicySpec defines the desired state of a ShadowNetworkPolicy.
type ShadowNetworkPolicySpec struct {
	// Source identifies the source policy.
	Source ShadowNetworkPolicySource `json:"source"`
	// Template is the deep-copied source policy spec with explicit effective policy types.
	Template networkingv1.NetworkPolicySpec `json:"template"`
	// InputRevision is a deterministic digest of the source policy and network context.
	InputRevision string `json:"inputRevision"`
	// SourceContext is the consumer network context used to interpret the template peers.
	// +kubebuilder:validation:Optional
	SourceContext ShadowNetworkPolicySourceContext `json:"sourceContext,omitempty"`
	// InputError, when set, requests an isolation-only generated policy. This gives the provider
	// a way to revoke old grants when the full input cannot be exported.
	// +kubebuilder:validation:Optional
	InputError *ShadowNetworkPolicyInputError `json:"inputError,omitempty"`
}

// ShadowNetworkPolicyStatus defines the observed state of a ShadowNetworkPolicy.
type ShadowNetworkPolicyStatus struct {
	// ObservedGeneration is the generation of the shadow observed by the provider.
	// +kubebuilder:validation:Optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`
	// ObservedInputRevision is the input revision the provider last processed.
	// +kubebuilder:validation:Optional
	ObservedInputRevision string `json:"observedInputRevision,omitempty"`
	// DestinationTopologyDigest is a digest of the provider network state used for resolution.
	// +kubebuilder:validation:Optional
	DestinationTopologyDigest string `json:"destinationTopologyDigest,omitempty"`
	// GeneratedPolicy references the generated provider NetworkPolicy.
	// +kubebuilder:validation:Optional
	GeneratedPolicy *corev1.ObjectReference `json:"generatedPolicy,omitempty"`
	// Conditions describe the current state of the shadow.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=liqo,shortName=snp;snps
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Source",type=string,JSONPath=`.spec.source.name`
// +kubebuilder:printcolumn:name="Input Revision",type=string,priority=1,JSONPath=`.spec.inputRevision`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// ShadowNetworkPolicy is the Schema for the ShadowNetworkPolicies API.
type ShadowNetworkPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ShadowNetworkPolicySpec   `json:"spec,omitempty"`
	Status ShadowNetworkPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ShadowNetworkPolicyList contains a list of ShadowNetworkPolicy.
type ShadowNetworkPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ShadowNetworkPolicy `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ShadowNetworkPolicy{}, &ShadowNetworkPolicyList{})
}
