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
	networking "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// PolicySource identifies the owner of a reflected policy inventory.
type PolicySource struct {
	Cluster      string `json:"cluster"`
	Namespace    string `json:"namespace"`
	NamespaceUID string `json:"namespaceUID"`
}

// PolicyPrefixPair maps a consumer-visible prefix to a native provider prefix.
type PolicyPrefixPair struct {
	Source      string `json:"source"`
	Destination string `json:"destination"`
}

// PolicyEndpointExport describes an active external address export through the consumer.
type PolicyEndpointExport struct {
	Original      string `json:"original"`
	Allocated     string `json:"allocated"`
	AllocationUID string `json:"allocationUID"`
	Namespace     string `json:"namespace"`
	Slice         string `json:"slice"`
}

// PolicySourceContext holds source network information for compiling a policy.
type PolicySourceContext struct {
	PendingExports     []string               `json:"pendingExports,omitempty"`
	LocalPodCIDRs      []string               `json:"localPodCIDRs,omitempty"`
	LocalProviderPairs []PolicyPrefixPair     `json:"localProviderPairs,omitempty"`
	IndirectCIDRs      []string               `json:"indirectCIDRs,omitempty"`
	ServiceCIDRs       []string               `json:"serviceCIDRs,omitempty"`
	Exports            []PolicyEndpointExport `json:"exports,omitempty"`
}

// ShadowNetworkPolicySpec describes the source policy and its address context.
type ShadowNetworkPolicySpec struct {
	Source     PolicySource                 `json:"source"`
	PolicyUID  string                       `json:"policyUID"`
	PolicyName string                       `json:"policyName"`
	Template   networking.NetworkPolicySpec `json:"template"`
	Context    PolicySourceContext          `json:"context"`
	InputError string                       `json:"inputError,omitempty"`
}

// ShadowNetworkPolicyStatus records the result for the current generation.
type ShadowNetworkPolicyStatus struct {
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ShadowNetworkPolicy transports a source NetworkPolicy to its provider.
type ShadowNetworkPolicy struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ShadowNetworkPolicySpec   `json:"spec"`
	Status            ShadowNetworkPolicyStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ShadowNetworkPolicyList contains reflected policies.
type ShadowNetworkPolicyList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ShadowNetworkPolicy `json:"items"`
}

// PolicySetMember identifies a particular revision of a shadow.
type PolicySetMember struct {
	Name       string `json:"name"`
	UID        string `json:"uid"`
	Generation int64  `json:"generation"`
}

// ShadowNetworkPolicySetSpec records the complete policy inventory.
type ShadowNetworkPolicySetSpec struct {
	PolicyRevision string            `json:"policyRevision"`
	Source         PolicySource      `json:"source"`
	Revision       string            `json:"revision"`
	Committed      bool              `json:"committed"`
	Members        []PolicySetMember `json:"members,omitempty"`
}

// ShadowNetworkPolicySetStatus acknowledges the applied inventory.
type ShadowNetworkPolicySetStatus struct {
	ObservedGeneration int64  `json:"observedGeneration,omitempty"`
	Revision           string `json:"revision,omitempty"`
	Ready              bool   `json:"ready,omitempty"`
	Message            string `json:"message,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status

// ShadowNetworkPolicySet provides a startup barrier for a mapped namespace.
type ShadowNetworkPolicySet struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`
	Spec              ShadowNetworkPolicySetSpec   `json:"spec"`
	Status            ShadowNetworkPolicySetStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ShadowNetworkPolicySetList contains reflected policy inventories.
type ShadowNetworkPolicySetList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ShadowNetworkPolicySet `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ShadowNetworkPolicy{}, &ShadowNetworkPolicyList{}, &ShadowNetworkPolicySet{}, &ShadowNetworkPolicySetList{})
}
