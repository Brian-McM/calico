// Copyright (c) 2026 Tigera, Inc. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package util_test

import (
	"context"
	"fmt"
	"sync"
	"testing"

	v3 "github.com/projectcalico/api/pkg/apis/projectcalico/v3"
	"k8s.io/apimachinery/pkg/watch"

	"github.com/projectcalico/calico/apiserver/pkg/registry/projectcalico/util"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/api"
	"github.com/projectcalico/calico/libcalico-go/lib/backend/model"
)

func tierAdded(name string) []api.Update {
	return []api.Update{{
		KVPair: model.KVPair{
			Key:   model.ResourceKey{Kind: v3.KindTier, Name: name},
			Value: v3.NewTier(),
		},
		UpdateType: api.UpdateTypeKVNew,
	}}
}

// A new tier arriving while policy watches are being opened and closed used to reset the
// watch map under the other goroutines' feet and crash the API server with a nil dereference.
func TestWatchManager_TierAddedWhileWatchesChurn(t *testing.T) {
	m := util.NewWatchManager(nil)

	var wg sync.WaitGroup
	for w := range 8 {
		wg.Go(func() {
			for i := range 2000 {
				ctx, cancel := context.WithCancel(context.Background())
				m.AddWatch(util.WatchRecord{
					ID:    fmt.Sprintf("%d-%d", w, i),
					Kind:  v3.KindGlobalNetworkPolicy,
					Watch: watch.NewFake(),
					Ctx:   ctx,
				})
				cancel()
			}
		})
	}
	wg.Go(func() {
		for i := range 2000 {
			m.OnUpdates(tierAdded(fmt.Sprintf("tier-%d", i)))
		}
	})
	wg.Wait()
}

func TestWatchManager_TierAddedStopsOpenWatches(t *testing.T) {
	m := util.NewWatchManager(nil)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	w := watch.NewFake()
	m.AddWatch(util.WatchRecord{ID: "gnp", Kind: v3.KindGlobalNetworkPolicy, Watch: w, Ctx: ctx})
	m.OnUpdates(tierAdded("new-tier"))

	if !w.IsStopped() {
		t.Fatal("watch open when the tier was added was not stopped")
	}
}
