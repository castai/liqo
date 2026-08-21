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

	"golang.zx2c4.com/wireguard/wgctrl/wgtypes"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	kerrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/gateway"
	"github.com/liqotech/liqo/pkg/gateway/forge"
	"github.com/liqotech/liqo/pkg/gateway/tunnel/wireguard"
	liqolabels "github.com/liqotech/liqo/pkg/utils/labels"
	mapsutil "github.com/liqotech/liqo/pkg/utils/maps"
)

const (
	wireguardVolumeName = "wireguard-config"
)

func filterWireGuardSecretsPredicate() predicate.Predicate {
	filterGatewayResources, err := predicate.LabelSelectorPredicate(liqolabels.GatewayResourceLabelSelector)
	utilruntime.Must(err)

	filterResourcesForRemote, err := predicate.LabelSelectorPredicate(liqolabels.ResourceForRemoteClusterLabelSelector)
	utilruntime.Must(err)

	return predicate.And(filterGatewayResources, filterResourcesForRemote)
}

func wireGuardSecretEnquerer(_ context.Context, obj client.Object) []ctrl.Request {
	secret, ok := obj.(*corev1.Secret)
	if !ok {
		return nil
	}

	return []ctrl.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: secret.Namespace,
				Name:      forge.GatewayResourceName(secret.Name),
			},
		},
	}
}

func clusterRoleBindingEnquerer(_ context.Context, obj client.Object) []ctrl.Request {
	crb, ok := obj.(*rbacv1.ClusterRoleBinding)
	if !ok {
		return nil
	}

	if crb.Labels == nil {
		return nil
	}
	gwName, ok := crb.Labels[consts.GatewayNameLabel]
	if !ok {
		return nil
	}
	gwNs, ok := crb.Labels[consts.GatewayNamespaceLabel]
	if !ok {
		return nil
	}

	return []ctrl.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: gwNs,
				Name:      gwName,
			},
		},
	}
}

func podEnquerer(_ context.Context, obj client.Object) []ctrl.Request {
	pod, ok := obj.(*corev1.Pod)
	if !ok {
		return nil
	}

	if pod.Labels == nil {
		return nil
	}
	gwName, ok := pod.Labels[consts.GatewayNameLabel]
	if !ok {
		return nil
	}
	gwNs, ok := pod.Labels[consts.GatewayNamespaceLabel]
	if !ok {
		return nil
	}

	return []ctrl.Request{
		{
			NamespacedName: types.NamespacedName{
				Namespace: gwNs,
				Name:      gwName,
			},
		},
	}
}

// ensureKeysSecret ensure the presence of the private and public keys for the Wireguard interface and save them inside a Secret resource and Options.
func ensureKeysSecret(ctx context.Context, cl client.Client, wgObj metav1.Object, mode gateway.Mode) error {
	var controllerRef metav1.OwnerReference
	for _, ref := range wgObj.GetOwnerReferences() {
		if ref.Controller != nil && *ref.Controller {
			switch ref.Kind {
			case networkingv1beta1.GatewayClientKind:
				controllerRef = ref
			case networkingv1beta1.GatewayServerKind:
				controllerRef = ref
			}
			break
		}
	}

	opts := &gateway.Options{
		Name:            controllerRef.Name,
		Namespace:       wgObj.GetNamespace(),
		RemoteClusterID: wgObj.GetLabels()[consts.RemoteClusterID],
		GatewayUID:      string(controllerRef.UID),
		Mode:            mode,
	}

	_, err := getWireGuardSecret(ctx, cl, wgObj)
	switch {
	case kerrors.IsNotFound(err):
		pri, err := wgtypes.GeneratePrivateKey()
		if err != nil {
			klog.Error(err)
			return err
		}
		pub := pri.PublicKey()
		if err := wireguard.CreateKeysSecret(ctx, cl, opts, pri, pub); err != nil {
			klog.Error(err)
			return err
		}
		klog.Infof("Keys secret for WireGuard gateway %q correctly enforced", wgObj.GetName())
		return nil
	case err != nil:
		klog.Error(err)
		return err
	default:
		return nil
	}
}

