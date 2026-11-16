// Package sqlctx provides minimal scaffolding for DumboDB to call into the
// Dolt SQL session package (dsess) without running inside a SQL engine.
package sqlctx

import (
	"context"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/branch_control"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb/gcctx"
	dsqle "github.com/dolthub/dolt/go/libraries/doltcore/sqle"
	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/config"
)

// engineVariables holds the system variable definitions and global values
// every DumboDB session shares, as sessions of one SQL engine would. Dolt reads
// its own variables, such as dolt_transactions_disabled, from it.
var engineVariables = dsqle.NewEngineVariables()

func NewSession(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) *dsess.DoltSession {
	return dsess.DefaultSessionWithVariables(provider, writeSessFunc, engineVariables)
}

func NewSessionWithGlobals(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc, globals config.ReadWriteConfig) *dsess.DoltSession {
	return NewSession(provider, writeSessFunc).WithGlobals(globals)
}

func NewSessionWithGC(
	provider dsess.DoltDatabaseProvider,
	writeSessFunc dsess.WriteSessFunc,
	gc *gcctx.GCSafepointController,
) (*dsess.DoltSession, error) {
	conf := config.NewMapConfig(make(map[string]string))
	return dsess.NewDetachedSessionWithVariables(
		provider,
		conf,
		branch_control.CreateDefaultController(context.TODO()),
		nil,
		writeSessFunc,
		gc,
		nil,
		engineVariables,
	)
}

func Wrap(ctx context.Context, sess *dsess.DoltSession) *sql.Context {
	return sql.NewContext(ctx, sql.WithSession(sess))
}

func New(ctx context.Context, provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) (*sql.Context, *dsess.DoltSession) {
	sess := NewSession(provider, writeSessFunc)
	return Wrap(ctx, sess), sess
}

// EnsureTxn returns the session's current tx, starting one if needed.
// dsess.StartTransaction wipes all per-db heads, so callers must invoke
// this only at txn-entry boundaries -- never lazily from inside a write.
func EnsureTxn(sqlCtx *sql.Context, sess *dsess.DoltSession) (sql.Transaction, error) {
	if tx := sess.GetTransaction(); tx != nil {
		return tx, nil
	}
	if scoper, ok := sess.Provider().(TxnStartScoper); ok {
		scoped, end := scoper.ScopeTxnStart(sqlCtx)
		defer end()
		sqlCtx = scoped
	}
	return sess.StartTransaction(sqlCtx, sql.ReadWrite)
}

// TxnStartScoper is implemented by a provider that tracks which databases a
// command uses. StartTransaction reads every open database; ScopeTxnStart
// returns the context to start it with, so those reads belong to the start
// alone, and the function that ends that scope.
type TxnStartScoper interface {
	ScopeTxnStart(ctx *sql.Context) (*sql.Context, func())
}
