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
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	ipamv1alpha1 "github.com/liqotech/liqo/apis/ipam/v1alpha1"
	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	networkingutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/utils"
	cidrutils "github.com/liqotech/liqo/pkg/utils/cidr"
	"github.com/liqotech/liqo/pkg/utils/events"
	ipamutils "github.com/liqotech/liqo/pkg/utils/ipam"
)

// ConfigurationReconciler manage Configuration lifecycle.
type ConfigurationReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	EventsRecorder record.EventRecorder

	localCIDR *networkingv1beta1.ClusterConfigCIDR
}

// NewConfigurationReconciler returns a new ConfigurationReconciler.
func NewConfigurationReconciler(cl client.Client, s *runtime.Scheme, er record.EventRecorder) *ConfigurationReconciler {
	return &ConfigurationReconciler{
		Client:         cl,
		Scheme:         s,
		EventsRecorder: er,

		localCIDR: nil,
	}
}

// cluster-role
// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations/status,verbs=get;list;watch;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=configurations/finalizers,verbs=update
// +kubebuilder:rbac:groups=ipam.liqo.io,resources=networks,verbs=get;list;watch;create;update;patch;delete
// +kubebuilder:rbac:groups=ipam.liqo.io,resources=networks/status,verbs=get;list;watch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=firewallconfigurations,verbs=get;list;watch;create;update;patch;delete

// Reconcile manage Configurations, remapping cidrs with Networks resources.
func (r *ConfigurationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	configuration := &networkingv1beta1.Configuration{}
	if err := r.Get(ctx, req.NamespacedName, configuration); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(6).Infof("There is no configuration %s", req.String())
			return ctrl.Result{}, nil
		}
		return ctrl.Result{}, fmt.Errorf("unable to get the configuration %q: %w", req.NamespacedName, err)
	}

	originalCfg := configuration.DeepCopy()
	if configuration.Spec.Local == nil {
		if err := r.defaultLocalNetwork(ctx, configuration); err != nil {
			return ctrl.Result{}, err
		}
	}

	events.Event(r.EventsRecorder, configuration, "Processing configuration")

	if err := r.RemapConfiguration(ctx, configuration, r.EventsRecorder); err != nil {
		return ctrl.Result{}, err
	}

	// Reserve the tunneled CIDRs to ensure they are not currently assigned or will be assigned by the IPAM.
	tunneledErr := r.reserveTunneledCIDRs(ctx, configuration, r.EventsRecorder)
	if tunneledErr != nil {
		return ctrl.Result{}, fmt.Errorf("reserving tunneled CIDRs for configuration %s: %w", req.NamespacedName, tunneledErr)
	}

	if err := r.ensureTunneledMasquerade(ctx, configuration); err != nil {
		return ctrl.Result{}, fmt.Errorf("ensuring tunneled masquerade for configuration %s: %w", req.NamespacedName, err)
	}

	// Update configuration conditions.
	r.setConfigurationConditions(configuration)

	if !equality.Semantic.DeepEqual(originalCfg.Status, configuration.Status) {
		if err := r.UpdateConfigurationStatus(ctx, configuration); err != nil {
			return ctrl.Result{}, err
		}

		klog.Infof("Configuration %s status updated", req.NamespacedName)

		if networkingutils.AreConfigurationNetworkCIDRsConfigured(configuration) {
			events.Event(r.EventsRecorder, configuration, "Configuration remapped")
		} else {
			events.Event(r.EventsRecorder, configuration, "Waiting for all networks to be ready")
		}

		if isTunneledReservationComplete(configuration) {
			events.Event(r.EventsRecorder, configuration, "All tunneled CIDRs reserved")
		} else {
			events.Event(r.EventsRecorder, configuration, "Waiting for all tunneled CIDRs to be reserved")
		}
	}

	return ctrl.Result{}, tunneledErr
}

func (r *ConfigurationReconciler) defaultLocalNetwork(ctx context.Context, cfg *networkingv1beta1.Configuration) error {
	if r.localCIDR == nil {
		podCIDRs, err := ipamutils.GetPodCIDRs(ctx, r.Client, corev1.NamespaceAll)
		if err != nil {
			return fmt.Errorf("unable to retrieve the podCIDR: %w", err)
		}

		externalCIDR, err := ipamutils.GetExternalCIDR(ctx, r.Client, corev1.NamespaceAll)
		if err != nil {
			return fmt.Errorf("unable to retrieve the externalCIDR: %w", err)
		}

		r.localCIDR = &networkingv1beta1.ClusterConfigCIDR{
			Pod:      cidrutils.FromStrings(podCIDRs),
			External: cidrutils.FromStrings([]string{externalCIDR}),
		}
	}

	cfg.Spec.Local = &networkingv1beta1.ClusterConfig{
		CIDR: *r.localCIDR,
	}
	return r.Client.Update(ctx, cfg)
}

