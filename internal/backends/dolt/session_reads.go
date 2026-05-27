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
	"fmt"
	"strings"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// qualifiedDbName returns the dsess-style revision-qualified name for
// (dbName, branch). Always includes the revision suffix so it matches
// branchState.RevisionDbName() (which uses the same formatter without a
// default-branch shortcut), making DirtyBranchRevisions comparisons
// symmetrical.
func qualifiedDbName(dbName, branch string) string {
	return doltdb.RevisionDbName(dbName, branch)
}

// dbNameDsessFriendly reports whether a db name is safe to pass through
// dsess's revision-qualified lookup paths. dsess's SplitRevisionDbName
// treats "@" and "/" as revision delimiters, so a db name that contains
// either as part of its literal name (e.g. dumbodb's all-digit-suffix
// special case, "mydb@1775505756999075683") would be misinterpreted.
// Callers that build a SetWorkingSet / LookupDbState path must skip the
// session route for these names and write through the dbState shadow.
func dbNameDsessFriendly(dbName string) bool {
	return !strings.ContainsAny(dbName, "/@")
}

// ensureDsessTxn returns the active dsess transaction, starting one if
// none is in flight. dsess.StartTransaction clears all per-db heads in the
// session, so callers MUST invoke this at txn-entry boundaries (wire
// startTransaction:true, or the first command in session-isolation mode)
// and never lazily from inside a write -- otherwise read-path state
// established by qsc.1 would be wiped.
func ensureDsessTxn(sqlCtx *sql.Context, sess *dsess.DoltSession) (sql.Transaction, error) {
	if tx := sess.GetTransaction(); tx != nil {
		return tx, nil
	}
	tx, err := sess.StartTransaction(sqlCtx, sql.ReadWrite)
	if err != nil {
		return nil, fmt.Errorf("ensureDsessTxn: StartTransaction: %w", err)
	}
	return tx, nil
}

// clearDsessTxn drops the current transaction reference from the session
// after a commit/rollback so the next txn-entry boundary starts a fresh
// one. CommitWorkingSet/DoltCommit (unlike CommitTransaction) don't reset
// ctx.Transaction themselves.
func clearDsessTxn(sqlCtx *sql.Context) {
	sqlCtx.SetTransaction(nil)
}

// workingSetViaSession returns the working set for (dbName, branch). When
// the calling session has an uncommitted overlay for this (db, branch) --
// dsess.DirtyBranchRevisions reports it -- the session's branchState is
// the source of truth and is returned. Otherwise, the dbState snapshot
// (fallback) is returned: it reflects committed writes from any session
// and stays current via the commit / non-txn write paths.
//
// We deliberately do NOT trust the session's branchState for "clean"
// reads, because dsess loads it at lookup time and never refreshes --
// using it for non-dirty reads would hide writes another session
// committed to disk.
func workingSetViaSession(ctx context.Context, sess *dsess.DoltSession, fallback *doltdb.WorkingSet, dbName, branch string) (*doltdb.WorkingSet, error) {
	if sess != nil {
		qualified := qualifiedDbName(dbName, branch)
		qualifiedLower := strings.ToLower(qualified)
		for _, d := range sess.DirtyBranchRevisions() {
			if strings.ToLower(d) == qualifiedLower {
				sqlCtx := sqlctx.Wrap(ctx, sess)
				sessState, ok, err := sess.LookupDbState(sqlCtx, qualified)
				if err != nil {
					return nil, fmt.Errorf("workingSetViaSession: LookupDbState for %q@%q: %w", dbName, branch, err)
				}
				if ok {
					if ws := sessState.WorkingSet(); ws != nil {
						return ws, nil
					}
				}
				break
			}
		}
	}
	if fallback == nil {
		return nil, fmt.Errorf("workingSetViaSession: no working set for %q@%q", dbName, branch)
	}
	return fallback, nil
}

// workingRootViaSession is workingSetViaSession that returns just the
// working root. Most callers only need the root.
func workingRootViaSession(ctx context.Context, sess *dsess.DoltSession, ws *doltdb.WorkingSet, dbName, branch string) (doltdb.RootValue, error) {
	out, err := workingSetViaSession(ctx, sess, ws, dbName, branch)
	if err != nil {
		return nil, err
	}
	return out.WorkingRoot(), nil
}

// sessionFromContext returns the calling client's session if conninfo's
// cached shadow carries one; otherwise nil. Internal callers (handler
// setup, capped cleanup) build a fresh ConnInfo with no shadow, so they
// get nil and read through the dbState snapshot directly.
func sessionFromContext(ctx context.Context) *dsess.DoltSession {
	ci := conninfo.GetIfPresent(ctx)
	if ci == nil {
		return nil
	}
	shadow, _ := ci.CachedShadow()
	if shadow == nil {
		return nil
	}
	return shadow.Session()
}

// sessionForOwner returns the live DoltSession registered under the given
// lsid (owner string), or nil if there is no active session for that lsid.
// Used by OnTransactionCommit / OnTransactionAbort / OnSessionEnd to drive
// the owner's session directly without requiring it to be reachable from
// the current request's conninfo.
func (b *Backend) sessionForOwner(owner string) *dsess.DoltSession {
	if b.sessions == nil {
		return nil
	}
	shadow, ok := b.sessions.Get(owner)
	if !ok || shadow == nil {
		return nil
	}
	return shadow.Session()
}
