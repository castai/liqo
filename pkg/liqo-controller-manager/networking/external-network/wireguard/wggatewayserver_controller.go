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
	"github.com/liqotech/liqo/pkg/utils"
	"github.com/liqotech/liqo/pkg/utils/getters"
	mapsutil "github.com/liqotech/liqo/pkg/utils/maps"
	"github.com/liqotech/liqo/pkg/utils/resource"
)

// WgGatewayServerReconciler manage WgGatewayServer lifecycle.
type WgGatewayServerReconciler struct {
	client.Client
	Scheme          *runtime.Scheme
	clusterRoleName string

	eventRecorder record.EventRecorder
}

// NewWgGatewayServerReconciler returns a new WgGatewayServerReconciler.
func NewWgGatewayServerReconciler(cl client.Client, s *runtime.Scheme,
	recorder record.EventRecorder,
	clusterRoleName string) *WgGatewayServerReconciler {
	return &WgGatewayServerReconciler{
		Client:          cl,
		Scheme:          s,
		clusterRoleName: clusterRoleName,

		eventRecorder: recorder,
	}
}

// +kubebuilder:rbac:groups=networking.liqo.io,resources=wggatewayservers,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=wggatewayservers/status,verbs=get;update;patch
// +kubebuilder:rbac:groups=networking.liqo.io,resources=wggatewayservers/finalizers,verbs=update
// +kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=core,resources=pods,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes,verbs=get;list;watch
// +kubebuilder:rbac:groups=core,resources=nodes/finalizers,verbs=update
// +kubebuilder:rbac:groups=core,resources=services,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=core,resources=secrets,verbs=get;list;create;delete;update
// +kubebuilder:rbac:groups=monitoring.coreos.com,resources=servicemonitors,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=rbac.authorization.k8s.io,resources=clusterrolebindings,verbs=get;list;watch;delete;create;update;patch
// +kubebuilder:rbac:groups=core,resources=serviceaccounts,verbs=get;list;watch;delete;create;update;patch

