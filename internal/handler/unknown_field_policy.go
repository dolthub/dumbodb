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

package handler

// unknownFieldPolicy classifies how a command must treat unrecognized top-level
// fields. MongoDB rejects unknown fields on almost every command (strict IDL
// parsing) but accepts them on a handful of legacy/diagnostic commands; DumboDB
// must match either behavior exactly.
type unknownFieldPolicy int

const (
	// strictRejects: the command rejects unknown top-level fields with
	// ErrIDLUnknownField (40415), matching MongoDB. Its handler calls
	// common.RejectUnknownFields, or it uses the strict handlerparams.Extract-
	// Params decoder (the CRUD commands). TestStrictCommandsRejectUnknownField
	// verifies each of these actually rejects.
	strictRejects unknownFieldPolicy = iota

	// legacyAccepts: MongoDB accepts (ignores) unknown top-level fields on this
	// command, so DumboDB must NOT reject them. Verified against real MongoDB via
	// the parity suite before a command is classified here.
	legacyAccepts

	// strictPending: MongoDB rejects unknown fields on this command, but DumboDB
	// does not -- because the command is unsupported and returns NotImplemented
	// for any input, so top-level unknown-field rejection is moot. If such a
	// command is ever implemented, promote it to strictRejects and wire
	// common.RejectUnknownFields with a validated allow-list.
	strictPending
)

// unknownFieldPolicies is the authoritative classification for EVERY registered
// command name and alias.
//
// INVARIANT (enforced by TestUnknownFieldPolicyCoverage): every registered
// command appears here exactly once, and every entry here is a registered
// command. Adding a version-control command or enabling a new MongoDB command
// FAILS that test until the command is classified here.
//
// DEFAULT TO strictRejects. That is what MongoDB does for nearly every command,
// so a new command should reject unknown fields (wire common.RejectUnknownFields
// into its handler) unless the parity suite proves MongoDB accepts them (then
// legacyAccepts) or the work is deferred (then strictPending, with a beads task).
var unknownFieldPolicies = map[string]unknownFieldPolicy{
	// --- CRUD (strict via ExtractParams) ---
	"find": strictRejects, "count": strictRejects, "distinct": strictRejects,
	"insert": strictRejects, "update": strictRejects, "delete": strictRejects,
	"findAndModify": strictRejects, "findandmodify": strictRejects,

	// --- dumbo*/dolt* version-control family ---
	"doltBranch": strictRejects, "dumboBranch": strictRejects,
	"doltBranchStatus": strictRejects, "dumboBranchStatus": strictRejects,
	"doltCherryPick": strictRejects, "dumboCherryPick": strictRejects,
	"doltCommit": strictRejects, "dumboCommit": strictRejects,
	"doltConflicts": strictRejects, "dumboConflicts": strictRejects,
	"doltDiff": strictRejects, "dumboDiff": strictRejects,
	"doltGC": strictRejects, "dumboGC": strictRejects,
	"doltLog": strictRejects, "dumboLog": strictRejects,
	"doltMerge": strictRejects, "dumboMerge": strictRejects,
	"doltRebase": strictRejects, "dumboRebase": strictRejects,
	"doltReset": strictRejects, "dumboReset": strictRejects,
	"doltRemote": strictRejects, "dumboRemote": strictRejects,
	"doltPush": strictRejects, "dumboPush": strictRejects,
	"doltResolveConflict": strictRejects, "dumboResolveConflict": strictRejects,
	"doltRevert": strictRejects, "dumboRevert": strictRejects,
	"doltStatus": strictRejects, "dumboStatus": strictRejects,
	"doltTag": strictRejects, "dumboTag": strictRejects,
	"doltUndrop": strictRejects, "dumboUndrop": strictRejects,

	// --- DDL ---
	"drop": strictRejects, "dropDatabase": strictRejects, "dropIndexes": strictRejects,
	"renameCollection": strictRejects, "collMod": strictRejects,

	// --- collection / db introspection ---
	"dbStats": strictRejects, "dbstats": strictRejects,
	"collStats": strictRejects, "dataSize": strictRejects,
	"hostInfo": strictRejects, "getLog": strictRejects,
	"getCmdLineOpts": strictRejects, "listDatabases": strictRejects,

	// --- cursor / session / transactions ---
	"ping": strictRejects, "killCursors": strictRejects, "getMore": strictRejects,
	"endSessions": strictRejects, "logout": strictRejects, "connectionStatus": strictRejects,
	"abortTransaction": strictRejects, "commitTransaction": strictRejects,

	// --- user / role management ---
	"createUser": strictRejects, "updateUser": strictRejects, "dropUser": strictRejects,
	"dropAllUsersFromDatabase": strictRejects, "usersInfo": strictRejects,
	"grantRolesToUser": strictRejects, "revokeRolesFromUser": strictRejects,
	"createRole": strictRejects, "updateRole": strictRejects, "dropRole": strictRejects,
	"dropAllRolesFromDatabase": strictRejects, "rolesInfo": strictRejects,
	"grantRolesToRole": strictRejects, "revokeRolesFromRole": strictRejects,
	"grantPrivilegesToRole": strictRejects, "revokePrivilegesFromRole": strictRejects,

	// --- aggregation / write ---
	"aggregate": strictRejects, "explain": strictRejects, "bulkWrite": strictRejects,

	// --- auth handshake ---
	"saslStart": strictRejects, "saslContinue": strictRejects,

	// --- DDL / introspection with larger MongoDB field sets (validated allow-lists) ---
	"create": strictRejects, "createIndexes": strictRejects,
	"listCollections": strictRejects, "listIndexes": strictRejects,
	"compact": strictRejects,

	// --- legacy: MongoDB accepts unknown fields (verified) -> must NOT reject ---
	"top": legacyAccepts, "hello": legacyAccepts, "isMaster": legacyAccepts,
	"ismaster": legacyAccepts, "buildInfo": legacyAccepts, "buildinfo": legacyAccepts,
	"whatsmyuri": legacyAccepts, "serverStatus": legacyAccepts, "listCommands": legacyAccepts,
	"currentOp": legacyAccepts, "getParameter": legacyAccepts, "setParameter": legacyAccepts,
	"debugError": legacyAccepts, "getFreeMonitoringStatus": legacyAccepts,
	"setFreeMonitoring": legacyAccepts, "startSession": legacyAccepts,
	"convertToCapped": legacyAccepts, "validate": legacyAccepts,

	// --- unsupported commands: DumboDB returns NotImplemented for the whole
	// command, so top-level unknown-field rejection is moot. autoCompact has no
	// background-compaction equivalent on Dolt-backed storage; search indexes are
	// unimplemented. Kept strictPending deliberately.
	"autoCompact":         strictPending,
	"createSearchIndexes": strictPending, "listSearchIndexes": strictPending,
	"dropSearchIndex": strictPending, "updateSearchIndex": strictPending,
}
