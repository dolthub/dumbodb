// Package sqlctx provides minimal scaffolding for DumboDB to call into the
// Dolt SQL session package (dsess) without running inside a SQL engine.
package sqlctx

import (
	"context"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/branch_control"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/config"
)

func NewSession(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) *dsess.DoltSession {
	return dsess.DefaultSession(provider, writeSessFunc)
}

func NewSessionWithGlobals(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc, globals config.ReadWriteConfig) *dsess.DoltSession {
	return dsess.DefaultSession(provider, writeSessFunc).WithGlobals(globals)
}

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
		nil,
		writeSessFunc,
		gc,
		nil,
	)
}

func Wrap(ctx context.Context, sess *dsess.DoltSession) *sql.Context {
	return sql.NewContext(ctx, sql.WithSession(sess))
}

func New(ctx context.Context, provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) (*sql.Context, *dsess.DoltSession) {
	sess := NewSession(provider, writeSessFunc)
	return Wrap(ctx, sess), sess
}

// EnsureTxn returns the session's current transaction, starting a new one
// if none is in flight. dsess.StartTransaction wipes all per-db heads, so
// callers must invoke this only at txn-entry boundaries (wire
// startTransaction:true frame, or first command in --session-isolation
// mode), never lazily inside a write.
func EnsureTxn(sqlCtx *sql.Context, sess *dsess.DoltSession) (sql.Transaction, error) {
	if tx := sess.GetTransaction(); tx != nil {
		return tx, nil
	}
	return sess.StartTransaction(sqlCtx, sql.ReadWrite)
}