// Reconcile manage WgGatewayServer lifecycle.
func (r *WgGatewayServerReconciler) Reconcile(ctx context.Context, req ctrl.Request) (res ctrl.Result, err error) {
	wgServer := &networkingv1beta1.WgGatewayServer{}
	if err = r.Get(ctx, req.NamespacedName, wgServer); err != nil {
		if apierrors.IsNotFound(err) {
			klog.V(4).Infof("WireGuard gateway server %q not found", req.NamespacedName)
			return ctrl.Result{}, nil
		}
		klog.Errorf("Unable to get the WireGuard gateway server %q: %v", req.NamespacedName, err)
		return ctrl.Result{}, err
	}

	if !wgServer.DeletionTimestamp.IsZero() {
		if controllerutil.ContainsFinalizer(wgServer, consts.ClusterRoleBindingFinalizer) {
			if err = enutils.DeleteClusterRoleBinding(ctx, r.Client, wgServer); err != nil {
				return ctrl.Result{}, err
			}

			controllerutil.RemoveFinalizer(wgServer, consts.ClusterRoleBindingFinalizer)
			if err = r.Update(ctx, wgServer); err != nil {
				klog.Errorf("Unable to remove finalizer %q from WireGuard gateway server %q: %v",
					consts.ClusterRoleBindingFinalizer, req.NamespacedName, err)
				return ctrl.Result{}, err
			}
		}

		// Resource is deleting and child resources are deleted as well by garbage collector. Nothing to do.
		return ctrl.Result{}, nil
	}

	originalWgServer := wgServer.DeepCopy()

	// Ensure ServiceAccount and ClusterRoleBinding (create or update)
	if err = enutils.EnsureServiceAccountAndClusterRoleBinding(ctx, r.Client, r.Scheme, &wgServer.Spec.Deployment, wgServer,
		r.clusterRoleName); err != nil {
		return ctrl.Result{}, err
	}

	// update if the wgServer has been updated
	if !equality.Semantic.DeepEqual(originalWgServer, wgServer) {
		if err := r.Update(ctx, wgServer); err != nil {
			return ctrl.Result{}, err
		}

		// we return here to avoid conflicts
		return ctrl.Result{}, nil
	}

	replicas := wgServer.Spec.Replicas
	baseDeployName := forge.GatewayResourceName(wgServer.Name)
	baseSvcName := forge.GatewayResourceName(wgServer.Name)

	// Handle status
	defer func() {
		newErr := r.Status().Update(ctx, wgServer)
		if newErr != nil {
			if err != nil {
				klog.Errorf("Error reconciling the WireGuard gateway server %q: %s", req.NamespacedName, err)
			}
			klog.Errorf("Unable to update the WireGuard gateway server status %q: %s", req.NamespacedName, newErr)
			err = newErr
			return
		}

		r.eventRecorder.Event(wgServer, corev1.EventTypeNormal, "Reconciled", "WireGuard gateway server reconciled")
	}()

	// If a secret has not been provided in the gateway specification, the controller is in charge of generating a secret with the Wireguard keys.
	if wgServer.Spec.SecretRef.Name == "" {
		// Ensure WireGuard keys secret (create or update)
		if err = ensureKeysSecret(ctx, r.Client, wgServer, gateway.ModeServer); err != nil {
			r.eventRecorder.Event(wgServer, corev1.EventTypeWarning, "KeysSecretEnforcedFailed", "Failed to enforce keys secret")
			return ctrl.Result{}, err
		}
		r.eventRecorder.Event(wgServer, corev1.EventTypeNormal, "KeysSecretEnforced", "Enforced keys secret")
	} else {
		// Check that the secret exists and ensure is correctly labeled
		if err = checkExistingKeysSecret(ctx, r.Client, wgServer.Spec.SecretRef.Name, wgServer.Namespace, wgServer.GetObjectMeta()); err != nil {
			r.eventRecorder.Event(wgServer, corev1.EventTypeWarning, "KeysSecretCheckFailed", fmt.Sprintf("Failed to check keys secret: %s", err))
			return ctrl.Result{}, err
		}
		r.eventRecorder.Event(wgServer, corev1.EventTypeNormal, "KeysSecretChecked", "Checked keys secret")
	}

	if err := r.handleSecretRefStatus(ctx, wgServer); err != nil {
		klog.Errorf("Error while handling secret ref status: %v", err)
		r.eventRecorder.Event(wgServer, corev1.EventTypeWarning, "SecretRefStatusFailed",
			fmt.Sprintf("Failed to handle secret ref status: %s", err))
		return ctrl.Result{}, err
	}

	// Ensure deployments (create or update)
	for i := int32(0); i < replicas; i++ {
		depNsName := types.NamespacedName{Namespace: wgServer.Namespace, Name: forge.ReplicaResourceName(wgServer.Name, i)}
		if _, err = r.ensureDeployment(ctx, wgServer, depNsName, i); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Delete obsolete deployments on scale-down.
	if err = deleteObsoleteDeployments(ctx, r.Client, wgServer, baseDeployName, replicas); err != nil {
		klog.Errorf("Error while deleting obsolete deployments: %v", err)
		r.eventRecorder.Event(wgServer, corev1.EventTypeWarning, "ObsoleteDeploymentsDeletionFailed",
			fmt.Sprintf("Failed to delete obsolete deployments: %s", err))
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(wgServer, corev1.EventTypeNormal, "DeploymentEnforced", "Enforced deployments")

	// Ensure services (create or update)
	for i := int32(0); i < replicas; i++ {
		svcNsName := types.NamespacedName{Namespace: wgServer.Namespace, Name: forge.ReplicaResourceName(wgServer.Name, i)}
		if _, err = r.ensureService(ctx, wgServer, svcNsName, i); err != nil {
			return ctrl.Result{}, err
		}
	}
	// Delete the legacy single-replica service, if still present. Per-replica Services are
	// owned by their Deployment and are garbage-collected when the Deployment is scaled down.
	if err = deleteObsoleteLegacyService(ctx, r.Client, wgServer, baseSvcName); err != nil {
		klog.Errorf("Error while deleting legacy service: %v", err)
		r.eventRecorder.Event(wgServer, corev1.EventTypeWarning, "LegacyServiceDeletionFailed",
			fmt.Sprintf("Failed to delete legacy service: %s", err))
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(wgServer, corev1.EventTypeNormal, "ServiceEnforced", "Enforced services")

	// Handle endpoint status after services have been enforced.
	if err := r.handleEndpointStatus(ctx, wgServer, replicas); err != nil {
		return ctrl.Result{}, err
	}

	// Handle internal endpoint status after deployments/pods have been enforced.
	if err := r.handleInternalEndpointStatus(ctx, wgServer); err != nil {
		klog.Errorf("Error while handling internal endpoint status: %v", err)
		r.eventRecorder.Event(wgServer, corev1.EventTypeWarning, "InternalEndpointStatusFailed",
			fmt.Sprintf("Failed to handle internal endpoint status: %s", err))
		return ctrl.Result{}, err
	}

	// Ensure Metrics (if set)
	err = enutils.EnsureMetrics(ctx,
		r.Client, r.Scheme,
		wgServer.Spec.Metrics, wgServer)
	if err != nil {
		return ctrl.Result{}, err
	}
	r.eventRecorder.Event(wgServer, corev1.EventTypeNormal, "MetricsEnforced", "Enforced metrics")

	return ctrl.Result{}, nil
}

// SetupWithManager register the WgGatewayServerReconciler to the manager.
func (r *WgGatewayServerReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).Named(consts.CtrlWGGatewayServer).
		For(&networkingv1beta1.WgGatewayServer{}).
		Owns(&appsv1.Deployment{}).
		Owns(&corev1.Service{}).
		Owns(&corev1.ServiceAccount{}).
		Watches(&corev1.Pod{}, handler.EnqueueRequestsFromMapFunc(podEnquerer)).
		Watches(&rbacv1.ClusterRoleBinding{},
			handler.EnqueueRequestsFromMapFunc(clusterRoleBindingEnquerer)).
		Watches(&corev1.Secret{},
			handler.EnqueueRequestsFromMapFunc(wireGuardSecretEnquerer),
			builder.WithPredicates(filterWireGuardSecretsPredicate())).
		Complete(r)
}

func (r *WgGatewayServerReconciler) ensureDeployment(ctx context.Context, wgServer *networkingv1beta1.WgGatewayServer,
	depNsName types.NamespacedName, replica int32) (*appsv1.Deployment, error) {
	dep := appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
		Name:      depNsName.Name,
		Namespace: depNsName.Namespace,
	}}

	op, err := resource.CreateOrUpdate(ctx, r.Client, &dep, func() error {
		return r.mutateFnWgServerDeployment(&dep, wgServer, replica)
	})
	if err != nil {
		klog.Errorf("error while creating/updating deployment %q (operation: %s): %v", depNsName, op, err)
		return nil, err
	}

	klog.Infof("Deployment %q correctly enforced (operation: %s)", depNsName, op)
	return &dep, nil
}

