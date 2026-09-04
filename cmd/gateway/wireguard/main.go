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

// Package wireguard contains the logic to configure the Wireguard interface.
package main

import (
	"context"
	"fmt"
	"net"
	"os"
	"time"

	"github.com/spf13/cobra"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/config"
	"sigs.k8s.io/controller-runtime/pkg/event"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/metrics/server"

	ipamv1alpha1 "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/gateway"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	"github.com/liqotech/liqo/pkg/gateway/leaderelection"
	"github.com/liqotech/liqo/pkg/gateway/tunnel/wireguard"
	flagsutils "github.com/liqotech/liqo/pkg/utils/flags"
	"github.com/liqotech/liqo/pkg/utils/mapper"
	"github.com/liqotech/liqo/pkg/utils/restcfg"
)

var (
	scheme                      = runtime.NewScheme()
	options                     = wireguard.NewOptions(gateway.NewOptions())
	leaderMarkerPath            string
	leaderElectionLeaseDuration time.Duration
	leaderElectionRenewDeadline time.Duration
	leaderElectionRetryPeriod   time.Duration
)

func init() {
	utilruntime.Must(corev1.AddToScheme(scheme))
	utilruntime.Must(networkingv1beta1.AddToScheme(scheme))
	utilruntime.Must(ipamv1alpha1.AddToScheme(scheme))
}

// +kubebuilder:rbac:groups=core,resources=events,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch;patch;update
// +kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete

func main() {
	var cmd = cobra.Command{
		Use:  "liqo-wireguard",
		RunE: run,
	}

	flagsutils.InitKlogFlags(cmd.Flags())
	restcfg.InitFlags(cmd.Flags())

	gateway.InitFlags(cmd.Flags(), options.GwOptions)
	wireguard.InitFlags(cmd.Flags(), options)
	cmd.Flags().StringVar(&leaderMarkerPath, "leader-marker-path", leaderelection.DefaultLeaderMarkerPath,
		"Path of the marker file created by the wireguard container when this pod is the leader")
	cmd.Flags().DurationVar(&leaderElectionLeaseDuration, "leader-lease-duration", leaderelection.DefaultLeaseDuration,
		"Duration of the gateway leader election lease")
	cmd.Flags().DurationVar(&leaderElectionRenewDeadline, "leader-renew-deadline", leaderelection.DefaultRenewDeadline,
		"Renew deadline of the gateway leader election lease")
	cmd.Flags().DurationVar(&leaderElectionRetryPeriod, "leader-retry-period", leaderelection.DefaultRetryPeriod,
		"Retry period of the gateway leader election lease")
	if err := wireguard.MarkFlagsRequired(&cmd, options); err != nil {
		klog.Error(err)
		os.Exit(1)
	}

	if err := cmd.Execute(); err != nil {
		klog.Error(err)
		os.Exit(1)
	}
}

// wireGuardSetupRunnable loads keys, creates the WireGuard interface and writes
// the leader marker file. It only runs when this pod holds the leader lease.
type wireGuardSetupRunnable struct {
	options    *wireguard.Options
	markerPath string
	dnsChan    chan event.GenericEvent
	client     client.Client
}

// NeedLeaderElection tells the manager to start this Runnable only after the
// Lease has been acquired.
func (r *wireGuardSetupRunnable) NeedLeaderElection() bool {
	return true
}

// Start loads WireGuard keys, creates the interface, starts the DNS routine if
// needed, writes the marker file and removes it when the context is cancelled.
func (r *wireGuardSetupRunnable) Start(ctx context.Context) error {
	// Load keys.
	if err := wireguard.LoadKeys(r.options); err != nil {
		return fmt.Errorf("unable to load keys: %w", err)
	}

	// Get interface list.
	ports, err := wireguard.GetWireguardPorts(r.options)
	if err != nil {
		return fmt.Errorf("failed to parse wireguard ports: %w", err)
	}

	// Create the wg-liqo interface and init the wireguard configuration depending on the mode (client/server).
	for i := range ports {
		if err := wireguard.InitWireguardLink(ctx, r.options, i); err != nil {
			return fmt.Errorf("failed to create WireGuard interface %d/%d: %w", i+1, len(ports), err)
		}
	}
	klog.Infof("Successfully setup %d WireGuard interfaces", len(ports))

	// Start the DNS resolution routine if required, or set a static endpoint IP.
	if r.options.GwOptions.Mode == gateway.ModeClient {
		if wireguard.IsDNSRoutineRequired(r.options) {
			go wireguard.StartDNSRoutine(ctx, r.dnsChan, r.options)
			klog.Infof("Starting DNS routine: resolving the endpoint address every %s", r.options.DNSCheckInterval.String())
		} else {
			r.options.EndpointIP = net.ParseIP(r.options.EndpointAddress)
			klog.Infof("Setting static endpoint IP: %s", r.options.EndpointIP.String())
		}
	}

	// Write the marker file so that the gateway and geneve containers know this
	// pod is the active replica.
	if err := leaderelection.CreateMarkerFile(r.markerPath); err != nil {
		return fmt.Errorf("unable to create leader marker file: %w", err)
	}
	klog.Infof("Created leader marker file %q", r.markerPath)

	// Label this pod as the leader so that the gateway Service and the Liqo
	// operator route traffic only to this replica.
	if err := r.setLeaderLabel(ctx, leaderelection.LeaderLabelValue); err != nil {
		return fmt.Errorf("unable to set leader label on pod: %w", err)
	}
	klog.Infof("Set leader label on pod %q", r.options.GwOptions.PodName)

	// Keep the runnable alive until the context is cancelled, then remove the
	// marker and the leader label.
	<-ctx.Done()
	_ = leaderelection.RemoveMarkerFile(r.markerPath)
	if err := r.setLeaderLabel(ctx, ""); err != nil {
		klog.Warningf("unable to remove leader label from pod %q: %v", r.options.GwOptions.PodName, err)
	}
	return nil
}

