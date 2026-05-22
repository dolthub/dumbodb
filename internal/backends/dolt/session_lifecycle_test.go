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
	"io"
	"log/slog"
	"testing"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
)

func TestBackendImplementsSessionAwareBackend(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	var _ backends.SessionAwareBackend = be
}

func TestDocLockManagerIsPerDbBranch(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	mA := be.docLockManager("mydb", "main")
	mAagain := be.docLockManager("mydb", "main")
	mB := be.docLockManager("mydb", "feat")
	mC := be.docLockManager("otherdb", "main")

	assert.Same(t, mA, mAagain, "same (db,branch) must return the same manager")
	assert.NotSame(t, mA, mB, "different branches must have different managers")
	assert.NotSame(t, mA, mC, "different dbs must have different managers")
}

func TestOnSessionEndReleasesAllLocks(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	var idA, idB, idC hash.Hash
	idA[0] = 0x01
	idB[0] = 0x02
	idC[0] = 0x03

	mMain := be.docLockManager("mydb", "main")
	mFeat := be.docLockManager("mydb", "feat")
	require.NoError(t, mMain.Acquire("ownerX", "col", []hash.Hash{idA}, LockKindUpdate))
	require.NoError(t, mFeat.Acquire("ownerX", "col", []hash.Hash{idB}, LockKindUpdate))

	require.NoError(t, mMain.Acquire("ownerY", "col", []hash.Hash{idC}, LockKindUpdate))

	be.OnSessionEnd("ownerX")

	assert.False(t, mMain.Holds("ownerX", "col", idA), "ownerX's mydb/main lock should be released")
	assert.False(t, mFeat.Holds("ownerX", "col", idB), "ownerX's mydb/feat lock should be released")
	assert.True(t, mMain.Holds("ownerY", "col", idC), "ownerY's lock must remain")
}

func TestOnSessionEndUnknownOwnerIsNoop(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	be.OnSessionEnd("ownerZ")

	var idA hash.Hash
	idA[0] = 0x01
	m := be.docLockManager("mydb", "main")
	require.NoError(t, m.Acquire("ownerX", "col", []hash.Hash{idA}, LockKindUpdate))
	be.OnSessionEnd("ownerZ")
	assert.True(t, m.Holds("ownerX", "col", idA), "ownerX's lock untouched by unrelated OnSessionEnd")
}
