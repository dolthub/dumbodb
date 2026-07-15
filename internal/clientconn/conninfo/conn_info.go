// Copyright 2021 FerretDB Inc.
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

// Package conninfo provides access to connection-specific information.
package conninfo

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/netip"
	"sync"

	"github.com/xdg-go/scram"

	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// contextKey is a named unexported type for the safe use of context.WithValue.
type contextKey struct{}

// Context key for WithConnInfo/Get.
var connInfoKey = contextKey{}

type ConnInfo struct {
	// the order of fields is weird to make the struct smaller due to alignment

	sc *scram.ServerConversation // protected by rw
	db string                    // protected by rw

	Peer netip.AddrPort // invalid for Unix domain sockets

	username string // protected by rw
	password string // protected by rw

	lsid             string         // protected by rw
	cachedShadow     *sqlctx.Shadow // protected by rw
	cachedShadowLsid string         // protected by rw

	rw sync.RWMutex

	inTransaction bool // protected by rw
	txnAborted    bool // protected by rw; set when server rejects a txn op, makes subsequent commitTransaction return NoSuchTransaction

	metadataRecv bool // protected by rw

	// If true, backend implementations should not perform authentication
	// by adding username and password to the connection string.
	// It is set to true for background connections (such us capped collections cleanup)
	// and by the new authentication.
	// See where it is used for more details.
	bypassBackendAuth  bool // protected by rw
	scramAuthenticated bool // protected by rw; set when SCRAM conversation succeeds, never cleared

	pendingAutoCommit map[string]AutoCommitTarget // protected by rw; keyed by db+"\x00"+branch, last writer wins
	autoCommitMsg     string                      // protected by rw; overrides drained targets' messages when set
}

type AutoCommitTarget struct {
	DB      string
	Branch  string
	Message string
}

func New() *ConnInfo {
	return new(ConnInfo)
}

func (connInfo *ConnInfo) Username() string {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.username
}

// Auth returns stored username, password (for PLAIN mechanism), SCRAM server conversation (if any) and user's authentication db.
func (connInfo *ConnInfo) Auth() (username, password string, sc *scram.ServerConversation, db string) {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.username, connInfo.password, connInfo.sc, connInfo.db
}

// SetAuth stores username, password (for PLAIN mechanism), SCRAM server conversation (if any) and user's authentication db.
func (connInfo *ConnInfo) SetAuth(username, password string, sc *scram.ServerConversation, db string) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.username = username
	connInfo.password = password
	connInfo.sc = sc
	connInfo.db = db
}

func (connInfo *ConnInfo) MetadataRecv() bool {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.metadataRecv
}

func (connInfo *ConnInfo) SetMetadataRecv() {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.metadataRecv = true
}

// SetSCRAMAuthenticated marks that SCRAM authentication completed successfully on this connection.
// This is never cleared, even after logout.
func (connInfo *ConnInfo) SetSCRAMAuthenticated() {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.scramAuthenticated = true
}

func (connInfo *ConnInfo) SCRAMAuthenticated() bool {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.scramAuthenticated
}

// SetBypassBackendAuth marks the connection as not requiring backend authentication.
func (connInfo *ConnInfo) SetBypassBackendAuth() {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.bypassBackendAuth = true
}

func (connInfo *ConnInfo) BypassBackendAuth() bool {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.bypassBackendAuth
}

func (connInfo *ConnInfo) LSID() string {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.lsid
}

func (connInfo *ConnInfo) SetLSID(lsid string) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.lsid = lsid
}

// EnsureLSID assigns a "synthetic:" prefixed id when none is present
// (handshake, ping, legacy clients with no implicit session). The
// prefix is opaque to the registry but distinguishes server-generated
// from driver-supplied ids in logs.
func (connInfo *ConnInfo) EnsureLSID() string {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	if connInfo.lsid != "" {
		return connInfo.lsid
	}
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		connInfo.lsid = fmt.Sprintf("synthetic:fallback-%p", connInfo)
		return connInfo.lsid
	}
	connInfo.lsid = "synthetic:" + hex.EncodeToString(b[:])
	return connInfo.lsid
}

// CachedShadow returns the cached shadow and the lsid it was acquired
// for. A mismatch with the current frame's lsid means the cache is
// stale and the caller must Connect again (e.g. handshake's synthetic
// id is replaced by a driver-supplied id on the next frame).
func (connInfo *ConnInfo) CachedShadow() (*sqlctx.Shadow, string) {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()
	return connInfo.cachedShadow, connInfo.cachedShadowLsid
}

func (connInfo *ConnInfo) SetCachedShadow(lsid string, s *sqlctx.Shadow) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()
	connInfo.cachedShadow = s
	connInfo.cachedShadowLsid = lsid
}

func (connInfo *ConnInfo) InTransaction() bool {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.inTransaction
}

func (connInfo *ConnInfo) SetInTransaction(v bool) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.inTransaction = v
	if !v {
		connInfo.txnAborted = false
	}
}

func (connInfo *ConnInfo) TxnAborted() bool {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.txnAborted
}

func (connInfo *ConnInfo) SetTxnAborted(v bool) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.txnAborted = v
}

func (connInfo *ConnInfo) Owner() string {
	if id := connInfo.LSID(); id != "" {
		return id
	}
	return fmt.Sprintf("conn:%p", connInfo)
}

// Ctx returns a derived context with the given ConnInfo.
func Ctx(ctx context.Context, connInfo *ConnInfo) context.Context {
	return context.WithValue(ctx, connInfoKey, connInfo)
}

func Get(ctx context.Context) *ConnInfo {
	value := ctx.Value(connInfoKey)
	if value == nil {
		panic("connInfo is not set in context")
	}

	connInfo, ok := value.(*ConnInfo)
	if !ok {
		panic("connInfo is set in context with wrong value type")
	}

	if connInfo == nil {
		panic("connInfo is set in context with nil value")
	}

	return connInfo
}

func GetIfPresent(ctx context.Context) *ConnInfo {
	value := ctx.Value(connInfoKey)
	if value == nil {
		return nil
	}
	connInfo, ok := value.(*ConnInfo)
	if !ok {
		return nil
	}
	return connInfo
}

// RecordAutoCommit notes that a write advanced db@branch, with the message to
// commit it under. Last writer for a branch wins.
func (connInfo *ConnInfo) RecordAutoCommit(db, branch, message string) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	if connInfo.pendingAutoCommit == nil {
		connInfo.pendingAutoCommit = make(map[string]AutoCommitTarget)
	}
	connInfo.pendingAutoCommit[db+"\x00"+branch] = AutoCommitTarget{DB: db, Branch: branch, Message: message}
}

// SetAutoCommitMessage overrides the message for every branch drained by the
// next DrainAutoCommit, e.g. a bulkWrite summary.
func (connInfo *ConnInfo) SetAutoCommitMessage(message string) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()
	connInfo.autoCommitMsg = message
}

// DrainAutoCommit returns and clears the branches recorded since the last drain.
func (connInfo *ConnInfo) DrainAutoCommit() []AutoCommitTarget {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	override := connInfo.autoCommitMsg
	connInfo.autoCommitMsg = ""
	if len(connInfo.pendingAutoCommit) == 0 {
		return nil
	}
	out := make([]AutoCommitTarget, 0, len(connInfo.pendingAutoCommit))
	for _, t := range connInfo.pendingAutoCommit {
		if override != "" {
			t.Message = override
		}
		out = append(out, t)
	}
	connInfo.pendingAutoCommit = nil
	return out
}