// setLeaderLabel adds or removes the leader label on this pod. An empty value
// removes the label.
func (r *wireGuardSetupRunnable) setLeaderLabel(ctx context.Context, value string) error {
	if r.client == nil || r.options.GwOptions.PodName == "" {
		return nil
	}

	var pod corev1.Pod
	if err := r.client.Get(ctx, client.ObjectKey{
		Namespace: r.options.GwOptions.Namespace,
		Name:      r.options.GwOptions.PodName,
	}, &pod); err != nil {
		return fmt.Errorf("unable to get pod: %w", err)
	}

	patch := client.MergeFrom(pod.DeepCopy())
	if pod.Labels == nil {
		pod.Labels = map[string]string{}
	}
	if value == "" {
		delete(pod.Labels, leaderelection.LeaderLabelKey)
	} else {
		pod.Labels[leaderelection.LeaderLabelKey] = value
	}

	if err := r.client.Patch(ctx, &pod, patch); err != nil {
		return fmt.Errorf("unable to patch pod labels: %w", err)
	}
	return nil
}

func run(cmd *cobra.Command, _ []string) error {
	// Set controller-runtime logger.
	log.SetLogger(klog.NewKlogr())

	// Get the rest config.
	cfg := config.GetConfigOrDie()

	// Create the manager. The wireguard container owns the leader lease for the
	// gateway pod and writes the marker file when elected.
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{
		MapperProvider: mapper.LiqoMapperProvider(scheme),
		Scheme:         scheme,
		Cache: cache.Options{
			DefaultNamespaces: map[string]cache.Config{
				options.GwOptions.Namespace: {},
			},
		},
		Metrics: server.Options{
			BindAddress: options.GwOptions.MetricsAddress,
		},
		HealthProbeBindAddress:        options.GwOptions.ProbeAddr,
		LeaderElection:                true,
		LeaderElectionID:              forge.GatewayResourceName(options.GwOptions.Name),
		LeaderElectionNamespace:       options.GwOptions.Namespace,
		LeaderElectionResourceLock:    resourcelock.LeasesResourceLock,
		LeaderElectionReleaseOnCancel: true,
		LeaseDuration:                 ptr.To(leaderElectionLeaseDuration),
		RenewDeadline:                 ptr.To(leaderElectionRenewDeadline),
		RetryPeriod:                   ptr.To(leaderElectionRetryPeriod),
	})
	if err != nil {
		return fmt.Errorf("unable to create manager: %w", err)
	}

	// Register the healthiness probes.
	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("unable to set up healthz probe: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", leaderelection.ReadyzCheck(leaderMarkerPath)); err != nil {
		return fmt.Errorf("unable to set up readyz probe: %w", err)
	}

	// Channel used by the DNS resolution routine to trigger reconciliation on endpoint changes.
	dnsChan := make(chan event.GenericEvent, 1)

	// Add the leader-only runnable that sets up the WireGuard interface and marker file.
	setupRunnable := &wireGuardSetupRunnable{
		options:    options,
		markerPath: leaderMarkerPath,
		dnsChan:    dnsChan,
	}
	if err := mgr.Add(setupRunnable); err != nil {
		return fmt.Errorf("unable to add wireguard setup runnable: %w", err)
	}
	// The client is only available after the manager is created.
	setupRunnable.client = mgr.GetClient()

	// Setup the controller.
	pkr, err := wireguard.NewPublicKeysReconciler(
		mgr.GetClient(),
		mgr.GetScheme(),
		mgr.GetEventRecorderFor("public-keys-controller"),
		options,
	)
	if err != nil {
		return fmt.Errorf("unable to create public keys reconciler: %w", err)
	}

	if err = pkr.SetupWithManager(mgr, dnsChan); err != nil {
		return fmt.Errorf("unable to setup public keys reconciler: %w", err)
	}

	// Create the Prometheus collector and register it inside the controller-runtime metrics server.
	promcollect, err := wireguard.NewPrometheusCollector(&wireguard.MetricsOptions{
		RemoteClusterID:  options.GwOptions.RemoteClusterID,
		Namespace:        options.GwOptions.Namespace,
		WgImplementation: options.Implementation,
	})
	if err != nil {
		return fmt.Errorf("unable to create prometheus collector: %w", err)
	}
	if err := metrics.Registry.Register(promcollect); err != nil {
		return fmt.Errorf("unable to register prometheus collector: %w", err)
	}

	// Start the manager.
	return mgr.Start(cmd.Context())
}
