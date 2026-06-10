// Copyright 2026 Dolthub, Inc.
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

package main

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"time"
)

// workload runs several specialized worker pools against the target
// dumbodb. Worker counts are independent so operators can dial
// individual surfaces up or down via flags.
type workload struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	cycles atomic.Int64
	errors atomic.Int64
}

// workloadConfig is what main.go fills out from flags.
type workloadConfig struct {
	URI            string
	SessionWorkers int
	CRUDWorkers    int
	BulkWorkers    int
	AggWorkers     int
	IndexedWorkers int
	VCSWorkers     int
	TxnWorkers     int
	SessionDelay   time.Duration
	OpsInterval    time.Duration
}

func (w *workload) cycleCount() int64 { return w.cycles.Load() }
func (w *workload) errCount() int64   { return w.errors.Load() }

func (w *workload) stop() {
	w.cancel()
	w.wg.Wait()
}

// startWorkload starts every configured worker pool and returns
// immediately. The returned workload's stop() cancels and joins all
// workers.
func startWorkload(parent context.Context, cfg workloadConfig) (*workload, error) {
	ctx, cancel := context.WithCancel(parent)
	w := &workload{cancel: cancel}

	// Shared collection across the long-lived-client pools so they
	// all contend for the same address-map / chunk store.
	collName := fmt.Sprintf("soak_%d", time.Now().UnixNano())

	for i := 0; i < cfg.SessionWorkers; i++ {
		w.wg.Add(1)
		go w.runSessionWorker(ctx, cfg.URI, cfg.SessionDelay)
	}
	for i := 0; i < cfg.CRUDWorkers; i++ {
		w.wg.Add(1)
		go w.runCRUDWorker(ctx, cfg.URI, collName, i, cfg.OpsInterval)
	}
	for i := 0; i < cfg.BulkWorkers; i++ {
		w.wg.Add(1)
		go w.runBulkWorker(ctx, cfg.URI, collName, i, cfg.OpsInterval)
	}
	for i := 0; i < cfg.AggWorkers; i++ {
		w.wg.Add(1)
		go w.runAggWorker(ctx, cfg.URI, collName, i, cfg.OpsInterval)
	}
	for i := 0; i < cfg.IndexedWorkers; i++ {
		w.wg.Add(1)
		go w.runIndexedWorker(ctx, cfg.URI, collName, i, cfg.OpsInterval)
	}
	for i := 0; i < cfg.VCSWorkers; i++ {
		w.wg.Add(1)
		go w.runVCSWorker(ctx, cfg.URI, collName, i, cfg.OpsInterval)
	}
	for i := 0; i < cfg.TxnWorkers; i++ {
		w.wg.Add(1)
		go w.runTxnWorker(ctx, cfg.URI, collName, i, cfg.OpsInterval)
	}
	return w, nil
}