// RemapConfiguration ensures one ipamv1alpha1.Network resource per spec CIDR (per cidr-type),
// deletes Networks for CIDRs no longer in the spec, and populates the configuration status with
// the IPAM-remapped values, index-aligned with the spec. Positions whose corresponding Network
// has not yet been remapped by the IPAM are left as empty CIDRs.
//
// Pod and external CIDRs are remapped here, and their failure is fatal. Tunneled CIDRs are reserved
// separately by reserveTunneledCIDRs, so that an optional tunneled CIDR cannot block the rest of the network.
func (r *ConfigurationReconciler) RemapConfiguration(ctx context.Context, cfg *networkingv1beta1.Configuration,
	er record.EventRecorder) error {
	for _, cidrType := range LabelCIDRTypeValues {
		if cidrType == LabelCIDRTypeTunneled {
			continue
		}
		if err := r.remapCIDRType(ctx, cfg, er, cidrType); err != nil {
			return err
		}
	}
	return nil
}

// reserveTunneledCIDRs reserves the tunneled CIDRs of the configuration. From the network standpoint it is
// best-effort: the reserved CIDRs are stored in the status, while the missing ones keep the
// ConfigurationConditionTunneledCIDRsConfigured condition false without blocking the rest of the network.
func (r *ConfigurationReconciler) reserveTunneledCIDRs(ctx context.Context, cfg *networkingv1beta1.Configuration,
	er record.EventRecorder) error {
	return r.remapCIDRType(ctx, cfg, er, LabelCIDRTypeTunneled)
}

// remapCIDRType ensures the Networks of a single cidr-type and writes the resulting status array.
// Pod and external CIDRs are remapped and stored index-aligned with the spec (empty placeholders included).
// Tunneled CIDRs are not remapped: only the ones actually reserved are stored.
func (r *ConfigurationReconciler) remapCIDRType(ctx context.Context, cfg *networkingv1beta1.Configuration,
	er record.EventRecorder, cidrType LabelCIDRTypeValue) error {
	specCIDRs := selectSpecCIDRs(cfg, cidrType)

	pendingDeletion, err := DeleteOrphanNetworks(ctx, r.Client, cfg, cidrType, specCIDRs)
	if err != nil {
		return fmt.Errorf("unable to delete orphan networks for cidr-type %q: %w", cidrType, err)
	}
	if pendingDeletion {
		klog.Infof(
			"Waiting for stale %q networks of configuration %q to be fully deleted before creating replacements",
			cidrType, client.ObjectKeyFromObject(cfg),
		)
		return nil
	}

	remapped := make([]networkingv1beta1.CIDR, 0, len(specCIDRs))
	for _, c := range specCIDRs {
		nw, err := EnsureNetwork(ctx, r.Client, r.Scheme, er, cfg, cidrType, c, ensureNetworkOptions(cidrType))
		if err != nil {
			return fmt.Errorf("ensuring network for CIDR %q: %w", c, err)
		}
		// Tunneled CIDRs are not remapped: store only the ones actually reserved. A Network whose IPAM
		// reservation has not completed yet has an empty status, and must not be written to the status.
		if cidrType == LabelCIDRTypeTunneled && nw.Status.CIDR == "" {
			continue
		}
		remapped = append(remapped, nw.Status.CIDR)
	}

	writeStatusForCIDRType(cfg, cidrType, remapped)
	return nil
}

// UpdateConfigurationStatus update the configuration.
func (r *ConfigurationReconciler) UpdateConfigurationStatus(ctx context.Context, cfg *networkingv1beta1.Configuration) error {
	if err := r.Client.Status().Update(ctx, cfg); err != nil {
		return fmt.Errorf("unable to update the status of the configuration %q: %w", client.ObjectKeyFromObject(cfg), err)
	}
	return nil
}

