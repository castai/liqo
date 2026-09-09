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

package ipmapping

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"

	ipamv1alpha1 "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
)

func newTestScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := corev1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("unable to add corev1 scheme: %v", err)
	}
	if err := networkingv1beta1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("unable to add networking scheme: %v", err)
	}
	if err := ipamv1alpha1.SchemeBuilder.AddToScheme(scheme); err != nil {
		t.Fatalf("unable to add ipam scheme: %v", err)
	}
	return scheme
}

func newTestConfiguration() *networkingv1beta1.Configuration {
	return &networkingv1beta1.Configuration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "cluster-a",
			Namespace: "liqo",
		},
		Spec: networkingv1beta1.ConfigurationSpec{
			Local: &networkingv1beta1.ClusterConfig{
				CIDR: networkingv1beta1.ClusterConfigCIDR{
					Pod:      []networkingv1beta1.CIDR{"10.1.0.0/16"},
					External: []networkingv1beta1.CIDR{"10.0.0.0/24"},
				},
			},
			Remote: networkingv1beta1.ClusterConfig{
				CIDR: networkingv1beta1.ClusterConfigCIDR{
					Pod:      []networkingv1beta1.CIDR{"10.2.0.0/16"},
					External: []networkingv1beta1.CIDR{"10.3.0.0/24"},
				},
			},
		},
		Status: networkingv1beta1.ConfigurationStatus{
			Remote: &networkingv1beta1.ClusterConfig{
				CIDR: networkingv1beta1.ClusterConfigCIDR{
					Pod:      []networkingv1beta1.CIDR{"10.2.0.0/16"},
					External: []networkingv1beta1.CIDR{"10.3.0.0/24"},
				},
			},
		},
	}
}

func newTestUnknownSourceIP(cfg *networkingv1beta1.Configuration, ip string) *ipamv1alpha1.IP {
	return &ipamv1alpha1.IP{
		ObjectMeta: metav1.ObjectMeta{
			Name:      forgeUnknownSourceIPName(cfg),
			Namespace: cfg.Namespace,
		},
		Spec: ipamv1alpha1.IPSpec{
			IP: networkingv1beta1.IP(ip),
		},
	}
}

func TestReconcileNodePortSupportDisabledDeletesUnknownSourceIP(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)
	cfg := newTestConfiguration()
	ip := newTestUnknownSourceIP(cfg, "10.3.0.1")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, ip).Build()

	rec := NewConfigurationReconciler(cl, scheme, nil, false)
	_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	deletedIP := &ipamv1alpha1.IP{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(ip), deletedIP); !apierrors.IsNotFound(err) {
		if err == nil {
			t.Errorf("expected unknown-source IP to be deleted, but it still exists")
		} else {
			t.Errorf("expected NotFound error, got: %v", err)
		}
	}
}

func TestReconcileNodePortSupportDisabledIgnoresMissingUnknownSourceIP(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)
	cfg := newTestConfiguration()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg).Build()

	rec := NewConfigurationReconciler(cl, scheme, nil, false)
	_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}
}

func TestReconcileNodePortSupportEnabledCreatesUnknownSourceIP(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)
	cfg := newTestConfiguration()
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg).Build()

	rec := NewConfigurationReconciler(cl, scheme, nil, true)
	_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	createdIP := &ipamv1alpha1.IP{}
	if err := cl.Get(ctx, types.NamespacedName{
		Name:      forgeUnknownSourceIPName(cfg),
		Namespace: cfg.Namespace,
	}, createdIP); err != nil {
		t.Fatalf("expected unknown-source IP to be created, got error: %v", err)
	}

	if createdIP.Spec.IP != networkingv1beta1.IP("10.3.0.0") {
		t.Errorf("unexpected unknown-source IP: got %q, want %q", createdIP.Spec.IP, "10.3.0.0")
	}
}

func TestReconcileNodePortSupportEnabledUpdatesExistingUnknownSourceIP(t *testing.T) {
	ctx := context.Background()
	scheme := newTestScheme(t)
	cfg := newTestConfiguration()
	ip := newTestUnknownSourceIP(cfg, "10.3.0.42")
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(cfg, ip).Build()

	rec := NewConfigurationReconciler(cl, scheme, nil, true)
	_, err := rec.Reconcile(ctx, reconcile.Request{NamespacedName: client.ObjectKeyFromObject(cfg)})
	if err != nil {
		t.Fatalf("Reconcile() error = %v", err)
	}

	updatedIP := &ipamv1alpha1.IP{}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(ip), updatedIP); err != nil {
		t.Fatalf("expected unknown-source IP to exist, got error: %v", err)
	}

	if updatedIP.Spec.IP != networkingv1beta1.IP("10.3.0.0") {
		t.Errorf("unexpected unknown-source IP: got %q, want %q", updatedIP.Spec.IP, "10.3.0.0")
	}
}
