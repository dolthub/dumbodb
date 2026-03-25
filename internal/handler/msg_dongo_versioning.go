// Copyright 2024 Dolt Inc.
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

	dbName, _ := branchFromDBName(encodedDB)

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
			entry := must.NotFail(types.NewDocument(
				"_id", m.ID,
				"a", m.A,
				"b", m.B,
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

// branchFromDBName parses the real database name and branch from an encoded db name.
//
// Dongo encodes branch information in the database name using a double-underscore separator:
// "mydb__branchname" → dbName="mydb", branch="branchname"
//
// If no separator is present the branch defaults to "main".
func branchFromDBName(encoded string) (dbName, branch string) {
	if idx := strings.Index(encoded, "__"); idx >= 0 {
		return encoded[:idx], encoded[idx+2:]
	}

	return encoded, "main"
}

// versioningBackend returns the VersioningBackend for the handler, or nil if not supported.
func (h *Handler) versioningBackend() backends.VersioningBackend {
	vb, ok := h.b.(backends.VersioningBackend)
	if !ok {
		return nil
	}

	return vb
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

	dbName, branch := branchFromDBName(encodedDB)

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

	dbName, fromBranch := branchFromDBName(encodedDB)

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

	dbName, intoBranch := branchFromDBName(encodedDB)

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

	dbName, branch := branchFromDBName(encodedDB)

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

	dbName, branch := branchFromDBName(encodedDB)

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
