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

package wireguard

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	"k8s.io/klog/v2"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/handler"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	enutils "github.com/liqotech/liqo/pkg/liqo-controller-manager/networking/external-network/utils"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// WgGatewayClientReconciler manage WgGatewayClient lifecycle.
type WgGatewayClientReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	clusterRoleName string

	eventRecorder record.EventRecorder
}

// NewWgGatewayClientReconciler returns a new WgGatewayClientReconciler.
func NewWgGatewayClientReconciler(cl client.Client, s *runtime.Scheme,
	recorder record.EventRecorder,
	clusterRoleName string) *WgGatewayClientReconciler {
	return &WgGatewayClientReconciler{
		Client:          cl,
		Scheme:          s,
		clusterRoleName: clusterRoleName,

		eventRecorder: recorder,
	}
}

// cluster-role
// +kubebuilder:rbac:groups=networking.liqo.io,resources=wggatewayclients,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=wggatewayclients/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=wggatewayclients/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;create;delete;update
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;delete;create;update;patch

// Reconcile manage WgGatewayClient lifecycle.
func (r *WgGatewayClientReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	wgClient := &networkingv1beta1.WgGatewayClient{}
	if err = r.Get(ctx, req.NamespacedName, wgClient); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("WireGuard gateway client %q not found", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.Errorf("Unable to get the WireGuard gateway client %q: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	if !wgClient.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(wgClient, consts.ClusterRoleBindingFinalizer) {
			if err = enutils.DeleteClusterRoleBinding(ctx, r.Client, wgClient); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(wgClient, consts.ClusterRoleBindingFinalizer)
			if err = r.Update(ctx, wgClient); err != nil {
				klog.Errorf("Unable to remove finalizer %q from WireGuard gateway client %q: %v",
					consts.ClusterRoleBindingFinalizer, req.NamespacedName, err)
				return ctrl.Result{}, err
			}
		}

		// Resource is deleting and child resources are deleted as well by garbage collector. Nothing to do.
		return ctrl.Result{}, nil
	}

	originalWgClient := wgClient.DeepCopy()

	// Ensure ServiceAccount and ClusterRoleBinding (create or update)
	if err = enutils.EnsureServiceAccountAndClusterRoleBinding(ctx, r.Client, r.Scheme, &wgClient.Spec.Deployment, wgClient,
		r.clusterRoleName); err != nil {
		return ctrl.Result{}, err
	}

	// update if the wgClient has been updated
	if !equality.Semantic.DeepEqual(originalWgClient, wgClient) {
		if err := r.Update(ctx, wgClient); err != nil {
			return ctrl.Result{}, err
		}

		// we return here to avoid conflicts
		return ctrl.Result{}, nil
	}

	replicas := wgClient.Spec.Replicas
	baseDeployName := forge.GatewayResourceName(wgClient.Name)

	// Handle status
	defer func() {
		newErr := r.Status().Update(ctx, wgClient)
		if newErr != nil {
			if err != nil {
				klog.Errorf("Error reconciling the WireGuard gateway client %q: %s", req.NamespacedName, err)
			}
			klog.Errorf("Unable to update the WireGuard gateway client status %q: %s", req.NamespacedName, newErr)
			err = newErr
			return
		}

		r.eventRecorder.Event(wgClient, corev1.EventTypeNormal, "Reconciled", "WireGuard gateway client reconciled")
	}()

	if err := r.handleInternalEndpointStatus(ctx, wgClient); err != nil {
		klog.Errorf("Error while handling internal endpoint status: %v", err)
		r.eventRecorder.Event(wgClient, corev1.EventTypeWarning, "InternalEndpointStatusFailed",
			fmt.Sprintf("Failed to handle internal endpoint status: %s", err))
		return ctrl.Result{}, err
	}

	// If a secret has not been provided in the gateway specification, the controller is in charge of generating a secret with the Wireguard keys.
	if wgClient.Spec.SecretRef.Name == "" {
		// Ensure WireGuard keys secret (create or update)
		if err = ensureKeysSecret(ctx, r.Client, wgClient, gateway.ModeClient); err != nil {
			r.eventRecorder.Event(wgClient, corev1.EventTypeWarning, "KeysSecretEnforcedFailed", "Failed to enforce keys secret")
			return ctrl.Result{}, err
		}
		r.eventRecorder.Event(wgClient, corev1.EventTypeNormal, "KeysSecretEnforced", "Enforced keys secret")
	} else {
		// Check that the secret exists and ensure is correctly labeled
		if err = checkExistingKeysSecret(ctx, r.Client, wgClient.Spec.SecretRef.Name, wgClient.Namespace, wgClient.GetObjectMeta()); err != nil {
			r.eventRecorder.Event(wgClient, corev1.EventTypeWarning, "KeysSecretCheckFailed", fmt.Sprintf("Failed to check keys secret: %s", err))
			return ctrl.Result{}, err
		}
		r.eventRecorder.Event(wgClient, corev1.EventTypeNormal, "KeysSecretChecked", "Checked keys secret")
	}

	if err := r.handleSecretRefStatus(ctx, wgClient); err != nil {
		klog.Errorf("Error while handling secret ref status: %v", err)
		r.eventRecorder.Event(wgClient, corev1.EventTypeWarning, "SecretRefStatusFailed",
			fmt.Sprintf("Failed to handle secret ref status: %s", err))
		return ctrl.Result{}, err
	}

	// Ensure deployments (create or update)
	for i := int32(0); i < replicas; i++ {
		depNsName := types.NamespacedName{Namespace: wgClient.Namespace, Name: forge.ReplicaResourceName(wgClient.Name, i)}
		if _, err = r.ensureDeployment(ctx, wgClient, depNsName, i); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Delete obsolete deployments on scale-down.
	if err = deleteObsoleteDeployments(ctx, r.Client, wgClient, baseDeployName, replicas); err != nil {
		klog.Errorf("Error while deleting obsolete deployments: %v", err)
		r.eventRecorder.Event(wgClient, corev1.EventTypeWarning, "ObsoleteDeploymentsDeletionFailed",
			fmt.Sprintf("Failed to delete obsolete deployments: %s", err))
		return ctrl.Result{}, err
	}

	r.eventRecorder.Event(wgClient, corev1.EventTypeNormal, "DeploymentEnforced", "Enforced deployments")

	// Ensure Metrics (if set)
	err = enutils.EnsureMetrics(ctx,
		r.Client, r.Scheme,
		wgClient.Spec.Metrics, wgClient)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(wgClient, corev1.EventTypeNormal, "MetricsEnforced", "Enforced metrics")

	return ctrl.Result{}, nil
}

// SetupWithManager register the WgGatewayClientReconciler to the manager.
func (r *WgGatewayClientReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlWGGatewayClient).
		For(&networkingv1beta1.WgGatewayClient{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.ServiceAccount{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podEnquerer)).
		Watches(&rbacv1.ClusterRoleBinding{},
			handler.EnqueueRequestsFromMapFunc(clusterRoleBindingEnquerer)).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(wireGuardSecretEnquerer),
			builder.WithPredicates(filterWireGuardSecretsPredicate())).
		Complete(r)
}

