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

func qualifiedDbName(dbName, branch string) string {
	return doltdb.RevisionDbName(dbName, branch)
}

// dbNameDsessFriendly: dsess.SplitRevisionDbName treats "@" and "/" as
// revision delimiters, so dumbodb's all-digit-suffix names get
// misparsed. Callers must skip the session route for those.
func dbNameDsessFriendly(dbName string) bool {
	return !strings.ContainsAny(dbName, "/@")
}

func alwaysAutoCommit(dbName string) bool {
	return dbName == "admin"
}

// ensureDsessTxn: dsess.StartTransaction wipes every per-db branchState,
// so callers MUST invoke this only at txn-entry boundaries.
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

func clearDsessTxn(sqlCtx *sql.Context) {
	sqlCtx.SetTransaction(nil)
}

// workingSetViaSession returns the session's branchState only when
// DirtyBranchRevisions reports the branch as dirty; otherwise returns
// the fallback. dsess never refreshes branchState after lookup, so a
// "clean" read from the session would hide writes from other sessions.
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

func workingRootViaSession(ctx context.Context, sess *dsess.DoltSession, ws *doltdb.WorkingSet, dbName, branch string) (doltdb.RootValue, error) {
	out, err := workingSetViaSession(ctx, sess, ws, dbName, branch)
	if err != nil {
		return nil, err
	}
	return out.WorkingRoot(), nil
}

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
