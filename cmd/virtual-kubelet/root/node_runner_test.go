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
	"errors"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

// errNodeStartup mimics the error returned by the node controller when its startup update fails
// (e.g. because the first ping to the remote API server timed out).
var errNodeStartup = errors.New("node ping failed")

// readyNodeRunner is a nodeRunner that becomes ready immediately and keeps running until its
// context is canceled.
type readyNodeRunner struct{}

func (readyNodeRunner) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (readyNodeRunner) Run(ctx context.Context) error {
	<-ctx.Done()
	return nil
}

// failingNodeRunner is a nodeRunner that never becomes ready and exits immediately with an error.
type failingNodeRunner struct{}

func (failingNodeRunner) Ready() <-chan struct{} { return make(chan struct{}) }

func (failingNodeRunner) Run(context.Context) error { return errNodeStartup }

// terminatingNodeRunner becomes ready immediately, then terminates with err once stop is closed (or
// returns nil when its context is canceled).
type terminatingNodeRunner struct {
	stop chan struct{}
	err  error
}

func (r *terminatingNodeRunner) Ready() <-chan struct{} {
	ch := make(chan struct{})
	close(ch)
	return ch
}

func (r *terminatingNodeRunner) Run(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return nil
	case <-r.stop:
		return r.err
	}
}

var _ = Describe("startNodeControllerWithRetry", func() {
	const retryDelay = 10 * time.Millisecond

	var (
		ctx             context.Context
		cancel          context.CancelFunc
		noopTermination = func(error) {}
	)

	BeforeEach(func() {
		ctx, cancel = context.WithCancel(context.Background())
		DeferCleanup(cancel)
	})

	It("starts the node controller on the first attempt", func() {
		var calls int
		newRunner := func() (nodeRunner, error) {
			calls++
			return readyNodeRunner{}, nil
		}

		Expect(startNodeControllerWithRetry(ctx, retryDelay, newRunner, noopTermination)).To(Succeed())
		Expect(calls).To(Equal(1))
	})

	It("retries until the node controller becomes ready", func() {
		var calls int
		newRunner := func() (nodeRunner, error) {
			calls++
			if calls < 3 {
				return failingNodeRunner{}, nil
			}
			return readyNodeRunner{}, nil
		}

		Expect(startNodeControllerWithRetry(ctx, retryDelay, newRunner, noopTermination)).To(Succeed())
		Expect(calls).To(Equal(3))
	})

	It("gives up after the maximum number of attempts", func() {
		var calls int
		newRunner := func() (nodeRunner, error) {
			calls++
			return failingNodeRunner{}, nil
		}

		err := startNodeControllerWithRetry(ctx, retryDelay, newRunner, noopTermination)
		Expect(err).To(HaveOccurred())
		Expect(errors.Is(err, errNodeStartup)).To(BeTrue())
		Expect(calls).To(Equal(nodeStartupMaxAttempts))
	})

	It("does not wait after the last failed attempt", func() {
		var calls int
		newRunner := func() (nodeRunner, error) {
			calls++
			return failingNodeRunner{}, nil
		}

		// The delay only separates consecutive attempts, so nodeStartupMaxAttempts attempts wait
		// (nodeStartupMaxAttempts - 1) times: the call must return strictly before that many delays.
		const delay = 100 * time.Millisecond
		start := time.Now()
		err := startNodeControllerWithRetry(ctx, delay, newRunner, noopTermination)
		elapsed := time.Since(start)

		Expect(err).To(HaveOccurred())
		Expect(calls).To(Equal(nodeStartupMaxAttempts))
		Expect(elapsed).To(BeNumerically("<", nodeStartupMaxAttempts*delay))
	})

	It("propagates the error when the controller cannot be created", func() {
		sentinel := errors.New("cannot create node controller")
		newRunner := func() (nodeRunner, error) {
			return nil, sentinel
		}

		err := startNodeControllerWithRetry(ctx, retryDelay, newRunner, noopTermination)
		Expect(errors.Is(err, sentinel)).To(BeTrue())
	})

	It("aborts when the context is canceled while retrying", func() {
		var calls int
		newRunner := func() (nodeRunner, error) {
			calls++
			return failingNodeRunner{}, nil
		}

		go func() {
			time.Sleep(retryDelay)
			cancel()
		}()

		err := startNodeControllerWithRetry(ctx, time.Hour, newRunner, noopTermination)
		Expect(errors.Is(err, context.Canceled)).To(BeTrue())
		Expect(calls).To(Equal(1))
	})

	It("invokes the callback if the controller dies after startup", func() {
		runner := &terminatingNodeRunner{stop: make(chan struct{}), err: errors.New("controller died")}
		newRunner := func() (nodeRunner, error) {
			return runner, nil
		}
		terminated := make(chan error, 1)

		Expect(startNodeControllerWithRetry(ctx, retryDelay, newRunner, func(err error) {
			terminated <- err
		})).To(Succeed())

		close(runner.stop)
		Eventually(terminated).Should(Receive(MatchError("controller died")))
	})

	It("does not invoke the callback on a regular shutdown", func() {
		newRunner := func() (nodeRunner, error) {
			return readyNodeRunner{}, nil
		}
		terminated := make(chan error, 1)

		Expect(startNodeControllerWithRetry(ctx, retryDelay, newRunner, func(err error) {
			terminated <- err
		})).To(Succeed())

		cancel()
		Consistently(terminated).WithTimeout(100 * time.Millisecond).ShouldNot(Receive())
	})
})
