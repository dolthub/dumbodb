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
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/require"
)

func TestBackgroundGCRootsProvider_PinnedRootsAreRefCountedPerDB(t *testing.T) {
	var p backgroundGCRootsProvider
	root := hash.Of([]byte("root"))

	visited := func(db string) []hash.Hash {
		var got []hash.Hash
		require.NoError(t, p.VisitGCRoots(context.Background(), db, func(h hash.Hash) bool {
			got = append(got, h)
			return false
		}))
		return got
	}

	releaseA := p.pinRoot("db1", root)
	releaseB := p.pinRoot("db1", root)
	require.Equal(t, []hash.Hash{root}, visited("db1"))
	require.Empty(t, visited("db2"))

	releaseA()
	releaseA()
	require.Equal(t, []hash.Hash{root}, visited("db1"), "a second release of the same pin is a no-op")

	releaseB()
	require.Empty(t, visited("db1"))
}

func TestBackgroundGCRootsProvider_ReportsUnkeepableRoot(t *testing.T) {
	var p backgroundGCRootsProvider
	release := p.pinRoot("db1", hash.Of([]byte("root")))
	defer release()

	err := p.VisitGCRoots(context.Background(), "db1", func(hash.Hash) bool { return true })
	require.Error(t, err)
}
