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
	"fmt"
	"net/netip"
	"sync"

	"github.com/xdg-go/scram"
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

	// lsid is the MongoDB logical session ID (UUID string) attached to the
	// most recent command on this connection. Wire-protocol handlers set it
	// via SetLSID; the backend reads it via LSID() to key per-session state
	// (e.g. document locks for default-mode transactions).
	// Empty until the first command that carries an lsid arrives. Protected by rw.
	lsid string

	rw sync.RWMutex

	metadataRecv bool // protected by rw

	// If true, backend implementations should not perform authentication
	// by adding username and password to the connection string.
	// It is set to true for background connections (such us capped collections cleanup)
	// and by the new authentication.
	// See where it is used for more details.
	bypassBackendAuth  bool // protected by rw
	scramAuthenticated bool // protected by rw; set when SCRAM conversation succeeds, never cleared
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

// LSID returns the MongoDB logical session ID most recently set on this
// connection, or "" if no command carrying an lsid has arrived yet.
func (connInfo *ConnInfo) LSID() string {
	connInfo.rw.RLock()
	defer connInfo.rw.RUnlock()

	return connInfo.lsid
}

// SetLSID records the lsid carried on the current command. Handlers call
// this on every command parse so the backend can resolve the owning
// session for per-session state (e.g. document locks).
func (connInfo *ConnInfo) SetLSID(lsid string) {
	connInfo.rw.Lock()
	defer connInfo.rw.Unlock()

	connInfo.lsid = lsid
}

// Owner returns a stable identifier for the entity that owns
// session-scoped state on this connection: the MongoDB lsid when one is
// present, otherwise a synthetic per-connection id derived from the
// ConnInfo's memory address. The fallback lets us key per-session state
// (like document locks) before lsid-aware command parsing lands, and
// keeps drivers that talk to DumboDB without explicit sessions working.
//
// The "conn:" prefix on the fallback makes it unambiguous in logs that
// this is not a real lsid.
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