func (r *WgGatewayServerReconciler) ensureService(ctx context.Context, wgServer *networkingv1beta1.WgGatewayServer,
	svcNsName types.NamespacedName, replica int32) (*corev1.Service, error) {
	// Retrieve the per-replica Deployment that will own the Service, so that scaling
	// down the replica automatically garbage-collects the Service.
	depNsName := types.NamespacedName{
		Namespace: wgServer.Namespace,
		Name:      forge.ReplicaResourceName(wgServer.Name, replica),
	}
	var deployment appsv1.Deployment
	if err := r.Get(ctx, depNsName, &deployment); err != nil {
		return nil, fmt.Errorf("getting owner deployment %q for service %q: %w", depNsName, svcNsName, err)
	}

	svc := corev1.Service{ObjectMeta: metav1.ObjectMeta{
		Name:      svcNsName.Name,
		Namespace: svcNsName.Namespace,
	}}

	op, err := resource.CreateOrUpdate(ctx, r.Client, &svc, func() error {
		return r.mutateFnWgServerService(&svc, wgServer, &deployment, replica)
	})
	if err != nil {
		klog.Errorf("error while creating/updating service %q (operation: %s): %v", svcNsName, op, err)
		return nil, err
	}

	klog.Infof("Service %q correctly enforced (operation: %s)", svcNsName, op)
	return &svc, nil
}

