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

package networkpolicy

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/equality"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/klog/v2"

	offloadingv1beta1 "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/virtualKubelet/forge"
)

const transportFieldManager = "liqo-networkpolicy-reflector"

var (
	shadowNetworkPolicyGVR = schema.GroupVersionResource{
		Group:    offloadingv1beta1.SchemeGroupVersion.Group,
		Version:  offloadingv1beta1.SchemeGroupVersion.Version,
		Resource: offloadingv1beta1.ShadowNetworkPolicyResource,
	}
	shadowNetworkPolicySetGVR = schema.GroupVersionResource{
		Group:    offloadingv1beta1.SchemeGroupVersion.Group,
		Version:  offloadingv1beta1.SchemeGroupVersion.Version,
		Resource: offloadingv1beta1.ShadowNetworkPolicySetResource,
	}
)

// applyShadow creates or updates the remote ShadowNetworkPolicy, refusing to adopt an object
// produced by a different source policy identity.
func (r *NetworkPolicyReflector) applyShadow(ctx context.Context, remoteNamespace string,
	shadow *offloadingv1beta1.ShadowNetworkPolicy) error {
	resource := r.remoteDynamic.Resource(shadowNetworkPolicyGVR).Namespace(remoteNamespace)

	existing, err := resource.Get(ctx, shadow.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		var current offloadingv1beta1.ShadowNetworkPolicy
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existing.Object, &current); err != nil {
			return fmt.Errorf("decoding the remote ShadowNetworkPolicy %q: %w", shadow.Name, err)
		}
		if current.Spec.Source.UID != shadow.Spec.Source.UID {
			return fmt.Errorf("the remote ShadowNetworkPolicy %q belongs to source UID %q, not %q",
				shadow.Name, current.Spec.Source.UID, shadow.Spec.Source.UID)
		}
		if equalityOfShadow(&current, shadow) {
			return nil
		}
		desired, err := shadowToUnstructured(shadow)
		if err != nil {
			return err
		}
		desired.SetResourceVersion(existing.GetResourceVersion())
		desired.SetUID(existing.GetUID())
		if _, err := resource.Update(ctx, desired, metav1.UpdateOptions{FieldManager: transportFieldManager}); err != nil {
			return fmt.Errorf("updating the remote ShadowNetworkPolicy %q: %w", shadow.Name, err)
		}
		return nil
	case errors.IsNotFound(err):
		desired, err := shadowToUnstructured(shadow)
		if err != nil {
			return err
		}
		if _, err := resource.Create(ctx, desired, metav1.CreateOptions{FieldManager: transportFieldManager}); err != nil {
			return fmt.Errorf("creating the remote ShadowNetworkPolicy %q: %w", shadow.Name, err)
		}
		return nil
	default:
		return fmt.Errorf("reading the remote ShadowNetworkPolicy %q: %w", shadow.Name, err)
	}
}

func (r *NetworkPolicyReflector) deleteShadow(ctx context.Context, remoteNamespace, name string) error {
	resource := r.remoteDynamic.Resource(shadowNetworkPolicyGVR).Namespace(remoteNamespace)
	existing, err := resource.Get(ctx, name, metav1.GetOptions{})
	if err != nil {
		if errors.IsNotFound(err) {
			return nil
		}
		return fmt.Errorf("reading the remote ShadowNetworkPolicy %q before deletion: %w", name, err)
	}
	if existing.GetLabels()[consts.ManagedByLabelKey] != consts.ManagedByShadowNetworkPolicyValue {
		klog.Warningf("Skipping deletion of ShadowNetworkPolicy %q as it is not managed by us", name)
		return nil
	}
	uid := existing.GetUID()
	if err := resource.Delete(ctx, name, metav1.DeleteOptions{
		Preconditions: &metav1.Preconditions{UID: &uid},
	}); err != nil && !errors.IsNotFound(err) && !errors.IsConflict(err) {
		return fmt.Errorf("deleting the remote ShadowNetworkPolicy %q: %w", name, err)
	}
	return nil
}

func (r *NetworkPolicyReflector) listShadows(ctx context.Context, remoteNamespace string) ([]offloadingv1beta1.ShadowNetworkPolicy, error) {
	list, err := r.remoteDynamic.Resource(shadowNetworkPolicyGVR).Namespace(remoteNamespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		return nil, err
	}
	out := make([]offloadingv1beta1.ShadowNetworkPolicy, 0, len(list.Items))
	for i := range list.Items {
		var shadow offloadingv1beta1.ShadowNetworkPolicy
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(list.Items[i].Object, &shadow); err != nil {
			return nil, err
		}
		out = append(out, shadow)
	}
	return out, nil
}

