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

	"github.com/dolthub/docudolt/internal/backends"
	"github.com/dolthub/docudolt/internal/handler/common"
	"github.com/dolthub/docudolt/internal/handler/handlererrors"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/lazyerrors"
	"github.com/dolthub/docudolt/internal/util/must"
)

// dbBranchSep is the separator between the database name and rootish in an
// encoded database name (e.g. "mydb__d_main", "mydb__d_feature/foo").
// Must match the value in internal/backends/dolt/backend.go.
const dbBranchSep = "__d_"

// MsgDocuDoltDiff implements the `docuDoltDiff` command.
//
// Returns the document-level diff between two states for the branch encoded in $db.
// Usage:
//
//	db.adminCommand({docuDoltDiff: 1})                          // working set vs HEAD
//	db.adminCommand({docuDoltDiff: 1, from: "<hash>"})          // commit hash to working set
//	db.adminCommand({docuDoltDiff: 1, from: "<hash>", to: "<hash>"}) // between two commits
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltDiff(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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
			"doltDiff: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltDiff(connCtx, &backends.DiffParams{
		DBName:      dbName,
		ConnRootish: connRootish,
		From:        from,
		To:          to,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	collections := types.MakeArray(len(res.Collections))

	for _, cd := range res.Collections {
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
				pairs := []any{"type", fd.Type, "path", fd.Path}
				if fd.Type != "added" {
					pairs = append(pairs, "from", fd.From)
				}
				if fd.Type != "removed" {
					pairs = append(pairs, "to", fd.To)
				}
				diffEntry := must.NotFail(types.NewDocument(pairs...))
				diffArray.Append(diffEntry)
			}
			entry := must.NotFail(types.NewDocument(
				"_id", m.ID,
				"diff", diffArray,
			))
			modified.Append(entry)
		}

		collEntry := must.NotFail(types.NewDocument(
			"name", cd.Name,
			"added", added,
			"removed", removed,
			"modified", modified,
		))
		collections.Append(collEntry)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"collections", collections,
			"ok", float64(1),
		)),
	)
}

// branchFromDBName parses the real database name and rootish from an encoded db name.
//
// DocuDolt encodes version information in the database name using the __d_ separator:
//
//	"mydb__d_branchname"                        → dbName="mydb", rootish="branchname",                        readOnly=false
//	"mydb__d_na7kfra98h45fr2u5qtr30o2ggm7vh61" → dbName="mydb", rootish="na7kfra98h45fr2u5qtr30o2ggm7vh61", readOnly=true  (commit hash)
//	"mydb__d_main~3"                            → dbName="mydb", rootish="main~3",                            readOnly=true  (ancestor expression)
//
// If no separator is present the rootish defaults to "main" and readOnly is false.
//
// readOnly is true when the rootish is syntactically a commit hash or ancestor expression.
// Bare names are assumed to be branch names (writable); tag detection requires a backend call
// not performed here.
//
// The rootish is validated by parseRootish; an error is returned for unsupported forms.
//
// All-digit strings after __d_ (e.g. Unix nanosecond timestamps used as database name suffixes)
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
		// like "parity_test__1775505756999075683" work without error.
		if !rootishAllDigits(candidate) {
			if err = parseRootish(candidate); err != nil {
				return "", "", false, err
			}
			return encoded[:idx], candidate, rootishIsReadOnly(candidate), nil
		}
	}

	return encoded, "main", false, nil
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
// Rejected forms (returned as ErrOperationFailed):
//   - HEAD and HEAD-relative forms (HEAD, HEAD~1, HEAD^)
//   - Reflog syntax (<ref>@{<spec>})
//   - Range syntax (<ref>..<ref>)
//   - Regex commit search (:/<pattern>)
//   - Type dereferencing (<ref>^{<type>})
//   - Caret parent selection (<ref>^, <ref>^N)
func parseRootish(s string) error {
	if s == "" {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"rootish must not be empty",
		)
	}

	// Reject HEAD and HEAD-relative forms.
	if s == "HEAD" || strings.HasPrefix(s, "HEAD~") || strings.HasPrefix(s, "HEAD^") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: HEAD and HEAD-relative forms are not supported; use a branch name, tag, commit hash, or <branch>~<N>", s),
		)
	}

	// Reject reflog syntax (<ref>@{...}).
	if strings.Contains(s, "@{") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: reflog syntax is not supported", s),
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

	// Reject caret forms: type dereferencing (^{...}) and caret parent selection (^, ^N).
	if strings.Contains(s, "^") {
		return handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			fmt.Sprintf("rootish %q: caret syntax (^, ^N, ^{type}) is not supported; use ~N for ancestor traversal", s),
		)
	}

	// Validate relative ancestor expression <branch>~<N>.
	if idx := strings.LastIndex(s, "~"); idx >= 0 {
		branch := s[:idx]
		nStr := s[idx+1:]
		if branch == "" {
			return handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				fmt.Sprintf("rootish %q: branch name must not be empty in relative ancestor expression", s),
			)
		}
		if nStr == "" {
			return handlererrors.NewCommandErrorMsg(
				handlererrors.ErrOperationFailed,
				fmt.Sprintf("rootish %q: ancestor count must not be empty in <branch>~<N>", s),
			)
		}
		for _, c := range nStr {
			if c < '0' || c > '9' {
				return handlererrors.NewCommandErrorMsg(
					handlererrors.ErrOperationFailed,
					fmt.Sprintf("rootish %q: ancestor count must be a non-negative integer in <branch>~<N>", s),
				)
			}
		}
	}

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

