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

package leaderelection

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	. "github.com/onsi/ginkgo/v2"
	. "github.com/onsi/gomega"
)

func TestLeaderelection(t *testing.T) {
	RegisterFailHandler(Fail)
	RunSpecs(t, "Leaderelection Suite")
}

var _ = Describe("LeaderMarkerRunnable", func() {
	var tmpDir string

	BeforeEach(func() {
		var err error
		tmpDir, err = os.MkdirTemp("", "leaderelection-test")
		Expect(err).ToNot(HaveOccurred())
	})

	AfterEach(func() {
		Expect(os.RemoveAll(tmpDir)).To(Succeed())
	})

	It("should wait for the marker file", func() {
		path := filepath.Join(tmpDir, "leader")

		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()

		errChan := make(chan error, 1)
		go func() { errChan <- WaitForMarkerFile(ctx, path, 50*time.Millisecond) }()

		Consistently(errChan, 200*time.Millisecond).ShouldNot(Receive())

		Expect(os.WriteFile(path, []byte{}, 0o644)).To(Succeed())
		Eventually(errChan, time.Second).Should(Receive(BeNil()))
	})

	It("should return context error when waiting is cancelled", func() {
		path := filepath.Join(tmpDir, "leader")
		ctx, cancel := context.WithCancel(context.Background())

		errChan := make(chan error, 1)
		go func() { errChan <- WaitForMarkerFile(ctx, path, 50*time.Millisecond) }()

		cancel()
		Eventually(errChan, time.Second).Should(Receive(MatchError(context.Canceled)))
	})

	It("should report ready only when the marker file exists", func() {
		path := filepath.Join(tmpDir, "leader")
		checker := ReadyzCheck(path)

		Expect(checker(nil)).To(HaveOccurred())

		Expect(os.WriteFile(path, []byte{}, 0o644)).To(Succeed())
		Expect(checker(nil)).To(Succeed())
	})
})
