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
	"sync/atomic"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// connectPingDisconnect drives one short-lived client lifecycle.
// Used by the session-churn worker pool to exercise the session
// registry and lsid release paths.
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

// runSessionWorker repeatedly connects, pings, and disconnects.
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
		if !sleep(ctx, delay) {
			return
		}
	}
}

// runCRUDWorker keeps one long-lived client and rotates through
// InsertOne / FindOne / UpdateOne / DeleteOne on docs of randomly
// chosen size. Each worker owns a disjoint slice of the key space
// so workers don't fight each other but their docs still share the
// same collection.
func (w *workload) runCRUDWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("soak").Collection(collName)
	r := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 7))

	for {
		if ctx.Err() != nil {
			return
		}
		op := r.Intn(4)
		key := fmt.Sprintf("crud-w%d-k%05d", workerID, r.Intn(500))
		if err := w.runOneCRUD(ctx, coll, op, key, r); err != nil {
			w.errors.Add(1)
		}
		w.cycles.Add(1)
		if !sleep(ctx, opsInterval) {
			return
		}
	}
}

func (w *workload) runOneCRUD(ctx context.Context, coll *mongo.Collection, op int, key string, r *rand.Rand) error {
	cctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	switch op {
	case 0:
		_, err := coll.InsertOne(cctx, makeDoc(r, key, pickSize(r)))
		if mongo.IsDuplicateKeyError(err) {
			return nil
		}
		return err
	case 1:
		var out bson.M
		err := coll.FindOne(cctx, bson.M{"_id": key}).Decode(&out)
		if err == mongo.ErrNoDocuments {
			return nil
		}
		return err
	case 2:
		_, err := coll.UpdateOne(cctx,
			bson.M{"_id": key},
			bson.M{"$set": bson.M{"score": r.Intn(1000), "updatedAt": time.Now()}})
		return err
	case 3:
		_, err := coll.DeleteOne(cctx, bson.M{"_id": key})
		return err
	}
	return nil
}

// runBulkWorker batches docs of mixed size via InsertMany. Exercises
// the bulk-write wire frame and the chunker on the large batches.
func (w *workload) runBulkWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("soak").Collection(collName)
	r := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 19))
	batchSeq := atomic.Int64{}

	for {
		if ctx.Err() != nil {
			return
		}
		// Batch size 25..200, mixed doc sizes within the batch.
		n := 25 + r.Intn(176)
		docs := make([]any, n)
		seq := batchSeq.Add(1)
		for i := range docs {
			key := fmt.Sprintf("bulk-w%d-b%05d-i%04d", workerID, seq, i)
			docs[i] = makeDoc(r, key, pickSize(r))
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err := coll.InsertMany(cctx, docs, options.InsertMany().SetOrdered(false))
		cancel()
		if err != nil {
			w.errors.Add(1)
		}
		w.cycles.Add(1)
		// Bulk inserts are heavier; back off proportionally.
		if !sleep(ctx, opsInterval*5) {
			return
		}
	}
}

// runAggWorker runs an aggregation pipeline against the shared
// collection and drains the cursor. Pipeline shape varies between
// cycles so we exercise $match, $group, $sort, $project, $limit.
func (w *workload) runAggWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("soak").Collection(collName)
	r := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 31))

	for {
		if ctx.Err() != nil {
			return
		}
		pipeline := randomPipeline(r)
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		cur, err := coll.Aggregate(cctx, pipeline)
		if err == nil {
			// Drain the cursor; the iteration is the point.
			for cur.Next(cctx) {
			}
			cur.Close(cctx)
		} else {
			w.errors.Add(1)
		}
		cancel()
		w.cycles.Add(1)
		if !sleep(ctx, opsInterval*2) {
			return
		}
	}
}

func randomPipeline(r *rand.Rand) mongo.Pipeline {
	switch r.Intn(4) {
	case 0:
		return mongo.Pipeline{
			{{"$match", bson.M{"score": bson.M{"$gte": r.Intn(500)}}}},
			{{"$count", "n"}},
		}
	case 1:
		return mongo.Pipeline{
			{{"$match", bson.M{"score": bson.M{"$gte": r.Intn(500)}}}},
			{{"$group", bson.M{"_id": "$tags", "avgScore": bson.M{"$avg": "$score"}, "n": bson.M{"$sum": 1}}}},
			{{"$sort", bson.M{"n": -1}}},
			{{"$limit", 20}},
		}
	case 2:
		return mongo.Pipeline{
			{{"$sort", bson.M{"score": -1}}},
			{{"$limit", 50}},
			{{"$project", bson.M{"email": 1, "score": 1}}},
		}
	default:
		return mongo.Pipeline{
			{{"$match", bson.M{"createdAt": bson.M{"$exists": true}}}},
			{{"$project", bson.M{"_id": 1, "score": 1}}},
			{{"$limit", 100}},
		}
	}
}