func (r *ConfigurationReconciler) setConfigurationConditions(cfg *networkingv1beta1.Configuration) {
	// The core network readiness (pod and external CIDRs) must NOT depend on the optional tunneled CIDRs:
	// a tunneled CIDR that cannot be reserved must not block the whole network configuration.
	remapCompleteStatus := metav1.ConditionFalse
	reason := conditionReasonWaitingForNetworks
	message := conditionMessageWaitingForNetworks
	if isRemapComplete(cfg) {
		remapCompleteStatus = metav1.ConditionTrue
		reason = conditionReasonNetworkCIDRsConfigured
		message = conditionMessageNetworkCIDRsConfigured
	}

	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               networkingv1beta1.ConfigurationConditionNetworkCIDRsConfigured,
		Status:             remapCompleteStatus,
		Reason:             reason,
		Message:            message,
		ObservedGeneration: cfg.Generation,
	})

	tunneledStatus := metav1.ConditionFalse
	tunneledReason := conditionReasonWaitingForTunneledCIDRs
	tunneledMessage := conditionMessageWaitingForTunneledCIDRs
	if isTunneledReservationComplete(cfg) {
		tunneledStatus = metav1.ConditionTrue
		tunneledReason = conditionReasonTunneledCIDRsConfigured
		tunneledMessage = conditionMessageTunneledCIDRsConfigured
	}
	// Report the tunneled CIDRs that have been reserved (i.e. configured) so far.
	if reserved := cidrutils.Strings(cfg.Status.TunneledCIDRs); len(reserved) > 0 {
		tunneledMessage = fmt.Sprintf("%s: %s", tunneledMessage, strings.Join(reserved, ", "))
	}

	meta.SetStatusCondition(&cfg.Status.Conditions, metav1.Condition{
		Type:               networkingv1beta1.ConfigurationConditionTunneledCIDRsConfigured,
		Status:             tunneledStatus,
		Reason:             tunneledReason,
		Message:            tunneledMessage,
		ObservedGeneration: cfg.Generation,
	})
}

func ensureNetworkOptions(cidrType LabelCIDRTypeValue) EnsureNetworkOptions {
	// Tunneled CIDRs are not remapped: they are reserved as-is and shared (ref-counted),
	// so multiple Configurations can reserve the same CIDR.
	if cidrType == LabelCIDRTypeTunneled {
		return EnsureNetworkOptions{NotRemapped: true, Shared: true}
	}
	return EnsureNetworkOptions{}
}

func selectSpecCIDRs(cfg *networkingv1beta1.Configuration, cidrType LabelCIDRTypeValue) []networkingv1beta1.CIDR {
	switch cidrType {
	case LabelCIDRTypePod:
		return cfg.Spec.Remote.CIDR.Pod
	case LabelCIDRTypeExternal:
		return cfg.Spec.Remote.CIDR.External
	case LabelCIDRTypeTunneled:
		return cfg.Spec.TunneledCIDRs
	}
	return nil
}

func writeStatusForCIDRType(cfg *networkingv1beta1.Configuration, cidrType LabelCIDRTypeValue, remapped []networkingv1beta1.CIDR) {
	if cfg.Status.Remote == nil && cidrType != LabelCIDRTypeTunneled {
		cfg.Status.Remote = &networkingv1beta1.ClusterConfig{}
	}

	switch cidrType {
	case LabelCIDRTypePod:
		cfg.Status.Remote.CIDR.Pod = remapped
	case LabelCIDRTypeExternal:
		cfg.Status.Remote.CIDR.External = remapped
	case LabelCIDRTypeTunneled:
		cfg.Status.TunneledCIDRs = remapped
	}
}

// isRemapComplete reports whether RemapConfiguration has populated the full status arrays for the pod and
// external CIDRs of the current spec: lengths match and no positions are empty. It does NOT check generation
// parity — that is the job that this function gates. Tunneled CIDRs are intentionally excluded: their
// reservation is reported by ConfigurationConditionTunneledCIDRsConfigured and must not gate the network.
func isRemapComplete(cfg *networkingv1beta1.Configuration) bool {
	if cfg.Status.Remote == nil {
		return false
	}
	if len(cfg.Status.Remote.CIDR.Pod) != len(cfg.Spec.Remote.CIDR.Pod) ||
		len(cfg.Status.Remote.CIDR.External) != len(cfg.Spec.Remote.CIDR.External) {
		return false
	}
	return cidrutils.AllNonVoid(cfg.Status.Remote.CIDR.Pod) &&
		cidrutils.AllNonVoid(cfg.Status.Remote.CIDR.External)
}

// isTunneledReservationComplete reports whether RemapConfiguration has reserved all the tunneled CIDRs of the
// current spec. Only reserved CIDRs are written to the status (no empty placeholder), hence all the spec CIDRs
// are reserved as soon as the lengths match. An empty spec yields true (nothing to reserve).
func isTunneledReservationComplete(cfg *networkingv1beta1.Configuration) bool {
	return len(cfg.Status.TunneledCIDRs) == len(cfg.Spec.TunneledCIDRs)
}

// SetupWithManager register the ConfigurationReconciler to the manager.
func (r *ConfigurationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlConfigurationExternal).
		For(&networkingv1beta1.Configuration{}).
		Owns(&ipamv1alpha1.Network{}).
		Owns(&networkingv1beta1.FirewallConfiguration{}).
		Complete(r)
}