func checkExistingKeysSecret(ctx context.Context, cl client.Client, secretName, namespace string, wgObj metav1.Object) error {
	var s corev1.Secret
	if err := cl.Get(ctx, types.NamespacedName{Name: secretName, Namespace: namespace}, &s); err != nil {
		return err
	}

	// Check needed data fields are present
	if s.Data == nil {
		return fmt.Errorf("mandatory data %q and %q are missing in secret %q", consts.PrivateKeyField, consts.PublicKeyField, secretName)
	}
	if _, ok := s.Data[consts.PrivateKeyField]; !ok {
		return fmt.Errorf("missing %q data in secret %q", consts.PrivateKeyField, secretName)
	}
	if _, ok := s.Data[consts.PublicKeyField]; !ok {
		return fmt.Errorf("missing %q data in secret %q", consts.PublicKeyField, secretName)
	}

	// Check remote cluster ID label match the parent wireguard object
	remoteClusterID, exists := wgObj.GetLabels()[consts.RemoteClusterID]
	if !exists || remoteClusterID == "" {
		return fmt.Errorf("missing %q label in WireGuard gateway %q", consts.RemoteClusterID, wgObj.GetName())
	}
	if s.Labels != nil {
		if v, ok := s.Labels[consts.RemoteClusterID]; ok && v != remoteClusterID {
			return fmt.Errorf("label %q in secret %q does not match the one in WireGuard gateway %q", consts.RemoteClusterID, secretName, wgObj.GetName())
		}
	}

	// Enforce correct labels on the secret if not present
	if s.Labels == nil || s.Labels[consts.RemoteClusterID] == "" || s.Labels[consts.GatewayResourceLabel] != consts.GatewayResourceLabelValue {
		s.SetLabels(labels.Merge(s.GetLabels(), map[string]string{
			consts.RemoteClusterID:      remoteClusterID,
			consts.GatewayResourceLabel: consts.GatewayResourceLabelValue,
		}))
		if err := cl.Update(ctx, &s); err != nil {
			return fmt.Errorf("unable to update labels in secret %q: %w", secretName, err)
		}
		klog.Infof("Enforced correct gateway labels in secret %q", secretName)
	}

	return nil
}

func getWireGuardSecret(ctx context.Context, cl client.Client, wgObj metav1.Object) (*corev1.Secret, error) {
	wgObjNsName := types.NamespacedName{Name: wgObj.GetName(), Namespace: wgObj.GetNamespace()}

	remoteClusterID, exists := wgObj.GetLabels()[consts.RemoteClusterID]
	if !exists {
		err := fmt.Errorf("missing %q label in WireGuard gateway %q", consts.RemoteClusterID, wgObjNsName)
		klog.Error(err)
		return nil, err
	}
	wgSecretSelector := client.MatchingLabels{
		consts.GatewayResourceLabel: consts.GatewayResourceLabelValue,
		consts.RemoteClusterID:      remoteClusterID,
	}

	var secrets corev1.SecretList
	err := cl.List(ctx, &secrets, client.InNamespace(wgObj.GetNamespace()), wgSecretSelector)
	if err != nil {
		klog.Errorf("Unable to list secrets associated to WireGuard gateway %q: %v", wgObjNsName, err)
		return nil, err
	}

	switch len(secrets.Items) {
	case 0:
		err = kerrors.NewNotFound(corev1.Resource("Secret"), wgObjNsName.Name)
		return nil, err
	case 1:
		return &secrets.Items[0], nil
	default:
		return nil, fmt.Errorf("found multiple secrets associated to WireGuard gateway %q", wgObjNsName)
	}
}

// listGatewayPods returns all running gateway pods across all replicas of the given gateway.
func listGatewayPods(ctx context.Context, cl client.Client, namespace string) ([]corev1.Pod, error) {
	podsSelector := client.MatchingLabelsSelector{Selector: labels.SelectorFromSet(gateway.ForgeGatewayPodLabels())}
	var podList corev1.PodList
	if err := cl.List(ctx, &podList, client.InNamespace(namespace), podsSelector); err != nil {
		klog.Errorf("Unable to list gateway pods in namespace %q: %v", namespace, err)
		return nil, fmt.Errorf("listing gateway pods: %w", err)
	}

	pods := make([]corev1.Pod, 0, len(podList.Items))
	for i := range podList.Items {
		if podList.Items[i].Status.Phase == corev1.PodRunning && podList.Items[i].DeletionTimestamp == nil &&
			podList.Items[i].Status.PodIP != "" {
			pods = append(pods, podList.Items[i])
		}
	}

	return pods, nil
}

