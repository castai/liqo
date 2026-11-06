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

// Package networkpolicy publishes consumer policy inventories.
package networkpolicy

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"golang.org/x/sync/errgroup"
	core "k8s.io/api/core/v1"
	networking "k8s.io/api/networking/v1"
	"k8s.io/apimachinery/pkg/api/equality"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/cache"
	"k8s.io/klog/v2"
	"k8s.io/utils/ptr"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"

	api "github.com/liqotech/liqo/apis/offloading/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
	"github.com/liqotech/liqo/pkg/leaderelection"
	compiler "github.com/liqotech/liqo/pkg/utils/networkpolicy"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/manager"
	"github.com/liqotech/liqo/pkg/virtualKubelet/reflection/options"
)

var _ manager.Reflector = (*Reflector)(nil)

// Reflector publishes complete snapshots for each active namespace mapping.
// The tenant leader is the only writer. A bounded rescan also observes changes
// to dependencies across namespace boundaries.
type Reflector struct {
	local, remote                               client.Client
	sourceCluster, targetCluster, liqoNamespace string
	localPodCIDRs                               []string
	workers                                     uint
	mu                                          sync.Mutex
	namespaces                                  map[string]*options.NamespacedOpts
	trigger                                     chan struct{}
}

// New constructs a reflector for one consumer/provider pair.
func New(local, remote client.Client, source, target, liqoNamespace string, podCIDRs []string, workers uint) *Reflector {
	return &Reflector{local: local, remote: remote, sourceCluster: source, targetCluster: target, liqoNamespace: liqoNamespace,
		localPodCIDRs: podCIDRs, workers: workers, namespaces: make(map[string]*options.NamespacedOpts), trigger: make(chan struct{}, 1)}
}

// String returns the reflector name.
func (r *Reflector) String() string { return "NetworkPolicy" }

// Start launches the inventory reconciliation loop.
func (r *Reflector) Start(ctx context.Context, _ *options.ReflectorOpts) {
	go func() {
		ticker := time.NewTicker(3 * time.Second)
		defer ticker.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			case <-r.trigger:
			}
			if !leaderelection.IsLeader() {
				continue
			}
			if err := r.sync(ctx); err != nil {
				klog.Warningf("NetworkPolicy reflection: %v", err)
			}
		}
	}()
}

// StartNamespace registers an accepted namespace mapping.
func (r *Reflector) StartNamespace(opts *options.NamespacedOpts) {
	r.mu.Lock()
	r.namespaces[opts.LocalNamespace] = opts
	r.mu.Unlock()
	wake := func() { _ = r.Resync() }
	handler := cache.ResourceEventHandlerFuncs{
		AddFunc: func(any) { wake() }, UpdateFunc: func(any, any) { wake() }, DeleteFunc: func(any) { wake() },
	}
	for _, informer := range []cache.SharedIndexInformer{
		opts.LocalFactory.Networking().V1().NetworkPolicies().Informer(),
		opts.LocalLiqoFactory.Ipam().V1alpha1().IPs().Informer(),
	} {
		if _, err := informer.AddEventHandler(handler); err != nil {
			klog.Errorf("Unable to watch policy dependency: %v", err)
		}
	}
	wake()
}

// StopNamespace stops publication for a mapping.
func (r *Reflector) StopNamespace(local, _ string) {
	r.mu.Lock()
	delete(r.namespaces, local)
	r.mu.Unlock()
	_ = r.Resync()
}

// Resync requests an inventory refresh.
func (r *Reflector) Resync() error {
	select {
	case r.trigger <- struct{}{}:
	default:
	}
	return nil
}

func (r *Reflector) sync(ctx context.Context) error {
	r.mu.Lock()
	mappings := make([]*options.NamespacedOpts, 0, len(r.namespaces))
	for _, mapping := range r.namespaces {
		mappings = append(mappings, mapping)
	}
	r.mu.Unlock()
	if len(mappings) == 0 || r.workers == 0 {
		return nil
	}
	slices.SortFunc(mappings, func(a, b *options.NamespacedOpts) int { return strings.Compare(a.LocalNamespace, b.LocalNamespace) })
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	input, contextErr := r.sourceContext(ctx, mappings)
	// Share one read-only address snapshot across workers. Wait for the whole
	// batch before reading a new snapshot so inventories for the same
	// namespace are always published in order.
	var group errgroup.Group
	group.SetLimit(int(min(r.workers, uint(len(mappings)))))
	for _, mapping := range mappings {
		group.Go(func() error {
			if ctx.Err() != nil || !leaderelection.IsLeader() {
				return nil
			}
			r.mu.Lock()
			active := r.namespaces[mapping.LocalNamespace] == mapping
			r.mu.Unlock()
			if active {
				if err := r.publish(ctx, mapping, input, contextErr); err != nil {
					klog.Warningf("NetworkPolicy reflection for %s: %v", mapping.LocalNamespace, err)
				}
			}
			return nil
		})
	}
	return group.Wait()
}

