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
	"context"
	"testing"

	networkingv1 "k8s.io/api/networking/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/utils/networkpolicy"
)

func readyConfiguration() *networkingv1beta1.Configuration {
	return &networkingv1beta1.Configuration{
		ObjectMeta: metav1.ObjectMeta{
			Name:       "configuration-c",
			Generation: 1,
			Labels:     map[string]string{consts.RemoteClusterID: "consumer"},
		},
		Spec: networkingv1beta1.ConfigurationSpec{
			Local: &networkingv1beta1.ClusterConfig{
				CIDR: networkingv1beta1.ClusterConfigCIDR{
					Pod:      []networkingv1beta1.CIDR{"10.80.0.0/16"},
					External: []networkingv1beta1.CIDR{"10.90.0.0/16"},
				},
			},
			Remote: networkingv1beta1.ClusterConfig{
				CIDR: networkingv1beta1.ClusterConfigCIDR{
					Pod:      []networkingv1beta1.CIDR{"10.20.0.0/16"},
					External: []networkingv1beta1.CIDR{"10.70.0.0/16"},
				},
			},
		},
		Status: networkingv1beta1.ConfigurationStatus{
			Conditions: []metav1.Condition{{
				Type:               networkingv1beta1.ConfigurationConditionNetworkCIDRsConfigured,
				Status:             metav1.ConditionTrue,
				ObservedGeneration: 1,
			}},
			Remote: &networkingv1beta1.ClusterConfig{
				CIDR: networkingv1beta1.ClusterConfigCIDR{
					Pod:      []networkingv1beta1.CIDR{"172.20.0.0/16"},
					External: []networkingv1beta1.CIDR{"172.31.0.0/16"},
				},
			},
		},
	}
}

func sourceShadow(spec networkingv1.NetworkPolicySpec) *offloadingv1beta1.ShadowNetworkPolicy {
	return &offloadingv1beta1.ShadowNetworkPolicy{
		ObjectMeta: metav1.ObjectMeta{Name: "shadow", Namespace: "dest", Generation: 1, UID: types.UID("shadow-uid")},
		Spec: offloadingv1beta1.ShadowNetworkPolicySpec{
			Source: offloadingv1beta1.ShadowNetworkPolicySource{
				ClusterID: "consumer",
				Namespace: "source",
				Name:      "policy",
				UID:       types.UID("policy-uid"),
			},
			Template:      spec,
			InputRevision: "revision",
			SourceContext: offloadingv1beta1.ShadowNetworkPolicySourceContext{
				Profile:  offloadingv1beta1.SourceSemanticsLiteral,
				PodCIDRs: []networkingv1beta1.CIDR{"10.20.0.0/16"},
			},
		},
	}
}

func egressSpec(peer networkingv1.NetworkPolicyPeer) networkingv1.NetworkPolicySpec {
	return networkingv1.NetworkPolicySpec{
		PodSelector: metav1.LabelSelector{MatchLabels: map[string]string{"app": "foo"}},
		PolicyTypes: []networkingv1.PolicyType{networkingv1.PolicyTypeEgress},
		Egress:      []networkingv1.NetworkPolicyEgressRule{{To: []networkingv1.NetworkPolicyPeer{peer}}},
	}
}

func TestBuildInputUsesProviderConfiguration(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "10.20.7.9/32"},
	}))

	input, diag, digest := BuildInput(shadow, readyConfiguration())
	if diag != nil {
		t.Fatalf("unexpected diagnostic: %v", diag)
	}
	if digest == "" {
		t.Fatalf("expected a topology digest")
	}
	if len(input.Context.PodMappings) != 1 {
		t.Fatalf("expected one pod mapping, got %v", input.Context.PodMappings)
	}
	if input.Context.PodMappings[0].Source.String() != "10.20.0.0/16" ||
		input.Context.PodMappings[0].Destination.String() != "172.20.0.0/16" {
		t.Fatalf("unexpected pod mapping: %v", input.Context.PodMappings[0])
	}

	result := networkpolicy.Compile(input)
	if result.Diagnostic != nil {
		t.Fatalf("unexpected compile diagnostic: %v", result.Diagnostic)
	}
	if len(result.Spec.Egress) != 1 || len(result.Spec.Egress[0].To) != 1 {
		t.Fatalf("unexpected generated egress: %v", result.Spec.Egress)
	}
	if result.Spec.Egress[0].To[0].IPBlock.CIDR != "172.20.7.9/32" {
		t.Fatalf("expected the remapped consumer pod, got %v", result.Spec.Egress[0].To)
	}
}

func TestBuildInputIndirectExport(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "172.26.4.7/32"},
	}))
	shadow.Spec.SourceContext = offloadingv1beta1.ShadowNetworkPolicySourceContext{
		Profile:         offloadingv1beta1.SourceSemanticsLiteral,
		PodCIDRs:        []networkingv1beta1.CIDR{"10.20.0.0/16"},
		ExternalCIDRs:   []networkingv1beta1.CIDR{"10.70.0.0/16"},
		AliasedRanges:   []networkingv1beta1.CIDR{"172.26.0.0/16"},
		ExportsComplete: true,
		Exports: []offloadingv1beta1.NetworkPolicyExportRecord{{
			SourceAddress:   "172.26.4.7",
			ExportedAddress: "10.70.0.42",
			Path:            offloadingv1beta1.ExportPathIndirect,
			TargetClusterID: "P1",
		}},
	}

	input, diag, _ := BuildInput(shadow, readyConfiguration())
	if diag != nil {
		t.Fatalf("unexpected diagnostic: %v", diag)
	}
	result := networkpolicy.Compile(input)
	if result.Diagnostic != nil {
		t.Fatalf("unexpected compile diagnostic: %v", result.Diagnostic)
	}
	if len(result.Spec.Egress) != 1 || len(result.Spec.Egress[0].To) != 1 {
		t.Fatalf("unexpected generated egress: %v", result.Spec.Egress)
	}
	if result.Spec.Egress[0].To[0].IPBlock.CIDR != "172.31.0.42/32" {
		t.Fatalf("expected the indirect alias, got %v", result.Spec.Egress[0].To)
	}
}

