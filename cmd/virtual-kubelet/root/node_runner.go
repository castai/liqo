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

package root

import (
	"context"
	"fmt"
	"time"

	"k8s.io/klog/v2"
)

const (
	// nodeStartupMaxAttempts is the number of times the node controller startup is attempted before
	// giving up.
	//
	// Startup is retried to tolerate a remote API server that is not yet reachable when the virtual
	// kubelet boots: the node controller performs a node status update during startup (ensureNode),
	// whose first step is a ping to the remote API server. A failed ping makes the controller exit
	// before it becomes ready, and there is no other chance to recover.
	nodeStartupMaxAttempts = 5
	// nodeStartupRetryDelay is the fixed delay between two node controller startup attempts.
	nodeStartupRetryDelay = 10 * time.Second
)

// nodeRunner is the subset of the virtual-kubelet node.NodeController used to start the node
// controller. It is factored out to allow testing the startup retry logic without a cluster.
type nodeRunner interface {
	// Run starts the node controller and blocks until it terminates or ctx is canceled.
	Run(ctx context.Context) error
	// Ready returns a channel that is closed once the node controller is ready.
	Ready() <-chan struct{}
}

// startNodeControllerWithRetry starts a node controller and waits for it to become ready, retrying
// up to nodeStartupMaxAttempts times with a fixed retryDelay.
//
// This tolerates a remote API server that is not yet reachable when the virtual kubelet starts: the
// first node ping failure otherwise makes the node controller exit before becoming ready.
//
// A fresh controller is created through newRunner for each attempt. The function blocks until the
// controller is ready, until the retry budget is exhausted (returning the last error), or until ctx
// is canceled.
//
// On success the controller keeps running until ctx is canceled. If it terminates before then,
// onUnexpectedTermination is called with the termination error, from its own goroutine.
func startNodeControllerWithRetry(
	ctx context.Context,
	retryDelay time.Duration,
	newRunner func() (nodeRunner, error),
	onUnexpectedTermination func(error),
) error {
	var lastErr error
	for attempt := 1; attempt <= nodeStartupMaxAttempts; attempt++ {
		if err := ctx.Err(); err != nil {
			return err
		}

		runner, err := newRunner()
		if err != nil {
			return fmt.Errorf("creating the node controller: %w", err)
		}

		// The attempt context is a child of ctx, so the controller is torn down on shutdown. The
		// controller also starts goroutines (e.g. the ping loop) before it can fail during startup;
		// canceling the attempt context when the runner returns stops them, so that a failed attempt
		// does not leave them running.
		attemptCtx, cancel := context.WithCancel(ctx)
		runErr := make(chan error, 1)
		go func() {
			defer cancel()
			runErr <- runner.Run(attemptCtx)
		}()

		select {
		case <-runner.Ready():
			// The controller started successfully and keeps running until ctx is canceled. Keep
			// watching it, so that a controller terminating before then can be handled: it can no
			// longer update the node status.
			go func() {
				if err := <-runErr; err != nil && ctx.Err() == nil && onUnexpectedTermination != nil {
					onUnexpectedTermination(err)
				}
			}()
			return nil
		case err := <-runErr:
			lastErr = err
		case <-ctx.Done():
			return ctx.Err()
		}

		// Wait before retrying, but only if another attempt is left: after the last attempt the error
		// is returned immediately, without an extra wait (and without risking to mask lastErr with a
		// context cancellation during that wait).
		if attempt < nodeStartupMaxAttempts {
			klog.Warningf("Node controller failed to start (attempt %d/%d): %v; retrying in %s",
				attempt, nodeStartupMaxAttempts, lastErr, retryDelay)

			timer := time.NewTimer(retryDelay)
			select {
			case <-timer.C:
			case <-ctx.Done():
				timer.Stop()
				return ctx.Err()
			}
		}
	}

	return fmt.Errorf("node controller failed to start after %d attempts: %w", nodeStartupMaxAttempts, lastErr)
}