// writeInventory stages, then commits, the ShadowNetworkPolicySet inventory.
func (r *NetworkPolicyReflector) writeInventory(ctx context.Context, remoteNamespace string,
	set *offloadingv1beta1.ShadowNetworkPolicySet, phase offloadingv1beta1.ShadowNetworkPolicySetPhase) error {
	resource := r.remoteDynamic.Resource(shadowNetworkPolicySetGVR).Namespace(remoteNamespace)

	desired := set.DeepCopy()
	desired.Spec.Phase = phase
	desired.Labels = inventoryLabels(set)

	existing, err := resource.Get(ctx, desired.Name, metav1.GetOptions{})
	switch {
	case err == nil:
		var current offloadingv1beta1.ShadowNetworkPolicySet
		if err := runtime.DefaultUnstructuredConverter.FromUnstructured(existing.Object, &current); err != nil {
			return err
		}
		if current.Spec.SourceClusterID != desired.Spec.SourceClusterID ||
			current.Spec.SourceNamespaceUID != desired.Spec.SourceNamespaceUID {
			return fmt.Errorf("the remote ShadowNetworkPolicySet %q belongs to a different source namespace", desired.Name)
		}
		if current.Spec.Phase == phase && equalityOfSetSpec(&current.Spec, &desired.Spec) {
			return nil
		}
		object, err := setToUnstructured(desired)
		if err != nil {
			return err
		}
		object.SetResourceVersion(existing.GetResourceVersion())
		object.SetUID(existing.GetUID())
		_, err = resource.Update(ctx, object, metav1.UpdateOptions{FieldManager: transportFieldManager})
		return err
	case errors.IsNotFound(err):
		object, err := setToUnstructured(desired)
		if err != nil {
			return err
		}
		_, err = resource.Create(ctx, object, metav1.CreateOptions{FieldManager: transportFieldManager})
		return err
	default:
		return fmt.Errorf("reading the remote ShadowNetworkPolicySet %q: %w", desired.Name, err)
	}
}

func shadowToUnstructured(shadow *offloadingv1beta1.ShadowNetworkPolicy) (*unstructured.Unstructured, error) {
	copy := shadow.DeepCopy()
	copy.TypeMeta = metav1.TypeMeta{
		APIVersion: shadowNetworkPolicyGVR.GroupVersion().String(),
		Kind:       "ShadowNetworkPolicy",
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(copy)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: object}, nil
}

func setToUnstructured(set *offloadingv1beta1.ShadowNetworkPolicySet) (*unstructured.Unstructured, error) {
	copy := set.DeepCopy()
	copy.TypeMeta = metav1.TypeMeta{
		APIVersion: shadowNetworkPolicySetGVR.GroupVersion().String(),
		Kind:       "ShadowNetworkPolicySet",
	}
	object, err := runtime.DefaultUnstructuredConverter.ToUnstructured(copy)
	if err != nil {
		return nil, err
	}
	return &unstructured.Unstructured{Object: object}, nil
}

func equalityOfShadow(a, b *offloadingv1beta1.ShadowNetworkPolicy) bool {
	return equality.Semantic.DeepEqual(a.Spec, b.Spec)
}

func equalityOfSetSpec(a, b *offloadingv1beta1.ShadowNetworkPolicySetSpec) bool {
	return equality.Semantic.DeepEqual(a, b)
}

// shadowLabels returns the reserved metadata assigned to a reflected shadow policy.
func (r *NetworkPolicyReflector) shadowLabels(sourceNamespace string, namespaceUID types.UID) map[string]string {
	out := map[string]string{
		consts.ManagedByLabelKey:           consts.ManagedByShadowNetworkPolicyValue,
		forge.LiqoOriginClusterIDKey:       string(r.localClusterID),
		forge.LiqoDestinationClusterIDKey:  string(r.remoteClusterID),
		consts.LiqoOriginNamespaceLabelKey: sourceNamespace,
	}
	if namespaceUID != "" {
		out[consts.LiqoOriginNamespaceUIDLabelKey] = string(namespaceUID)
	}
	return out
}

func inventoryLabels(set *offloadingv1beta1.ShadowNetworkPolicySet) map[string]string {
	out := map[string]string{
		consts.ManagedByLabelKey:           consts.ManagedByShadowNetworkPolicyValue,
		forge.LiqoOriginClusterIDKey:       string(set.Spec.SourceClusterID),
		consts.LiqoOriginNamespaceLabelKey: set.Spec.SourceNamespace,
	}
	if set.Spec.SourceNamespaceUID != "" {
		out[consts.LiqoOriginNamespaceUIDLabelKey] = string(set.Spec.SourceNamespaceUID)
	}
	return out
}