// runIndexedWorker creates a secondary index on the shared
// collection, runs indexed find queries through it, and periodically
// drops + recreates it. Exercises createIndexes / listIndexes /
// dropIndexes and the indexed-query planner path.
func (w *workload) runIndexedWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("soak").Collection(collName)
	r := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 53))
	idxName := fmt.Sprintf("score_w%d", workerID)

	cycle := 0
	for {
		if ctx.Err() != nil {
			return
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		// Refresh the index every 20 cycles so we exercise create/drop.
		if cycle%20 == 0 {
			_ = coll.Indexes().DropOne(cctx, idxName)
			_, err := coll.Indexes().CreateOne(cctx, mongo.IndexModel{
				Keys:    bson.D{{"score", 1}},
				Options: options.Index().SetName(idxName),
			})
			if err != nil {
				w.errors.Add(1)
			}
		}
		lo := r.Intn(900)
		cur, err := coll.Find(cctx, bson.M{"score": bson.M{"$gte": lo, "$lt": lo + 100}}, options.Find().SetLimit(50))
		if err == nil {
			for cur.Next(cctx) {
			}
			cur.Close(cctx)
		} else {
			w.errors.Add(1)
		}
		cancel()
		cycle++
		w.cycles.Add(1)
		if !sleep(ctx, opsInterval) {
			return
		}
	}
}

// runVCSWorker cycles through commit / branch / commit-on-branch /
// merge. Branch addressing uses dumbodb's "db@branchname" convention,
// passed straight to client.Database() so the mongo URI parser never
// sees the '@' (which it would treat as a user:pass separator).
func (w *workload) runVCSWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	r := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 71))
	mainDB := client.Database("soak")
	mainColl := mainDB.Collection(collName)
	branchSeq := atomic.Int64{}

	for {
		if ctx.Err() != nil {
			return
		}
		// Commit on main.
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("vcs-w%d-main-%d", workerID, time.Now().UnixNano())
			_, _ = mainColl.InsertOne(ctx, makeDoc(r, key, pickSize(r)))
		}
		if err := runCmd(ctx, mainDB, 30*time.Second, bson.D{{"dumboCommit", 1}, {"message", fmt.Sprintf("vcs-w%d main commit", workerID)}}); err != nil {
			w.errors.Add(1)
		}

		// Create + populate + commit on the feature branch.
		branch := fmt.Sprintf("vcs-w%d-b%d", workerID, branchSeq.Add(1))
		if err := runCmd(ctx, mainDB, 30*time.Second, bson.D{{"dumboBranch", 1}, {"branch", branch}}); err != nil {
			w.errors.Add(1)
		}
		branchDB := client.Database("soak@" + branch)
		branchColl := branchDB.Collection(collName)
		for i := 0; i < 3; i++ {
			key := fmt.Sprintf("vcs-w%d-%s-%d", workerID, branch, time.Now().UnixNano())
			_, _ = branchColl.InsertOne(ctx, makeDoc(r, key, pickSize(r)))
		}
		if err := runCmd(ctx, branchDB, 30*time.Second, bson.D{{"dumboCommit", 1}, {"message", "branch commit"}}); err != nil {
			w.errors.Add(1)
		}

		// Merge the branch back to main. Conflicts can arise when
		// other workers (or other VCS workers) touched main between
		// the branch-off and the merge; those count as errors.
		if err := runCmd(ctx, mainDB, 60*time.Second, bson.D{
			{"dumboMerge", 1},
			{"merge_in", branch},
			{"message", fmt.Sprintf("soak merge of %s", branch)},
			{"author", "soak <soak@dumbodb>"},
		}); err != nil {
			w.errors.Add(1)
		}

		w.cycles.Add(1)
		if !sleep(ctx, opsInterval*30) {
			return
		}
	}
}

// runCmd is a small helper that scopes a timeout, dispatches the
// command, and returns the wire-level error.
func runCmd(ctx context.Context, db *mongo.Database, timeout time.Duration, cmd bson.D) error {
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	return db.RunCommand(cctx, cmd).Err()
}

// runTxnWorker runs a short multi-op session.WithTransaction block.
// dumbodb accepts the transaction wire commands and tracks the
// session state through them even though it does not implement full
// multi-doc ACID semantics; the soak's job is to keep that path warm.
func (w *workload) runTxnWorker(ctx context.Context, uri, collName string, workerID int, opsInterval time.Duration) {
	defer w.wg.Done()
	client, err := mongo.Connect(options.Client().ApplyURI(uri))
	if err != nil {
		w.errors.Add(1)
		return
	}
	defer client.Disconnect(context.Background())
	coll := client.Database("soak").Collection(collName)
	r := rand.New(rand.NewSource(int64(workerID)*1_000_003 + 97))

	for {
		if ctx.Err() != nil {
			return
		}
		sess, err := client.StartSession()
		if err != nil {
			w.errors.Add(1)
			if !sleep(ctx, opsInterval) {
				return
			}
			continue
		}
		cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
		_, err = sess.WithTransaction(cctx, func(sctx context.Context) (any, error) {
			n := 3 + r.Intn(15)
			for i := 0; i < n; i++ {
				key := fmt.Sprintf("txn-w%d-i%d-%d", workerID, i, time.Now().UnixNano())
				if _, err := coll.InsertOne(sctx, makeDoc(r, key, pickSize(r))); err != nil {
					return nil, err
				}
			}
			return nil, nil
		})
		cancel()
		sess.EndSession(context.Background())
		if err != nil {
			w.errors.Add(1)
		}
		w.cycles.Add(1)
		if !sleep(ctx, opsInterval*3) {
			return
		}
	}
}

// sleep returns false if ctx fires before the delay elapses, so the
// caller can exit its loop cleanly.
func sleep(ctx context.Context, d time.Duration) bool {
	if d <= 0 {
		return ctx.Err() == nil
	}
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