// injectReplicaID injects the replica identifier into the deployment: label, selector, and container args.
func injectReplicaID(deployment *appsv1.Deployment, replica int32) {
	replicaStr := strconv.Itoa(int(replica))

	if deployment.Spec.Template.Labels == nil {
		deployment.Spec.Template.Labels = map[string]string{}
	}
	deployment.Spec.Template.Labels[consts.GatewayReplicaID] = replicaStr

	if deployment.Spec.Selector == nil {
		deployment.Spec.Selector = &metav1.LabelSelector{}
	}
	if deployment.Spec.Selector.MatchLabels == nil {
		deployment.Spec.Selector.MatchLabels = map[string]string{}
	}
	deployment.Spec.Selector.MatchLabels[consts.GatewayReplicaID] = replicaStr

	deployment.Spec.Replicas = ptr.To(int32(1))

	for i := range deployment.Spec.Template.Spec.Containers {
		container := &deployment.Spec.Template.Spec.Containers[i]
		container.Args = setOrAppendArg(container.Args, "--replica-id", replicaStr)
	}
}

// setWireGuardSecretVolume replaces the wireguard-config volume secret name in the deployment.
func setWireGuardSecretVolume(deployment *appsv1.Deployment, secretName string) {
	for i := range deployment.Spec.Template.Spec.Volumes {
		if deployment.Spec.Template.Spec.Volumes[i].Name == wireguardVolumeName {
			deployment.Spec.Template.Spec.Volumes[i].Secret = &corev1.SecretVolumeSource{
				SecretName: secretName,
			}
			return
		}
	}
}

// wgDeploymentMutator applies common per-replica mutations to a WireGuard gateway deployment.
type wgDeploymentMutator func(*appsv1.Deployment) error

// mutateWgDeployment forges the common fields of a WireGuard gateway deployment:
// metadata, spec, replica ID, secret volume and owner reference. The caller can
// apply additional type-specific mutations through the mutator callback.
func mutateWgDeployment(deployment *appsv1.Deployment, owner metav1.Object, scheme *runtime.Scheme,
	deploymentSpec appsv1.DeploymentSpec, replica int32, secretRef *corev1.ObjectReference,
	mutator wgDeploymentMutator) error {
	mapsutil.SmartMergeLabels(deployment, deploymentSpec.Template.ObjectMeta.GetLabels())
	mapsutil.SmartMergeAnnotations(deployment, deploymentSpec.Template.ObjectMeta.GetAnnotations())

	deployment.Spec = deploymentSpec
	// Re-apply Kubernetes defaults so that fields added by the API server (e.g.
	// RevisionHistoryLimit, Strategy) are preserved. Without this, the next
	// CreateOrUpdate comparison would always detect a diff and update the
	// Deployment in a loop.
	scheme.Default(deployment)

	injectReplicaID(deployment, replica)

	if secretRef != nil {
		setWireGuardSecretVolume(deployment, secretRef.Name)
	}

	if mutator != nil {
		if err := mutator(deployment); err != nil {
			return err
		}
	}

	return controllerutil.SetControllerReference(owner, deployment, scheme)
}

// findContainerByName returns the container with the given name in the deployment pod spec.
func findContainerByName(deployment *appsv1.Deployment, name string) *corev1.Container {
	for i := range deployment.Spec.Template.Spec.Containers {
		if deployment.Spec.Template.Spec.Containers[i].Name == name {
			return &deployment.Spec.Template.Spec.Containers[i]
		}
	}
	return nil
}

// setOrAppendArg sets or appends a key=value argument in the container args slice.
func setOrAppendArg(args []string, key, value string) []string {
	prefix := key + "="
	for i, arg := range args {
		if strings.HasPrefix(arg, prefix) {
			args[i] = prefix + value
			return args
		}
	}
	return append(args, prefix+value)
}

