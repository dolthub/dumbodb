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

package handler

import (
	"errors"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func errorCode(t *testing.T, err error) handlererrors.ErrorCode {
	t.Helper()
	var ce *handlererrors.CommandError
	require.True(t, errors.As(err, &ce), "expected a CommandError, got %v", err)
	return ce.Code()
}

func TestValidateCommitIdentity(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name    string
		cname   string
		email   string
		wantErr bool
	}{
		{"ok", "Alice Example", "alice@acme.com", false},
		{"ok subdomain", "a", "a.b@x.y.z", false},
		{"empty name", "", "alice@acme.com", true},
		{"name has angle", "Al <ice", "alice@acme.com", true},
		{"empty email", "Alice", "", true},
		{"email no at", "Alice", "aliceacme.com", true},
		{"email two at", "Alice", "a@b@acme.com", true},
		{"email empty local", "Alice", "@acme.com", true},
		{"email empty domain", "Alice", "alice@", true},
		{"email space", "Alice", "ali ce@acme.com", true},
		{"email angle", "Alice", "<alice@acme.com>", true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := validateCommitIdentity(tc.cname, tc.email)
			if tc.wantErr {
				require.Error(t, err)
				require.Equal(t, handlererrors.ErrBadValue, errorCode(t, err))
			} else {
				require.NoError(t, err)
			}
		})
	}
}

func TestCommitIdentityWithFallback(t *testing.T) {
	t.Parallel()

	cases := []struct {
		name                string
		inName, inEmail     string
		user, db            string
		wantName, wantEmail string
	}{
		{"full identity kept", "Alice", "alice@acme.com", "alice", "appid", "Alice", "alice@acme.com"},
		{"unset falls back fully", "", "", "bob", "store", "bob", "bob@store"},
		{"name only fills email", "Bob B", "", "bob", "store", "Bob B", "bob@store"},
		{"email only fills name", "", "b@x.io", "bob", "store", "bob", "b@x.io"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			gotName, gotEmail := commitIdentityWithFallback(tc.inName, tc.inEmail, tc.user, tc.db)
			require.Equal(t, tc.wantName, gotName)
			require.Equal(t, tc.wantEmail, gotEmail)
		})
	}
}

func TestParseCommitIdentity(t *testing.T) {
	t.Parallel()

	t.Run("absent", func(t *testing.T) {
		t.Parallel()
		doc := must.NotFail(types.NewDocument("createUser", "u"))
		got, err := parseCommitIdentity(doc)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("null", func(t *testing.T) {
		t.Parallel()
		doc := must.NotFail(types.NewDocument("commitIdentity", types.Null))
		got, err := parseCommitIdentity(doc)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("empty doc treated as absent", func(t *testing.T) {
		t.Parallel()
		doc := must.NotFail(types.NewDocument("commitIdentity", must.NotFail(types.NewDocument())))
		got, err := parseCommitIdentity(doc)
		require.NoError(t, err)
		require.Nil(t, got)
	})

	t.Run("full identity", func(t *testing.T) {
		t.Parallel()
		id := must.NotFail(types.NewDocument("name", "Alice", "email", "alice@acme.com"))
		doc := must.NotFail(types.NewDocument("commitIdentity", id))
		got, err := parseCommitIdentity(doc)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.Equal(t, "Alice", must.NotFail(got.Get("name")))
		require.Equal(t, "alice@acme.com", must.NotFail(got.Get("email")))
	})

	t.Run("name only", func(t *testing.T) {
		t.Parallel()
		id := must.NotFail(types.NewDocument("name", "Alice"))
		doc := must.NotFail(types.NewDocument("commitIdentity", id))
		got, err := parseCommitIdentity(doc)
		require.NoError(t, err)
		require.NotNil(t, got)
		require.True(t, got.Has("name"))
		require.False(t, got.Has("email"))
	})

	t.Run("unknown subfield", func(t *testing.T) {
		t.Parallel()
		id := must.NotFail(types.NewDocument("name", "Alice", "nickname", "Al"))
		doc := must.NotFail(types.NewDocument("commitIdentity", id))
		_, err := parseCommitIdentity(doc)
		require.Error(t, err)
		require.Equal(t, handlererrors.ErrBadValue, errorCode(t, err))
	})

	t.Run("wrong type", func(t *testing.T) {
		t.Parallel()
		doc := must.NotFail(types.NewDocument("commitIdentity", "alice"))
		_, err := parseCommitIdentity(doc)
		require.Error(t, err)
		require.Equal(t, handlererrors.ErrTypeMismatch, errorCode(t, err))
	})

	t.Run("invalid email rejected", func(t *testing.T) {
		t.Parallel()
		id := must.NotFail(types.NewDocument("name", "Alice", "email", "bad"))
		doc := must.NotFail(types.NewDocument("commitIdentity", id))
		_, err := parseCommitIdentity(doc)
		require.Error(t, err)
		require.Equal(t, handlererrors.ErrBadValue, errorCode(t, err))
	})

	t.Run("subfield wrong type", func(t *testing.T) {
		t.Parallel()
		id := must.NotFail(types.NewDocument("name", int32(5)))
		doc := must.NotFail(types.NewDocument("commitIdentity", id))
		_, err := parseCommitIdentity(doc)
		require.Error(t, err)
		require.Equal(t, handlererrors.ErrTypeMismatch, errorCode(t, err))
	})
}
