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
	"strings"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgDumboDBTag implements the `dumboTag` admin command.
//
// Tags share Dolt's tag refspec (refs/tags/<name>) and use the Dolt tag
// flatbuffer (TagValue), so tags created here are visible to `dolt tag`
// and tags created by `dolt tag` are listed here.
//
// Usage:
//
//	db.adminCommand({dumboTag: 1})                                       // list all tags
//	db.adminCommand({dumboTag: 1, name: "v1.0", hash: "<rootish>"})      // create tag at rootish
//	db.adminCommand({dumboTag: 1, name: "v1.0"})                         // create tag at current branch HEAD
//	db.adminCommand({dumboTag: 1, name: "v1.0", delete: true})           // delete tag
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

	email, err := common.GetOptionalParam[string](document, "email", "")
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
		Email:   email,
	})
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	tagsArr := types.MakeArray(len(res.Tags))
	for _, t := range res.Tags {
		entry := must.NotFail(types.NewDocument(
			"name", t.Name,
			"commitId", t.CommitID,
			"tagger", t.Tagger,
			"email", t.Email,
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
