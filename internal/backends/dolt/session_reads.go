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

	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
)

// workingSetViaSession returns the working set for (dbName, branch) viewed
// through the calling session's branchState. The ws argument is the
// caller's already-snapshotted in-memory working set from
// dbState.workingSets[branch] (so the caller owns whatever dbState locking
// is appropriate at the call site). When sess is non-nil, ws is pushed into
// the session's branchState via SetWorkingSet so the read reflects
// in-memory state that has not yet been flushed to the working-set ref
// (j:false batched writes); the working set is then read back through the
// session. When sess is nil -- internal callers like handler init or
// capped cleanup that arrive without a registered client session -- the
// snapshot is returned directly.
//
// This is the routing point that workspace-qsc.1 uses to thread every
// non-bypass read through the session API. The ws snapshot is the
// transitional shadow path; workspace-qsc.3 removes it once writes flow
// through the session too.
func workingSetViaSession(ctx context.Context, sess *dsess.DoltSession, ws *doltdb.WorkingSet, dbName, branch string) (*doltdb.WorkingSet, error) {
	if ws == nil {
		return nil, fmt.Errorf("workingSetViaSession: no working set for %q@%q", dbName, branch)
	}
	if sess == nil {
		return ws, nil
	}

	qualified := dbName
	if branch != defaultBranch {
		qualified = doltdb.RevisionDbName(dbName, branch)
	}

	sqlCtx := sqlctx.Wrap(ctx, sess)
	if err := sess.SetWorkingSet(sqlCtx, qualified, ws); err != nil {
		return nil, fmt.Errorf("workingSetViaSession: SetWorkingSet for %q@%q: %w", dbName, branch, err)
	}

	sessState, _, err := sess.LookupDbState(sqlCtx, qualified)
	if err != nil {
		return nil, fmt.Errorf("workingSetViaSession: LookupDbState for %q@%q: %w", dbName, branch, err)
	}
	return sessState.WorkingSet(), nil
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
