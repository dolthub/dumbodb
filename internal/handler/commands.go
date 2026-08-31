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

package handler

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/logging"
)

type Command struct {
	// anonymous indicates that the command does not require authentication.
	anonymous bool

	// Handler processes this command.
	//
	// The passed context is canceled when the client disconnects.
	Handler func(context.Context, *wire.OpMsg) (*wire.OpMsg, error)

	// Help is shown in the `listCommands` command output. Empty means hidden.
	Help string

	// Durable routes through Shadow.Commit (writeMu fence) instead of
	// Shadow.Use; a concurrent reconnect/sweep waits for fsync.
	Durable bool

	// BlockedInTxn returns code 263 OperationNotSupportedInTransaction
	// when ConnInfo.InTransaction(); the txn is then aborted server-side.
	BlockedInTxn bool
}

// register adds c to the command map under every supplied name. A single
// *Command can answer to multiple names (aliases) without duplicating its
// definition.
func (h *Handler) register(c *Command, names ...string) {
	for _, n := range names {
		h.commands[n] = c
	}
}

func (h *Handler) initCommands() {
	if h.paramStore == nil {
		h.paramStore = newParameterStore()
	}
	h.commands = map[string]*Command{
		// Single-name commands. Aliased and flag-bearing commands are
		// registered below via h.register(...). Keep this map sorted
		// alphabetically.
		"aggregate":                {Handler: h.MsgAggregate, Help: "Returns aggregated data."},
		"autoCompact":              {Handler: h.MsgAutoCompact, Help: "Enables or disables background compaction (MongoDB 8.0+)."},
		"bulkWrite":                {Handler: h.MsgBulkWrite, Help: "Performs multiple write operations across collections in a single command."},
		"convertToCapped":          {Handler: h.MsgConvertToCapped, Help: "Converts an existing collection to a capped collection."},
		"collStats":                {Handler: h.MsgCollStats, Help: "Returns storage data for a collection."},
		"compact":                  {Handler: h.MsgCompact, Help: "Reduces the disk space collection takes and refreshes its statistics."},
		"connectionStatus":         {Handler: h.MsgConnectionStatus, anonymous: true, Help: "Returns information about the current connection, specifically the state of authenticated users and their available permissions."},
		"count":                    {Handler: h.MsgCount, Help: "Returns the count of documents that's matched by the query."},
		"create":                   {Handler: h.MsgCreate, Help: "Creates the collection."},
		"currentOp":                {Handler: h.MsgCurrentOp, Help: "Returns information about operations currently in progress."},
		"dataSize":                 {Handler: h.MsgDataSize, Help: "Returns the size of the collection in bytes."},
		"debugError":               {Handler: h.MsgDebugError, Help: "Returns error for debugging."},
		"delete":                   {Handler: h.MsgDelete, Help: "Deletes documents matched by the query."},
		"distinct":                 {Handler: h.MsgDistinct, Help: "Returns an array of distinct values for the given field."},
		"dropIndexes":              {Handler: h.MsgDropIndexes, Help: "Drops indexes on a collection."},
		"explain":                  {Handler: h.MsgExplain, Help: "Returns the execution plan."},
		"find":                     {Handler: h.MsgFind, Help: "Returns documents matched by the query."},
		"getCmdLineOpts":           {Handler: h.MsgGetCmdLineOpts, Help: "Returns a summary of all runtime and configuration options."},
		"getFreeMonitoringStatus":  {Handler: h.msgFreeMonitoringNotSupported, Help: "Returns a status of the free monitoring."},
		"getLog":                   {Handler: h.MsgGetLog, Help: "Returns the most recent logged events from memory."},
		"getMore":                  {Handler: h.MsgGetMore, Help: "Returns the next batch of documents from a cursor."},
		"getParameter":             {Handler: h.MsgGetParameter, Help: "Returns the value of the parameter."},
		"hello":                    {Handler: h.MsgHello, anonymous: true, Help: "Returns the role of the DumboDB instance."},
		"hostInfo":                 {Handler: h.MsgHostInfo, Help: "Returns a summary of the system information."},
		"insert":                   {Handler: h.MsgInsert, Help: "Inserts documents into the database."},
		"killCursors":              {Handler: h.MsgKillCursors, Help: "Closes server cursors."},
		"listCollections":          {Handler: h.MsgListCollections, Help: "Returns the information of the collections and views in the database."},
		"listCommands":             {Handler: h.MsgListCommands, Help: "Returns a list of currently supported commands."},
		"listDatabases":            {Handler: h.MsgListDatabases, Help: "Returns a summary of all the databases."},
		"listIndexes":              {Handler: h.MsgListIndexes, Help: "Returns a summary of indexes of the specified collection."},
		"logout":                   {Handler: h.MsgLogout, anonymous: true, Help: "Logs out from the current session."},
		"startSession":             {Handler: h.MsgStartSession, anonymous: true, Help: "Creates a new server session."},
		"top":                      {Handler: h.MsgTop, Help: "Returns per-collection usage statistics (degenerate -- DumboDB does not track per-op counters)."},
		"createSearchIndexes":      {Handler: h.MsgCreateSearchIndexes, Help: "Creates Atlas Search indexes (not supported)."},
		"listSearchIndexes":        {Handler: h.MsgListSearchIndexes, Help: "Lists Atlas Search indexes (not supported)."},
		"dropSearchIndex":          {Handler: h.MsgDropSearchIndex, Help: "Drops an Atlas Search index (not supported)."},
		"updateSearchIndex":        {Handler: h.MsgUpdateSearchIndex, Help: "Updates an Atlas Search index (not supported)."},
		"abortTransaction":         {Handler: h.MsgAbortTransaction, Help: "Aborts a MongoDB transaction."},
		"endSessions":              {Handler: h.MsgEndSessions, anonymous: true, Help: "Ends server sessions."},
		"ping":                     {Handler: h.MsgPing, anonymous: true, Help: "Returns a pong response."},
		"saslStart":                {Handler: h.MsgSASLStart, anonymous: true},
		"saslContinue":             {Handler: h.MsgSASLContinue, anonymous: true},
		"serverStatus":             {Handler: h.MsgServerStatus, Help: "Returns an overview of the databases state."},
		"setFreeMonitoring":        {Handler: h.msgFreeMonitoringNotSupported, Help: "Toggles free monitoring."},
		"setParameter":             {Handler: h.MsgSetParameter, Help: "Sets the value of a runtime-settable server parameter."},
		"update":                   {Handler: h.MsgUpdate, Help: "Updates documents that are matched by the query."},
		"validate":                 {Handler: h.MsgValidate, Help: "Validates collection."},
		"whatsmyuri":               {Handler: h.MsgWhatsMyURI, anonymous: true, Help: "Returns peer information."},
		"createUser":               {Handler: h.MsgCreateUser, Help: "Creates a new user."},
		"dropAllUsersFromDatabase": {Handler: h.MsgDropAllUsersFromDatabase, Help: "Drops all users from database."},
		"dropUser":                 {Handler: h.MsgDropUser, Help: "Drops user."},
		"updateUser":               {Handler: h.MsgUpdateUser, Help: "Updates user."},
		"usersInfo":                {Handler: h.MsgUsersInfo, Help: "Returns information about users."},
		"grantRolesToUser":         {Handler: h.MsgGrantRolesToUser, Help: "Grants roles to a user."},
		"revokeRolesFromUser":      {Handler: h.MsgRevokeRolesFromUser, Help: "Revokes roles from a user."},
		"createRole":               {Handler: h.MsgCreateRole, Help: "Creates a new user-defined role."},
		"updateRole":               {Handler: h.MsgUpdateRole, Help: "Updates a user-defined role."},
		"dropRole":                 {Handler: h.MsgDropRole, Help: "Drops a user-defined role."},
		"dropAllRolesFromDatabase": {Handler: h.MsgDropAllRolesFromDatabase, Help: "Drops all user-defined roles from database."},
		"grantPrivilegesToRole":    {Handler: h.MsgGrantPrivilegesToRole, Help: "Grants privileges to a user-defined role."},
		"revokePrivilegesFromRole": {Handler: h.MsgRevokePrivilegesFromRole, Help: "Revokes privileges from a user-defined role."},
		"grantRolesToRole":         {Handler: h.MsgGrantRolesToRole, Help: "Grants inherited roles to a user-defined role."},
		"revokeRolesFromRole":      {Handler: h.MsgRevokeRolesFromRole, Help: "Revokes inherited roles from a user-defined role."},
		"rolesInfo":                {Handler: h.MsgRolesInfo, Help: "Returns information about roles."},
	}

	// Lowercase-variant handshake / introspection aliases.
	h.register(&Command{Handler: h.MsgBuildInfo, anonymous: true, Help: "Returns a summary of the build information."}, "buildInfo", "buildinfo")
	h.register(&Command{Handler: h.MsgDBStats, Help: "Returns the statistics of the database."}, "dbStats", "dbstats")
	h.register(&Command{Handler: h.MsgFindAndModify, Help: "Updates or deletes, and returns a document matched by the query."}, "findAndModify", "findandmodify")
	h.register(&Command{Handler: h.MsgIsMaster, anonymous: true, Help: "Returns the role of the DumboDB instance."}, "isMaster", "ismaster")

	// DumboDB version-control commands accept both dolt* and dumbo* prefixes.
	h.register(&Command{Handler: h.MsgDumboDBBranch, Help: "Creates or deletes a DumboDB branch, or with no branch name lists every branch."}, "doltBranch", "dumboBranch")
	h.register(&Command{Handler: h.MsgDumboDBBranchStatus, Help: "Reports how many commits each target refspec is ahead and behind a base refspec."}, "doltBranchStatus", "dumboBranchStatus")
	h.register(&Command{Handler: h.MsgDumboDBCherryPick, Help: "Applies the diff introduced by the named commit onto the current branch encoded in the database name."}, "doltCherryPick", "dumboCherryPick")
	h.register(&Command{Handler: h.MsgDumboDBConflicts, Help: "Returns conflict information for the current in-progress merge on the branch encoded in the database name."}, "doltConflicts", "dumboConflicts")
	h.register(&Command{Handler: h.MsgDumboDBDiff, Help: "Returns document-level diff between two states for the branch encoded in the database name."}, "doltDiff", "dumboDiff")
	h.register(&Command{Handler: h.MsgDumboDBLog, Help: "Returns commit history for the branch encoded in the database name."}, "doltLog", "dumboLog")
	h.register(&Command{Handler: h.MsgDumboDBMerge, Help: "Merges a source branch into the branch encoded in the database name."}, "doltMerge", "dumboMerge")
	h.register(&Command{Handler: h.MsgDumboDBRebase, Help: "Reapplies commits on the current branch onto the tip of another branch, rewriting history."}, "doltRebase", "dumboRebase")
	h.register(&Command{Handler: h.MsgDumboDBReset, Help: "Resets the branch HEAD to a target commit, optionally resetting the working tree."}, "doltReset", "dumboReset")
	h.register(&Command{Handler: h.MsgDumboDBRemote, Help: "Adds, lists, or removes a named remote (name + url) for the database. Stored in admin.system.remotes."}, "doltRemote", "dumboRemote")
	h.register(&Command{Handler: h.MsgDumboDBPush, Help: "Pushes a branch's committed HEAD to a configured remote (reuses Dolt push)."}, "doltPush", "dumboPush")
	h.register(&Command{Handler: h.MsgDumboDBFetch, Help: "Fetches a branch from a configured remote and updates the local tracking ref (reuses Dolt fetch)."}, "doltFetch", "dumboFetch")
	h.register(&Command{Handler: h.MsgDumboDBPull, Help: "Fetches from a remote and merges the fetched commit into the current branch (git pull)."}, "doltPull", "dumboPull")
	h.register(&Command{Handler: h.MsgDumboDBClone, Help: "Creates a new database by cloning a file:// remote. Run against the admin database."}, "doltClone", "dumboClone")
	h.register(&Command{Handler: h.MsgDumboDBResolveConflict, Help: "Resolves a single conflict (document, view, metadata, or validation) in the current in-progress merge, cherry-pick, or rebase."}, "doltResolveConflict", "dumboResolveConflict")
	h.register(&Command{Handler: h.MsgDumboDBRevert, Help: "Reverts the changes introduced by the named commit, creating a new inverse commit."}, "doltRevert", "dumboRevert")
	h.register(&Command{Handler: h.MsgDumboDBStatus, Help: "Returns uncommitted changes on the branch encoded in the database name."}, "doltStatus", "dumboStatus")
	h.register(&Command{Handler: h.MsgDumboDBTag, Help: "Creates, lists, or deletes tags. Tags share the dolt tag refspec (refs/tags/<name>)."}, "doltTag", "dumboTag")
	h.register(&Command{Handler: h.MsgDumboDBUndrop, Help: "Restores a soft-deleted database, or with no name lists databases available to undrop. Admin-only."}, "doltUndrop", "dumboUndrop")

	// Durable boundaries: routed through Shadow.Commit (writeMu fence).
	h.register(&Command{Handler: h.MsgDumboDBCommit, Durable: true, Help: "Commits the current working set on the branch encoded in the database name."}, "doltCommit", "dumboCommit")
	h.register(&Command{Handler: h.MsgCommitTransaction, Durable: true, Help: "Commits a MongoDB transaction."}, "commitTransaction")
	h.register(&Command{Handler: h.MsgDumboDBGC, Durable: true, Help: "Runs garbage collection on the database's chunk store. Optional mode: \"default\" or \"full\"."}, "doltGC", "dumboGC")

	// Commands rejected with code 263 OperationNotSupportedInTransaction.
	h.register(&Command{Handler: h.MsgDrop, BlockedInTxn: true, Help: "Drops the collection."}, "drop")
	h.register(&Command{Handler: h.MsgDropDatabase, BlockedInTxn: true, Help: "Drops production database."}, "dropDatabase")
	h.register(&Command{Handler: h.MsgCreateIndexes, BlockedInTxn: true, Help: "Creates indexes on a collection."}, "createIndexes")
	h.register(&Command{Handler: h.MsgRenameCollection, BlockedInTxn: true, Help: "Changes the name of an existing collection."}, "renameCollection")
	h.register(&Command{Handler: h.MsgCollMod, BlockedInTxn: true, Help: "Adds options to a collection or modify view definitions."}, "collMod")

	// Wrap each *Command's Handler with auth and logging exactly once,
	// even when multiple aliases point to the same *Command. Iterating
	// h.commands by name would double-wrap aliased entries.
	seen := make(map[*Command]bool)
	for _, cmd := range h.commands {
		if seen[cmd] {
			continue
		}
		seen[cmd] = true

		inner := cmd.Handler

		if !cmd.anonymous {
			authed := inner
			enableNewAuth := h.EnableNewAuth
			inner = func(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
				authenticated := enableNewAuth || conninfo.Get(ctx).SCRAMAuthenticated()
				if authenticated {
					if err := checkSCRAMConversation(ctx, wireCommandName(msg), h.L); err != nil {
						if h.localhostExceptionApplies(ctx, wireCommandName(msg)) {
							res, createErr := authed(ctx, msg)
							if createErr == nil {
								h.bootstrapLatch.Store(true)
							}
							return res, createErr
						}

						return nil, err
					}
				}
				if guardErr := guardAdminMutation(wireCommandTarget(msg)); guardErr != nil {
					return nil, guardErr
				}
				if authenticated {
					if err := h.authorize(ctx, msg); err != nil {
						return nil, err
					}
				}
				return authed(ctx, msg)
			}
		}

		l := h.L
		next := inner
		cmd.Handler = func(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
			start := time.Now()

			var db, ns, cmdName string
			if doc, err := opMsgDocument(msg); err == nil {
				if v, err := doc.Get("$db"); err == nil {
					if s, ok := v.(string); ok {
						db = s
					}
				}
				if keys := doc.Keys(); len(keys) > 0 {
					cmdName = keys[0]
					if v, err := doc.Get(keys[0]); err == nil {
						if col, ok := v.(string); ok && col != "" {
							ns = db + "." + col
						}
					}
				}
			}
			if ns == "" {
				ns = db
			}

			conn := ""
			if info := conninfo.Get(ctx); info != nil && info.Peer.IsValid() {
				conn = info.Peer.String()
			}

			res, handlerErr := next(ctx, msg)

			durationMs := time.Since(start).Milliseconds()

			if handlerErr != nil {
				l.InfoContext(ctx, "command error",
					slog.String("conn", conn),
					slog.String("cmd", cmdName),
					slog.String("db", db),
					slog.String("ns", ns),
					slog.Int64("duration_ms", durationMs),
					logging.Error(handlerErr),
				)
			} else {
				l.InfoContext(ctx, "command",
					slog.String("conn", conn),
					slog.String("cmd", cmdName),
					slog.String("db", db),
					slog.String("ns", ns),
					slog.Int64("duration_ms", durationMs),
				)
			}

			return res, handlerErr
		}
	}
}

