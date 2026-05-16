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

// TestBackendImplementsSessionAwareBackend is a compile-time check that the
// dolt backend satisfies the optional session-lifecycle interface so the
// handler's type assertion succeeds.
func TestBackendImplementsSessionAwareBackend(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	var _ backends.SessionAwareBackend = be
}

// TestDocLockManagerIsPerDbBranch verifies that calling docLockManager with
// different (db, branch) tuples returns distinct managers, and calling
// again with the same tuple returns the same one (caching).
func TestDocLockManagerIsPerDbBranch(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
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

// TestOnSessionEndReleasesAllLocks verifies that OnSessionEnd releases
// every lock the owner holds, across every DocLockManager, leaving other
// owners' locks untouched. This is the .3.7 acceptance: "endSession ...
// releases any held doc locks."
func TestOnSessionEndReleasesAllLocks(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	var idA, idB, idC hash.Hash
	idA[0] = 0x01
	idB[0] = 0x02
	idC[0] = 0x03

	// ownerX locks one doc on each of two (db,branch) tuples.
	mMain := be.docLockManager("mydb", "main")
	mFeat := be.docLockManager("mydb", "feat")
	require.NoError(t, mMain.Acquire("ownerX", "col", []hash.Hash{idA}))
	require.NoError(t, mFeat.Acquire("ownerX", "col", []hash.Hash{idB}))

	// ownerY locks one doc that ownerX shouldn't touch on release.
	require.NoError(t, mMain.Acquire("ownerY", "col", []hash.Hash{idC}))

	be.OnSessionEnd("ownerX")

	assert.False(t, mMain.Holds("ownerX", "col", idA), "ownerX's mydb/main lock should be released")
	assert.False(t, mFeat.Holds("ownerX", "col", idB), "ownerX's mydb/feat lock should be released")
	assert.True(t, mMain.Holds("ownerY", "col", idC), "ownerY's lock must remain")
}

// TestOnSessionEndUnknownOwnerIsNoop -- ending a session that never
// acquired any locks must not error, matching the idempotency requirement.
func TestOnSessionEndUnknownOwnerIsNoop(t *testing.T) {
	be, err := newBackend(t.TempDir(), slog.New(slog.NewTextHandler(io.Discard, nil)), false)
	require.NoError(t, err)
	t.Cleanup(be.Close)

	// No locks at all -- just confirm no panic/error.
	be.OnSessionEnd("ownerZ")

	// And with one DocLockManager present but no relevant owner.
	var idA hash.Hash
	idA[0] = 0x01
	m := be.docLockManager("mydb", "main")
	require.NoError(t, m.Acquire("ownerX", "col", []hash.Hash{idA}))
	be.OnSessionEnd("ownerZ")
	assert.True(t, m.Holds("ownerX", "col", idA), "ownerX's lock untouched by unrelated OnSessionEnd")
}
