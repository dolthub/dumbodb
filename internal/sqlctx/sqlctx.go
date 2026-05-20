// Package sqlctx provides minimal scaffolding for DumboDB to call into the
// Dolt SQL session package (dsess) without running inside a SQL engine.
package sqlctx

import (
	"context"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/branch_control"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/config"
)

func NewSession(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) *dsess.DoltSession {
	return dsess.DefaultSession(provider, writeSessFunc)
}

func NewSessionWithGlobals(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc, globals config.ReadWriteConfig) *dsess.DoltSession {
	return dsess.DefaultSession(provider, writeSessFunc).WithGlobals(globals)
}

// NewSessionWithGC constructs a DoltSession via dsess.NewDetachedSession,
// wiring in the given GCSafepointController so VisitGCRoots is meaningful.
// Used by the SessionRegistry factory in production paths; the controller
// drives chunk pinning while the session is in the registry.
func NewSessionWithGC(
	provider dsess.DoltDatabaseProvider,
	writeSessFunc dsess.WriteSessFunc,
	gc *gcctx.GCSafepointController,
) (*dsess.DoltSession, error) {
	conf := config.NewMapConfig(make(map[string]string))
	return dsess.NewDetachedSession(
		provider,
		conf,
		branch_control.CreateDefaultController(context.TODO()),
		nil, // statsProvider
		writeSessFunc,
		gc,
		nil, // branchActivityTracker
	)
}

func Wrap(ctx context.Context, sess *dsess.DoltSession) *sql.Context {
	return sql.NewContext(ctx, sql.WithSession(sess))
}

func New(ctx context.Context, provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) (*sql.Context, *dsess.DoltSession) {
	sess := NewSession(provider, writeSessFunc)
	return Wrap(ctx, sess), sess
}
