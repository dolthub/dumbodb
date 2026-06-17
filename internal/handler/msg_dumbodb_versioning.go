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

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// dbBranchSep is the separator between the database name and rootish in an
// encoded database name (e.g. "mydb@main", "mydb@feature/foo").
// Must match the value in internal/backends/dolt/backend.go.
const dbBranchSep = "@"

// defaultBranch is the name of the default branch.
// Must match the value in internal/backends/dolt/backend.go.
const defaultBranch = "main"

// MsgDumboDBDiff implements the `dumboDBDiff` command.
//
// Returns the document-level diff between two states for the branch encoded in $db.
// Usage:
//
//	db.runCommand({dumboDBDiff: 1})                          // working set vs HEAD
//	db.runCommand({dumboDBDiff: 1, from: "<hash>"})          // commit hash to working set
//	db.runCommand({dumboDBDiff: 1, from: "<hash>", to: "<hash>"}) // between two commits
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBDiff(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, connRootish, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	from, err := common.GetOptionalParam[string](document, "from", "")
	if err != nil {
		return nil, err
	}

	to, err := common.GetOptionalParam[string](document, "to", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboDiff: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBDiff(connCtx, &backends.DiffParams{
		DBName:      dbName,
		ConnRootish: connRootish,
		From:        from,
		To:          to,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	collections := types.MakeArray(len(res.Collections))

	for _, cd := range res.Collections {
		collections.Append(collectionDiffToDoc(cd))
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"collections", collections,
			"ok", float64(1),
		)),
	)
}

// tableStatusToDoc renders a backends.TableStatus as a wire document.
// Shared by MsgDumboDBStatus and the Stat block of MsgDumboDBLog. The
// three index name lists are ALWAYS emitted, as empty arrays when
// nothing of that kind changed, so consumers do not have to branch on
// field presence.
func tableStatusToDoc(t backends.TableStatus) *types.Document {
	return must.NotFail(types.NewDocument(
		"name", t.Name,
		"status", t.Status,
		"added", int32(t.Added),
		"modified", int32(t.Modified),
		"deleted", int32(t.Deleted),
		"addedIndexes", stringArray(t.AddedIndexes),
		"modifiedIndexes", stringArray(t.ModifiedIndexes),
		"removedIndexes", stringArray(t.RemovedIndexes),
	))
}

// collectionDiffToDoc renders a backends.CollectionDiff as a wire
// document. Shared by MsgDumboDBDiff and the Patch block of
// MsgDumboDBLog. All six change arrays (added/removed/modified for
// docs and addedIndexes/modifiedIndexes/removedIndexes for indexes)
// are always emitted, empty when there's no change of that kind.
//
// addedIndexes and removedIndexes carry full IndexInfo entries.
// modifiedIndexes carries {from, to} pairs with the full IndexInfo on
// each side.
func collectionDiffToDoc(cd backends.CollectionDiff) *types.Document {
	added := types.MakeArray(len(cd.Added))
	for _, doc := range cd.Added {
		added.Append(doc)
	}
	removed := types.MakeArray(len(cd.Removed))
	for _, doc := range cd.Removed {
		removed.Append(doc)
	}
	modified := types.MakeArray(len(cd.Modified))
	for _, m := range cd.Modified {
		diffArray := types.MakeArray(len(m.Diff))
		for _, fd := range m.Diff {
			fdPairs := []any{"type", fd.Type, "path", fd.Path}
			if fd.Type != "added" {
				fdPairs = append(fdPairs, "from", fd.From)
			}
			if fd.Type != "removed" {
				fdPairs = append(fdPairs, "to", fd.To)
			}
			diffArray.Append(must.NotFail(types.NewDocument(fdPairs...)))
		}
		modified.Append(must.NotFail(types.NewDocument(
			"_id", m.ID,
			"diff", diffArray,
		)))
	}

	addedIdx := types.MakeArray(len(cd.AddedIndexes))
	for _, info := range cd.AddedIndexes {
		i := info
		addedIdx.Append(indexInfoToDoc(&i))
	}
	removedIdx := types.MakeArray(len(cd.RemovedIndexes))
	for _, info := range cd.RemovedIndexes {
		i := info
		removedIdx.Append(indexInfoToDoc(&i))
	}
	modifiedIdx := types.MakeArray(len(cd.ModifiedIndexes))
	for _, ch := range cd.ModifiedIndexes {
		from := ch.From
		to := ch.To
		modifiedIdx.Append(must.NotFail(types.NewDocument(
			"from", indexInfoToDoc(&from),
			"to", indexInfoToDoc(&to),
		)))
	}

	return must.NotFail(types.NewDocument(
		"name", cd.Name,
		"status", cd.Status,
		"added", added,
		"removed", removed,
		"modified", modified,
		"addedIndexes", addedIdx,
		"modifiedIndexes", modifiedIdx,
		"removedIndexes", removedIdx,
	))
}

// stringArray renders a []string as a wire-array, returning an empty
// array (not nil) for nil/empty input.
func stringArray(s []string) *types.Array {
	arr := types.MakeArray(len(s))
	for _, v := range s {
		arr.Append(v)
	}
	return arr
}

// indexInfoToDoc renders an IndexInfo as a wire document for dumboDiff
// output. Includes the keys (name + asc/desc + special-kind flags) and
// the unique / sparse / partial flags. Omitted fields take their
// MongoDB defaults.
func indexInfoToDoc(info *backends.IndexInfo) *types.Document {
	keys := types.MakeArray(len(info.Key))
	for _, kp := range info.Key {
		kpPairs := []any{"field", kp.Field}
		switch {
		case kp.Hashed:
			kpPairs = append(kpPairs, "kind", "hashed")
		case kp.Text:
			kpPairs = append(kpPairs, "kind", "text")
		case kp.Geo2D:
			kpPairs = append(kpPairs, "kind", "2d")
		case kp.Geo2DSphere:
			kpPairs = append(kpPairs, "kind", "2dsphere")
		default:
			direction := int32(1)
			if kp.Descending {
				direction = -1
			}
			kpPairs = append(kpPairs, "direction", direction)
		}
		keys.Append(must.NotFail(types.NewDocument(kpPairs...)))
	}
	pairs := []any{
		"name", info.Name,
		"keys", keys,
	}
	if info.Unique {
		pairs = append(pairs, "unique", true)
	}
	if info.Sparse {
		pairs = append(pairs, "sparse", true)
	}
	if info.PartialFilterExpression != nil {
		pairs = append(pairs, "partialFilterExpression", info.PartialFilterExpression)
	}
	return must.NotFail(types.NewDocument(pairs...))
}

// branchFromDBName parses the real database name and rootish from an encoded db name.
//
// DumboDB encodes version information in the database name using the '@' separator:
//
//	"mydb@branchname"                        -> dbName="mydb", rootish="branchname",                        readOnly=false
//	"mydb@na7kfra98h45fr2u5qtr30o2ggm7vh61" -> dbName="mydb", rootish="na7kfra98h45fr2u5qtr30o2ggm7vh61", readOnly=true  (commit hash)
//	"mydb@main~3"                            -> dbName="mydb", rootish="main~3",                            readOnly=true  (ancestor expression)
//	"mydb@HEAD"                              -> error: HEAD is not supported in connection strings
//	"mydb@HEAD~2"                            -> error: HEAD is not supported in connection strings
//
// If no separator is present the rootish defaults to "main" and readOnly is false.
//
// readOnly is true when the rootish is syntactically a commit hash or ancestor expression.
// Bare names are assumed to be branch names (writable); tag detection requires a backend call
// not performed here.
//
// HEAD and HEAD~N are rejected: DumboDB connections are
// stateless, so the only meaningful "current branch" is the default branch (main). Writing
// via "HEAD" therefore mutates main's working set, same as writing via "main". Callers of
// branchFromDBName never see the literal "HEAD".
//
// The rootish is validated by parseRootish; an error is returned for unsupported forms.
//
// All-digit strings after '@' (e.g. Unix nanosecond timestamps used as database name suffixes)
// are not valid rootish expressions and cause the whole encoded name to be treated as a plain
// database name rather than returning an error or misinterpreting the suffix as a branch name.
func branchFromDBName(encoded string) (dbName, rootish string, readOnly bool, err error) {
	if idx := strings.Index(encoded, dbBranchSep); idx > 0 {
		raw := encoded[idx+len(dbBranchSep):]
		candidate, decErr := url.PathUnescape(raw)
		if decErr != nil {
			return "", "", false, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				fmt.Sprintf("rootish %q: invalid percent-encoding: %v", raw, decErr),
			)
		}
		// All-digit strings (e.g. UnixNano timestamps) are not valid rootish
		// expressions. Fall through to plain-DB treatment so that database names
		// like "parity_test@1775505756999075683" work without error.
		if !rootishAllDigits(candidate) {
			if err = parseRootish(candidate); err != nil {
				return "", "", false, err
			}
			if err = rejectHEAD(candidate); err != nil {
				return "", "", false, err
			}
			return encoded[:idx], candidate, rootishIsReadOnly(candidate), nil
		}
	}

	return encoded, defaultBranch, false, nil
}

