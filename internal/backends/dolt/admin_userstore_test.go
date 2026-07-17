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

package dolt

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/users"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/util/password"
)

// getUserDoc reads a document from admin.system.users by _id, failing if absent.
func getUserDoc(t *testing.T, b *Backend, id string) *types.Document {
	t.Helper()

	db, err := b.Database("admin")
	require.NoError(t, err)
	coll, err := db.Collection("system.users")
	require.NoError(t, err)

	filter := must.NotFail(types.NewDocument("_id", id))
	qr, err := coll.Query(context.Background(), &backends.QueryParams{Filter: filter})
	require.NoError(t, err)
	defer qr.Iter.Close()

	for {
		_, doc, err := qr.Iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		require.NoError(t, err)

		if got, _ := doc.Get("_id"); got == id {
			return doc
		}
	}

	t.Fatalf("user doc %q not found in committed admin.system.users", id)
	return nil
}

// TestUserStore_CreateUserPersistsAndCommits proves the centralized-admin
// storage decision end to end: createUser writes to admin.system.users, the
// write always commits to admin's main (audit history), and the committed
// document has the expected shape including the SCRAM credential blob.
func TestUserStore_CreateUserPersistsAndCommits(t *testing.T) {
	b, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(b.Close)

	ci := conninfo.New()
	ctx := conninfo.Ctx(context.Background(), ci)

	err = users.CreateUser(ctx, b, &users.CreateUserParams{
		Database: "testdb",
		Username: "alice",
		Password: password.WrapPassword("s3cret-pw"),
		Roles:    must.NotFail(types.NewArray("readWrite")),
	})
	require.NoError(t, err)

	// admin writes always record an auto-commit regardless of server mode;
	// apply them exactly like the handler's AutoCommitBoundary.
	targets := ci.DrainAutoCommit()
	require.NotEmpty(t, targets, "createUser must record an admin auto-commit")

	committed := false
	for _, tgt := range targets {
		require.Equal(t, "admin", tgt.DB, "only admin should auto-commit here")
		ok, err := b.AutoCommit(ctx, tgt.DB, tgt.Branch, tgt.Message)
		require.NoError(t, err)
		committed = committed || ok
	}
	require.True(t, committed, "the user write must produce a commit")

	// The committed document has the expected shape.
	doc := getUserDoc(t, b, "testdb.alice")
	require.Equal(t, "testdb.alice", must.NotFail(doc.Get("_id")))
	require.Equal(t, "testdb", must.NotFail(doc.Get("db")))
	require.Equal(t, "alice", must.NotFail(doc.Get("user")))

	rolesArr, ok := must.NotFail(doc.Get("roles")).(*types.Array)
	require.True(t, ok)
	require.Equal(t, 1, rolesArr.Len())
	role0 := must.NotFail(rolesArr.Get(0)).(*types.Document)
	require.Equal(t, "readWrite", must.NotFail(role0.Get("role")))
	require.Equal(t, "testdb", must.NotFail(role0.Get("db")))

	// SCRAM-SHA-256 credential blob shape.
	creds := must.NotFail(doc.Get("credentials")).(*types.Document)
	sha256 := must.NotFail(creds.Get("SCRAM-SHA-256")).(*types.Document)
	require.Equal(t, int32(15000), must.NotFail(sha256.Get("iterationCount")))
	require.NotEmpty(t, must.NotFail(sha256.Get("salt")))
	require.NotEmpty(t, must.NotFail(sha256.Get("storedKey")))
	require.NotEmpty(t, must.NotFail(sha256.Get("serverKey")))

	// admin's dolt log records the write, confirming durable audit history.
	logRes, err := b.DumboDBLog(ctx, &backends.LogParams{DBName: "admin", Branch: "main"})
	require.NoError(t, err)
	require.NotEmpty(t, logRes.Commits, "admin main log must show the user write")
}