func (r *WgGatewayClientReconciler) ensureDeployment(ctx context.Context, wgClient *networkingv1beta1.WgGatewayClient,
	depNsName types.NamespacedName, replica int32) (*appsv1.Deployment, error) {
	dep := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      depNsName.Name,
		Namespace: depNsName.Namespace,
	}}

	op, err := resource.CreateOrUpdate(ctx, r.Client, &dep, func() error {
		return r.mutateFnWgClientDeployment(&dep, wgClient, replica)
	})
	if err != nil {
		klog.Errorf("error while creating/updating deployment %q (operation: %s): %v", depNsName, op, err)
		return nil, err
	}

	klog.Infof("Deployment %q correctly enforced (operation: %s)", depNsName, op)
	return &dep, nil
}

func (r *WgGatewayClientReconciler) mutateFnWgClientDeployment(deployment *appsv1.Deployment, wgClient *networkingv1beta1.WgGatewayClient,
	replica int32) error {
	return mutateWgDeployment(deployment, wgClient, r.Scheme, wgClient.Spec.Deployment.Spec, replica, wgClient.Status.SecretRef,
		func(deployment *appsv1.Deployment) error {
			// Override the server endpoint for this replica if multiple endpoints are configured.
			ep := replicaEndpoint(wgClient, replica)
			if ep == nil {
				return fmt.Errorf("no endpoint available for gateway client %q replica %d (endpoints: %d, replicas: %d)",
					wgClient.Name, replica, len(wgClient.Spec.Endpoints), wgClient.Spec.Replicas)
			}
			wireguardContainer := findContainerByName(deployment, "wireguard")
			if wireguardContainer == nil {
				return fmt.Errorf("wireguard container not found in deployment for gateway client %q replica %d", wgClient.Name, replica)
			}
			if len(ep.Addresses) == 0 {
				return fmt.Errorf("endpoint for gateway client %q replica %d has no addresses", wgClient.Name, replica)
			}

			endpointPorts := endpointPortsString(ep)
			if endpointPorts == "" {
				return fmt.Errorf("endpoint for gateway client %q replica %d has no ports", wgClient.Name, replica)
			}

			klog.Infof("Injecting endpoint %s:%s into gateway client %q replica %d",
				ep.Addresses[0], endpointPorts, wgClient.Name, replica)
			wireguardContainer.Args = setOrAppendArg(wireguardContainer.Args, "--endpoint-address", ep.Addresses[0])
			wireguardContainer.Args = setOrAppendArg(wireguardContainer.Args, "--endpoint-ports", endpointPorts)
			return nil
		})
}

