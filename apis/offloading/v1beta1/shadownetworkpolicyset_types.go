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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
)

// ShadowNetworkPolicySetPhase is the phase of a ShadowNetworkPolicySet inventory.
type ShadowNetworkPolicySetPhase string

const (
	// ShadowNetworkPolicySetPhaseStaging means the inventory is being written and must not gate pods yet.
	ShadowNetworkPolicySetPhaseStaging ShadowNetworkPolicySetPhase = "Staging"
	// ShadowNetworkPolicySetPhaseCommitted means the inventory is complete and can be acknowledged.
	ShadowNetworkPolicySetPhaseCommitted ShadowNetworkPolicySetPhase = "Committed"
)

// ShadowNetworkPolicySetConditionType represents the type of a ShadowNetworkPolicySet condition.
type ShadowNetworkPolicySetConditionType string

const (
	// ShadowNetworkPolicySetConditionReady indicates whether the committed inventory is acknowledged
	// by the provider.
	ShadowNetworkPolicySetConditionReady ShadowNetworkPolicySetConditionType = "Ready"
)

// ShadowNetworkPolicySetMember identifies one expected shadow policy in the inventory.
type ShadowNetworkPolicySetMember struct {
	// Name is the name of the expected ShadowNetworkPolicy.
	Name string `json:"name"`
	// SourceUID is the UID of the source NetworkPolicy.
	SourceUID types.UID `json:"sourceUID"`
	// InputRevision is the expected source input revision.
	InputRevision string `json:"inputRevision"`
}

// ShadowNetworkPolicySetSpec defines the desired state of a ShadowNetworkPolicySet.
type ShadowNetworkPolicySetSpec struct {
	// SourceClusterID is the ID of the consumer cluster.
	SourceClusterID liqov1beta1.ClusterID `json:"sourceClusterID"`
	// SourceNamespace is the name of the consumer namespace.
	SourceNamespace string `json:"sourceNamespace"`
	// SourceNamespaceUID is the UID of the consumer namespace.
	SourceNamespaceUID types.UID `json:"sourceNamespaceUID"`
	// Revision is a deterministic digest of the member identities and input revisions.
	Revision string `json:"revision"`
	// Phase is the inventory phase.
	Phase ShadowNetworkPolicySetPhase `json:"phase"`
	// Policies is the sorted list of expected shadow policies.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=name
	Policies []ShadowNetworkPolicySetMember `json:"policies,omitempty"`
}

// ShadowNetworkPolicySetStatus defines the observed state of a ShadowNetworkPolicySet.
type ShadowNetworkPolicySetStatus struct {
	// ObservedInventoryGeneration is the generation of the inventory observed by the provider.
	// +kubebuilder:validation:Optional
	ObservedInventoryGeneration int64 `json:"observedInventoryGeneration,omitempty"`
	// ObservedRevision is the inventory revision acknowledged by the provider.
	// +kubebuilder:validation:Optional
	ObservedRevision string `json:"observedRevision,omitempty"`
	// Conditions describe the current state of the inventory.
	// +kubebuilder:validation:Optional
	// +listType=map
	// +listMapKey=type
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=liqo,shortName=snpset;snpsets
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Namespace",type=string,JSONPath=`.spec.sourceNamespace`
// +kubebuilder:printcolumn:name="Revision",type=string,JSONPath=`.spec.revision`
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.spec.phase`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
// +genclient

// ShadowNetworkPolicySet is the Schema for the ShadowNetworkPolicySets API.
type ShadowNetworkPolicySet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ShadowNetworkPolicySetSpec   `json:"spec,omitempty"`
	Status ShadowNetworkPolicySetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ShadowNetworkPolicySetList contains a list of ShadowNetworkPolicySet.
type ShadowNetworkPolicySetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ShadowNetworkPolicySet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ShadowNetworkPolicySet{}, &ShadowNetworkPolicySetList{})
}
