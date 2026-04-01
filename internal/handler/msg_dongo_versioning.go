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
	"fmt"
	"strings"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgDongoDiff implements the `dongoDiff` command.
//
// Returns the document-level diff between two states for the branch encoded in $db.
// Usage:
//
//	db.adminCommand({dongoDiff: 1})                          // working set vs HEAD
//	db.adminCommand({dongoDiff: 1, from: "<hash>"})          // commit hash to working set
//	db.adminCommand({dongoDiff: 1, from: "<hash>", to: "<hash>"}) // between two commits
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoDiff(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	encodedDB, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	dbName, _, _, err := branchFromDBName(encodedDB)
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
			"dongoDiff: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoDiff(connCtx, &backends.DiffParams{
		DBName: dbName,
		From:   from,
		To:     to,
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
					pairs = append(pairs, "a", fd.A)
				}
				if fd.Type != "removed" {
					pairs = append(pairs, "b", fd.B)
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
// Dongo encodes version information in the database name using a double-underscore separator:
//
//	"mydb__branchname"                        → dbName="mydb", rootish="branchname",                        readOnly=false
//	"mydb__na7kfra98h45fr2u5qtr30o2ggm7vh61" → dbName="mydb", rootish="na7kfra98h45fr2u5qtr30o2ggm7vh61", readOnly=true  (commit hash)
//	"mydb__main~3"                            → dbName="mydb", rootish="main~3",                            readOnly=true  (ancestor expression)
//
// If no separator is present the rootish defaults to "main" and readOnly is false.
//
// readOnly is true when the rootish is syntactically a commit hash or ancestor expression.
// Bare names are assumed to be branch names (writable); tag detection requires a backend call
// not performed here.
//
// The rootish is validated by parseRootish; an error is returned for unsupported forms.
func branchFromDBName(encoded string) (dbName, rootish string, readOnly bool, err error) {
	if idx := strings.Index(encoded, "__"); idx > 0 {
		rootish = encoded[idx+2:]
		if err = parseRootish(rootish); err != nil {
			return "", "", false, err
		}
		return encoded[:idx], rootish, rootishIsReadOnly(rootish), nil
	}

	return encoded, "main", false, nil
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

// MsgDongoCurrentBranch implements the `dongoCurrentBranch` command.
//
// It returns the branch name for the connection encoded in $db.
// Usage: db.getSiblingDB("mydb__feature").runCommand({dongoCurrentBranch: 1})
//
// Returns an OperationFailed error if the connection is read-only (commit hash or ancestor expression).
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoCurrentBranch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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
			"dongoCurrentBranch: connection is read-only (commit hash or ancestor expression); there is no current branch",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dongoCurrentBranch: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoCurrentBranch(connCtx, &backends.CurrentBranchParams{
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

// MsgDongoCommit implements the `dongoCommit` command.
//
// It commits the current working set on the branch encoded in $db (format: "dbname__branch").
// Usage: db.getSiblingDB("mydb__feature").runCommand({dongoCommit: 1, message: "my commit"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoCommit(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dongoCommit: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoCommit(connCtx, &backends.CommitParams{
		DBName:  dbName,
		Branch:  branch,
		Message: message,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"hash", res.Hash,
			"branch", res.Branch,
			"message", res.Message,
			"ok", float64(1),
		)),
	)
}

// MsgDongoBranch implements the `dongoBranch` command.
//
// It creates a new branch from the current branch encoded in $db (format: "dbname__branch").
// Usage: db.getSiblingDB("mydb__main").runCommand({dongoBranch: 1, branch: "feature"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoBranch(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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
			"dongoBranch: branch name must not be empty",
			"branch",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dongoBranch: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoBranch(connCtx, &backends.BranchParams{
		DBName: dbName,
		From:   fromBranch,
		Name:   newBranch,
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

// MsgDongoMerge implements the `dongoMerge` command.
//
// It merges a source branch into the current branch encoded in $db (format: "dbname__branch").
// Usage: db.getSiblingDB("mydb__main").runCommand({dongoMerge: 1, from: "feature"})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoMerge(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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

	fromBranch, err := common.GetRequiredParam[string](document, "from")
	if err != nil {
		return nil, err
	}

	if fromBranch == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dongoMerge: from branch name must not be empty",
			"from",
		)
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dongoMerge: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoMerge(connCtx, &backends.MergeParams{
		DBName: dbName,
		Into:   intoBranch,
		From:   fromBranch,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"hash", res.Hash,
			"message", res.Message,
			"ok", float64(1),
		)),
	)
}

// MsgDongoLog implements the `dongoLog` command.
//
// It returns the commit history for the branch encoded in $db (format: "dbname__branch").
// Usage: db.getSiblingDB("mydb__feature").runCommand({dongoLog: 1, limit: 10})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoLog(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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
			"dongoLog: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoLog(connCtx, &backends.LogParams{
		DBName: dbName,
		Branch: branch,
		Limit:  limit,
		From:   from,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	commits := types.MakeArray(len(res.Commits))

	for _, c := range res.Commits {
		pairs := []any{
			"hash", c.Hash,
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

// MsgDongoReset implements the `dongoReset` command.
//
// It moves the branch HEAD to the specified commit hash. Two modes:
//
//	Soft (default): HEAD moves to target, working tree is untouched, staged root = target.
//	Hard (hard: true): HEAD moves to target, working tree and staged root are reset to target.
//
// Usage:
//
//	db.runCommand({dongoReset: 1, to: "<hash>"})
//	db.runCommand({dongoReset: 1, to: "<hash>", hard: true})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoReset(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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

	to, err := common.GetRequiredParam[string](document, "to")
	if err != nil {
		return nil, err
	}

	if to == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"dongoReset: 'to' parameter must not be empty",
			"to",
		)
	}

	hard, err := common.GetOptionalParam[bool](document, "hard", false)
	if err != nil {
		return nil, err
	}

	vb := h.versioningBackend()
	if vb == nil {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrOperationFailed,
			"dongoReset: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoReset(connCtx, &backends.ResetParams{
		DBName: dbName,
		Branch: branch,
		Hash:   to,
		Hard:   hard,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"hash", res.Hash,
			"ok", float64(1),
		)),
	)
}

// MsgDongoStatus implements the `dongoStatus` command.
//
// It returns the uncommitted changes on the branch encoded in $db (format: "dbname__branch").
// Usage: db.getSiblingDB("mydb__feature").runCommand({dongoStatus: 1})
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgDongoStatus(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
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
			"dongoStatus: versioning is not supported by the current backend",
		)
	}

	res, err := vb.DongoStatus(connCtx, &backends.VersioningStatusParams{
		DBName: dbName,
		Branch: branch,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	tables := types.MakeArray(len(res.Tables))

	for _, t := range res.Tables {
		entry := must.NotFail(types.NewDocument(
			"name", t.Name,
			"status", t.Status,
		))
		tables.Append(entry)
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"branch", res.Branch,
			"tables", tables,
			"ok", float64(1),
		)),
	)
}