func TestBuildInputMissingConfigurationIsTemporary(t *testing.T) {
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "10.20.7.9/32"},
	}))
	_, diag, _ := BuildInput(shadow, nil)
	if diag == nil || diag.Reason != networkpolicy.ReasonWaitingForNetworkMapping || !diag.Temporary {
		t.Fatalf("expected a temporary network mapping diagnostic, got %v", diag)
	}
}

func TestBuildInputRejectsMismatchedMappings(t *testing.T) {
	configuration := readyConfiguration()
	configuration.Status.Remote.CIDR.Pod = []networkingv1beta1.CIDR{"172.20.0.0/16", "172.21.0.0/16"}
	shadow := sourceShadow(egressSpec(networkingv1.NetworkPolicyPeer{
		IPBlock: &networkingv1.IPBlock{CIDR: "10.20.7.9/32"},
	}))
	_, diag, _ := BuildInput(shadow, configuration)
	if diag == nil || diag.Reason != networkpolicy.ReasonWaitingForNetworkMapping {
		t.Fatalf("expected a network mapping diagnostic, got %v", diag)
	}
}

func TestGate(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := offloadingv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the offloading scheme: %v", err)
	}

	newSet := func(phase offloadingv1beta1.ShadowNetworkPolicySetPhase, revision, observedRevision string, ready bool) *offloadingv1beta1.ShadowNetworkPolicySet {
		state := metav1.ConditionFalse
		reason := "InventoryNotReady"
		if ready {
			state = metav1.ConditionTrue
			reason = "InventoryReady"
		}
		return &offloadingv1beta1.ShadowNetworkPolicySet{
			ObjectMeta: metav1.ObjectMeta{
				Name:       "inventory",
				Namespace:  "dest",
				Generation: 1,
				Labels: map[string]string{
					"offloading.liqo.io/origin": "consumer",
				},
			},
			Spec: offloadingv1beta1.ShadowNetworkPolicySetSpec{
				SourceClusterID: "consumer",
				SourceNamespace: "source",
				Revision:        revision,
				Phase:           phase,
			},
			Status: offloadingv1beta1.ShadowNetworkPolicySetStatus{
				ObservedInventoryGeneration: 1,
				ObservedRevision:            observedRevision,
				Conditions: []metav1.Condition{{
					Type:               string(offloadingv1beta1.ShadowNetworkPolicySetConditionReady),
					Status:             state,
					Reason:             reason,
					ObservedGeneration: 1,
				}},
			},
		}
	}

	tests := []struct {
		name    string
		objects []*offloadingv1beta1.ShadowNetworkPolicySet
		ready   bool
		reason  string
	}{
		{
			name:   "missing inventory",
			ready:  false,
			reason: "MissingInventory",
		},
		{
			name:    "committed and ready",
			objects: []*offloadingv1beta1.ShadowNetworkPolicySet{newSet(offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted, "r1", "r1", true)},
			ready:   true,
			reason:  "InventoryReady",
		},
		{
			name:    "staging inventory",
			objects: []*offloadingv1beta1.ShadowNetworkPolicySet{newSet(offloadingv1beta1.ShadowNetworkPolicySetPhaseStaging, "r1", "", false)},
			ready:   false,
			reason:  "StagingInventory",
		},
		{
			name:    "stale revision",
			objects: []*offloadingv1beta1.ShadowNetworkPolicySet{newSet(offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted, "r2", "r1", true)},
			ready:   false,
			reason:  "StaleInventory",
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			builder := fake.NewClientBuilder().WithScheme(scheme)
			for _, object := range test.objects {
				builder = builder.WithObjects(object)
			}
			gate := &Gate{Client: builder.Build()}

			ready, reason, err := gate.Check(context.Background(), "dest", "consumer", "source")
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if ready != test.ready || reason != test.reason {
				t.Fatalf("expected (%v, %q), got (%v, %q)", test.ready, test.reason, ready, reason)
			}
		})
	}
}

func TestGateRejectsForeignSourceCluster(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := offloadingv1beta1.AddToScheme(scheme); err != nil {
		t.Fatalf("adding the offloading scheme: %v", err)
	}
	set := &offloadingv1beta1.ShadowNetworkPolicySet{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "inventory",
			Namespace: "dest",
			Labels:    map[string]string{"offloading.liqo.io/origin": "consumer"},
		},
		Spec: offloadingv1beta1.ShadowNetworkPolicySetSpec{
			SourceClusterID: "consumer",
			SourceNamespace: "source",
			Phase:           offloadingv1beta1.ShadowNetworkPolicySetPhaseCommitted,
		},
	}
	gate := &Gate{Client: fake.NewClientBuilder().WithScheme(scheme).WithObjects(set).Build()}
	ready, reason, err := gate.Check(context.Background(), "dest", "consumer", "other")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if ready || reason != "MissingInventory" {
		t.Fatalf("expected a missing inventory for the foreign namespace, got (%v, %q)", ready, reason)
	}
}