func (r *WgGatewayServerReconciler) mutateFnWgServerDeployment(deployment *appsv1.Deployment, wgServer *networkingv1beta1.WgGatewayServer,
	replica int32) error {
	return mutateWgDeployment(deployment, wgServer, r.Scheme, wgServer.Spec.Deployment.Spec, replica, wgServer.Status.SecretRef, nil)
}

func (r *WgGatewayServerReconciler) mutateFnWgServerService(service *corev1.Service, wgServer *networkingv1beta1.WgGatewayServer,
	deployment *appsv1.Deployment, replica int32) error {
	// Forge metadata
	mapsutil.SmartMergeLabels(service, wgServer.Spec.Service.Metadata.GetLabels())
	mapsutil.SmartMergeAnnotations(service, wgServer.Spec.Service.Metadata.GetAnnotations())

	// Forge spec
	serviceClassName := service.Spec.LoadBalancerClass
	service.Spec = wgServer.Spec.Service.Spec
	if wgServer.Spec.Service.Spec.LoadBalancerClass == nil {
		service.Spec.LoadBalancerClass = serviceClassName
	}

	// Ensure the service selects only pods of this replica.
	replicaStr := strconv.Itoa(int(replica))
	if service.Spec.Selector == nil {
		service.Spec.Selector = map[string]string{}
	}
	service.Spec.Selector[replicaIDLabel] = replicaStr

	// Ensure the Service is owned by the per-replica Deployment.
	return controllerutil.SetOwnerReference(deployment, service, r.Scheme)
}

func (r *WgGatewayServerReconciler) handleEndpointStatus(ctx context.Context, wgServer *networkingv1beta1.WgGatewayServer, replicas int32) error {
	endpoints := make([]networkingv1beta1.EndpointStatus, 0, replicas)

	for i := int32(0); i < replicas; i++ {
		svcNsName := types.NamespacedName{Namespace: wgServer.Namespace, Name: forge.ReplicaResourceName(wgServer.Name, i)}

		var service corev1.Service
		if err := r.Get(ctx, svcNsName, &service); err != nil {
			klog.Error(err)
			return err
		}

		var endpointStatus *networkingv1beta1.EndpointStatus
		var err error
		switch service.Spec.Type {
		case corev1.ServiceTypeClusterIP:
			endpointStatus, err = r.forgeEndpointStatusClusterIP(&service)
		case corev1.ServiceTypeNodePort:
			endpointStatus, err = r.forgeEndpointStatusNodePort(ctx, &service)
		case corev1.ServiceTypeLoadBalancer:
			endpointStatus, err = r.forgeEndpointStatusLoadBalancer(&service)
		default:
			err = fmt.Errorf("service type %q not supported for WireGuard server Service %q", service.Spec.Type, svcNsName)
			klog.Error(err)
		}

		if err != nil {
			return fmt.Errorf("forging endpoint status for wireguard server %s: %w", client.ObjectKeyFromObject(wgServer), err)
		}

		endpoints = append(endpoints, *endpointStatus)
	}

	wgServer.Status.Endpoints = endpoints
	return nil
}

func (r *WgGatewayServerReconciler) forgeEndpointStatusClusterIP(service *corev1.Service) (*networkingv1beta1.EndpointStatus, error) {
	if len(service.Spec.Ports) == 0 {
		err := fmt.Errorf("service %s/%s has no ports", service.Namespace, service.Name)
		klog.Error(err)
		return nil, err
	}

	port := service.Spec.Ports[0].Port
	protocol := &service.Spec.Ports[0].Protocol
	addresses := service.Spec.ClusterIPs

	return &networkingv1beta1.EndpointStatus{
		Protocol:  protocol,
		Ports:     []int32{port},
		Addresses: addresses,
	}, nil
}

