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

package connection

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/liqotech/liqo/pkg/gateway/connection/conncheck"
	"github.com/liqotech/liqo/pkg/gateway/tunnel"
)

const (
	driverLabel = "gateway"
)

// PrometheusCollector is a prometheus.Collector that exposes latency and connection
// status metrics, reading directly from the in-memory ConnChecker state.
type PrometheusCollector struct {
	connChecker     *conncheck.ConnChecker
	remoteClusterID string
}

// NewPrometheusCollector creates a new PrometheusCollector.
func NewPrometheusCollector(connChecker *conncheck.ConnChecker, remoteClusterID string) *PrometheusCollector {
	return &PrometheusCollector{
		connChecker:     connChecker,
		remoteClusterID: remoteClusterID,
	}
}

// Describe implements prometheus.Collector.
func (c *PrometheusCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- tunnel.MetricsPeerLatency
	ch <- tunnel.MetricsPeerIsConnected
	tunnel.MetricsPeerLatencyHistogram.Describe(ch)
}

// Collect implements prometheus.Collector.
func (c *PrometheusCollector) Collect(ch chan<- prometheus.Metric) {
	labels := []string{driverLabel, c.remoteClusterID}

	status, err := c.connChecker.GetStatus(c.remoteClusterID)
	if err != nil {
		ch <- prometheus.NewInvalidMetric(tunnel.MetricsPeerLatency, err)
		ch <- prometheus.NewInvalidMetric(tunnel.MetricsPeerIsConnected, err)
		return
	}

	var connectedVal float64
	if status.Connected {
		connectedVal = 1
	}
	ch <- prometheus.MustNewConstMetric(
		tunnel.MetricsPeerIsConnected,
		prometheus.GaugeValue,
		connectedVal,
		labels...,
	)

	// Only emit latency when connected; a latency of 0 with connected=false is misleading.
	if !status.Connected {
		return
	}

	ch <- prometheus.MustNewConstMetric(
		tunnel.MetricsPeerLatency,
		prometheus.GaugeValue,
		float64(status.Latency.Microseconds()),
		labels...,
	)

	tunnel.MetricsPeerLatencyHistogram.Collect(ch)
}