// MsgDocuDoltCurrentBranch implements the `docuDoltCurrentBranch` command.
//
// It returns the branch name for the connection encoded in $db.
// Usage: db.getSiblingDB("mydb__d_feature").runCommand({docuDoltCurrentBranch: 1})
//
// Returns an OperationFailed error if the connection is read-only (commit hash or ancestor expression).
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltCurrentBranch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, readOnly, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	if readOnly {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltCurrentBranch: no current branch name (connection is at a specific commit, not a named branch)",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltCurrentBranch: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltCurrentBranch(connCtx, &backends.CurrentBranchParams{
		DBName: dbName,
		Branch: branch,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"branch", res.Branch,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltCommit implements the `docuDoltCommit` command.
//
// It commits the current working set on the branch encoded in $db (format: "dbname__d_branch").
// Usage: db.getSiblingDB("mydb__d_feature").runCommand({docuDoltCommit: 1, message: "my commit"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltCommit(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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

	author, err := common.GetRequiredParam[string](document, "author")
	if err != nil {
		return nil, err
	}

	ts, err := common.GetOptionalParam[time.Time](document, "timestamp", time.Time{})
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltCommit: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltCommit(connCtx, &backends.CommitParams{
		DBName:    dbName,
		Branch:    branch,
		Message:   message,
		Author:    author,
		Timestamp: ts,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"branch", res.Branch,
			"message", res.Message,
			"author", res.Author,
			"timestamp", time.UnixMilli(res.Timestamp),
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltBranch implements the `docuDoltBranch` command.
//
// It creates a new branch from the current branch encoded in $db (format: "dbname__d_branch").
// Usage: db.getSiblingDB("mydb__d_main").runCommand({docuDoltBranch: 1, branch: "feature"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltBranch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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
			"doltBranch: branch name must not be empty",
			"branch",
		)
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
			"doltBranch: delete and forceDelete are mutually exclusive",
			"delete",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltBranch: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltBranch(connCtx, &backends.BranchParams{
		DBName: dbName,
		From:   fromBranch,
		Name:   newBranch,
		Delete: safeDelete || forceDelete,
		Force:  forceDelete,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"branch", res.Branch,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltMerge implements the `docuDoltMerge` command.
//
// Merges a source branch into the current branch encoded in $db (format: "dbname__d_branch").
// Usage:
//
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltMerge: 1, merge_in: "feature"})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltMerge: 1, merge_in: "feature", noFF: true})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltMerge: 1, merge_in: "feature", ffOnly: true})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltMerge: 1, continue: true})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltMerge: 1, abort: true})
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
// values for conflicting documents. Use docuDoltResolveConflict to resolve conflicts, then
// docuDoltMerge continue:true to complete the merge.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltMerge(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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
			"doltMerge: versioning is not supported by the current backend",
		)
	}

	if abort {
		res, mergeErr := vb.DocuDoltMerge(connCtx, &backends.MergeParams{
			DBName: dbName,
			Into:   intoBranch,
			Abort:  true,
		})
		if mergeErr != nil {
			return nil, lazyerrors.Error(mergeErr)
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
		res, mergeErr := vb.DocuDoltMerge(connCtx, &backends.MergeParams{
			DBName:   dbName,
			Into:     intoBranch,
			Continue: true,
			Message:  message,
			Author:   author,
		})
		if mergeErr != nil {
			return nil, lazyerrors.Error(mergeErr)
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"commitId", res.CommitID,
				"message", res.Message,
				"ok", float64(1),
			)),
		)
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
			"doltMerge: noFF and ffOnly are mutually exclusive",
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
			"doltMerge: from branch name must not be empty",
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

	res, mergeErr := vb.DocuDoltMerge(connCtx, &backends.MergeParams{
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
		return nil, lazyerrors.Error(mergeErr)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"message", res.Message,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltConflicts implements the `docuDoltConflicts` command.
//
// Returns conflict information for the current in-progress merge on the branch encoded in $db.
// Usage:
//
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltConflicts: 1})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltConflicts: 1, collection: "items"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltConflicts(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, branch, _, err := branchFromDBName(encodedDB)
	if err != nil {
		return nil, err
	}

	collection, err := common.GetOptionalParam[string](document, "collection", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltConflicts: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltConflicts(connCtx, &backends.ConflictsParams{
		DBName:     dbName,
		Branch:     branch,
		Collection: collection,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if collection == "" {
		collectionsArr := types.MakeArray(len(res.Collections))
		for _, c := range res.Collections {
			entry := must.NotFail(types.NewDocument(
				"name", c.Collection,
				"count", int32(c.Count),
			))
			collectionsArr.Append(entry)
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"collections", collectionsArr,
				"ok", float64(1),
			)),
		)
	}

	conflictsArr := types.MakeArray(len(res.Conflicts))
	for _, cf := range res.Conflicts {
		pairs := []any{
			"conflictId", cf.ConflictID,
		}

		if cf.Base != nil {
			pairs = append(pairs, "base", cf.Base)
		} else {
			pairs = append(pairs, "base", types.Null)
		}

		if cf.Ours != nil {
			pairs = append(pairs, "ours", cf.Ours)
		} else {
			pairs = append(pairs, "ours", types.Null)
		}

		if cf.Theirs != nil {
			pairs = append(pairs, "theirs", cf.Theirs)
		} else {
			pairs = append(pairs, "theirs", types.Null)
		}

		pairs = append(pairs,
			"ourDiffType", cf.OurDiffType,
			"theirDiffType", cf.TheirDiffType,
		)

		entry := must.NotFail(types.NewDocument(pairs...))
		conflictsArr.Append(entry)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"conflicts", conflictsArr,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltResolveConflict implements the `docuDoltResolveConflict` command.
//
// Resolves a single document conflict in the current in-progress merge.
// Usage:
//
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltResolveConflict: 1, collection: "items", conflictId: "c0", resolution: "ours"})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltResolveConflict: 1, collection: "items", conflictId: "c0", resolution: "theirs"})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltResolveConflict: 1, collection: "items", conflictId: "c0", resolution: "custom", value: {_id:1, v:42}})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltResolveConflict(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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
				"doltResolveConflict: resolution 'custom' requires a 'value' document",
				"value",
			)
		}
		value = rawValue
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltResolveConflict: versioning is not supported by the current backend",
		)
	}

	_, err = vb.DocuDoltResolveConflict(connCtx, &backends.ResolveConflictParams{
		DBName:     dbName,
		Branch:     branch,
		Collection: collection,
		ConflictID: conflictID,
		Resolution: resolution,
		Value:      value,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltLog implements the `docuDoltLog` command.
//
// It returns the commit history for the branch encoded in $db (format: "dbname__d_branch").
// Usage: db.getSiblingDB("mydb__d_feature").runCommand({docuDoltLog: 1, limit: 10})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltLog(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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

	from, err := common.GetOptionalParam[string](document, "from", "")
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltLog: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltLog(connCtx, &backends.LogParams{
		DBName:     dbName,
		Branch:     branch,
		ConnBranch: branch,
		Limit:      limit,
		From:       from,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
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
		)
		entry := must.NotFail(types.NewDocument(pairs...))
		commits.Append(entry)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"branch", branch,
			"commits", commits,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltReset implements the `docuDoltReset` command.
//
// It moves the branch HEAD to the specified commit hash. Two modes:
//
//	Soft (default): HEAD moves to target, working tree is untouched, staged root = target.
//	Hard (hard: true): HEAD moves to target, working tree and staged root are reset to target.
//
// Usage:
//
//	db.runCommand({docuDoltReset: 1})                       // reset to HEAD (discard uncommitted changes if hard)
//	db.runCommand({docuDoltReset: 1, to: "<hash>"})
//	db.runCommand({docuDoltReset: 1, to: "<hash>", hard: true})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltReset(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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

	hard, err := common.GetOptionalParam[bool](document, "hard", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"doltReset: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltReset(connCtx, &backends.ResetParams{
		DBName:   dbName,
		Branch:   branch,
		CommitID: to,
		Hard:     hard,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltStatus implements the `docuDoltStatus` command.
//
// It returns the uncommitted changes on the branch encoded in $db (format: "dbname__d_branch").
// Usage: db.getSiblingDB("mydb__d_feature").runCommand({docuDoltStatus: 1})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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
			"doltStatus: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DocuDoltStatus(connCtx, &backends.VersioningStatusParams{
		DBName: dbName,
		Branch: branch,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	collections := types.MakeArray(len(res.Tables))

	for _, t := range res.Tables {
		entry := must.NotFail(types.NewDocument(
			"name", t.Name,
			"status", t.Status,
		))
		collections.Append(entry)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"branch", res.Branch,
			"collections", collections,
			"ok", float64(1),
		)),
	)
}

// MsgDocuDoltCherryPick implements the `docuDoltCherryPick` command.
//
// Applies the diff introduced by the named commit onto the current branch encoded
// in $db and creates a new commit. On conflict, the cherry-pick is staged but not
// committed; use docuDoltConflicts / docuDoltResolveConflict to inspect and resolve
// conflicts, then docuDoltCherryPick continue:true to complete.
//
// Usage:
//
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltCherryPick: 1, commit: "<hash>"})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltCherryPick: 1, abort: 1})
//	db.getSiblingDB("mydb__d_main").runCommand({docuDoltCherryPick: 1, continue: 1})
//
// Optional parameters for cherry-pick initiation:
//   - message (string): custom commit message (default: original message + annotation)
//   - author (string): 'Name <email>' for the commit author
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDocuDoltCherryPick(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
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
			"doltCherryPick: versioning is not supported by the current backend",
		)
	}

	if abort {
		res, pickErr := vb.DocuDoltCherryPick(connCtx, &backends.CherryPickParams{
			DBName: dbName,
			Branch: branch,
			Abort:  true,
		})
		if pickErr != nil {
			return nil, lazyerrors.Error(pickErr)
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
		res, pickErr := vb.DocuDoltCherryPick(connCtx, &backends.CherryPickParams{
			DBName:   dbName,
			Branch:   branch,
			Continue: true,
			Message:  message,
			Author:   author,
		})
		if pickErr != nil {
			return nil, lazyerrors.Error(pickErr)
		}
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"commitId", res.CommitID,
				"message", res.Message,
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
			"doltCherryPick: "+err.Error(),
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

	res, pickErr := vb.DocuDoltCherryPick(connCtx, &backends.CherryPickParams{
		DBName:  dbName,
		Branch:  branch,
		Commit:  commit,
		Message: message,
		Author:  author,
	})

	if pickErr != nil {
		var conflictErr *backends.DocuDoltCherryPickConflictError
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
		return nil, lazyerrors.Error(pickErr)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"commitId", res.CommitID,
			"message", res.Message,
			"ok", float64(1),
		)),
	)
}