// rejectHEAD returns an error if the rootish is HEAD or starts with HEAD~ / HEAD^.
// DumboDB connections are stateless -- there is no per-session "current branch",
// so HEAD has no meaning in the connection string. Use a branch name instead.
func rejectHEAD(rootish string) error {
	if rootish == "HEAD" || strings.HasPrefix(rootish, "HEAD~") || strings.HasPrefix(rootish, "HEAD^") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: HEAD is not supported in connection strings; use a branch name instead", rootish),
		)
	}
	return nil
}

// rootishAllDigits reports whether s consists entirely of ASCII decimal digits
// AND is not a valid Dolt commit hash length.
//
// Dolt commit hashes are exactly 32 base32 characters (0-9a-v); a 32-char
// all-digit string is a valid hash and must still be treated as a rootish.
// Any other all-digit string (e.g. a 19-digit UnixNano timestamp) cannot be
// a branch name, tag name, hash, or ancestor expression.
func rootishAllDigits(s string) bool {
	if s == "" || len(s) == 32 {
		return false
	}
	for _, c := range s {
		if c < '0' || c > '9' {
			return false
		}
	}
	return true
}

// rootishIsReadOnly reports whether the rootish is a read-only snapshot reference.
//
// A rootish is read-only if it is syntactically a Dolt commit hash or a relative
// ancestor expression (contains ~). Bare names are assumed to be branch names and
// are treated as writable.
//
// Dolt commit hashes are exactly 32 lowercase base32 characters (0-9a-v). Only
// full-length hashes are detected here; abbreviated forms are indistinguishable
// from branch names at parse time and are resolved at runtime by the backend.
func rootishIsReadOnly(rootish string) bool {
	// Ancestor expression: <branch>~<N>
	if strings.Contains(rootish, "~") {
		return true
	}

	// Caret parent selection: <ref>^, <ref>^N
	if strings.Contains(rootish, "^") {
		return true
	}

	// Dolt commit hash: exactly 32 lowercase base32 characters (0-9a-v).
	if len(rootish) != 32 {
		return false
	}
	for _, c := range rootish {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'v')) {
			return false
		}
	}
	return true
}

// validateRefName checks that a branch or tag name does not end with a path
// segment that looks like a commit hash. The last segment (after the final '/')
// must not be exactly 32 lowercase base32 characters, which would be ambiguous
// with a Dolt commit hash.
func validateRefName(name, kind string) error {
	lastSeg := name
	if idx := strings.LastIndex(name, "/"); idx >= 0 {
		lastSeg = name[idx+1:]
	}
	if _, ok := hash.MaybeParse(lastSeg); ok && len(lastSeg) == 32 {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("%s: name %q ends with a segment that looks like a commit hash (32 lowercase base32 chars) and would be ambiguous", kind, name),
			"name",
		)
	}
	return nil
}

// enforceWritableRootish returns an OperationFailed error if the encoded database name
// resolves to a read-only rootish (commit hash or ancestor expression).
//
// Call this at the top of any write handler before touching the backend.
func enforceWritableRootish(encodedDB string) error {
	_, _, readOnly, err := branchFromDBName(encodedDB)
	if err != nil {
		return err
	}
	if readOnly {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"cannot write to a read-only database snapshot",
		)
	}
	return nil
}

// parseRootish validates a rootish expression at parse time.
//
// Accepted forms:
//   - Branch name (resolved as refs/heads/<rootish>)
//   - Tag name (resolved as refs/tags/<rootish>)
//   - Bare commit hash (full 32-char lowercase base32, i.e. 0-9a-v)
//   - Relative ancestor expression (<branch>~<N>)
//
// Rejected by rejectHEAD (called separately after parseRootish):
//   - HEAD, HEAD~N, HEAD^N (no per-session current branch in DumboDB)
//
// Rejected forms (returned as ErrOperationFailed):
//   - Any '@' (reserved as the database/branch delimiter; covers reflog <ref>@{...} too)
//   - Range syntax (<ref>..<ref>)
//   - Regex commit search (:/<pattern>)
//   - Type dereferencing (<ref>^{<type>})
//
// Supported caret forms:
//   - <ref>^ or <ref>^1 -- first parent
//   - <ref>^2 -- second parent (merge commits only)
//   - <ref>^0 -- the commit itself
func parseRootish(s string) error {
	if s == "" {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"rootish must not be empty",
		)
	}

	// Reject any '@'  -- it is reserved as the database/branch delimiter and
	// is forbidden in raw branch names. This also covers reflog syntax (<ref>@{...}).
	if strings.Contains(s, "@") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: '@' is reserved as the database/branch delimiter", s),
		)
	}

	// Reject range syntax (<ref>..<ref>).
	if strings.Contains(s, "..") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: range syntax is not supported", s),
		)
	}

	// Reject regex commit search (:/<pattern>).
	if strings.HasPrefix(s, ":/") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: regex commit search is not supported", s),
		)
	}

	// Reject type dereferencing (^{...}) but allow caret parent selection (^, ^N).
	if strings.Contains(s, "^{") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: type dereferencing (^{type}) is not supported", s),
		)
	}

	// Traversal operators (~ and ^) are validated at resolution time,
	// not here, because they can be chained (e.g. main~1^2, HEAD^^).

	return nil
}

