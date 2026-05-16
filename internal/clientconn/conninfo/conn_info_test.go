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
)

// TestLSIDRoundTrip verifies basic set/get on the lsid field.
func TestLSIDRoundTrip(t *testing.T) {
	c := New()
	assert.Equal(t, "", c.LSID())

	c.SetLSID("abc-123")
	assert.Equal(t, "abc-123", c.LSID())

	// Overwriting is fine -- handlers update lsid on every command.
	c.SetLSID("def-456")
	assert.Equal(t, "def-456", c.LSID())
}

// TestOwnerPrefersLSID verifies the Owner helper returns the lsid when one
// is set (the real case for Mongo clients that explicitly start sessions).
func TestOwnerPrefersLSID(t *testing.T) {
	c := New()
	c.SetLSID("real-lsid-uuid")
	assert.Equal(t, "real-lsid-uuid", c.Owner())
}

// TestOwnerFallsBackToConnSyntheticID verifies the Owner helper synthesizes
// a per-connection id when no lsid is set. This is the path for mongosh /
// drivers that don't explicitly call startSession; the synthetic id must
// be (a) stable across calls on the same connection and (b) distinct
// across different ConnInfo instances.
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

// TestInTransactionRoundTrip verifies set/get on the transaction flag,
// including that it defaults to false on a fresh connection.
func TestInTransactionRoundTrip(t *testing.T) {
	c := New()
	assert.False(t, c.InTransaction(), "fresh ConnInfo must default to not-in-transaction")

	c.SetInTransaction(true)
	assert.True(t, c.InTransaction())

	c.SetInTransaction(false)
	assert.False(t, c.InTransaction())
}

// TestSetLSIDOverridesFallback verifies that once an lsid arrives, the
// Owner switches from synthetic-conn to the lsid. This matches the wire-
// protocol expectation: the first command may not carry lsid (older drivers,
// hello/isMaster) but a later one might, and the owner must update.
func TestSetLSIDOverridesFallback(t *testing.T) {
	c := New()
	syntheticOwner := c.Owner()
	assert.True(t, strings.HasPrefix(syntheticOwner, "conn:"))

	c.SetLSID("real-lsid")
	assert.Equal(t, "real-lsid", c.Owner(),
		"Owner must prefer lsid once it is set, not the synthetic fallback")
}
