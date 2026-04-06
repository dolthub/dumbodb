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

	"github.com/dolthub/docudolt/internal/clientconn/conninfo"
	"github.com/dolthub/docudolt/internal/handler/handlererrors"
	"github.com/dolthub/docudolt/internal/util/logging"
)

// command represents a handler for single command.
type command struct {
	// anonymous indicates that the command does not require authentication.
	anonymous bool

	// Handler processes this command.
	//
	// The passed context is canceled when the client disconnects.
	Handler func(context.Context, *wire.OpMsg) (*wire.OpMsg, error)

	// Help is shown in the `listCommands` command output.
	// If empty, that command is hidden, but still can be used.
	Help string
}

// initCommands initializes the commands map for that handler instance.
func (h *Handler) initCommands() {
	h.commands = map[string]*command{
		// sorted alphabetically
		"aggregate": {
			Handler: h.MsgAggregate,
			Help:    "Returns aggregated data.",
		},
		"autoCompact": {
			Handler: h.MsgAutoCompact,
			Help:    "Enables or disables background compaction (MongoDB 8.0+).",
		},
		"buildInfo": {
			Handler:   h.MsgBuildInfo,
			anonymous: true,
			Help:      "Returns a summary of the build information.",
		},
		"buildinfo": { // old lowercase variant
			Handler:   h.MsgBuildInfo,
			anonymous: true,
			Help:      "", // hidden
		},
		"collMod": {
			Handler: h.MsgCollMod,
			Help:    "Adds options to a collection or modify view definitions.",
		},
		"convertToCapped": {
			Handler: h.MsgConvertToCapped,
			Help:    "Converts an existing collection to a capped collection.",
		},
		"collStats": {
			Handler: h.MsgCollStats,
			Help:    "Returns storage data for a collection.",
		},
		"compact": {
			Handler: h.MsgCompact,
			Help:    "Reduces the disk space collection takes and refreshes its statistics.",
		},
		"connectionStatus": {
			Handler:   h.MsgConnectionStatus,
			anonymous: true,
			Help: "Returns information about the current connection, " +
				"specifically the state of authenticated users and their available permissions.",
		},
		"count": {
			Handler: h.MsgCount,
			Help:    "Returns the count of documents that's matched by the query.",
		},
		"create": {
			Handler: h.MsgCreate,
			Help:    "Creates the collection.",
		},
		"createIndexes": {
			Handler: h.MsgCreateIndexes,
			Help:    "Creates indexes on a collection.",
		},
		"currentOp": {
			Handler: h.MsgCurrentOp,
			Help:    "Returns information about operations currently in progress.",
		},
		"dataSize": {
			Handler: h.MsgDataSize,
			Help:    "Returns the size of the collection in bytes.",
		},
		"dbStats": {
			Handler: h.MsgDBStats,
			Help:    "Returns the statistics of the database.",
		},
		"dbstats": { // old lowercase variant
			Handler: h.MsgDBStats,
			Help:    "", // hidden
		},
		"debugError": {
			Handler: h.MsgDebugError,
			Help:    "Returns error for debugging.",
		},
		"doltBranch": {
			Handler: h.MsgDocuDoltBranch,
			Help:    "Creates a new DocuDolt branch from the current branch encoded in the database name.",
		},
		"doltCherryPick": {
			Handler: h.MsgDocuDoltCherryPick,
			Help:    "Applies the diff introduced by the named commit onto the current branch encoded in the database name.",
		},
		"doltConflicts": {
			Handler: h.MsgDocuDoltConflicts,
			Help:    "Returns conflict information for the current in-progress merge on the branch encoded in the database name.",
		},
		"doltDiff": {
			Handler: h.MsgDocuDoltDiff,
			Help:    "Returns document-level diff between two states for the branch encoded in the database name.",
		},
		"doltCommit": {
			Handler: h.MsgDocuDoltCommit,
			Help:    "Commits the current working set on the branch encoded in the database name.",
		},
		"doltCurrentBranch": {
			Handler: h.MsgDocuDoltCurrentBranch,
			Help:    "Returns the current branch name for the connection encoded in the database name.",
		},
		"doltLog": {
			Handler: h.MsgDocuDoltLog,
			Help:    "Returns commit history for the branch encoded in the database name.",
		},
		"doltMerge": {
			Handler: h.MsgDocuDoltMerge,
			Help:    "Merges a source branch into the branch encoded in the database name.",
		},
		"doltReset": {
			Handler: h.MsgDocuDoltReset,
			Help:    "Resets the branch HEAD to a target commit, optionally resetting the working tree.",
		},
		"doltResolveConflict": {
			Handler: h.MsgDocuDoltResolveConflict,
			Help:    "Resolves a single document conflict in the current in-progress merge.",
		},
		"doltStatus": {
			Handler: h.MsgDocuDoltStatus,
			Help:    "Returns uncommitted changes on the branch encoded in the database name.",
		},
		"delete": {
			Handler: h.MsgDelete,
			Help:    "Deletes documents matched by the query.",
		},
		"distinct": {
			Handler: h.MsgDistinct,
			Help:    "Returns an array of distinct values for the given field.",
		},
		"drop": {
			Handler: h.MsgDrop,
			Help:    "Drops the collection.",
		},
		"dropDatabase": {
			Handler: h.MsgDropDatabase,
			Help:    "Drops production database.",
		},
		"dropIndexes": {
			Handler: h.MsgDropIndexes,
			Help:    "Drops indexes on a collection.",
		},
		"explain": {
			Handler: h.MsgExplain,
			Help:    "Returns the execution plan.",
		},
		"find": {
			Handler: h.MsgFind,
			Help:    "Returns documents matched by the query.",
		},
		"findAndModify": {
			Handler: h.MsgFindAndModify,
			Help:    "Updates or deletes, and returns a document matched by the query.",
		},
		"findandmodify": { // old lowercase variant
			Handler: h.MsgFindAndModify,
			Help:    "", // hidden
		},
		"getCmdLineOpts": {
			Handler: h.MsgGetCmdLineOpts,
			Help:    "Returns a summary of all runtime and configuration options.",
		},
		"getFreeMonitoringStatus": {
			Handler: h.MsgGetFreeMonitoringStatus,
			Help:    "Returns a status of the free monitoring.",
		},
		"getLog": {
			Handler: h.MsgGetLog,
			Help:    "Returns the most recent logged events from memory.",
		},
		"getMore": {
			Handler: h.MsgGetMore,
			Help:    "Returns the next batch of documents from a cursor.",
		},
		"getParameter": {
			Handler: h.MsgGetParameter,
			Help:    "Returns the value of the parameter.",
		},
		"hello": {
			Handler:   h.MsgHello,
			anonymous: true,
			Help:      "Returns the role of the FerretDB instance.",
		},
		"hostInfo": {
			Handler: h.MsgHostInfo,
			Help:    "Returns a summary of the system information.",
		},
		"insert": {
			Handler: h.MsgInsert,
			Help:    "Inserts documents into the database.",
		},
		"isMaster": {
			Handler:   h.MsgIsMaster,
			anonymous: true,
			Help:      "Returns the role of the FerretDB instance.",
		},
		"ismaster": { // old lowercase variant
			Handler:   h.MsgIsMaster,
			anonymous: true,
			Help:      "", // hidden
		},
		"killCursors": {
			Handler: h.MsgKillCursors,
			Help:    "Closes server cursors.",
		},
		"listCollections": {
			Handler: h.MsgListCollections,
			Help:    "Returns the information of the collections and views in the database.",
		},
		"listCommands": {
			Handler: h.MsgListCommands,
			Help:    "Returns a list of currently supported commands.",
		},
		"listDatabases": {
			Handler: h.MsgListDatabases,
			Help:    "Returns a summary of all the databases.",
		},
		"listIndexes": {
			Handler: h.MsgListIndexes,
			Help:    "Returns a summary of indexes of the specified collection.",
		},
		"logout": {
			Handler:   h.MsgLogout,
			anonymous: true,
			Help:      "Logs out from the current session.",
		},
		"startSession": {
			Handler:   h.MsgStartSession,
			anonymous: true,
			Help:      "Creates a new server session.",
		},
		"commitTransaction": {
			Handler: h.MsgCommitTransaction,
			Help:    "Commits a transaction (no-op: transactions are not isolated).",
		},
		"createSearchIndexes": {
			Handler: h.MsgCreateSearchIndexes,
			Help:    "Creates Atlas Search indexes (not supported).",
		},
		"listSearchIndexes": {
			Handler: h.MsgListSearchIndexes,
			Help:    "Lists Atlas Search indexes (not supported).",
		},
		"dropSearchIndex": {
			Handler: h.MsgDropSearchIndex,
			Help:    "Drops an Atlas Search index (not supported).",
		},
		"updateSearchIndex": {
			Handler: h.MsgUpdateSearchIndex,
			Help:    "Updates an Atlas Search index (not supported).",
		},
		"abortTransaction": {
			Handler: h.MsgAbortTransaction,
			Help:    "Aborts a transaction (no-op: operations cannot be rolled back).",
		},
		"endSessions": {
			Handler:   h.MsgEndSessions,
			anonymous: true,
			Help:      "Ends server sessions.",
		},
		"ping": {
			Handler:   h.MsgPing,
			anonymous: true,
			Help:      "Returns a pong response.",
		},
		"renameCollection": {
			Handler: h.MsgRenameCollection,
			Help:    "Changes the name of an existing collection.",
		},
		"saslStart": {
			Handler:   h.MsgSASLStart,
			anonymous: true,
			Help:      "", // hidden
		},
		"saslContinue": {
			Handler:   h.MsgSASLContinue,
			anonymous: true,
			Help:      "", // hidden
		},
		"serverStatus": {
			Handler: h.MsgServerStatus,
			Help:    "Returns an overview of the databases state.",
		},
		"setFreeMonitoring": {
			Handler: h.MsgSetFreeMonitoring,
			Help:    "Toggles free monitoring.",
		},
		"update": {
			Handler: h.MsgUpdate,
			Help:    "Updates documents that are matched by the query.",
		},
		"validate": {
			Handler: h.MsgValidate,
			Help:    "Validates collection.",
		},
		"whatsmyuri": {
			Handler:   h.MsgWhatsMyURI,
			anonymous: true,
			Help:      "Returns peer information.",
		},
		// please keep sorted alphabetically
	}

	// User management commands are always registered (anonymous so they work without auth).
	// When EnableNewAuth is false these are effectively no-ops (system.users is always empty).
	// sorted alphabetically
	h.commands["createUser"] = &command{
		anonymous: true,
		Handler:   h.MsgCreateUser,
		Help:      "Creates a new user.",
	}
	h.commands["dropAllUsersFromDatabase"] = &command{
		anonymous: true,
		Handler:   h.MsgDropAllUsersFromDatabase,
		Help:      "Drops all users from database.",
	}
	h.commands["dropUser"] = &command{
		anonymous: true,
		Handler:   h.MsgDropUser,
		Help:      "Drops user.",
	}
	h.commands["updateUser"] = &command{
		anonymous: true,
		Handler:   h.MsgUpdateUser,
		Help:      "Updates user.",
	}
	h.commands["usersInfo"] = &command{
		anonymous: true,
		Handler:   h.MsgUsersInfo,
		Help:      "Returns information about users.",
	}
	// please keep sorted alphabetically

	for name, cmd := range h.commands {
		if !cmd.anonymous {
			cmdHandler := h.commands[name].Handler
			enableNewAuth := h.EnableNewAuth

			h.commands[name].Handler = func(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
				if enableNewAuth || conninfo.Get(ctx).SCRAMAuthenticated() {
					if err := checkSCRAMConversation(ctx, name, h.L); err != nil {
						return nil, err
					}
				}

				return cmdHandler(ctx, msg)
			}
		}
	}

	// Wrap all commands with per-command request logging.
	for name, cmd := range h.commands {
		cmdName := name
		cmdHandler := cmd.Handler
		l := h.L

		h.commands[name].Handler = func(ctx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
			start := time.Now()

			// Extract db and ns for logging; ignore parse errors here since the real handler will catch them.
			var db, ns string
			if doc, err := opMsgDocument(msg); err == nil {
				if v, err := doc.Get("$db"); err == nil {
					if s, ok := v.(string); ok {
						db = s
					}
				}
				keys := doc.Keys()
				if len(keys) > 0 {
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

			// Get connection identifier from peer address.
			conn := ""
			if info := conninfo.Get(ctx); info != nil && info.Peer.IsValid() {
				conn = info.Peer.String()
			}

			res, handlerErr := cmdHandler(ctx, msg)

			durationMs := time.Since(start).Milliseconds()

			if handlerErr != nil {
				l.WarnContext(ctx, "command error",
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

// Commands returns a map of enabled commands.
func (h *Handler) Commands() map[string]*command {
	return h.commands
}
