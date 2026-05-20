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

package conninfo

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestLSIDRoundTrip(t *testing.T) {
	c := New()
	assert.Equal(t, "", c.LSID())

	c.SetLSID("abc-123")
	assert.Equal(t, "abc-123", c.LSID())

	c.SetLSID("def-456")
	assert.Equal(t, "def-456", c.LSID())
}

func TestOwnerPrefersLSID(t *testing.T) {
	c := New()
	c.SetLSID("real-lsid-uuid")
	assert.Equal(t, "real-lsid-uuid", c.Owner())
}

func TestOwnerFallsBackToConnSyntheticID(t *testing.T) {
	c1 := New()
	c2 := New()

	o1a := c1.Owner()
	o1b := c1.Owner()
	o2 := c2.Owner()

	assert.True(t, strings.HasPrefix(o1a, "conn:"),
		"synthetic owner should be unambiguous: got %q", o1a)
	assert.Equal(t, o1a, o1b, "Owner must be stable across calls on same ConnInfo")
	assert.NotEqual(t, o1a, o2, "Owner must distinguish different connections")
}

func TestInTransactionRoundTrip(t *testing.T) {
	c := New()
	assert.False(t, c.InTransaction(), "fresh ConnInfo must default to not-in-transaction")

	c.SetInTransaction(true)
	assert.True(t, c.InTransaction())

	c.SetInTransaction(false)
	assert.False(t, c.InTransaction())
}

func TestSetLSIDOverridesFallback(t *testing.T) {
	c := New()
	syntheticOwner := c.Owner()
	assert.True(t, strings.HasPrefix(syntheticOwner, "conn:"))

	c.SetLSID("real-lsid")
	assert.Equal(t, "real-lsid", c.Owner(),
		"Owner must prefer lsid once it is set, not the synthetic fallback")
}

// .6.4.7 tests below.

func TestCachedShadow_DefaultsToNil(t *testing.T) {
	c := New()
	s, lsid := c.CachedShadow()
	assert.Nil(t, s)
	assert.Equal(t, "", lsid)
}

func TestCachedShadow_RoundTrip(t *testing.T) {
	c := New()
	s, _ := c.CachedShadow()
	require.Nil(t, s)

	c.SetCachedShadow("some-lsid", nil) // explicit nil is a valid clear
	s, lsid := c.CachedShadow()
	assert.Nil(t, s)
	assert.Equal(t, "some-lsid", lsid)
}

func TestEnsureLSID_GeneratesSyntheticOnEmpty(t *testing.T) {
	c := New()
	require.Equal(t, "", c.LSID())

	got := c.EnsureLSID()
	assert.True(t, strings.HasPrefix(got, "synthetic:"), "EnsureLSID must prefix server-generated ids with 'synthetic:'; got %q", got)
	assert.Equal(t, got, c.LSID(), "EnsureLSID must persist the generated id on the ConnInfo")
	assert.Greater(t, len(got), len("synthetic:"), "synthetic id must include random bytes after the prefix")
}

func TestEnsureLSID_NoopOnDriverSupplied(t *testing.T) {
	c := New()
	c.SetLSID("driver-supplied-id")

	got := c.EnsureLSID()
	assert.Equal(t, "driver-supplied-id", got, "EnsureLSID must not overwrite a driver-supplied lsid")
	assert.Equal(t, "driver-supplied-id", c.LSID())
}

func TestEnsureLSID_OwnerPrefersSyntheticOverConnFallback(t *testing.T) {
	c := New()
	require.True(t, strings.HasPrefix(c.Owner(), "conn:"), "before EnsureLSID, Owner uses conn:%p")

	id := c.EnsureLSID()
	assert.Equal(t, id, c.Owner(), "after EnsureLSID, Owner returns the synthetic lsid")
	assert.True(t, strings.HasPrefix(c.Owner(), "synthetic:"))
}

func TestEnsureLSID_StableAcrossCalls(t *testing.T) {
	c := New()
	first := c.EnsureLSID()
	second := c.EnsureLSID()
	assert.Equal(t, first, second, "subsequent EnsureLSID calls must return the same id")
}