// versioningBackend returns the VersioningBackend for the handler, or nil if not supported.
func (h *Handler) versioningBackend() backends.VersioningBackend {
	vb, ok := h.b.(backends.VersioningBackend)
	if !ok {
		return nil
	}

	return vb
}

// MsgDumboDBCommit implements the `dumboDBCommit` command.
//
// It commits the current working set on the branch encoded in $db (format: "dbname@branch").
// Usage: db.getSiblingDB("mydb@feature").runCommand({dumboDBCommit: 1, message: "my commit"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBCommit(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	message, err := common.GetOptionalParam[string](document, "message", "")
	if err != nil {
		return nil, err
	}

	author, err := common.GetOptionalParam[string](document, "author", "")
	if err != nil {
		return nil, err
	}
	if author == "" {
		author = "dumbodb <dumbodb@dumbodb>"
	}

	ts, err := common.GetOptionalParam[time.Time](document, "timestamp", time.Time{})
	if err != nil {
		return nil, err
	}

	allowEmpty, err := common.GetOptionalBoolOrIntParam(document, "allowEmpty", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboCommit: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBCommit(connCtx, &backends.CommitParams{
		DBName:     dbName,
		Branch:     branch,
		Message:    message,
		Author:     author,
		Timestamp:  ts,
		AllowEmpty: allowEmpty,
	})
	if err != nil {
		if errors.Is(err, backends.ErrEmptyCommit) {
			return nil, handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				"dumboCommit: "+err.Error(),
			)
		}
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"branch", res.Branch,
			"message", res.Message,
			"author", res.Author,
			"timestamp", time.UnixMilli(res.Timestamp),
			"committer", res.Committer,
			"committerTimestamp", time.UnixMilli(res.CommitterTimestamp),
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBBranch implements the `dumboDBBranch` command.
//
// It creates a new branch from the current branch encoded in $db (format: "dbname@branch").
// Usage: db.getSiblingDB("mydb@main").runCommand({dumboDBBranch: 1, branch: "feature"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBBranch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, fromBranch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	newBranch, err := common.GetRequiredParam[string](document, "branch")
	if err != nil {
		return nil, err
	}

	if newBranch == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboBranch: branch name must not be empty",
			"branch",
		)
	}

	if strings.Contains(newBranch, "@") {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboBranch: branch name must not contain '@' (reserved as the database/branch delimiter)",
			"branch",
		)
	}

	if err := validateRefName(newBranch, "dumboBranch"); err != nil {
		return nil, err
	}

	safeDelete, err := common.GetOptionalBoolOrIntParam(document, "delete", false)
	if err != nil {
		return nil, err
	}

	forceDelete, err := common.GetOptionalBoolOrIntParam(document, "forceDelete", false)
	if err != nil {
		return nil, err
	}

	if safeDelete && forceDelete {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboBranch: delete and forceDelete are mutually exclusive",
			"delete",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboBranch: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBBranch(connCtx, &backends.BranchParams{
		DBName: dbName,
		From:   fromBranch,
		Name:   newBranch,
		Delete: safeDelete || forceDelete,
		Force:  forceDelete,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"branch", res.Branch,
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBMerge implements the `dumboDBMerge` command.
//
// Merges a source branch into the current branch encoded in $db (format: "dbname@branch").
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({dumboDBMerge: 1, merge_in: "feature"})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBMerge: 1, merge_in: "feature", noFF: true})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBMerge: 1, merge_in: "feature", ffOnly: true})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBMerge: 1, continue: true})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBMerge: 1, abort: true})
//
// Optional parameters for merge initiation:
//   - message (string): custom merge commit message (ignored on fast-forward / already-up-to-date)
//   - author (string): 'Name <email>' for the merge commit author
//   - noFF (bool): force a merge commit even when fast-forward is possible
//   - ffOnly (bool): fail if a fast-forward is not possible (mutually exclusive with noFF)
//
// When a merge produces document-level conflicts, the response includes ok:0 with a
// conflicts array describing which collections have unresolved conflicts. The branch
// HEAD is unchanged; the staged working set reflects the partial merge with "ours"
// values for conflicting documents. Use dumboDBResolveConflict to resolve conflicts, then
// dumboDBMerge continue:true to complete the merge.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBMerge(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, intoBranch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	abort, err := common.GetOptionalBoolOrIntParam(document, "abort", false)
	if err != nil {
		return nil, err
	}

	continueParam, err := common.GetOptionalBoolOrIntParam(document, "continue", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboMerge: versioning is not supported by the current backend",
		)
	}

	if abort {
		res, mergeErr := vb.DumboDBMerge(connCtx, &backends.MergeParams{
			DBName: dbName,
			Into:   intoBranch,
			Abort:  true,
		})
		if mergeErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, mergeErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"message", res.Message,
				"ok", float64(1),
			)),
		)
	}

	if continueParam {
		message, err := common.GetOptionalParam[string](document, "message", "")
		if err != nil {
			return nil, err
		}
		author, err := common.GetOptionalParam[string](document, "author", "")
		if err != nil {
			return nil, err
		}
		res, mergeErr := vb.DumboDBMerge(connCtx, &backends.MergeParams{
			DBName:   dbName,
			Into:     intoBranch,
			Continue: true,
			Message:  message,
			Author:   author,
		})
		if mergeErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, mergeErr.Error())
		}
		contDoc := must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"message", res.Message,
		))
		if res.Author != "" {
			contDoc.Set("author", res.Author)
			contDoc.Set("timestamp", time.UnixMilli(res.Timestamp))
			contDoc.Set("committer", res.Committer)
			contDoc.Set("committerTimestamp", time.UnixMilli(res.CommitterTimestamp))
		}
		contDoc.Set("ok", float64(1))
		return documentOpMsg(contDoc)
	}

	noFF, err := common.GetOptionalParam[bool](document, "noFF", false)
	if err != nil {
		return nil, err
	}

	ffOnly, err := common.GetOptionalParam[bool](document, "ffOnly", false)
	if err != nil {
		return nil, err
	}

	if noFF && ffOnly {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboMerge: noFF and ffOnly are mutually exclusive",
			"noFF",
		)
	}

	fromBranch, err := common.GetRequiredParam[string](document, "merge_in")
	if err != nil {
		return nil, err
	}

	if fromBranch == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboMerge: from branch name must not be empty",
			"merge_in",
		)
	}

	message, err := common.GetOptionalParam[string](document, "message", "")
	if err != nil {
		return nil, err
	}

	author, err := common.GetOptionalParam[string](document, "author", "")
	if err != nil {
		return nil, err
	}

	res, mergeErr := vb.DumboDBMerge(connCtx, &backends.MergeParams{
		DBName:  dbName,
		Into:    intoBranch,
		From:    fromBranch,
		Message: message,
		Author:  author,
		NoFF:    noFF,
		FFOnly:  ffOnly,
	})

	if mergeErr != nil {
		var conflictErr *backends.MergeConflictError
		if errors.As(mergeErr, &conflictErr) {
			// Return a structured ok:0 response with per-collection conflict counts.
			conflictsArr := types.MakeArray(len(conflictErr.Conflicts))
			for _, c := range conflictErr.Conflicts {
				entry := must.NotFail(types.NewDocument(
					"collection", c.Collection,
					"count", int32(c.Count),
				))
				conflictsArr.Append(entry)
			}
			return documentOpMsg(
				must.NotFail(types.NewDocument(
					"conflicts", conflictsArr,
					"ok", float64(0),
					"code", int32(handlererrors.ErrOperationFailed),
					"errmsg", conflictErr.Error(),
				)),
			)
		}
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, mergeErr.Error())
	}

	mergeDoc := must.NotFail(types.NewDocument(
		"commitId", res.CommitID,
		"message", res.Message,
	))
	if res.Author != "" {
		mergeDoc.Set("author", res.Author)
		mergeDoc.Set("timestamp", time.UnixMilli(res.Timestamp))
		mergeDoc.Set("committer", res.Committer)
		mergeDoc.Set("committerTimestamp", time.UnixMilli(res.CommitterTimestamp))
	}
	mergeDoc.Set("ok", float64(1))
	return documentOpMsg(mergeDoc)
}

