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

package id

import (
	"context"
	"fmt"
	"math"
	"sync"

	"k8s.io/apimachinery/pkg/util/runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	networkingv1beta1 "github.com/liqotech/liqo/apis/networking/v1beta1"
	"github.com/liqotech/liqo/pkg/consts"
)

var ecmpMarkManager *Manager[uint32]
var ecmpMarkOnce sync.Once

// ResetECMPMarkManager resets the singleton ECMP mark manager. It is intended for tests only.
func ResetECMPMarkManager() {
	ecmpMarkManager = nil
	ecmpMarkOnce = sync.Once{}
}

// GetECMPMarkManager returns the Manager for ECMP policy-routing marks or creates it if it does not exist.
// It is a singleton and recovers previously allocated marks from InternalFabric status on first use.
func GetECMPMarkManager(ctx context.Context, cl client.Client) *Manager[uint32] {
	ecmpMarkOnce.Do(func() {
		// Allocate marks in the dedicated Liqo ECMP range [base, base + 2^24).
		ecmpMarkManager = NewWithRange[uint32](consts.ECMPReplicaMarkBase, consts.ECMPReplicaMarkBase+(1<<24))

		var fabricList networkingv1beta1.InternalFabricList
		err := cl.List(ctx, &fabricList)
		runtime.Must(err)

		for i := range fabricList.Items {
			fabric := &fabricList.Items[i]
			if fabric.Status.ECMPMark != nil {
				mark := *fabric.Status.ECMPMark
				if mark < 0 || mark > math.MaxUint32 {
					panic(fmt.Sprintf("invalid ECMP mark %d for InternalFabric %q: out of uint32 range",
						mark, client.ObjectKeyFromObject(fabric).String()))
				}

				markUint32 := uint32(mark)
				err = ecmpMarkManager.Configure(client.ObjectKeyFromObject(fabric).String(), markUint32)
				runtime.Must(err)
			}
		}
	})

	return ecmpMarkManager
}
