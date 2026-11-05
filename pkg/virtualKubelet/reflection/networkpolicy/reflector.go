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

// Package networkpolicy exports consumer NetworkPolicies to a provider cluster as
// ShadowNetworkPolicy resources, together with a ShadowNetworkPolicySet inventory used as the
// ShadowPod materialization barrier.
package networkpolicy

import (
	"context"
	"fmt"
	"sync"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/wait"
	"k8s.io/apimachinery/pkg/watch"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/dynamic/dynamicinformer"
	networkingv1informers "k8s.io/client-go/informers/networking/v1"
	"k8s.io/client-go/tools/cache"
	"k8s.io/client-go/util/workqueue"
	"k8s.io/klog/v2"

	liqov1beta1 "github.com/liqotech/liqo/apis/core/v1beta1"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/manager"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/options"
)

const (
	// ReflectorName is the name associated with the NetworkPolicy reflector.
	ReflectorName = "NetworkPolicy"
)

var (
	configurationGVR = schema.GroupVersionResource{Group: "networking.liqo.io", Version: "v1beta1", Resource: "configurations"}
	ipGVR            = schema.GroupVersionResource{Group: "ipam.liqo.io", Version: "v1alpha1", Resource: "ips"}
	networkGVR       = schema.GroupVersionResource{Group: "ipam.liqo.io", Version: "v1alpha1", Resource: "networks"}
)

var _ manager.Reflector = (*NetworkPolicyReflector)(nil)

type namespacedState struct {
	localNamespace  string
	remoteNamespace string
	namespaceUID    types.UID
	// mu serializes the inventory writes for this namespace mapping.
	mu sync.Mutex

	policyInformer networkingv1informers.NetworkPolicyInformer
	shadowInformer cache.SharedIndexInformer
	setInformer    cache.SharedIndexInformer
	dynamicFactory dynamicinformer.DynamicSharedInformerFactory
	policyFactory  interface{ Shutdown() }
}

// NetworkPolicyReflector exports the NetworkPolicies of each mapped namespace to the provider.
type NetworkPolicyReflector struct {
	sync.Mutex

	localDynamic  dynamic.Interface
	remoteDynamic dynamic.Interface
	resync        time.Duration
	liqoNamespace string

	localClusterID  liqov1beta1.ClusterID
	remoteClusterID liqov1beta1.ClusterID
	localPodCIDRs   []string
	sourceProfile   string
	workers         uint

	ctx context.Context
	// Cluster-scoped caches shared by every mapped namespace.
	configFactory   dynamicinformer.DynamicSharedInformerFactory
	ipFactory       dynamicinformer.DynamicSharedInformerFactory
	networkFactory  dynamicinformer.DynamicSharedInformerFactory
	configInformer  cache.SharedIndexInformer
	ipInformer      cache.SharedIndexInformer
	networkInformer cache.SharedIndexInformer

	queue      workqueue.RateLimitingInterface
	namespaces map[string]*namespacedState
	started    bool
}

// NewNetworkPolicyReflector builds a NetworkPolicy reflector using dynamic clients for Liqo
// resources and a typed client for the local Kubernetes NetworkPolicies.
func NewNetworkPolicyReflector(localDynamic, remoteDynamic dynamic.Interface, resync time.Duration,
	liqoNamespace string, localClusterID, remoteClusterID liqov1beta1.ClusterID, localPodCIDRs []string,
	sourceProfile string, workers uint) *NetworkPolicyReflector {
	return &NetworkPolicyReflector{
		localDynamic:    localDynamic,
		remoteDynamic:   remoteDynamic,
		resync:          resync,
		liqoNamespace:   liqoNamespace,
		localClusterID:  localClusterID,
		remoteClusterID: remoteClusterID,
		localPodCIDRs:   localPodCIDRs,
		sourceProfile:   sourceProfile,
		workers:         workers,
		queue:           workqueue.NewNamedRateLimitingQueue(workqueue.DefaultControllerRateLimiter(), ReflectorName),
		namespaces:      make(map[string]*namespacedState),
	}
}