func digest(v any) string {
	return fmt.Sprintf("%x", sha256.Sum256(mustJSON(v)))
}

func sourcePolicyRevision(policies []networking.NetworkPolicy) string {
	type entry struct {
		UID  types.UID
		Spec networking.NetworkPolicySpec
	}
	entries := make([]entry, 0, len(policies))
	for _, p := range policies {
		entries = append(entries, entry{p.UID, p.Spec})
	}
	slices.SortFunc(entries, func(a, b entry) int { return strings.Compare(string(a.UID), string(b.UID)) })
	return digest(entries)
}

func (r *Reflector) publish(ctx context.Context, mapping *options.NamespacedOpts, input api.PolicySourceContext, contextErr error) error {
	var ns core.Namespace
	if err := r.local.Get(ctx, client.ObjectKey{Name: mapping.LocalNamespace}, &ns); err != nil {
		return err
	}
	if !ns.DeletionTimestamp.IsZero() {
		return nil
	}
	var policies networking.NetworkPolicyList
	if err := r.local.List(ctx, &policies, client.InNamespace(ns.Name)); err != nil {
		return err
	}
	source := api.PolicySource{Cluster: r.sourceCluster, Namespace: ns.Name, NamespaceUID: string(ns.UID)}
	revision := sourcePolicyRevision(policies.Items)
	desired := make(map[string]api.ShadowNetworkPolicySpec, len(policies.Items))
	for _, policy := range policies.Items {
		spec := api.ShadowNetworkPolicySpec{Source: source, PolicyUID: string(policy.UID), PolicyName: policy.Name, Template: *policy.Spec.DeepCopy(), Context: input}
		switch {
		case !subjectPreserved(&policy.Spec.PodSelector, mapping.ForgingOpts.LabelsNotReflected):
			spec.InputError = "subject selector uses a label that offloading changes"
		case contextErr != nil && (len(policy.Spec.Ingress) > 0 || len(policy.Spec.Egress) > 0):
			spec.InputError = contextErr.Error()
		case len(policy.Spec.Ingress) == 0 && len(policy.Spec.Egress) == 0:
			spec.Context = api.PolicySourceContext{}
		}
		if len(mustJSON(spec)) > 512*1024 {
			spec.InputError = "reflected policy exceeds input size limit"
		}
		if spec.InputError != "" {
			spec.Template = *compiler.Isolation(&policy.Spec)
			spec.Context = api.PolicySourceContext{}
		}
		spec.Template.PolicyTypes = compiler.Isolation(&policy.Spec).PolicyTypes
		desired["liqo-np-"+digest([]string{source.Cluster, source.NamespaceUID, string(policy.UID)})[:32]] = spec
	}
	var existing api.ShadowNetworkPolicyList
	if err := r.remote.List(ctx, &existing, client.InNamespace(mapping.RemoteNamespace)); err != nil {
		return err
	}
	set := &api.ShadowNetworkPolicySet{ObjectMeta: metav1.ObjectMeta{Namespace: mapping.RemoteNamespace, Name: compiler.InventoryName}}
	setSpec := api.ShadowNetworkPolicySetSpec{Source: source, PolicyRevision: revision, Revision: digest(desired)}
	unchanged := true
	for _, s := range existing.Items {
		wanted, ok := desired[s.Name]
		if !ok || !equality.Semantic.DeepEqual(wanted, s.Spec) || !s.DeletionTimestamp.IsZero() {
			unchanged = false
			break
		}
		setSpec.Members = append(setSpec.Members, api.PolicySetMember{Name: s.Name, UID: string(s.UID), Generation: s.Generation})
	}
	if unchanged && len(existing.Items) == len(desired) {
		setSpec.Committed = true
		return r.writeSet(ctx, set, &setSpec)
	}
	setSpec.Members = nil
	if err := r.writeSet(ctx, set, &setSpec); err != nil {
		return err
	}
	stagedUID, stagedGeneration := set.UID, set.Generation
	for name, spec := range desired {
		shadow := &api.ShadowNetworkPolicy{ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: set.Namespace}}
		if _, err := controllerutil.CreateOrUpdate(ctx, r.remote, shadow, func() error {
			if shadow.UID != "" && (shadow.Spec.Source != source || shadow.Spec.PolicyUID != spec.PolicyUID) {
				return fmt.Errorf("policy shadow %s is owned by a different source", name)
			}
			shadow.Spec = *spec.DeepCopy()
			return nil
		}); err != nil {
			return err
		}
		setSpec.Members = append(setSpec.Members, api.PolicySetMember{Name: shadow.Name, UID: string(shadow.UID), Generation: shadow.Generation})
	}
	for i := range existing.Items {
		s := &existing.Items[i]
		if _, ok := desired[s.Name]; ok {
			continue
		}
		if s.Spec.Source.Cluster == source.Cluster && s.Spec.Source.Namespace == source.Namespace {
			if err := r.remote.Delete(ctx, s, client.Preconditions{UID: ptr.To(s.UID)}); client.IgnoreNotFound(err) != nil {
				return err
			}
		}
	}
	setSpec.Committed = true
	return r.writeSet(ctx, set, &setSpec, func() error {
		if set.UID != stagedUID || set.Generation != stagedGeneration || set.Spec.Committed {
			return fmt.Errorf("policy inventory staging was superseded")
		}
		return nil
	})
}

