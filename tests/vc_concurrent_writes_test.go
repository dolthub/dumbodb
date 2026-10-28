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

package tests

import (
	"context"
	"errors"
	"fmt"
	"math/rand/v2"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// Clients keep writing to main while dumboCommit, merge, cherry-pick, revert
// and rebase run on it (dolthub/dumbodb#167). An operation may be refused --
// the branch has uncommitted changes, or a write landed while it ran -- and is
// then retried, but no write a client was told succeeded may be lost, and every
// operation that reported success must have taken effect.
func TestVCOpsUnderConcurrentWritesKeepEveryAcknowledgedWrite(t *testing.T) {
	const rounds = 25
	const maxAttempts = 400

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("vcstress%d", rand.Int64N(1_000_000))
	main := env.Client.Database(dbName)
	_, err := main.Collection("items").InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	_, err = main.Collection("side").InsertOne(ctx, bson.D{{Key: "_id", Value: "seed"}})
	require.NoError(t, err)
	dumboDBCommit(t, env, dbName, "base", "alice")

	var acked sync.Map
	var ackedCount, failedWrites atomic.Int64
	stop := make(chan struct{})
	var writers sync.WaitGroup
	writerClient := siClient(t, env)
	writerItems := writerClient.Database(dbName).Collection("items")
	stopped := func() bool {
		select {
		case <-stop:
			return true
		default:
			return false
		}
	}
	// Writes come in bursts with random pauses between them. A branch written
	// continuously is never clean, so no operation could start; in a pause one
	// can, and a burst that begins while it runs is the race under test.
	var writing atomic.Bool
	writers.Add(1)
	go func() {
		defer writers.Done()
		for !stopped() {
			writing.Store(true)
			time.Sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
			writing.Store(false)
			time.Sleep(time.Duration(5+rand.IntN(20)) * time.Millisecond)
		}
	}()
	waitToWrite := func() bool {
		for !writing.Load() {
			if stopped() {
				return false
			}
			time.Sleep(500 * time.Microsecond)
		}
		return !stopped()
	}
	for w := 0; w < 2; w++ {
		writers.Add(1)
		go func() {
			defer writers.Done()
			for i := 0; waitToWrite(); i++ {
				id := fmt.Sprintf("plain%d-%d", w, i)
				if _, err := writerItems.InsertOne(ctx, bson.D{{Key: "_id", Value: id}}); err != nil {
					failedWrites.Add(1)
				} else {
					acked.Store(id, true)
					ackedCount.Add(1)
				}
				time.Sleep(time.Millisecond)
			}
		}()
	}
	writers.Add(1)
	go func() {
		defer writers.Done()
		sess, err := writerClient.StartSession()
		if err != nil {
			failedWrites.Add(1)
			return
		}
		defer sess.EndSession(ctx)
		for i := 0; waitToWrite(); i++ {
			id := fmt.Sprintf("txn-%d", i)
			_, err := sess.WithTransaction(ctx, func(sc context.Context) (any, error) {
				return writerItems.InsertOne(sc, bson.D{{Key: "_id", Value: id}})
			})
			if err != nil {
				failedWrites.Add(1)
			} else {
				acked.Store(id, true)
				ackedCount.Add(1)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	defer func() {
		select {
		case <-stop:
		default:
			close(stop)
		}
		writers.Wait()
	}()

	onBranch := func(branch string) *mongo.Database { return env.Client.Database(dbName + "@" + branch) }
	run := func(db *mongo.Database, cmd bson.D) (bson.M, error) {
		var res bson.M
		err := db.RunCommand(ctx, cmd).Decode(&res)
		return res, err
	}
	commit := func(branch, message string) string {
		res, err := run(onBranch(branch), bson.D{
			{Key: "dumboCommit", Value: int32(1)}, {Key: "message", Value: message},
			{Key: "author", Value: "op <op@example.com>"}, {Key: "allowEmpty", Value: true},
		})
		require.NoError(t, err, "dumboCommit on %s", branch)
		return res["commitId"].(string)
	}
	branchWithSideDoc := func(branch, sideID string) string {
		_, err := run(onBranch("main"), bson.D{{Key: "doltBranch", Value: int32(1)}, {Key: "action", Value: "add"}, {Key: "branch", Value: branch}})
		require.NoError(t, err)
		_, err = onBranch(branch).Collection("side").InsertOne(ctx, bson.D{{Key: "_id", Value: sideID}})
		require.NoError(t, err)
		return commit(branch, sideID)
	}
	// refused reports an operation that did nothing and may simply be retried:
	// the branch had uncommitted changes, or a write landed while it ran.
	refused := func(err error) bool {
		var cmdErr mongo.CommandError
		return errors.As(err, &cmdErr) && (cmdErr.Code == 112 || cmdErr.Code == 117)
	}
	attempts := map[string]int{}
	// races counts attempts that started on a clean branch and lost to a write
	// landing while they ran, the case #167 is about.
	races := 0
	retry := func(what string, cmd bson.D) bson.M {
		for attempt := 1; attempt <= maxAttempts; attempt++ {
			commit("main", "before "+what)
			res, err := run(onBranch("main"), cmd)
			if err == nil {
				attempts[strings.Fields(what)[0]] += attempt
				return res
			}
			if mongoCommandCode(err) == 112 {
				races++
			}
			require.True(t, refused(err), "%s failed: %v", what, err)
		}
		t.Fatalf("%s was refused %d times in a row", what, maxAttempts)
		return nil
	}

	wantSide := map[string]bool{"seed": true}
	for r := 0; r < rounds; r++ {
		rebaseOnto := fmt.Sprintf("onto%d", r)
		branchWithSideDoc(rebaseOnto, fmt.Sprintf("rebased%d", r))

		mergeFrom := fmt.Sprintf("merge%d", r)
		branchWithSideDoc(mergeFrom, fmt.Sprintf("merged%d", r))
		retry(fmt.Sprintf("merge %d", r), bson.D{{Key: "dumboMerge", Value: int32(1)}, {Key: "mergeIn", Value: mergeFrom}, {Key: "author", Value: "op <op@example.com>"}})

		pick := branchWithSideDoc(fmt.Sprintf("pick%d", r), fmt.Sprintf("picked%d", r))
		picked := retry(fmt.Sprintf("cherry-pick %d", r), bson.D{{Key: "dumboCherryPick", Value: int32(1)}, {Key: "commit", Value: pick}})

		retry(fmt.Sprintf("revert %d", r), bson.D{{Key: "dumboRevert", Value: int32(1)}, {Key: "commit", Value: picked["commitId"].(string)}, {Key: "author", Value: "op <op@example.com>"}})

		retry(fmt.Sprintf("rebase %d", r), bson.D{{Key: "dumboRebase", Value: int32(1)}, {Key: "onto", Value: rebaseOnto}})

		wantSide[fmt.Sprintf("merged%d", r)] = true
		wantSide[fmt.Sprintf("rebased%d", r)] = true
	}

	close(stop)
	writers.Wait()

	stored := map[string]bool{}
	cur, err := main.Collection("items").Find(ctx, bson.D{})
	require.NoError(t, err)
	var docs []bson.M
	require.NoError(t, cur.All(ctx, &docs))
	for _, d := range docs {
		stored[d["_id"].(string)] = true
	}
	var lost []string
	acked.Range(func(id, _ any) bool {
		if !stored[id.(string)] {
			lost = append(lost, id.(string))
		}
		return true
	})
	t.Logf("acknowledged writes: %d, failed writes: %d, attempts per operation: %v, lost races: %d",
		ackedCount.Load(), failedWrites.Load(), attempts, races)
	require.NotZero(t, ackedCount.Load(), "no write was acknowledged, so this proves nothing")
	require.Empty(t, lost, "%d acknowledged writes were lost", len(lost))

	cur, err = main.Collection("side").Find(ctx, bson.D{})
	require.NoError(t, err)
	docs = nil
	require.NoError(t, cur.All(ctx, &docs))
	gotSide := map[string]bool{}
	for _, d := range docs {
		gotSide[d["_id"].(string)] = true
	}
	require.Equal(t, wantSide, gotSide, "merges and rebases must have landed, and each reverted cherry-pick must be gone")
}