func (r *WgGatewayClientReconciler) handleSecretRefStatus(ctx context.Context, wgClient *networkingv1beta1.WgGatewayClient) error {
	secret, err := getWireGuardSecret(ctx, r.Client, wgClient)
	switch {
	case apierrors.IsNotFound(err):
		wgClient.Status.SecretRef = nil
		return fmt.Errorf("WireGuard keys secret not found for gateway client %q", client.ObjectKeyFromObject(wgClient))
	case err != nil:
		return err
	default:
		wgClient.Status.SecretRef = &corev1.ObjectReference{
			Name:      secret.Name,
			Namespace: secret.Namespace,
		}
		return nil
	}
}

func (r *WgGatewayClientReconciler) handleInternalEndpointStatus(ctx context.Context,
	wgClient *networkingv1beta1.WgGatewayClient) error {
	gwPods, err := listGatewayPods(ctx, r.Client, wgClient.Namespace)
	if err != nil {
		return fmt.Errorf("retrieving gateway pods: %w", err)
	}

	wgClient.Status.InternalEndpoints = forgeInternalEndpoints(gwPods, wgClient.Name)
	return nil
}

// replicaEndpoint returns the server endpoint to use for the given client replica.
// The API guarantees that the number of endpoints matches the number of replicas,
// so the replica index maps directly to the endpoint index.
func replicaEndpoint(wgClient *networkingv1beta1.WgGatewayClient, replica int32) *networkingv1beta1.EndpointStatus {
	if int(replica) >= len(wgClient.Spec.Endpoints) {
		return nil
	}
	return &wgClient.Spec.Endpoints[int(replica)]
}

// endpointPortsString returns the comma-separated list of ports to use for the given endpoint.
// It prefers the new Ports list and falls back to the deprecated Port field.
func endpointPortsString(ep *networkingv1beta1.EndpointStatus) string {
	if len(ep.Ports) > 0 {
		parts := make([]string, len(ep.Ports))
		for i, p := range ep.Ports {
			parts[i] = strconv.Itoa(int(p))
		}
		return strings.Join(parts, ",")
	}
	if ep.Port != 0 {
		return strconv.Itoa(int(ep.Port))
	}
	return ""
}
