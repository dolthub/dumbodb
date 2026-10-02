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
	"log/slog"
	"math/rand/v2"
	"sync"
	"testing"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/FerretDB/wire/wireclient"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
)

// One lsid on several connections must keep working. MongoDB drivers pool
// logical sessions independently of connections, so an implicit session is
// handed to whichever connection needs it and one lsid reaches many
// connections over the life of a client. That is ordinary, not a takeover.
//
// Sending one lsid from several raw connections at once is the stand-in: it
// is the same server-side condition a pooled implicit session produces over
// time, and it produces it reliably instead of waiting on pool timing. Raw
// connections are used because a driver Session is not safe for concurrent
// use and the driver has no way to send a caller-chosen lsid.
//
// Before the fix this failed 302 of 320 operations with code 225, "session was
// taken over by a newer connection on this lsid".
func TestSession_OneLsidAcrossManyConnections(t *testing.T) {
	const workers, perWorker = 8, 40

	env := startDumboDB(t)
	ctx := context.Background()
	dbName := fmt.Sprintf("lsid_%d", rand.Int64N(1_000_000))
	lsidUUID := uuid.New()
	lsid := wirebson.MustDocument("id", wirebson.Binary{B: lsidUUID[:], Subtype: wirebson.BinaryUUID})

	failures := make([][]error, workers)

	var writers sync.WaitGroup
	for w := 0; w < workers; w++ {
		conn, err := wireclient.Connect(ctx, fmt.Sprintf("mongodb://127.0.0.1:%d/", env.Port), slog.New(slog.DiscardHandler))
		require.NoError(t, err)
		t.Cleanup(func() { _ = conn.Close() })

		writers.Add(1)
		go func(w int) {
			defer writers.Done()
			for i := 0; i < perWorker; i++ {
				if err := insertWithLSID(ctx, conn, dbName, lsid, int32(w*1000+i)); err != nil {
					failures[w] = append(failures[w], err)
				}
			}
		}(w)
	}
	writers.Wait()

	failed := 0
	var first error
	for _, errs := range failures {
		for _, err := range errs {
			if first == nil {
				first = err
			}
			failed++
		}
	}
	require.Zero(t, failed,
		"%d of %d writes failed on a shared lsid; first: %v", failed, workers*perWorker, first)

	count, err := siClient(t, env).Database(dbName).Collection("docs").CountDocuments(ctx, bson.D{})
	require.NoError(t, err)
	require.EqualValues(t, workers*perWorker, count, "every acknowledged insert must be stored")
}

func insertWithLSID(ctx context.Context, conn *wireclient.Conn, dbName string, lsid *wirebson.Document, id int32) error {
	_, resBody, err := conn.Request(ctx, wire.MustOpMsg(
		"insert", "docs",
		"documents", wirebson.MustArray(wirebson.MustDocument("_id", id)),
		"lsid", lsid,
		"$db", dbName,
	))
	if err != nil {
		return err
	}
	raw, err := resBody.(*wire.OpMsg).RawDocument()
	if err != nil {
		return err
	}
	res, err := raw.Decode()
	if err != nil {
		return err
	}
	if ok, _ := res.Get("ok").(float64); ok != 1 || res.Get("writeErrors") != nil {
		return fmt.Errorf("insert %d failed: %v", id, res)
	}
	return nil
}
