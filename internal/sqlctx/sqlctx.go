// Package sqlctx provides minimal scaffolding for DumboDB to call into the
// Dolt SQL session package (dsess) without running inside a SQL engine.
//
// dsess assumes one DoltSession per SQL connection and threads everything
// through *sql.Context. DumboDB speaks the Mongo wire protocol, so it needs
// a path from a context.Context to a (*sql.Context, *dsess.DoltSession) pair.
// This package is that path.
//
// The DoltDatabaseProvider that the session is built against is supplied by
// the caller (typically the dolt backend in internal/backends/dolt). This
// package does not embed a provider, only wraps the construction.
package sqlctx

import (
	"context"

	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
	"github.com/dolthub/dolt/go/libraries/utils/config"
)

// NewSession builds a *dsess.DoltSession backed by the supplied provider.
// writeSessFunc may be nil for read-only paths; the backend will supply a real
// one once write-session integration lands.
func NewSession(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) *dsess.DoltSession {
	return dsess.DefaultSession(provider, writeSessFunc)
}

// NewSessionWithGlobals is the same as NewSession with caller-supplied global
// configuration; useful when DumboDB needs to override session variables that
// gate dsess behavior.
func NewSessionWithGlobals(provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc, globals config.ReadWriteConfig) *dsess.DoltSession {
	return dsess.DefaultSession(provider, writeSessFunc).WithGlobals(globals)
}

// Wrap builds a *sql.Context around an existing *dsess.DoltSession and a
// caller-supplied context.Context. The returned context inherits the caller's
// cancellation; lifecycle of the session is the caller's responsibility.
func Wrap(ctx context.Context, sess *dsess.DoltSession) *sql.Context {
	return sql.NewContext(ctx, sql.WithSession(sess))
}

// New is a convenience that builds a session and wraps it in one call. Prefer
// NewSession + Wrap when the caller needs to hold the session across multiple
// *sql.Context constructions (e.g. one DoltSession per Mongo lsid, many
// short-lived sql.Contexts per command).
func New(ctx context.Context, provider dsess.DoltDatabaseProvider, writeSessFunc dsess.WriteSessFunc) (*sql.Context, *dsess.DoltSession) {
	sess := NewSession(provider, writeSessFunc)
	return Wrap(ctx, sess), sess
}