func (r *WgGatewayServerReconciler) forgeEndpointStatusNodePort(ctx context.Context, service *corev1.Service) (*networkingv1beta1.EndpointStatus, error) {
	if len(service.Spec.Ports) == 0 {
		err := fmt.Errorf("service %s/%s has no ports", service.Namespace, service.Name)
		klog.Error(err)
		return nil, err
	}

	port := service.Spec.Ports[0].NodePort
	protocol := &service.Spec.Ports[0].Protocol

	// Select the active pod matching this service's replica selector.
	podsSelector := client.MatchingLabels(service.Spec.Selector)
	var podList corev1.PodList
	if err := r.Client.List(ctx, &podList, client.InNamespace(service.Namespace), podsSelector); err != nil {
		return nil, fmt.Errorf("listing pods for service %s/%s: %w", service.Namespace, service.Name, err)
	}

	var activePod *corev1.Pod
	for i := range podList.Items {
		if podList.Items[i].Status.Phase == corev1.PodRunning && podList.Items[i].DeletionTimestamp == nil {
			activePod = &podList.Items[i]
			break
		}
	}
	if activePod == nil {
		return nil, fmt.Errorf("no gateway pod found for service %s/%s", service.Namespace, service.Name)
	}

	node := &corev1.Node{}
	if err := r.Get(ctx, types.NamespacedName{Name: activePod.Spec.NodeName}, node); err != nil {
		klog.Errorf("Unable to get node %q: %v", activePod.Spec.NodeName, err)
		return nil, fmt.Errorf("getting node %q for gateway pod %q: %w", activePod.Spec.NodeName, activePod.Name, err)
	}

	if !utils.IsNodeReady(node) {
		return nil, fmt.Errorf("node %q for gateway pod %q is not ready", activePod.Spec.NodeName, activePod.Name)
	}

	address, err := utils.GetAddress(node)
	if err != nil {
		klog.Errorf("Unable to get address of node %q: %v", activePod.Spec.NodeName, err)
		return nil, err
	}

	return &networkingv1beta1.EndpointStatus{
		Protocol:  protocol,
		Ports:     []int32{port},
		Addresses: []string{address},
	}, nil
}

func (r *WgGatewayServerReconciler) forgeEndpointStatusLoadBalancer(service *corev1.Service) (*networkingv1beta1.EndpointStatus, error) {
	if len(service.Spec.Ports) == 0 {
		err := fmt.Errorf("service %s/%s has no ports", service.Namespace, service.Name)
		klog.Error(err)
		return nil, err
	}

	port := service.Spec.Ports[0].Port
	protocol := &service.Spec.Ports[0].Protocol

	addresses := getters.CollectLoadBalancerAddresses(service.Status.LoadBalancer.Ingress)

	return &networkingv1beta1.EndpointStatus{
		Protocol:  protocol,
		Ports:     []int32{port},
		Addresses: addresses,
	}, nil
}

func (r *WgGatewayServerReconciler) handleSecretRefStatus(ctx context.Context, wgServer *networkingv1beta1.WgGatewayServer) error {
	secret, err := getWireGuardSecret(ctx, r.Client, wgServer)
	switch {
	case apierrors.IsNotFound(err):
		wgServer.Status.SecretRef = nil
		return fmt.Errorf("WireGuard keys secret not found for gateway server %q", client.ObjectKeyFromObject(wgServer))
	case err != nil:
		return err
	default:
		wgServer.Status.SecretRef = &corev1.ObjectReference{
			Name:      secret.Name,
			Namespace: secret.Namespace,
		}
		return nil
	}
}

func (r *WgGatewayServerReconciler) handleInternalEndpointStatus(ctx context.Context,
	wgServer *networkingv1beta1.WgGatewayServer) error {
	gwPods, err := listGatewayPods(ctx, r.Client, wgServer.Namespace)
	if err != nil {
		return fmt.Errorf("retrieving gateway pods: %w", err)
	}

	wgServer.Status.InternalEndpoints = forgeInternalEndpoints(gwPods)
	return nil
}
