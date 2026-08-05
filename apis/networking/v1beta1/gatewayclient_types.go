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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"
)

// EDIT THIS FILE!  THIS IS SCAFFOLDING FOR YOU TO OWN!
// NOTE: json tags are required.  Any new fields you add must have json tags for the fields to be serialized.

// GatewayClientResource the name of the gatewayclient resources.
var GatewayClientResource = "gatewayclients"

// GatewayClientKind specifies the kind of the gatewayclient resources.
var GatewayClientKind = "GatewayClient"

// GatewayClientGroupResource specifies the group and the resource of the gatewayclient resources.
var GatewayClientGroupResource = schema.GroupResource{Group: GroupVersion.Group, Resource: GatewayClientResource}

// GatewayClientGroupVersionResource specifies the group, the version and the resource of the gatewayclient resources.
var GatewayClientGroupVersionResource = GroupVersion.WithResource(GatewayClientResource)

// GatewayClientSpec defines the desired state of GatewayClient.
// +kubebuilder:validation:XValidation:rule="size(self.endpoints) > 0 ? size(self.endpoints) == self.replicas : self.replicas == 1",message="the number of endpoints must match the number of replicas"
type GatewayClientSpec struct {
	// ClientTemplateRef specifies the reference to the client template.
	ClientTemplateRef corev1.ObjectReference `json:"clientTemplateRef,omitempty"`
	// MTU specifies the MTU of the tunnel.
	MTU int `json:"mtu,omitempty"`
	// Endpoint specifies the endpoint of the tunnel.
	// Deprecated: use Endpoints instead to support multiple gateway replicas.
	Endpoint EndpointStatus `json:"endpoint,omitempty"`
	// Endpoints specifies the endpoints of the tunnel, one per gateway replica.
	// +kubebuilder:validation:MaxItems=64
	Endpoints []EndpointStatus `json:"endpoints,omitempty"`
	// SecretRef specifies the reference to the secret containing configurations.
	// Leave it empty to let the operator create a new secret.
	SecretRef corev1.LocalObjectReference `json:"secretRef,omitempty"`
	// Replicas specifies the number of gateway deployments to create.
	// Each deployment has 1 replica and receives a stable replica identifier.
	// +kubebuilder:default=1
	// +kubebuilder:validation:Minimum=1
	Replicas int32 `json:"replicas,omitempty"`
}

// GatewayClientStatus defines the observed state of GatewayClient.
type GatewayClientStatus struct {
	// ClientRef specifies the reference to the client.
	ClientRef *corev1.ObjectReference `json:"clientRef,omitempty"`
	// SecretRef specifies the reference to the secret.
	SecretRef *corev1.ObjectReference `json:"secretRef,omitempty"`
	// InternalEndpoint specifies the endpoint for the internal network.
	// Deprecated: use InternalEndpoints instead.
	InternalEndpoint *InternalGatewayEndpoint `json:"internalEndpoint,omitempty"`
	// InternalEndpoints specifies the endpoints for the internal network, one per replica.
	// +kubebuilder:validation:MaxItems=64
	InternalEndpoints []InternalGatewayEndpoint `json:"internalEndpoints,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:resource:categories=liqo,shortName=gwc;gwclient
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Template Kind",type=string,JSONPath=`.spec.clientTemplateRef.kind`, priority=1
// +kubebuilder:printcolumn:name="Template Name",type=string,JSONPath=`.spec.clientTemplateRef.name`
// +kubebuilder:printcolumn:name="Template Namespace",type=string,JSONPath=`.spec.clientTemplateRef.namespace`, priority=1
// +kubebuilder:printcolumn:name="IP",type=string,JSONPath=`.spec.endpoint.addresses[*]`
// +kubebuilder:printcolumn:name="Port",type=string,JSONPath=`.spec.endpoint.port`
// +kubebuilder:printcolumn:name="Protocol",type=string,JSONPath=`.spec.endpoint.protocol`, priority=1
// +kubebuilder:printcolumn:name="MTU",type=integer,JSONPath=`.spec.mtu`, priority=1
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// GatewayClient defines a gateway client that needs to point to a remote gateway server.
type GatewayClient struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   GatewayClientSpec   `json:"spec,omitempty"`
	Status GatewayClientStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// GatewayClientList contains a list of GatewayClient.
type GatewayClientList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []GatewayClient `json:"items"`
}

func init() {
	SchemeBuilder.Register(&GatewayClient{}, &GatewayClientList{})
}
