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
	PIDResolver    PodPIDResolver
}

// PodPIDResolver resolves the sandbox (pause) container PID for a pod.
type PodPIDResolver interface {
	PodPID(ctx context.Context, pod *corev1.Pod) (int, error)
}

// +kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch

// Reconcile implements sigs.k8s.io/controller-runtime.Reconciler.
// SetupWithManager registers the reconciler with the controller-runtime manager.
func (r *PodReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&corev1.Pod{}).
		Complete(r)
}

func (r *PodReconciler) resolvePodPID(ctx context.Context, pod *corev1.Pod) (int, error) {
	if r.PIDResolver != nil {
		return r.PIDResolver.PodPID(ctx, pod)
	}
	return 0, nil
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
	if pod.Spec.HostNetwork {
		return ctrl.Result{}, nil
	}
	// Gateway pods run their own return-path setup (tc_gw_return.o on
	 // liqo-tunnel). Injecting the pod forward-path program here would
	 // conflict with that setup, so skip them.
	 if pod.Labels != nil && pod.Labels["networking.liqo.io/component"] == "gateway" {
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

	pid, err := r.resolvePodPID(ctx, &pod)
	if err != nil {
		return ctrl.Result{}, fmt.Errorf("resolving pid for pod %s/%s: %w", pod.Namespace, pod.Name, err)
	}
	if pid == 0 {
		// The pause container may not be running yet; requeue.
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