// String returns the reflector name.
func (r *NetworkPolicyReflector) String() string {
	return ReflectorName
}

// Start starts the shared informers and the workers.
func (r *NetworkPolicyReflector) Start(ctx context.Context, _ *options.ReflectorOpts) {
	r.Lock()
	if r.started {
		r.Unlock()
		return
	}
	r.ctx = ctx
	r.started = true
	r.Unlock()

	r.configFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.localDynamic, r.resync, metav1.NamespaceAll, nil)
	r.ipFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.localDynamic, r.resync, r.liqoNamespace, nil)
	r.networkFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(r.localDynamic, r.resync, r.liqoNamespace, nil)

	r.configInformer = r.configFactory.ForResource(configurationGVR).Informer()
	r.ipInformer = r.ipFactory.ForResource(ipGVR).Informer()
	r.networkInformer = r.networkFactory.ForResource(networkGVR).Informer()

	handler := cache.ResourceEventHandlerFuncs{
		AddFunc:    func(interface{}) { r.enqueueAll() },
		UpdateFunc: func(interface{}, interface{}) { r.enqueueAll() },
		DeleteFunc: func(interface{}) { r.enqueueAll() },
	}
	r.configInformer.AddEventHandler(handler)
	r.ipInformer.AddEventHandler(handler)
	r.networkInformer.AddEventHandler(handler)

	r.configFactory.Start(ctx.Done())
	r.ipFactory.Start(ctx.Done())
	r.networkFactory.Start(ctx.Done())

	for i := uint(0); i < r.workers; i++ {
		go wait.Until(r.runWorker, time.Second, ctx.Done())
	}
	go func() {
		<-ctx.Done()
		r.queue.ShutDown()
	}()
}

// StartNamespace starts the reflection for the given namespace pair.
func (r *NetworkPolicyReflector) StartNamespace(opts *options.NamespacedOpts) {
	r.Lock()
	defer r.Unlock()

	if !r.started {
		klog.Errorf("Attempted to start %s reflection before the reflector was started", ReflectorName)
		return
	}
	if _, found := r.namespaces[opts.LocalNamespace]; found {
		return
	}

	state := &namespacedState{
		localNamespace:  opts.LocalNamespace,
		remoteNamespace: opts.RemoteNamespace,
		policyInformer:  opts.LocalFactory.Networking().V1().NetworkPolicies(),
	}

	if namespace, err := opts.LocalClient.CoreV1().Namespaces().Get(r.ctx, opts.LocalNamespace, metav1.GetOptions{}); err != nil {
		klog.Warningf("Unable to read namespace %q while starting NetworkPolicy reflection: %v", opts.LocalNamespace, err)
	} else {
		state.namespaceUID = namespace.UID
	}

	state.dynamicFactory = dynamicinformer.NewFilteredDynamicSharedInformerFactory(
		r.remoteDynamic, r.resync, opts.RemoteNamespace, nil)
	state.shadowInformer = state.dynamicFactory.ForResource(shadowNetworkPolicyGVR).Informer()
	state.setInformer = state.dynamicFactory.ForResource(shadowNetworkPolicySetGVR).Informer()

	policyKeyer := options.Keyer(func(metadata metav1.Object) []types.NamespacedName {
		return []types.NamespacedName{{Namespace: state.localNamespace, Name: metadata.GetName()}}
	})
	state.policyInformer.Informer().AddEventHandler(opts.HandlerFactory(policyKeyer))

	inventoryKeyer := options.Keyer(func(metadata metav1.Object) []types.NamespacedName {
		return []types.NamespacedName{{Namespace: state.localNamespace}}
	})
	shadowHandler := opts.HandlerFactory(inventoryKeyer)
	state.shadowInformer.AddEventHandler(shadowHandler)
	state.setInformer.AddEventHandler(shadowHandler)

	r.namespaces[opts.LocalNamespace] = state
	state.dynamicFactory.Start(r.ctx.Done())

	go func() {
		if !cache.WaitForCacheSync(r.ctx.Done(), state.policyInformer.Informer().HasSynced,
			state.shadowInformer.HasSynced, state.setInformer.HasSynced) {
			return
		}
		r.enqueueNamespace(state)
	}()
}