// wireCommandName returns the command name from msg's first BSON field,
// or empty string on parse failure.
func wireCommandName(msg *wire.OpMsg) string {
	doc, err := opMsgDocument(msg)
	if err != nil {
		return ""
	}
	keys := doc.Keys()
	if len(keys) == 0 {
		return ""
	}
	return keys[0]
}

// checkSCRAMConversation returns error if SCRAM conversation is not valid.
func checkSCRAMConversation(ctx context.Context, command string, l *slog.Logger) error {
	_, _, conv, _ := conninfo.Get(ctx).Auth()

	switch {
	case conv == nil:
		l.WarnContext(ctx, "checkSCRAMConversation: no conversation")

	case !conv.Valid():
		l.WarnContext(
			ctx,
			"checkSCRAMConversation: invalid conversation",
			slog.String("username", conv.Username()), slog.Bool("valid", conv.Valid()), slog.Bool("done", conv.Done()),
		)

	default:
		l.DebugContext(
			ctx,
			"checkSCRAMConversation: passed",
			slog.String("username", conv.Username()), slog.Bool("valid", conv.Valid()), slog.Bool("done", conv.Done()),
		)

		return nil
	}

	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrUnauthorized,
		fmt.Sprintf("Command %s requires authentication", command),
		"checkSCRAMConversation",
	)
}

