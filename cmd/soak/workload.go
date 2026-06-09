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
	"math/rand"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// workload runs two pools of long-lived goroutines: short-lived
// connect/ping/disconnect to exercise the session registry, and a
// mixed CRUD loop on a long-lived client to exercise per-request
// retention.
type workload struct {
	cancel context.CancelFunc
	wg     sync.WaitGroup
	cycles atomic.Int64
	errors atomic.Int64
}

func (w *workload) cycleCount() int64 { return w.cycles.Load() }
func (w *workload) errCount() int64   { return w.errors.Load() }

func (w *workload) stop() {
	w.cancel()
	w.wg.Wait()
}

// startWorkload starts the workers and returns immediately. The
// returned workload's stop() cancels and joins all workers.
func startWorkload(parent context.Context, uri string, sessionWorkers, opsWorkers int, sessionDelay, opsInterval time.Duration) (*workload, error) {
	ctx, cancel := context.WithCancel(parent)
	w := &workload{cancel: cancel}

	for i := 0; i < sessionWorkers; i++ {
		w.wg.Add(1)
		go w.runSessionWorker(ctx, uri, sessionDelay)
	}

	collName := fmt.Sprintf("soak_%d", time.Now().UnixNano())
	for i := 0; i < opsWorkers; i++ {
		w.wg.Add(1)
		go w.runOpsWorker(ctx, uri, collName, i, opsInterval)
	}
	return w, nil
}

// runSessionWorker repeatedly connects, pings, and disconnects.
// Drives the session-lifecycle path: every cycle creates a new lsid,
// runs one command, then ends the session.
func (w *workload) runSessionWorker(ctx context.Context, uri string, delay time.Duration) {
	defer w.wg.Done()
	for {
		if ctx.Err() != nil {
			return
		}
		if err := connectPingDisconnect(ctx, uri); err != nil {
			w.errors.Add(1)
		}
		w.cycles.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(delay):
		}
	}
}

func connectPingDisconnect(ctx context.Context, uri string) error {
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		return fmt.Errorf("connect: %w", err)
	}
	defer client.Disconnect(context.Background())
	if err := client.Ping(cctx, nil); err != nil {
		return fmt.Errorf("ping: %w", err)
	}
	return nil
}

// runOpsWorker keeps a long-lived client and rotates through a small
// CRUD mix on a shared collection. Each worker owns a disjoint slice
// of the key space so the workers don't fight each other.
func (w *workload) runOpsWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("soak").Collection(collName)
	r := rand.New(rand.NewSource(int64(workerID) * 1_000_003))

	for {
		if ctx.Err() != nil {
			return
		}
		op := r.Intn(4)
		key := fmt.Sprintf("w%d-k%04d", workerID, r.Intn(100))
		if err := w.runOneOp(ctx, coll, op, key, r); err != nil {
			w.errors.Add(1)
		}
		w.cycles.Add(1)
		select {
		case <-ctx.Done():
			return
		case <-time.After(opsInterval):
		}
	}
}

func (w *workload) runOneOp(ctx context.Context, coll *mongo.Collection, op int, key string, r *rand.Rand) error {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	switch op {
	case 0: // insert (idempotent; an existing _id is treated as success).
		_, err := coll.InsertOne(cctx, makeDoc(key, r))
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return err
	case 1: // findOne
		var out bson.M
		err := coll.FindOne(cctx, bson.M{"_id": key}).Decode(&out)
		if err == mongo.ErrNoDocuments {
			return nil
		}
		return err
	case 2: // update
		_, err := coll.UpdateOne(cctx,
			bson.M{"_id": key},
			bson.M{"$set": bson.M{"score": r.Intn(1000), "updatedAt": time.Now()}})
		return err
	case 3: // delete
		_, err := coll.DeleteOne(cctx, bson.M{"_id": key})
		return err
	}
	return nil
}

// makeDoc emits a small mixed-type document. Payload is constant per
// worker pid so the doc size doesn't vary across cycles -- variance
// in stored bytes would noise up storage observations layered atop a
// soak run.
func makeDoc(key string, r *rand.Rand) bson.M {
	return bson.M{
		"_id":       key,
		"email":     fmt.Sprintf("user-%s@example.invalid", key),
		"tags":      []string{"alpha", "beta", "gamma"},
		"createdAt": time.Now(),
		"score":     r.Intn(1000),
		"payload":   strings.Repeat("x", 200),
	}
}
