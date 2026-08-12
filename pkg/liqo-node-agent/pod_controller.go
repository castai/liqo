// SPDX-License-Identifier: Apache-2.0
// Copyright 2019-2026 The Liqo Authors

package nodeagent

import (
	"context"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/runtime"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

// PodReconciler watches pods scheduled on the local node and injects the
// eBPF-based overlay datapath into their network namespace.
type PodReconciler struct {
	client.Client
	Scheme         *runtime.Scheme
	NodeName       string
	RouteMapPath   string
	PodEncapObject string
	PodTunnelName  string
	GenevePort     uint16
	Injector       Injector
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile implements sigs.k8s.io/controller-runtime.Reconciler.
// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}

func (r *PodReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	var pod corev1.Pod
	if err := r.Get(ctx, req.NamespacedName, &pod); err != nil {
		if client.IgnoreNotFound(err) != nil {
			return ctrl.Result{}, err
		}
		return ctrl.Result{}, nil
	}

	if pod.Spec.NodeName != r.NodeName {
		return ctrl.Result{}, nil
	}
	if pod.Status.Phase != corev1.PodRunning {
		return ctrl.Result{}, nil
	}
	if pod.Status.PodIP == "" {
		return ctrl.Result{}, nil
	}

	// Avoid repeated injection using an annotation.
	if pod.Annotations != nil && pod.Annotations["ebpf-overlay.liqo.io/injected"] == "true" {
		return ctrl.Result{}, nil
	}

	pid := podPID(&pod)
	if pid == 0 {
		// Kubernetes does not expose sandbox PIDs in the Pod API; a production
		// implementation resolves the pause container PID via the container
		// runtime (CRI).  For the PoC we requeue until a PID is available.
		return ctrl.Result{Requeue: true}, nil
	}

	if err := r.Injector.Inject(ctx, InjectRequest{
		PodNamespace:   pod.Namespace,
		PodName:        pod.Name,
		PodIP:          pod.Status.PodIP,
		PID:            pid,
		RouteMapPath:   r.RouteMapPath,
		PodEncapObject: r.PodEncapObject,
		TunnelName:     r.PodTunnelName,
		GenevePort:     r.GenevePort,
	}); err != nil {
		return ctrl.Result{}, fmt.Errorf("injecting pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	if pod.Annotations == nil {
		pod.Annotations = map[string]string{}
	}
	pod.Annotations["ebpf-overlay.liqo.io/injected"] = "true"
	if err := r.Update(ctx, &pod); err != nil {
		return ctrl.Result{}, fmt.Errorf("annotating injected pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}

	return ctrl.Result{}, nil
}

// podPID resolves the sandbox (pause) container PID for the pod.
// Kubernetes does not expose this directly, so the PoC uses a placeholder that
// must be replaced with a CRI lookup in a production implementation.
func podPID(pod *corev1.Pod) int {
	if pod.Status.ContainerStatuses == nil {
		return 0
	}
	for _, cs := range pod.Status.ContainerStatuses {
		if cs.ContainerID != "" && cs.State.Running != nil {
			_ = cs
			return 0
		}
	}
	return 0
}