func (h *Handler) localhostExceptionApplies(ctx context.Context, command string) bool {
	if command != "createUser" {
		return false
	}

	if h.bootstrapLatch.Load() {
		return false
	}

	info := conninfo.Get(ctx)
	if info == nil || !info.Peer.IsValid() || !info.Peer.Addr().IsLoopback() {
		return false
	}

	n, err := h.userCount(ctx)
	if err != nil {
		h.L.WarnContext(ctx, "localhostExceptionApplies: user count failed", logging.Error(err))
		return false
	}

	return n == 0
}

func (h *Handler) userCount(ctx context.Context) (int64, error) {
	adminDB, err := h.b.Database("admin")
	if err != nil {
		return 0, lazyerrors.Error(err)
	}

	usersCol, err := adminDB.Collection("system.users")
	if err != nil {
		return 0, lazyerrors.Error(err)
	}

	res, err := usersCol.Count(ctx, new(backends.CountParams))
	if err != nil {
		return 0, lazyerrors.Error(err)
	}

	return res.Count, nil
}

var adminMutationDenyCode = map[string]handlererrors.ErrorCode{
	"insert":        handlererrors.ErrUnauthorized,
	"update":        handlererrors.ErrUnauthorized,
	"delete":        handlererrors.ErrUnauthorized,
	"findAndModify": handlererrors.ErrUnauthorized,
	"findandmodify": handlererrors.ErrUnauthorized,
	"create":        handlererrors.ErrUnauthorized,
	"drop":          handlererrors.ErrIllegalOperation,
}