// MsgDumboDBConflicts implements the `dumboDBConflicts` command.
//
// Returns conflict information for the current in-progress merge on the branch encoded in $db.
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({dumboDBConflicts: 1})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBConflicts: 1, collection: "items"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBConflicts(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboConflicts: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBConflicts(connCtx, &backends.ConflictsParams{
		DBName: dbName,
		Branch: branch,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	collectionsArr := types.MakeArray(len(res.Collections))
	for _, cc := range res.Collections {
		conflictsArr := types.MakeArray(len(cc.Conflicts))
		for _, cf := range cc.Conflicts {
			pairs := []any{
				"conflictId", cf.ConflictID,
			}

			// Extract _id from whichever document is non-nil and promote it
			// to the top level so it isn't repeated inside base/ours/theirs.
			var docID any
			for _, doc := range []*types.Document{cf.Ours, cf.Theirs, cf.Base} {
				if doc != nil {
					if v, getErr := doc.Get("_id"); getErr == nil {
						docID = v
						break
					}
				}
			}
			if docID != nil {
				pairs = append(pairs, "_id", docID)
			}

			// Build base/ours/theirs without the _id field.
			for _, kv := range []struct {
				key string
				doc *types.Document
			}{
				{"base", cf.Base},
				{"ours", cf.Ours},
				{"theirs", cf.Theirs},
			} {
				if kv.doc == nil {
					pairs = append(pairs, kv.key, types.Null)
					continue
				}
				stripped := must.NotFail(types.NewDocument())
				for _, k := range kv.doc.Keys() {
					if k == "_id" {
						continue
					}
					v, _ := kv.doc.Get(k)
					stripped.Set(k, v)
				}
				pairs = append(pairs, kv.key, stripped)
			}

			pairs = append(pairs,
				"ourDiffType", cf.OurDiffType,
				"theirDiffType", cf.TheirDiffType,
			)

			conflictsArr.Append(must.NotFail(types.NewDocument(pairs...)))
		}

		collectionsArr.Append(must.NotFail(types.NewDocument(
			"collection", cc.Collection,
			"conflicts", conflictsArr,
		)))
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"collections", collectionsArr,
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBResolveConflict implements the `dumboDBResolveConflict` command.
//
// Resolves a single document conflict in the current in-progress merge.
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({dumboDBResolveConflict: 1, collection: "items", conflictId: "c0", resolution: "ours"})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBResolveConflict: 1, collection: "items", conflictId: "c0", resolution: "theirs"})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBResolveConflict: 1, collection: "items", conflictId: "c0", resolution: "custom", value: {_id:1, v:42}})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBResolveConflict(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	collection, err := common.GetRequiredParam[string](document, "collection")
	if err != nil {
		return nil, err
	}

	conflictID, err := common.GetRequiredParam[string](document, "conflictId")
	if err != nil {
		return nil, err
	}

	resolution, err := common.GetRequiredParam[string](document, "resolution")
	if err != nil {
		return nil, err
	}

	var value *types.Document
	if resolution == "custom" {
		rawValue, getErr := common.GetOptionalParam[*types.Document](document, "value", nil)
		if getErr != nil {
			return nil, getErr
		}
		if rawValue == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"dumboResolveConflict: resolution 'custom' requires a 'value' document",
				"value",
			)
		}
		value = rawValue
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboResolveConflict: versioning is not supported by the current backend",
		)
	}

	_, err = vb.DumboDBResolveConflict(connCtx, &backends.ResolveConflictParams{
		DBName:     dbName,
		Branch:     branch,
		Collection: collection,
		ConflictID: conflictID,
		Resolution: resolution,
		Value:      value,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBLog implements the `dumboDBLog` command.
//
// It returns the commit history for the branch encoded in $db (format: "dbname@branch").
// Usage: db.getSiblingDB("mydb@feature").runCommand({dumboDBLog: 1, limit: 10})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBLog(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	limit, err := common.GetOptionalParam[int32](document, "limit", int32(0))
	if err != nil {
		return nil, err
	}

	// "from" accepts a single hash string or an array of hash strings (the
	// frontier seed set returned as "next" on a prior page).
	var fromSeeds []string
	if v, _ := document.Get("from"); v != nil {
		switch fv := v.(type) {
		case string:
			if fv != "" {
				fromSeeds = []string{fv}
			}
		case *types.Array:
			for i := 0; i < fv.Len(); i++ {
				el, _ := fv.Get(i)
				s, ok := el.(string)
				if !ok {
					return nil, handlererrors.NewCommandErrorMsgWithArgument(
						handlererrors.ErrTypeMismatch,
						"dumboLog: 'from' array elements must be commit hash strings",
						"from",
					)
				}
				fromSeeds = append(fromSeeds, s)
			}
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"dumboLog: 'from' must be a commit hash string or an array of strings",
				"from",
			)
		}
	}

	stat, err := common.GetOptionalBoolOrIntParam(document, "stat", false)
	if err != nil {
		return nil, err
	}

	patch, err := common.GetOptionalBoolOrIntParam(document, "patch", false)
	if err != nil {
		return nil, err
	}

	all, err := common.GetOptionalBoolOrIntParam(document, "all", false)
	if err != nil {
		return nil, err
	}

	// "all" seeds the walk with every branch HEAD; it owns the seed set, so it
	// cannot be combined with an explicit "from".
	if all && len(fromSeeds) > 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboLog: 'all' and 'from' are mutually exclusive",
			"all",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboLog: versioning is not supported by the current backend",
		)
	}

	// limit=0 explicitly means "return zero commits". Short-circuit before touching the backend.
	if document.Has("limit") && limit == 0 {
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"commits", types.MakeArray(0),
				"ok", float64(1),
			)),
		)
	}

	res, err := vb.DumboDBLog(connCtx, &backends.LogParams{
		DBName:     dbName,
		Branch:     branch,
		ConnBranch: branch,
		Limit:      limit,
		From:       fromSeeds,
		Stat:       stat,
		Patch:      patch,
		All:        all,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	commits := types.MakeArray(len(res.Commits))

	for _, c := range res.Commits {
		pairs := []any{
			"commitId", c.CommitID,
		}
		if len(c.Refs) > 0 {
			refsArr := types.MakeArray(len(c.Refs))
			for _, r := range c.Refs {
				refsArr.Append(r)
			}
			pairs = append(pairs, "refs", refsArr)
		}
		if c.Parent1 != "" {
			pairs = append(pairs, "parent1", c.Parent1)
		}
		if c.Parent2 != "" {
			pairs = append(pairs, "parent2", c.Parent2)
		}
		pairs = append(pairs,
			"message", c.Message,
			"timestamp", time.UnixMilli(c.Timestamp),
			"author", c.Author,
			"committer", c.Committer,
			"committerTimestamp", time.UnixMilli(c.CommitterTimestamp),
		)
		entry := must.NotFail(types.NewDocument(pairs...))

		if len(c.Stat) > 0 {
			statArr := types.MakeArray(len(c.Stat))
			for _, s := range c.Stat {
				statArr.Append(tableStatusToDoc(s))
			}
			entry.Set("stat", statArr)
		}

		if len(c.Diff) > 0 {
			diffArr := types.MakeArray(len(c.Diff))
			for _, cd := range c.Diff {
				diffArr.Append(collectionDiffToDoc(cd))
			}
			entry.Set("diff", diffArr)
		}

		commits.Append(entry)
	}

	reply := must.NotFail(types.NewDocument("commits", commits))

	// "next" is the frontier seed set for the following page; omitted when the
	// traversal is exhausted.
	if len(res.Next) > 0 {
		nextArr := types.MakeArray(len(res.Next))
		for _, h := range res.Next {
			nextArr.Append(h)
		}
		reply.Set("next", nextArr)
	}

	reply.Set("ok", float64(1))
	return documentOpMsg(reply)
}

// MsgDumboDBBranchStatus implements the `dumboBranchStatus` command.
//
// It reports, for each target refspec, how many commits it is ahead and behind the
// base refspec. Refspecs are commit hashes, branch/tag names, ancestor expressions,
// or HEAD/HEAD~N (resolved against the connection's branch).
//
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({dumboBranchStatus: 1, base: "main", targets: ["feature", "HEAD~1"]})
//
// Both base and targets are required; targets must name at least one refspec. A
// single target string is accepted and normalized to a one-element array. The
// response echoes each input refspec verbatim alongside its resolved commit hash:
//
//	{ base: {target, hash}, targets: [{target, hash, commitsAhead, commitsBehind}], ok: 1 }
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBBranchStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	base, err := common.GetRequiredParam[string](document, "base")
	if err != nil {
		return nil, err
	}

	// targets is required: an array of strings or a single string (normalized to
	// a one-element array). At least one target must be supplied.
	tv, _ := document.Get("targets")
	if tv == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrMissingField,
			"BSON field 'dumboBranchStatus.targets' is missing but a required field",
			"targets")
	}
	var origTargets []string
	switch t := tv.(type) {
	case *types.Array:
		for i := 0; i < t.Len(); i++ {
			elem, gErr := t.Get(i)
			if gErr != nil {
				return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, gErr.Error())
			}
			s, ok := elem.(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrTypeMismatch, "dumboBranchStatus: each target must be a string", "targets")
			}
			origTargets = append(origTargets, s)
		}
	case string:
		origTargets = []string{t}
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch, "dumboBranchStatus: targets must be a string or array of strings", "targets")
	}
	if len(origTargets) == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue, "dumboBranchStatus: at least one target is required", "targets")
	}

	// rewriteHead validates a refspec and rewrites HEAD/HEAD~N to the connection's
	// branch so the backend's rootish resolver sees a concrete reference, matching
	// MsgDumboDBReset.
	rewriteHead := func(s, argName string) (string, error) {
		if perr := parseRootish(s); perr != nil {
			return "", handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue, "dumboBranchStatus: "+perr.Error(), argName)
		}
		if s == "HEAD" {
			return branch, nil
		}
		if strings.HasPrefix(s, "HEAD~") {
			return branch + s[len("HEAD"):], nil
		}
		return s, nil
	}

	resolvedBase, err := rewriteHead(base, "base")
	if err != nil {
		return nil, err
	}

	resolvedTargets := make([]string, len(origTargets))
	for i, target := range origTargets {
		resolvedTargets[i], err = rewriteHead(target, "targets")
		if err != nil {
			return nil, err
		}
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboBranchStatus: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBBranchStatus(connCtx, &backends.BranchStatusParams{
		DBName:  dbName,
		Base:    resolvedBase,
		Targets: resolvedTargets,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	// Echo the original (verbatim) base refspec; report the resolved hash.
	targetsArr := types.MakeArray(len(res.Entries))
	for i, e := range res.Entries {
		// res.Entries is ordered to match the input targets; echo the verbatim refspec.
		shown := e.Target
		if i < len(origTargets) {
			shown = origTargets[i]
		}
		targetsArr.Append(must.NotFail(types.NewDocument(
			"target", shown,
			"hash", e.Hash,
			"commitsAhead", e.CommitsAhead,
			"commitsBehind", e.CommitsBehind,
		)))
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"base", must.NotFail(types.NewDocument(
				"target", base,
				"hash", res.BaseHash,
			)),
			"targets", targetsArr,
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBReset implements the `dumboDBReset` command.
//
// It moves the branch HEAD to the specified commit hash. Two modes:
//
//	Soft (default): HEAD moves to target, working tree is untouched, staged root = target.
//	Hard (hard: true): HEAD moves to target, working tree and staged root are reset to target.
//
// Usage:
//
//	db.runCommand({dumboDBReset: 1})                       // reset to HEAD (discard uncommitted changes if hard)
//	db.runCommand({dumboDBReset: 1, to: "<hash>"})
//	db.runCommand({dumboDBReset: 1, to: "<hash>", hard: true})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBReset(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	to, err := common.GetOptionalParam[string](document, "to", "")
	if err != nil {
		return nil, err
	}

	if to != "" {
		if err := parseRootish(to); err != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"dumboReset: "+err.Error(),
				"to",
			)
		}
		// HEAD / HEAD~N target the connection's branch, not the literal default.
		// Rewrite to <branch> / <branch>~N so the backend's rootish resolver sees
		// a concrete branch reference.
		if to == "HEAD" {
			to = branch
		} else if strings.HasPrefix(to, "HEAD~") {
			to = branch + to[len("HEAD"):]
		}
	}

	hard, err := common.GetOptionalParam[bool](document, "hard", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboReset: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBReset(connCtx, &backends.ResetParams{
		DBName:   dbName,
		Branch:   branch,
		CommitID: to,
		Hard:     hard,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBStatus implements the `dumboDBStatus` command.
//
// It returns the uncommitted changes on the branch encoded in $db (format: "dbname@branch").
// Usage: db.getSiblingDB("mydb@feature").runCommand({dumboDBStatus: 1})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, readOnly, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboStatus: versioning is not supported by the current backend",
		)
	}

	// Read-only connections (commit hash, ancestor, tag) have no working set.
	if readOnly {
		statusDoc := must.NotFail(types.NewDocument(
			"branch", branch,
			"dirty", false,
			"readonly", true,
		))

		// Resolve the rootish to a commit hash via doltLog limit:1.
		if logRes, logErr := vb.DumboDBLog(connCtx, &backends.LogParams{
			DBName: dbName, Branch: branch, ConnBranch: branch, Limit: 1,
		}); logErr == nil && len(logRes.Commits) > 0 {
			statusDoc.Set("commitId", logRes.Commits[0].CommitID)
		}

		statusDoc.Set("ok", float64(1))
		return documentOpMsg(statusDoc)
	}

	res, err := vb.DumboDBStatus(connCtx, &backends.VersioningStatusParams{
		DBName: dbName,
		Branch: branch,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	dirty := len(res.Tables) > 0 || res.MergeOp != ""

	collections := types.MakeArray(len(res.Tables))

	for _, t := range res.Tables {
		collections.Append(tableStatusToDoc(t))
	}

	statusDoc := must.NotFail(types.NewDocument(
		"branch", res.Branch,
		"dirty", dirty,
		"readonly", false,
	))
	if res.CommitID != "" {
		statusDoc.Set("commitId", res.CommitID)
	}
	statusDoc.Set("collections", collections)

	if res.MergeOp != "" {
		statusDoc.Set("mergeState", res.MergeOp)
		conflictsArr := types.MakeArray(len(res.Conflicts))
		for _, c := range res.Conflicts {
			entry := must.NotFail(types.NewDocument(
				"collection", c.Collection,
				"count", int32(c.Count),
			))
			conflictsArr.Append(entry)
		}
		statusDoc.Set("conflicts", conflictsArr)
	}

	statusDoc.Set("ok", float64(1))

	return documentOpMsg(statusDoc)
}

// MsgDumboDBCherryPick implements the `dumboDBCherryPick` command.
//
// Applies the diff introduced by the named commit onto the current branch encoded
// in $db and creates a new commit. On conflict, the cherry-pick is staged but not
// committed; use dumboDBConflicts / dumboDBResolveConflict to inspect and resolve
// conflicts, then dumboCherryPick continue:true to complete.
//
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({dumboDBCherryPick: 1, commit: "<hash>"})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBCherryPick: 1, abort: 1})
//	db.getSiblingDB("mydb@main").runCommand({dumboDBCherryPick: 1, continue: 1})
//
// Optional parameters for cherry-pick initiation:
//   - message (string): custom commit message (default: original message + annotation)
//   - author (string): 'Name <email>' for the commit author
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBCherryPick(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	abort, err := common.GetOptionalBoolOrIntParam(document, "abort", false)
	if err != nil {
		return nil, err
	}

	continueParam, err := common.GetOptionalBoolOrIntParam(document, "continue", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboCherryPick: versioning is not supported by the current backend",
		)
	}

	if abort {
		res, pickErr := vb.DumboDBCherryPick(connCtx, &backends.CherryPickParams{
			DBName: dbName,
			Branch: branch,
			Abort:  true,
		})
		if pickErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, pickErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"message", res.Message,
				"ok", float64(1),
			)),
		)
	}

	if continueParam {
		message, err := common.GetOptionalParam[string](document, "message", "")
		if err != nil {
			return nil, err
		}
		if v, _ := common.GetOptionalParam[string](document, "author", ""); v != "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"dumboCherryPick: 'author' is not supported; use 'committer' to set the committer identity",
				"author",
			)
		}
		committerParam, err := common.GetOptionalParam[string](document, "committer", "")
		if err != nil {
			return nil, err
		}
		res, pickErr := vb.DumboDBCherryPick(connCtx, &backends.CherryPickParams{
			DBName:    dbName,
			Branch:    branch,
			Continue:  true,
			Message:   message,
			Committer: committerParam,
		})
		if pickErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, pickErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"commitId", res.CommitID,
				"message", res.Message,
				"author", res.Author,
				"timestamp", time.UnixMilli(res.Timestamp),
				"committer", res.Committer,
				"committerTimestamp", time.UnixMilli(res.CommitterTimestamp),
				"ok", float64(1),
			)),
		)
	}

	commit, err := common.GetRequiredParam[string](document, "commit")
	if err != nil {
		return nil, err
	}

	if err := parseRootish(commit); err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboCherryPick: "+err.Error(),
			"commit",
		)
	}

	message, err := common.GetOptionalParam[string](document, "message", "")
	if err != nil {
		return nil, err
	}

	if v, _ := common.GetOptionalParam[string](document, "author", ""); v != "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboCherryPick: 'author' is not supported; use 'committer' to set the committer identity",
			"author",
		)
	}

	committerParam, err := common.GetOptionalParam[string](document, "committer", "")
	if err != nil {
		return nil, err
	}

	res, pickErr := vb.DumboDBCherryPick(connCtx, &backends.CherryPickParams{
		DBName:    dbName,
		Branch:    branch,
		Commit:    commit,
		Message:   message,
		Committer: committerParam,
	})

	if pickErr != nil {
		var conflictErr *backends.DumboDBCherryPickConflictError
		if errors.As(pickErr, &conflictErr) {
			// Return a structured ok:0 response with per-collection conflict counts.
			conflictsArr := types.MakeArray(len(conflictErr.Conflicts))
			for _, c := range conflictErr.Conflicts {
				entry := must.NotFail(types.NewDocument(
					"collection", c.Collection,
					"count", int32(c.Count),
				))
				conflictsArr.Append(entry)
			}
			return documentOpMsg(
				must.NotFail(types.NewDocument(
					"conflicts", conflictsArr,
					"ok", float64(0),
					"code", int32(handlererrors.ErrOperationFailed),
					"errmsg", conflictErr.Error(),
				)),
			)
		}
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, pickErr.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"message", res.Message,
			"author", res.Author,
			"timestamp", time.UnixMilli(res.Timestamp),
			"committer", res.Committer,
			"committerTimestamp", time.UnixMilli(res.CommitterTimestamp),
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBRebase implements the `doltRebase` command.
//
// Reapplies all commits on the current branch (encoded in $db) not reachable from Onto
// onto the tip of Onto, rewriting branch history. On conflict, the rebase is paused;
// use doltConflicts / doltResolveConflict to inspect and resolve conflicts, then
// doltRebase continue:true to proceed.
//
// Usage:
//
//	db.getSiblingDB("mydb@feature").runCommand({doltRebase: 1, onto: "main"})
//	db.getSiblingDB("mydb@feature").runCommand({doltRebase: 1, abort: 1})
//	db.getSiblingDB("mydb@feature").runCommand({doltRebase: 1, continue: 1})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBRebase(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	abort, err := common.GetOptionalBoolOrIntParam(document, "abort", false)
	if err != nil {
		return nil, err
	}

	continueParam, err := common.GetOptionalBoolOrIntParam(document, "continue", false)
	if err != nil {
		return nil, err
	}

	if v, _ := common.GetOptionalParam[string](document, "author", ""); v != "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboRebase: 'author' is not supported; use 'committer' to set the committer identity",
			"author",
		)
	}

	rebaseCommitterEarly, err := common.GetOptionalParam[string](document, "committer", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboRebase: versioning is not supported by the current backend",
		)
	}

	if abort {
		res, rebaseErr := vb.DumboDBRebase(connCtx, &backends.RebaseParams{
			DBName: dbName,
			Branch: branch,
			Abort:  true,
		})
		if rebaseErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, rebaseErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"newTip", res.NewTip,
				"ok", float64(1),
			)),
		)
	}

	if continueParam {
		res, rebaseErr := vb.DumboDBRebase(connCtx, &backends.RebaseParams{
			DBName:    dbName,
			Branch:    branch,
			Committer: rebaseCommitterEarly,
			Continue:  true,
		})
		if rebaseErr != nil {
			var conflictErr *backends.DumboDBRebaseConflictError
			if errors.As(rebaseErr, &conflictErr) {
				conflictsArr := types.MakeArray(len(conflictErr.Conflicts))
				for _, c := range conflictErr.Conflicts {
					entry := must.NotFail(types.NewDocument(
						"collection", c.Collection,
						"count", int32(c.Count),
					))
					conflictsArr.Append(entry)
				}
				return documentOpMsg(
					must.NotFail(types.NewDocument(
						"conflicts", conflictsArr,
						"conflictCommit", conflictErr.ConflictCommit,
						"ok", float64(0),
						"code", int32(handlererrors.ErrOperationFailed),
						"errmsg", conflictErr.Error(),
					)),
				)
			}
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, rebaseErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"commitsReplayed", int32(res.CommitsReplayed),
				"newTip", res.NewTip,
				"ok", float64(1),
			)),
		)
	}

	onto, err := common.GetRequiredParam[string](document, "onto")
	if err != nil {
		return nil, err
	}

	if err := parseRootish(onto); err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboRebase: "+err.Error(),
			"onto",
		)
	}

	res, rebaseErr := vb.DumboDBRebase(connCtx, &backends.RebaseParams{
		DBName:    dbName,
		Branch:    branch,
		Onto:      onto,
		Committer: rebaseCommitterEarly,
	})

	if rebaseErr != nil {
		var conflictErr *backends.DumboDBRebaseConflictError
		if errors.As(rebaseErr, &conflictErr) {
			conflictsArr := types.MakeArray(len(conflictErr.Conflicts))
			for _, c := range conflictErr.Conflicts {
				entry := must.NotFail(types.NewDocument(
					"collection", c.Collection,
					"count", int32(c.Count),
				))
				conflictsArr.Append(entry)
			}
			return documentOpMsg(
				must.NotFail(types.NewDocument(
					"conflicts", conflictsArr,
					"conflictCommit", conflictErr.ConflictCommit,
					"ok", float64(0),
					"code", int32(handlererrors.ErrOperationFailed),
					"errmsg", conflictErr.Error(),
				)),
			)
		}
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, rebaseErr.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitsReplayed", int32(res.CommitsReplayed),
			"newTip", res.NewTip,
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBRevert implements the `doltRevert` command.
//
// Applies the inverse diff introduced by the named commit onto the current branch,
// creating a new commit that undoes those changes. On conflict, the revert is staged
// but not committed; use doltConflicts / doltResolveConflict to inspect and resolve
// conflicts, then doltRevert continue:true to complete. Use abort:true to abandon.
//
// Usage:
//
//	db.getSiblingDB("mydb@main").runCommand({doltRevert: 1, commit: "<hash>"})
//	db.getSiblingDB("mydb@main").runCommand({doltRevert: 1, abort: 1})
//	db.getSiblingDB("mydb@main").runCommand({doltRevert: 1, continue: 1})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBRevert(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	abort, err := common.GetOptionalBoolOrIntParam(document, "abort", false)
	if err != nil {
		return nil, err
	}

	continueParam, err := common.GetOptionalBoolOrIntParam(document, "continue", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboRevert: versioning is not supported by the current backend",
		)
	}

	if abort {
		res, revertErr := vb.DumboDBRevert(connCtx, &backends.RevertParams{
			DBName: dbName,
			Branch: branch,
			Abort:  true,
		})
		if revertErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, revertErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"message", res.Message,
				"ok", float64(1),
			)),
		)
	}

	if continueParam {
		message, err := common.GetOptionalParam[string](document, "message", "")
		if err != nil {
			return nil, err
		}
		author, err := common.GetOptionalParam[string](document, "author", "")
		if err != nil {
			return nil, err
		}
		res, revertErr := vb.DumboDBRevert(connCtx, &backends.RevertParams{
			DBName:   dbName,
			Branch:   branch,
			Continue: true,
			Message:  message,
			Author:   author,
		})
		if revertErr != nil {
			return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, revertErr.Error())
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"commitId", res.CommitID,
				"message", res.Message,
				"author", res.Author,
				"timestamp", time.UnixMilli(res.Timestamp),
				"committer", res.Committer,
				"committerTimestamp", time.UnixMilli(res.CommitterTimestamp),
				"ok", float64(1),
			)),
		)
	}

	commit, err := common.GetRequiredParam[string](document, "commit")
	if err != nil {
		return nil, err
	}

	if err := parseRootish(commit); err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboRevert: "+err.Error(),
			"commit",
		)
	}

	message, err := common.GetOptionalParam[string](document, "message", "")
	if err != nil {
		return nil, err
	}

	author, err := common.GetOptionalParam[string](document, "author", "")
	if err != nil {
		return nil, err
	}

	res, revertErr := vb.DumboDBRevert(connCtx, &backends.RevertParams{
		DBName:  dbName,
		Branch:  branch,
		Commit:  commit,
		Message: message,
		Author:  author,
	})

	if revertErr != nil {
		var conflictErr *backends.DumboDBRevertConflictError
		if errors.As(revertErr, &conflictErr) {
			// Return a structured ok:0 response with per-collection conflict counts.
			conflictsArr := types.MakeArray(len(conflictErr.Conflicts))
			for _, c := range conflictErr.Conflicts {
				entry := must.NotFail(types.NewDocument(
					"collection", c.Collection,
					"count", int32(c.Count),
				))
				conflictsArr.Append(entry)
			}
			return documentOpMsg(
				must.NotFail(types.NewDocument(
					"conflicts", conflictsArr,
					"ok", float64(0),
					"code", int32(handlererrors.ErrOperationFailed),
					"errmsg", conflictErr.Error(),
				)),
			)
		}
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, revertErr.Error())
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"message", res.Message,
			"author", res.Author,
			"timestamp", time.UnixMilli(res.Timestamp),
			"committer", res.Committer,
			"committerTimestamp", time.UnixMilli(res.CommitterTimestamp),
			"ok", float64(1),
		)),
	)
}
// MsgDumboDBTag implements the `dumboTag` command.
//
// Tags share Dolt's tag refspec (refs/tags/<name>) and use the Dolt tag
// flatbuffer (TagValue), so tags created here are visible to `dolt tag`
// and tags created by `dolt tag` are listed here.
//
// Usage:
//
//	db.runCommand({dumboTag: 1})                                       // list all tags
//	db.runCommand({dumboTag: 1, name: "v1.0", hash: "<rootish>"})      // create tag at rootish
//	db.runCommand({dumboTag: 1, name: "v1.0"})                         // create tag at current branch HEAD
//	db.runCommand({dumboTag: 1, name: "v1.0", delete: true})           // delete tag
//
// Optional metadata for create:
//   - message (string): tag description
//   - author  (string): tagger name
//   - email   (string): tagger email
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBTag(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	name, err := common.GetOptionalParam[string](document, "name", "")
	if err != nil {
		return nil, err
	}

	deleteTag, err := common.GetOptionalBoolOrIntParam(document, "delete", false)
	if err != nil {
		return nil, err
	}

	tagHash, err := common.GetOptionalParam[string](document, "hash", "")
	if err != nil {
		return nil, err
	}

	message, err := common.GetOptionalParam[string](document, "message", "")
	if err != nil {
		return nil, err
	}

	author, err := common.GetOptionalParam[string](document, "author", "")
	if err != nil {
		return nil, err
	}

	if name == "" && deleteTag {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dumboTag: tag name is required for delete",
			"name",
		)
	}

	if name != "" {
		if strings.Contains(name, "@") {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"dumboTag: tag name must not contain '@' (reserved as the database/branch delimiter)",
				"name",
			)
		}
		if strings.ContainsAny(name, " \t\n") {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"dumboTag: tag name must not contain whitespace",
				"name",
			)
		}
		if err := validateRefName(name, "dumboTag"); err != nil {
			return nil, err
		}
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboTag: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBTag(connCtx, &backends.TagParams{
		DBName:  dbName,
		Branch:  branch,
		Name:    name,
		Hash:    tagHash,
		Delete:  deleteTag,
		Message: message,
		Author:  author,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	// For create/delete (name was provided), return a single tag object.
	// For list (no name), return an array of all tags.
	if name != "" && len(res.Tags) == 1 {
		t := res.Tags[0]
		doc := must.NotFail(types.NewDocument(
			"name", t.Name,
			"commitId", t.CommitID,
		))
		if t.Author != "" {
			doc.Set("author", t.Author)
			doc.Set("message", t.Message)
			doc.Set("timestamp", time.UnixMilli(t.Timestamp))
		}
		doc.Set("ok", float64(1))
		return documentOpMsg(doc)
	}

	// List mode: return all tags as an array.
	tagsArr := types.MakeArray(len(res.Tags))
	for _, t := range res.Tags {
		entry := must.NotFail(types.NewDocument(
			"name", t.Name,
			"commitId", t.CommitID,
			"author", t.Author,
			"message", t.Message,
			"timestamp", time.UnixMilli(t.Timestamp),
		))
		tagsArr.Append(entry)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"tags", tagsArr,
			"ok", float64(1),
		)),
	)
}

