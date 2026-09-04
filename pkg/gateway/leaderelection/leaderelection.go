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
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"time"

	"sigs.k8s.io/controller-runtime/pkg/healthz"
)

const (
	// DefaultLeaseDuration is the default duration of the gateway leader lease.
	DefaultLeaseDuration = 5 * time.Second
	// DefaultRenewDeadline is the default renew deadline of the gateway leader lease.
	DefaultRenewDeadline = 3 * time.Second
	// DefaultRetryPeriod is the default retry period of the gateway leader lease.
	DefaultRetryPeriod = 1 * time.Second
	// DefaultPollInterval is the default interval used when polling the marker file.
	DefaultPollInterval = 500 * time.Millisecond
	// DefaultLeaderMarkerPath is the default path of the leader marker file.
	DefaultLeaderMarkerPath = "/run/liqo-gateway/leader"

	// LeaderLabelKey is the label key set on the leader gateway pod.
	LeaderLabelKey = "networking.liqo.io/active"
	// LeaderLabelValue is the label value set on the leader gateway pod.
	LeaderLabelValue = "true"
)

// CreateMarkerFile creates the leader marker file, creating parent directories if needed.
func CreateMarkerFile(path string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return fmt.Errorf("unable to create leader marker directory %q: %w", filepath.Dir(path), err)
	}

	f, err := os.Create(path)
	if err != nil {
		return fmt.Errorf("unable to create leader marker file %q: %w", path, err)
	}
	return f.Close()
}

// RemoveMarkerFile removes the leader marker file, ignoring not-exist errors.
func RemoveMarkerFile(path string) error {
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("unable to remove leader marker file %q: %w", path, err)
	}
	return nil
}

// WaitForMarkerFile blocks until the marker file exists or the context is cancelled.
func WaitForMarkerFile(ctx context.Context, path string, interval time.Duration) error {
	if interval <= 0 {
		interval = DefaultPollInterval
	}

	ticker := time.NewTicker(interval)
	defer ticker.Stop()

	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
		}
	}
}

// ReadyzCheck returns a healthz checker that succeeds only if the marker file exists.
func ReadyzCheck(path string) healthz.Checker {
	return func(_ *http.Request) error {
		if _, err := os.Stat(path); err != nil {
			return fmt.Errorf("leader marker file %q not found: %w", path, err)
		}
		return nil
	}
}