func wireCommandTarget(msg *wire.OpMsg) (command, db, collection string) {
	doc, err := opMsgDocument(msg)
	if err != nil {
		return "", "", ""
	}
	if v, err := doc.Get("$db"); err == nil {
		if s, ok := v.(string); ok {
			db = s
		}
	}
	keys := doc.Keys()
	if len(keys) == 0 {
		return "", db, ""
	}
	command = keys[0]
	if v, err := doc.Get(keys[0]); err == nil {
		if s, ok := v.(string); ok {
			collection = s
		}
	}
	return command, db, collection
}

func guardAdminMutation(command, db, collection string) error {
	if db != "admin" {
		return nil
	}

	code, guarded := adminMutationDenyCode[command]
	if !guarded {
		return nil
	}

	return handlererrors.NewCommandErrorMsgWithArgument(
		code,
		fmt.Sprintf("cannot %s %q: the admin database is reserved; manage its contents through the user management commands", command, collection),
		command,
	)
}

func (h *Handler) Commands() map[string]*Command {
	return h.commands
}

// msgAuthNotSupported returns an OperationFailed error for user management
// commands. Authentication is not implemented in this release.
func (h *Handler) msgAuthNotSupported(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	return nil, handlererrors.NewCommandErrorMsg(
		handlererrors.ErrOperationFailed,
		"authentication is not supported in this release of DumboDB",
	)
}

// msgFreeMonitoringNotSupported returns an OperationFailed error for free
// monitoring commands. Free monitoring is a MongoDB Atlas feature not
// applicable to DumboDB.
func (h *Handler) msgFreeMonitoringNotSupported(_ context.Context, _ *wire.OpMsg) (*wire.OpMsg, error) {
	return nil, handlererrors.NewCommandErrorMsg(
		handlererrors.ErrOperationFailed,
		"free monitoring is not supported by DumboDB",
	)
}