// MsgDumboDBGC implements the `dumboGC` command. It runs garbage
// collection on the database's chunk store, sweeping unreachable
// chunks (default mode) or rewriting every chunk (full mode).
//
// Usage:
//
//	db.runCommand({dumboGC: 1})                  // default mode
//	db.runCommand({dumboGC: 1, mode: "full"})    // full compaction
//
// The target database is implicit: the runCommand is scoped to one
// database via getSiblingDB or the connection URI. Branch selectors
// in the wire name (mydb@feature) collapse to the base database --
// one chunk store per logical database, holding every branch's
// chunks, and GC sweeps that store.
//
// Response shape:
//
//	{ok: 1, db, mode, durationMs, sizeBefore, sizeAfter,
//	 chunksBefore, chunksAfter}
//
// On error: {ok: 0, errmsg, code}.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDumboDBGC(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, _, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	mode, err := common.GetOptionalParam[string](document, "mode", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dumboGC: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DumboDBGC(connCtx, &backends.GCParams{
		DBName: dbName,
		Mode:   mode,
	})
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsg(handlererrors.ErrOperationFailed, err.Error())
	}

	// Encode numeric fields as float64 (BSON Double). Int64s
	// serialize as {"low","high"} via mongosh JSON.stringify, which
	// most clients have to special-case; double avoids that. Chunk
	// counts and byte sizes both fit exactly under 2^53.
	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"db", res.DB,
			"mode", res.Mode,
			"durationMs", float64(res.DurationMs),
			"sizeBefore", float64(res.SizeBefore),
			"sizeAfter", float64(res.SizeAfter),
			"chunksBefore", float64(res.ChunksBefore),
			"chunksAfter", float64(res.ChunksAfter),
			"ok", float64(1),
		)),
	)
}
