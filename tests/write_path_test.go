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
	"fmt"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"
)

// A write reaches the reconciliation path only when a session transaction is
// live (internal/backends/dolt/helpers.go, updateWorkingRoot). Nothing starts
// one for an ordinary write, so an ordinary write publishes a whole working set
// computed from a snapshot taken before any lock, and a concurrent writer's
// acknowledged change is overwritten.
//
// These tests run the same workload down both paths. See
// docs/design/write-path-audit.md.

type disjointWriteRound struct {
	matchedA, matchedB int64
	errA, errB         error
	storedA, storedB   any
}

// lostAnAcknowledgedWrite reports whether a write the server acknowledged is
// absent from the stored document.
func (r disjointWriteRound) lostAnAcknowledgedWrite() bool {
	if r.matchedA == 1 && r.storedA != "A" {
		return true
	}
	return r.matchedB == 1 && r.storedB != "B"
}

func (r disjointWriteRound) String() string {
	return fmt.Sprintf("matchedA=%d errA=%v matchedB=%d errB=%v stored={a:%v b:%v}",
		r.matchedA, r.errA, r.matchedB, r.errB, r.storedA, r.storedB)
}

// runDisjointWrites releases two writers at the same instant against one
// document, each owning a different field.
func runDisjointWrites(t *testing.T, ctx context.Context, collA, collB *mongo.Collection, id string) disjointWriteRound {
	t.Helper()

	filter := bson.D{{Key: "_id", Value: id}}
	start := make(chan struct{})
	var writers sync.WaitGroup
	var round disjointWriteRound

	write := func(coll *mongo.Collection, field, value string, matched *int64, failure *error) {
		defer writers.Done()
		<-start
		result, err := coll.UpdateOne(ctx, filter,
			bson.D{{Key: "$set", Value: bson.D{{Key: field, Value: value}}}})
		if err != nil {
			*failure = err
			return
		}
		*matched = result.MatchedCount
	}

	writers.Add(2)
	go write(collA, "a", "A", &round.matchedA, &round.errA)
	go write(collB, "b", "B", &round.matchedB, &round.errB)
	close(start)
	writers.Wait()

	var stored bson.M
	require.NoError(t, collA.FindOne(ctx, filter).Decode(&stored))
	round.storedA, round.storedB = stored["a"], stored["b"]
	return round
}

// An acknowledged write must not vanish. Two clients own different fields of
// one document, so there is nothing for them to disagree about.
func TestWritePath_OrdinaryWritesKeepAcknowledgedWrites(t *testing.T) {
	const rounds = 40

	env := startDumboDB(t)
	ctx := context.Background()
	collA := siClient(t, env).Database("write_path").Collection("documents")
	collB := siClient(t, env).Database("write_path").Collection("documents")

	lost := 0
	var firstLoss disjointWriteRound
	for round := 0; round < rounds; round++ {
		id := fmt.Sprintf("ordinary-%d", round)
		_, err := collA.InsertOne(ctx, bson.D{
			{Key: "_id", Value: id},
			{Key: "a", Value: "seed"},
			{Key: "b", Value: "seed"},
		})
		require.NoError(t, err)

		result := runDisjointWrites(t, ctx, collA, collB, id)
		require.NoError(t, result.errA)
		require.NoError(t, result.errB)
		if result.lostAnAcknowledgedWrite() {
			if lost == 0 {
				firstLoss = result
			}
			lost++
		}
	}

	require.Zero(t, lost,
		"%d of %d rounds lost an acknowledged write; first: %s", lost, rounds, firstLoss)
}

// The same workload inside explicit transactions, which do reach the
// reconciliation path. This is the control: it isolates the loss above to the
// path an ordinary write takes rather than to the workload.
func TestWritePath_TransactionalWritesKeepAcknowledgedWrites(t *testing.T) {
	const rounds = 20

	env := startDumboDB(t)
	ctx := context.Background()
	setup := siClient(t, env).Database("write_path_txn").Collection("documents")

	clientA, clientB := siClient(t, env), siClient(t, env)
	sessA, err := clientA.StartSession()
	require.NoError(t, err)
	defer sessA.EndSession(ctx)
	sessB, err := clientB.StartSession()
	require.NoError(t, err)
	defer sessB.EndSession(ctx)

	lost, acknowledged := 0, 0
	var firstLoss disjointWriteRound
	for round := 0; round < rounds; round++ {
		id := fmt.Sprintf("txn-%d", round)
		_, err := setup.InsertOne(ctx, bson.D{
			{Key: "_id", Value: id},
			{Key: "a", Value: "seed"},
			{Key: "b", Value: "seed"},
		})
		require.NoError(t, err)

		result := transactionalDisjointWrites(t, ctx, clientA, sessA, clientB, sessB, id)
		acknowledged += int(result.matchedA + result.matchedB)

		// A rejected transaction is a correct outcome: the client is told. A
		// silently discarded acknowledged write is not.
		if result.lostAnAcknowledgedWrite() {
			if lost == 0 {
				firstLoss = result
			}
			lost++
		}
	}

	// Without this the test passes vacuously: if every transaction were
	// rejected, no write would be acknowledged and nothing could be lost.
	require.NotZero(t, acknowledged,
		"no transactional write was acknowledged, so this proves nothing")
	t.Logf("acknowledged transactional writes: %d over %d rounds", acknowledged, rounds)

	require.Zero(t, lost,
		"%d of %d transactional rounds lost an acknowledged write; first: %s", lost, rounds, firstLoss)
}

func transactionalDisjointWrites(
	t *testing.T,
	ctx context.Context,
	clientA *mongo.Client, sessA *mongo.Session,
	clientB *mongo.Client, sessB *mongo.Session,
	id string,
) disjointWriteRound {
	t.Helper()

	filter := bson.D{{Key: "_id", Value: id}}
	var round disjointWriteRound
	var writers sync.WaitGroup
	start := make(chan struct{})

	write := func(client *mongo.Client, sess *mongo.Session, field, value string, matched *int64, failure *error) {
		defer writers.Done()
		<-start
		*failure = mongo.WithSession(ctx, sess, func(sc context.Context) error {
			if err := sess.StartTransaction(options.Transaction()); err != nil {
				return err
			}
			result, err := client.Database("write_path_txn").Collection("documents").
				UpdateOne(sc, filter, bson.D{{Key: "$set", Value: bson.D{{Key: field, Value: value}}}})
			if err != nil {
				_ = sess.AbortTransaction(ctx)
				return err
			}
			if err := sess.CommitTransaction(ctx); err != nil {
				return err
			}
			*matched = result.MatchedCount
			return nil
		})
	}

	writers.Add(2)
	go write(clientA, sessA, "a", "A", &round.matchedA, &round.errA)
	go write(clientB, sessB, "b", "B", &round.matchedB, &round.errB)
	close(start)
	writers.Wait()

	// A writer whose transaction did not commit did not have its write
	// acknowledged, so it makes no claim on stored state.
	if round.errA != nil {
		round.matchedA = 0
	}
	if round.errB != nil {
		round.matchedB = 0
	}

	var stored bson.M
	require.NoError(t, clientA.Database("write_path_txn").Collection("documents").
		FindOne(ctx, filter).Decode(&stored))
	round.storedA, round.storedB = stored["a"], stored["b"]
	return round
}