func mustJSON(v any) []byte {
	data, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return data
}

func (r *Reflector) writeSet(ctx context.Context, set *api.ShadowNetworkPolicySet, spec *api.ShadowNetworkPolicySetSpec, checks ...func() error) error {
	slices.SortFunc(spec.Members, func(a, b api.PolicySetMember) int { return strings.Compare(a.Name, b.Name) })
	if len(spec.Members) == 0 {
		spec.Members = nil
	}
	_, err := controllerutil.CreateOrUpdate(ctx, r.remote, set, func() error {
		if set.UID != "" && (set.Spec.Source.Cluster != spec.Source.Cluster || set.Spec.Source.Namespace != spec.Source.Namespace) {
			return fmt.Errorf("namespace inventory is owned by a different source")
		}
		for _, check := range checks {
			if err := check(); err != nil {
				return err
			}
		}
		set.Spec = *spec
		return nil
	})
	return err
}

func subjectPreserved(selector *metav1.LabelSelector, removed []string) bool {
	check := func(key string) bool {
		return !slices.Contains(removed, key) && !strings.HasPrefix(key, "liqo.io/") && !strings.HasPrefix(key, "offloading.liqo.io/") && key != consts.ManagedByLabelKey
	}
	for key := range selector.MatchLabels {
		if !check(key) {
			return false
		}
	}
	for _, expr := range selector.MatchExpressions {
		if !check(expr.Key) {
			return false
		}
	}
	return true
}

// CheckReady holds new ShadowPods until the provider acknowledges the source policy snapshot.
func (r *Reflector) CheckReady(ctx context.Context, local, remote string) error {
	var set api.ShadowNetworkPolicySet
	if err := r.remote.Get(ctx, client.ObjectKey{Namespace: remote, Name: compiler.InventoryName}, &set); err != nil {
		if apierrors.IsNotFound(err) {
			return fmt.Errorf("waiting for NetworkPolicy inventory")
		}
		return err
	}
	var policies networking.NetworkPolicyList
	if err := r.local.List(ctx, &policies, client.InNamespace(local)); err != nil {
		return err
	}
	var ns core.Namespace
	if err := r.local.Get(ctx, client.ObjectKey{Name: local}, &ns); err != nil {
		return err
	}
	if !compiler.Ready(&set) || set.Spec.Source != (api.PolicySource{Cluster: r.sourceCluster, Namespace: local, NamespaceUID: string(ns.UID)}) ||
		set.Spec.PolicyRevision != sourcePolicyRevision(policies.Items) {
		return fmt.Errorf("waiting for provider to apply NetworkPolicies")
	}
	return nil
}
