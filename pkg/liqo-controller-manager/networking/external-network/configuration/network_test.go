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

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/record"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	ipamv1alpha1 "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/apis/networking/v1beta1/firewall"
	"github.com/liqotech/liqo/pkg/consts"
)

var _ = Describe("Configuration networks", func() {
	var (
		ctx    context.Context
		scheme *runtime.Scheme
		cfg    *networkingv1beta1.Configuration
	)

	BeforeEach(func() {
		ctx = context.Background()
		scheme = runtime.NewScheme()
		utilruntime.Must(networkingv1beta1.AddToScheme(scheme))
		utilruntime.Must(ipamv1alpha1.AddToScheme(scheme))

		cfg = &networkingv1beta1.Configuration{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "small-sound",
				Namespace: "liqo-tenant-small-sound",
				UID:       types.UID("configuration-uid"),
				Labels: map[string]string{
					consts.RemoteClusterID: "small-sound",
				},
			},
		}
	})

	It("deletes legacy-named duplicates while keeping the canonical network", func() {
		legacy := &ipamv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "small-sound-pod",
				Namespace: cfg.Namespace,
				Labels: map[string]string{
					consts.RemoteClusterID: cfg.Labels[consts.RemoteClusterID],
					LabelCIDRType:          string(LabelCIDRTypePod),
				},
				OwnerReferences: []metav1.OwnerReference{buildOwnerReference(cfg)},
			},
			Spec: ipamv1alpha1.NetworkSpec{CIDR: "10.2.0.0/16"},
		}

		canonical := &ipamv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ForgeNetworkName(cfg, LabelCIDRTypePod, "10.2.0.0/16"),
				Namespace: cfg.Namespace,
				Labels: map[string]string{
					consts.RemoteClusterID: cfg.Labels[consts.RemoteClusterID],
					LabelCIDRType:          string(LabelCIDRTypePod),
				},
				OwnerReferences: []metav1.OwnerReference{buildOwnerReference(cfg)},
			},
			Spec: ipamv1alpha1.NetworkSpec{CIDR: "10.2.0.0/16"},
		}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, legacy, canonical).Build()

		pendingDeletion, err := DeleteOrphanNetworks(ctx, cl, cfg, LabelCIDRTypePod, []networkingv1beta1.CIDR{"10.2.0.0/16"})
		Expect(err).NotTo(HaveOccurred())
		Expect(pendingDeletion).To(BeTrue())
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(legacy), &ipamv1alpha1.Network{})).ToNot(Succeed())
		Expect(cl.Get(ctx, client.ObjectKeyFromObject(canonical), &ipamv1alpha1.Network{})).To(Succeed())
	})

	It("waits for legacy deletion before creating the canonical network", func() {
		cfg.Spec.Remote = networkingv1beta1.ClusterConfig{
			CIDR: networkingv1beta1.ClusterConfigCIDR{
				Pod: []networkingv1beta1.CIDR{"10.2.0.0/16"},
			},
		}

		legacy := &ipamv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name:      "small-sound-pod",
				Namespace: cfg.Namespace,
				Labels: map[string]string{
					consts.RemoteClusterID: cfg.Labels[consts.RemoteClusterID],
					LabelCIDRType:          string(LabelCIDRTypePod),
				},
				OwnerReferences: []metav1.OwnerReference{buildOwnerReference(cfg)},
			},
			Spec: ipamv1alpha1.NetworkSpec{CIDR: "10.2.0.0/16"},
		}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, legacy).Build()
		r := &ConfigurationReconciler{
			Client:         cl,
			Scheme:         scheme,
			EventsRecorder: record.NewFakeRecorder(16),
		}

		Expect(r.RemapConfiguration(ctx, cfg, r.EventsRecorder)).To(Succeed())

		canonicalKey := client.ObjectKey{Namespace: cfg.Namespace, Name: ForgeNetworkName(cfg, LabelCIDRTypePod, "10.2.0.0/16")}
		Expect(cl.Get(ctx, canonicalKey, &ipamv1alpha1.Network{})).ToNot(Succeed())

		Expect(r.RemapConfiguration(ctx, cfg, r.EventsRecorder)).To(Succeed())
		Expect(cl.Get(ctx, canonicalKey, &ipamv1alpha1.Network{})).To(Succeed())
	})

	It("reserves tunneled CIDRs as not-remapped and shared networks", func() {
		cfg.Spec.TunneledCIDRs = []networkingv1beta1.CIDR{"8.8.8.8/32"}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg).Build()
		r := &ConfigurationReconciler{
			Client:         cl,
			Scheme:         scheme,
			EventsRecorder: record.NewFakeRecorder(16),
		}

		Expect(r.reserveTunneledCIDRs(ctx, cfg, r.EventsRecorder)).To(Succeed())

		key := client.ObjectKey{Namespace: cfg.Namespace, Name: ForgeNetworkName(cfg, LabelCIDRTypeTunneled, "8.8.8.8/32")}
		nw := &ipamv1alpha1.Network{}
		Expect(cl.Get(ctx, key, nw)).To(Succeed())
		Expect(nw.Labels).To(HaveKeyWithValue(consts.NetworkNotRemappedLabelKey, consts.NetworkNotRemappedLabelValue))
		Expect(nw.Labels).To(HaveKeyWithValue(consts.NetworkSharedLabelKey, consts.NetworkSharedLabelValue))
		Expect(nw.Labels).To(HaveKeyWithValue(LabelCIDRType, string(LabelCIDRTypeTunneled)))
		Expect(nw.Spec.CIDR).To(Equal(networkingv1beta1.CIDR("8.8.8.8/32")))
	})

	It("mirrors the reserved tunneled CIDR into the configuration status", func() {
		cfg.Spec.TunneledCIDRs = []networkingv1beta1.CIDR{"8.8.8.8/32"}

		nw := &ipamv1alpha1.Network{
			ObjectMeta: metav1.ObjectMeta{
				Name:      ForgeNetworkName(cfg, LabelCIDRTypeTunneled, "8.8.8.8/32"),
				Namespace: cfg.Namespace,
				Labels: map[string]string{
					consts.RemoteClusterID: cfg.Labels[consts.RemoteClusterID],
					LabelCIDRType:          string(LabelCIDRTypeTunneled),
				},
				OwnerReferences: []metav1.OwnerReference{buildOwnerReference(cfg)},
			},
			Spec:   ipamv1alpha1.NetworkSpec{CIDR: "8.8.8.8/32"},
			Status: ipamv1alpha1.NetworkStatus{CIDR: "8.8.8.8/32"},
		}

		cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, nw).Build()
		r := &ConfigurationReconciler{
			Client:         cl,
			Scheme:         scheme,
			EventsRecorder: record.NewFakeRecorder(16),
		}

		Expect(r.reserveTunneledCIDRs(ctx, cfg, r.EventsRecorder)).To(Succeed())
		Expect(cfg.Status.TunneledCIDRs).To(Equal([]networkingv1beta1.CIDR{"8.8.8.8/32"}))
	})

	It("forges a MASQUERADE rule per reserved tunneled CIDR on tunnel egress", func() {
		cfg.Status.TunneledCIDRs = []networkingv1beta1.CIDR{"8.8.8.8/32"}

		chain := forgeTunneledMasqueradeChain(cfg)
		Expect(chain.Hook).To(HaveValue(Equal(firewall.ChainHookPostrouting)))
		Expect(chain.Type).To(Equal(firewall.ChainTypeNAT))
		Expect(chain.Rules.NatRules).To(HaveLen(1))
		Expect(chain.Rules.NatRules[0].NatType).To(Equal(firewall.NatTypeMasquerade))
		Expect(chain.Rules.NatRules[0].Match).To(HaveLen(2))
	})

	It("does not block the network CIDRs condition when a tunneled CIDR is not reserved", func() {
		cfg.Spec.Remote = networkingv1beta1.ClusterConfig{
			CIDR: networkingv1beta1.ClusterConfigCIDR{
				Pod:      []networkingv1beta1.CIDR{"10.2.0.0/16"},
				External: []networkingv1beta1.CIDR{"10.3.0.0/16"},
			},
		}
		cfg.Spec.TunneledCIDRs = []networkingv1beta1.CIDR{"8.8.8.8/32"}
		cfg.Status.Remote = &networkingv1beta1.ClusterConfig{
			CIDR: networkingv1beta1.ClusterConfigCIDR{
				Pod:      []networkingv1beta1.CIDR{"10.2.0.0/16"},
				External: []networkingv1beta1.CIDR{"10.3.0.0/16"},
			},
		}

		r := &ConfigurationReconciler{}
		r.setConfigurationConditions(cfg)

		Expect(meta.IsStatusConditionTrue(cfg.Status.Conditions,
			networkingv1beta1.ConfigurationConditionNetworkCIDRsConfigured)).To(BeTrue())
		Expect(meta.IsStatusConditionTrue(cfg.Status.Conditions,
			networkingv1beta1.ConfigurationConditionTunneledCIDRsConfigured)).To(BeFalse())

		// Once the tunneled CIDR is reserved, the dedicated condition becomes true.
		cfg.Status.TunneledCIDRs = []networkingv1beta1.CIDR{"8.8.8.8/32"}
		r.setConfigurationConditions(cfg)
		Expect(meta.IsStatusConditionTrue(cfg.Status.Conditions,
			networkingv1beta1.ConfigurationConditionTunneledCIDRsConfigured)).To(BeTrue())
	})
})

func buildOwnerReference(cfg *networkingv1beta1.Configuration) metav1.OwnerReference {
	return metav1.OwnerReference{
		APIVersion: networkingv1beta1.GroupVersion.String(),
		Kind:       "Configuration",
		Name:       cfg.Name,
		UID:        cfg.UID,
		Controller: ptrTo(true),
	}
}

func ptrTo[T any](value T) *T {
	return &value
}