// StopNamespace stops the reflection for the given namespace pair.
func (r *NetworkPolicyReflector) StopNamespace(local, _ string) {
	r.Lock()
	defer r.Unlock()

	state, found := r.namespaces[local]
	if !found {
		return
	}
	if state.dynamicFactory != nil {
		state.dynamicFactory.Shutdown()
	}
	delete(r.namespaces, local)
}

// Resync re-enqueues every known policy and inventory.
func (r *NetworkPolicyReflector) Resync() error {
	r.enqueueAll()
	return nil
}

func (r *NetworkPolicyReflector) ready() bool {
	return r.configInformer != nil && r.configInformer.HasSynced() &&
		r.ipInformer != nil && r.ipInformer.HasSynced() &&
		r.networkInformer != nil && r.networkInformer.HasSynced()
}

func (r *NetworkPolicyReflector) enqueueAll() {
	if !r.ready() {
		return
	}
	r.Lock()
	states := make([]*namespacedState, 0, len(r.namespaces))
	for _, state := range r.namespaces {
		states = append(states, state)
	}
	r.Unlock()
	for _, state := range states {
		r.enqueueNamespace(state)
	}
}

func (r *NetworkPolicyReflector) enqueueNamespace(state *namespacedState) {
	if state.policyInformer == nil || !state.policyInformer.Informer().HasSynced() {
		return
	}
	for _, object := range state.policyInformer.Informer().GetStore().List() {
		key := object.(metav1.Object)
		r.queue.Add(types.NamespacedName{Namespace: state.localNamespace, Name: key.GetName()})
	}
	r.queue.Add(types.NamespacedName{Namespace: state.localNamespace})
}

func (r *NetworkPolicyReflector) runWorker() {
	for r.processNextItem() {
	}
}

func (r *NetworkPolicyReflector) processNextItem() bool {
	item, shutdown := r.queue.Get()
	if shutdown {
		return false
	}
	defer r.queue.Done(item)

	key, ok := item.(types.NamespacedName)
	if !ok {
		r.queue.Forget(item)
		return true
	}

	if err := r.handle(key); err != nil {
		klog.Errorf("Failed to reflect NetworkPolicy %q: %v", key, err)
		r.queue.AddRateLimited(item)
		return true
	}
	r.queue.Forget(item)
	return true
}

func (r *NetworkPolicyReflector) handle(key types.NamespacedName) error {
	r.Lock()
	state, found := r.namespaces[key.Namespace]
	r.Unlock()
	if !found {
		return nil
	}
	if !state.policyInformer.Informer().HasSynced() {
		return fmt.Errorf("the NetworkPolicy informer is not synced yet")
	}
	if !r.ready() {
		return fmt.Errorf("the Liqo network caches are not synced yet")
	}

	state.mu.Lock()
	defer state.mu.Unlock()

	if key.Name != "" {
		if err := r.reconcilePolicy(state, key.Name); err != nil {
			return err
		}
	}
	return r.reconcileInventory(state)
}

func (r *NetworkPolicyReflector) reconcilePolicy(state *namespacedState, name string) error {
	policy, err := state.policyInformer.Lister().NetworkPolicies(state.localNamespace).Get(name)
	if err != nil {
		if errors.IsNotFound(err) {
			// Policy deletion is handled by the inventory writer, which removes every managed shadow
			// that is no longer part of the expected set.
			return nil
		}
		return err
	}

	shadow, _, err := r.desiredShadow(state, policy)
	if err != nil {
		return err
	}
	return r.applyShadow(r.ctx, state.remoteNamespace, shadow)
}

// objectToTyped converts an unstructured informer object into the given typed target.
func objectToTyped(object interface{}, target interface{}) error {
	u, ok := object.(*unstructured.Unstructured)
	if !ok {
		return fmt.Errorf("unexpected informer object type %T", object)
	}
	return runtime.DefaultUnstructuredConverter.FromUnstructured(u.Object, target)
}

var _ = ipGVR
var _ = networkGVR
var _ = watch.Event{}
var _ = corev1.Namespace{}
