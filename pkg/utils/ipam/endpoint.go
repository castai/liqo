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

package ipam

import (
	"fmt"
	api "github.com/liqotech/liqo/apis/ipam/v1alpha1"
)

// EndpointAllocation finds the current, unambiguous allocation for an endpoint.
func EndpointAllocation(ips []*api.IP, original string) (*api.IP, error) {
	var match *api.IP
	for _, ip := range ips {
		if !ip.DeletionTimestamp.IsZero() || ip.Spec.IP.String() != original {
			continue
		}
		if match != nil && match.Status.IP != ip.Status.IP {
			return nil, fmt.Errorf("ambiguous allocation for %s", original)
		}
		match = ip
	}
	if match == nil || match.Status.IP == "" {
		return nil, fmt.Errorf("no ready allocation for %s", original)
	}
	return match, nil
}
