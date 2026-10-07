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
	"crypto/x509"
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

func TestPeerCertificateRoundTrip(t *testing.T) {
	c := New()
	assert.Nil(t, c.PeerCertificate())
	assert.False(t, c.UsesTLS())
	certificate := &x509.Certificate{RawSubject: []byte("subject")}
	c.SetUsesTLS(true)
	c.SetPeerCertificate(certificate)
	assert.Same(t, certificate, c.PeerCertificate())
	assert.True(t, c.UsesTLS())
}

func TestOwnerPrefersLSID(t *testing.T) {
	c := New()
	c.SetLSID("real-lsid-uuid")
	assert.Equal(t, "\x00real-lsid-uuid", c.Owner())
}

func TestOwnerIsScopedToUserAndAuthDB(t *testing.T) {
	onApp, onAdmin := New(), New()
	onApp.SetAuth("alice", "", nil, "app")
	onAdmin.SetAuth("alice", "", nil, "admin")
	onApp.SetLSID("same-lsid")
	onAdmin.SetLSID("same-lsid")

	assert.Equal(t, Principal("alice", "app")+"\x00same-lsid", onApp.Owner())
	assert.NotEqual(t, onApp.Owner(), onAdmin.Owner())

	principal, id, ok := SplitSessionKey(onApp.Owner())
	require.True(t, ok)
	assert.Equal(t, Principal("alice", "app"), principal)
	assert.Equal(t, "same-lsid", id)
}

func TestOwnerFallsBackToConnSyntheticID(t *testing.T) {
	c1 := New()
	c2 := New()

	o1a := c1.Owner()
	o1b := c1.Owner()
	o2 := c2.Owner()

	assert.Contains(t, o1a, "conn:",
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
	assert.Contains(t, syntheticOwner, "conn:")

	c.SetLSID("real-lsid")
	assert.Equal(t, "\x00real-lsid", c.Owner(),
		"Owner must prefer lsid once it is set, not the synthetic fallback")
}

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

	c.SetCachedShadow("some-lsid", nil)
	s, lsid := c.CachedShadow()
	assert.Nil(t, s)
	assert.Equal(t, "some-lsid", lsid)
}

func TestEnsureLSID_GeneratesSyntheticOnEmpty(t *testing.T) {
	c := New()
	require.Equal(t, "", c.LSID())

	got := c.EnsureLSID()
	assert.True(t, strings.HasPrefix(got, "synthetic:"))
	assert.Equal(t, got, c.LSID())
	assert.Greater(t, len(got), len("synthetic:"))
}

func TestEnsureLSID_NoopOnDriverSupplied(t *testing.T) {
	c := New()
	c.SetLSID("driver-supplied-id")

	got := c.EnsureLSID()
	assert.Equal(t, "driver-supplied-id", got)
	assert.Equal(t, "driver-supplied-id", c.LSID())
}

func TestEnsureLSID_OwnerPrefersSyntheticOverConnFallback(t *testing.T) {
	c := New()
	require.Contains(t, c.Owner(), "conn:")

	id := c.EnsureLSID()
	assert.Equal(t, "\x00"+id, c.Owner())
	assert.Contains(t, c.Owner(), "synthetic:")
}

func TestOwner_ScopesByAuthenticatedUser(t *testing.T) {
	c := New()
	c.SetLSID("lsid-shared")

	anon := c.Owner()

	c.SetAuth("alice", "", nil, "admin")
	alice := c.Owner()

	c.SetAuth("bob", "", nil, "admin")
	bob := c.Owner()

	assert.NotEqual(t, anon, alice)
	assert.NotEqual(t, alice, bob)
	assert.Equal(t, bob, c.SessionKeyFor("lsid-shared"))
	assert.Contains(t, alice, "lsid-shared")
}

func TestEnsureLSID_StableAcrossCalls(t *testing.T) {
	c := New()
	first := c.EnsureLSID()
	second := c.EnsureLSID()
	assert.Equal(t, first, second)
}

// Before the fix the principal was user + "@" + db with no escaping, so
// alice@tenant in auth database 123 and alice in auth database tenant@123
// shared one owner string and could use each other's cursors and sessions.
func TestSessionPrincipalIsUnambiguous(t *testing.T) {
	a, b := New(), New()
	a.SetAuth("alice@tenant", "", nil, "123")
	b.SetAuth("alice", "", nil, "tenant@123")
	assert.NotEqual(t, a.SessionPrincipal(), b.SessionPrincipal())

	a.SetLSID("same-lsid")
	b.SetLSID("same-lsid")
	assert.NotEqual(t, a.Owner(), b.Owner())
}

// A NUL in a username must not shift where a session key splits.
func TestSessionKeySplitsWithNULInUsername(t *testing.T) {
	c := New()
	c.SetAuth("ev\x00il", "", nil, "db")
	c.SetLSID("lsid")

	principal, id, ok := SplitSessionKey(c.Owner())
	require.True(t, ok)
	assert.Equal(t, "lsid", id)
	assert.Equal(t, c.SessionPrincipal(), principal)
}

func TestSplitPrincipalRoundTrip(t *testing.T) {
	for _, c := range [][2]string{{"alice", "app"}, {"alice@tenant", "123"}, {"alice", "tenant@123"}, {"ev\x00il", "d\"b"}, {"", ""}} {
		user, db, ok := SplitPrincipal(Principal(c[0], c[1]))
		require.True(t, ok, "%q", c)
		assert.Equal(t, c[0], user)
		assert.Equal(t, c[1], db)
	}
	_, _, ok := SplitPrincipal("")
	assert.False(t, ok)
}
