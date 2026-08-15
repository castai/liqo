// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"context"
	"encoding/binary"
	"net"
	"testing"

	"github.com/stretchr/testify/assert"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
)

func TestIPv4ToMapKey(t *testing.T) {
	cases := []string{"0.0.0.0", "10.0.0.1", "192.168.1.1", "255.255.255.255"}

	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			ip := net.ParseIP(tc).To4()
			got := ipv4ToMapKey(ip)

			var raw [4]byte
			binary.NativeEndian.PutUint32(raw[:], got)
			assert.Equal(t, []byte(ip), raw[:])
		})
	}
}

func TestIPv4ToTunnelIPv4(t *testing.T) {
	cases := []string{"0.0.0.0", "10.0.0.1", "192.168.1.1", "255.255.255.255"}

	for _, tc := range cases {
		t.Run(tc, func(t *testing.T) {
			ip := net.ParseIP(tc).To4()
			got := ipv4ToTunnelIPv4(ip)

			var raw [4]byte
			binary.BigEndian.PutUint32(raw[:], got)
			assert.Equal(t, []byte(ip), raw[:])
		})
	}
}

func TestIPv4HelpersNil(t *testing.T) {
	assert.Equal(t, uint32(0), ipv4ToMapKey(nil))
	assert.Equal(t, uint32(0), ipv4ToTunnelIPv4(nil))
}

func TestInternalEndpointIP(t *testing.T) {
	ip := networkingv1beta1.IP("10.20.30.40")
	client := &networkingv1beta1.GatewayClient{
		Status: networkingv1beta1.GatewayClientStatus{
			InternalEndpoint: &networkingv1beta1.InternalGatewayEndpoint{IP: &ip},
		},
	}
	assert.Equal(t, "10.20.30.40", internalEndpointIP(client).String())

	server := &networkingv1beta1.GatewayServer{
		Status: networkingv1beta1.GatewayServerStatus{
			InternalEndpoint: &networkingv1beta1.InternalGatewayEndpoint{IP: &ip},
		},
	}
	assert.Equal(t, "10.20.30.40", internalEndpointIP(server).String())

	empty := &networkingv1beta1.GatewayClient{}
	assert.Nil(t, internalEndpointIP(empty))
}

func TestResolveGatewayIP(t *testing.T) {
	scheme := runtime.NewScheme()
	assert.NoError(t, corev1.AddToScheme(scheme))
	assert.NoError(t, networkingv1beta1.AddToScheme(scheme))

	ip := networkingv1beta1.IP("10.20.30.40")
	clusterID := "test-cluster"
	conf := &networkingv1beta1.Configuration{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-conf",
			Namespace: "default",
			Labels:    map[string]string{consts.RemoteClusterID: clusterID},
		},
	}
	client := &networkingv1beta1.GatewayClient{
		ObjectMeta: metav1.ObjectMeta{
			Name:      "test-gwc",
			Namespace: "default",
			Labels:    map[string]string{consts.RemoteClusterID: clusterID},
		},
		Status: networkingv1beta1.GatewayClientStatus{
			InternalEndpoint: &networkingv1beta1.InternalGatewayEndpoint{IP: &ip},
		},
	}

	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(conf, client).Build()
	r := &RouteReconciler{Client: cl}

	got, err := r.resolveGatewayIP(context.Background(), conf)
	assert.NoError(t, err)
	assert.Equal(t, "10.20.30.40", got.String())
}
