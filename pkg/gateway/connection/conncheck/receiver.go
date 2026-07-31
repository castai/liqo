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

package conncheck

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"sync"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/klog/v2"

	"github.com/liqotech/liqo/pkg/gateway/tunnel"
)

// Peer represents a peer.
type Peer struct {
	connected bool
	latency   time.Duration
	// lastReceivedTimestamp is the timestamp when the last received PING has been sent.
	lastReceivedTimestamp time.Time
}

// Receiver is a receiver for conncheck messages.
type Receiver struct {
	peers map[string]*Peer
	m     sync.RWMutex
	buff  []byte
	conn  *net.UDPConn
	opts  *Options
}

// NewReceiver creates a new conncheck receiver.
func NewReceiver(conn *net.UDPConn, opts *Options) *Receiver {
	return &Receiver{
		peers: make(map[string]*Peer),
		buff:  make([]byte, opts.PingBufferSize),
		conn:  conn,
		opts:  opts,
	}
}

// SendPong sends a PONG message to the given address.
func (r *Receiver) SendPong(raddr *net.UDPAddr, msg *Msg) error {
	msg.MsgType = PONG
	b, err := json.Marshal(msg)
	if err != nil {
		return fmt.Errorf("failed to marshal msg: %w", err)
	}
	_, err = r.conn.WriteToUDP(b, raddr)
	if err != nil {
		return fmt.Errorf("failed to write to %s: %w", raddr.String(), err)
	}
	klog.V(8).Infof("conncheck receiver: sent a PONG -> %s", msg)
	return nil
}

// ReceivePong receives a PONG message.
func (r *Receiver) ReceivePong(msg *Msg, receivedAt time.Time) {
	r.m.Lock()
	defer r.m.Unlock()
	peer, ok := r.peers[msg.ClusterID]
	if !ok {
		klog.V(8).Infof("%s sender has not been initialized", msg.ClusterID)
		return
	}
	if msg.TimeStamp.Before(peer.lastReceivedTimestamp) {
		klog.V(8).Infof("dropped a PONG message from %s because out-of-order", msg.ClusterID)
		return
	}
	peer.lastReceivedTimestamp = msg.TimeStamp
	peer.latency = receivedAt.Sub(msg.TimeStamp)
	peer.connected = true

	// Observe the latency at the point of measurement, not at scrape time.
	// This ensures the histogram captures every real RTT sample (every ping interval)
	// rather than repeating the last value once per Prometheus scrape.
	tunnel.MetricsPeerLatencyHistogram.With(prometheus.Labels{
		tunnel.MetricsLabels[0]: "gateway",
		tunnel.MetricsLabels[1]: msg.ClusterID,
	}).Observe(float64(peer.latency.Microseconds()))
}

// InitPeer initializes a peer.
func (r *Receiver) InitPeer(clusterID string) {
	r.m.Lock()
	defer r.m.Unlock()
	r.peers[clusterID] = &Peer{
		connected:             false,
		latency:               0,
		lastReceivedTimestamp: time.Now(),
	}
}

// Run starts the receiver.
func (r *Receiver) Run(ctx context.Context) {
	klog.Infof("conncheck receiver: started")
	err := wait.PollUntilContextCancel(ctx, time.Duration(0), false, func(_ context.Context) (done bool, err error) {
		n, raddr, err := r.conn.ReadFromUDP(r.buff)
		if err != nil {
			klog.Errorf("conncheck receiver: failed to read from %s: %v", raddr.String(), err)
			return false, nil
		}
		receivedAt := time.Now()

		msgr := &Msg{}
		err = json.Unmarshal(r.buff[:n], msgr)
		if err != nil {
			klog.Errorf("conncheck receiver: failed to unmarshal msg: %v", err)
			return false, nil
		}
		klog.V(9).Infof("conncheck receiver: received a msg -> %s", msgr)
		switch msgr.MsgType {
		case PING:
			klog.V(8).Infof("conncheck receiver: received a PING %s -> %s", raddr, msgr)
			err = r.SendPong(raddr, msgr)
		case PONG:
			klog.V(8).Infof("conncheck receiver: received a PONG from %s  -> %s", raddr, msgr)
			r.ReceivePong(msgr, receivedAt)
		}
		if err != nil {
			klog.Errorf("conncheck receiver: %v", err)
		}
		return false, nil
	})
	if err != nil {
		klog.Errorf("conncheck receiver: %v", err)
	}
}

// RunDisconnectObserver starts the disconnect observer.
func (r *Receiver) RunDisconnectObserver(ctx context.Context) {
	klog.Infof("conncheck receiver disconnect checker: started")
	// Ignore errors because only caused by context cancellation.
	err := wait.PollUntilContextCancel(ctx, time.Duration(r.opts.PingLossThreshold)*r.opts.PingInterval/10, true,
		func(_ context.Context) (done bool, err error) {
			r.m.Lock()
			defer r.m.Unlock()
			for id, peer := range r.peers {
				if time.Since(peer.lastReceivedTimestamp.Add(peer.latency)) <= r.opts.PingInterval*time.Duration(r.opts.PingLossThreshold) {
					continue
				}
				klog.V(8).Infof("conncheck receiver: %s unreachable", id)
				peer.connected = false
				peer.latency = 0
			}
			return false, nil
		})
	if err != nil {
		klog.Errorf("conncheck disconnect observer: %v", err)
	}
}
