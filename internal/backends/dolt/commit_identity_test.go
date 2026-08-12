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
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSplitIdent(t *testing.T) {
	t.Parallel()

	cases := []struct {
		in         string
		name, mail string
	}{
		{"Alice <alice@acme.com>", "Alice", "alice@acme.com"},
		{"alice", "alice", "alice@dumbodb"},
		{"dumbodb <dumbodb@localhost>", "dumbodb", "dumbodb@localhost"},
	}
	for _, tc := range cases {
		n, e := splitIdent(tc.in)
		require.Equal(t, tc.name, n, tc.in)
		require.Equal(t, tc.mail, e, tc.in)
	}
}

func TestCommitterOrAuthor(t *testing.T) {
	t.Parallel()

	require.Equal(t, "a", committerOrAuthor("a", ""))
	require.Equal(t, "c", committerOrAuthor("a", "c"))
}

func TestCommitMetaAC(t *testing.T) {
	t.Parallel()

	t.Run("distinct author and committer", func(t *testing.T) {
		t.Parallel()
		ts := time.Unix(1_700_000_000, 0)
		meta, err := commitMetaAC("Alice <alice@acme.com>", "Bob <bob@acme.com>", "msg", ts)
		require.NoError(t, err)
		require.Equal(t, "Alice", meta.Author.Name)
		require.Equal(t, "alice@acme.com", meta.Author.Email)
		require.Equal(t, "Bob", meta.Committer.Name)
		require.Equal(t, "bob@acme.com", meta.Committer.Email)
		require.Equal(t, ts.Unix(), meta.Author.Date.Time().Unix(), "author date is set")
	})

	t.Run("empty committer defaults to author", func(t *testing.T) {
		t.Parallel()
		meta, err := commitMetaAC("Alice <alice@acme.com>", "", "msg", time.Time{})
		require.NoError(t, err)
		require.Equal(t, "Alice", meta.Author.Name)
		require.Equal(t, "Alice", meta.Committer.Name)
		require.Equal(t, "alice@acme.com", meta.Committer.Email)
	})

	t.Run("bare name gets dumbodb email fallback", func(t *testing.T) {
		t.Parallel()
		meta, err := commitMetaAC("alice", "", "msg", time.Time{})
		require.NoError(t, err)
		require.Equal(t, "alice", meta.Author.Name)
		require.Equal(t, "alice@dumbodb", meta.Author.Email)
	})
}