// deleteObsoleteDeployments removes deployments with an index greater than or equal to replicas,
// and also deletes legacy deployments created before the gw- prefix was introduced.
func deleteObsoleteDeployments(ctx context.Context, cl client.Client, owner metav1.Object, baseName string, replicas int32) error {
	var deployments appsv1.DeploymentList
	if err := cl.List(ctx, &deployments, client.InNamespace(owner.GetNamespace())); err != nil {
		return fmt.Errorf("listing deployments: %w", err)
	}

	for i := range deployments.Items {
		dep := &deployments.Items[i]
		if !metav1.IsControlledBy(dep, owner) {
			continue
		}

		// Delete legacy deployments created before per-replica naming was introduced.
		// Old single-replica deployments were named gw-<owner.Name> (i.e. baseName),
		// while new per-replica deployments are named gw-<owner.Name>-<idx>.
		if dep.Name == baseName {
			if err := cl.Delete(ctx, dep); err != nil && !kerrors.IsNotFound(err) {
				return fmt.Errorf("deleting legacy deployment %q: %w", dep.Name, err)
			}
			klog.Infof("Deleted legacy deployment %q", dep.Name)
			continue
		}

		idx, ok := parseReplicaIndex(dep.Name, baseName)
		if !ok {
			continue
		}
		if idx >= replicas {
			if err := cl.Delete(ctx, dep); err != nil && !kerrors.IsNotFound(err) {
				return fmt.Errorf("deleting deployment %q: %w", dep.Name, err)
			}
			klog.Infof("Deleted obsolete deployment %q", dep.Name)
		}
	}

	return nil
}

// parseReplicaIndex extracts the replica index from a per-replica resource name.
// It returns (0, false) if the name does not match the expected pattern.
func parseReplicaIndex(name, baseName string) (int32, bool) {
	prefix := baseName + "-"
	if !strings.HasPrefix(name, prefix) {
		return 0, false
	}
	idx, err := strconv.Atoi(strings.TrimPrefix(name, prefix))
	if err != nil || idx < 0 {
		return 0, false
	}
	return int32(idx), true
}

// deleteObsoleteLegacyService deletes the legacy single-replica Service named baseName,
// which was created before per-replica naming was introduced and is not garbage-collected
// by per-replica Deployment deletion (new per-replica Services are owned by their Deployment).
func deleteObsoleteLegacyService(ctx context.Context, cl client.Client, owner metav1.Object, baseName string) error {
	svc := corev1.Service{ObjectMeta: metav1.ObjectMeta{Name: baseName, Namespace: owner.GetNamespace()}}
	if err := cl.Get(ctx, client.ObjectKeyFromObject(&svc), &svc); err != nil {
		if kerrors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("getting legacy service %q: %w", baseName, err)
	}
	if !metav1.IsControlledBy(&svc, owner) {
		return nil
	}
	if err := cl.Delete(ctx, &svc); err != nil && !kerrors.IsNotFound(err) {
		return fmt.Errorf("deleting legacy service %q: %w", baseName, err)
	}
	return nil
}

// forgeInternalEndpoints builds a list of internal endpoints from gateway pods.
func forgeInternalEndpoints(pods []corev1.Pod, gatewayName string) []networkingv1beta1.InternalGatewayEndpoint {
	endpoints := make([]networkingv1beta1.InternalGatewayEndpoint, 0, len(pods))
	baseName := forge.GatewayResourceName(gatewayName)
	for i := range pods {
		replicaID := replicaIDFromPod(&pods[i], baseName)
		endpoints = append(endpoints, networkingv1beta1.InternalGatewayEndpoint{
			IP:        ptr.To(networkingv1beta1.IP(pods[i].Status.PodIP)),
			Node:      &pods[i].Spec.NodeName,
			ReplicaID: replicaID,
		})
	}
	return endpoints
}

// replicaIDFromPod returns the replica ID of a gateway pod, using the replica-id label
// if present, or falling back to parsing the pod name based on the per-replica deployment name.
func replicaIDFromPod(pod *corev1.Pod, baseName string) int32 {
	if idStr, ok := pod.Labels[consts.GatewayReplicaID]; ok {
		if id, err := strconv.ParseInt(idStr, 10, 32); err == nil {
			klog.Infof("Pod %s replica label %q -> %d", pod.Name, idStr, id)
			return int32(id)
		}
		klog.Warningf("Pod %s has invalid replica label %q", pod.Name, idStr)
	}

	prefix := baseName + "-"
	if strings.HasPrefix(pod.Name, prefix) {
		suffix := pod.Name[len(prefix):]
		parts := strings.SplitN(suffix, "-", 2)
		if id, err := strconv.ParseInt(parts[0], 10, 32); err == nil {
			klog.Infof("Pod %s parsed replica ID from name -> %d", pod.Name, id)
			return int32(id)
		}
	}

	klog.Warningf("Unable to determine replica ID for pod %s (baseName %q), defaulting to 0", pod.Name, baseName)
	return 0
}
