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

	"github.com/FerretDB/wire"

	"github.com/dolthub/dongo/internal/backends"
	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// MsgCollMod implements `collMod` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgCollMod(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	ignoredFields := []string{
		"index",
		"pipeline",
		"viewOn",
		"recordPreImages",
		"changeStreamPreAndPostImages",
		"expireAfterSeconds",
		"timeseries",
		"writeConcern",
		"comment",
	}
	common.Ignored(document, h.L, ignoredFields...)

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	command := document.Command()

	collectionName, err := common.GetRequiredParam[string](document, command)
	if err != nil {
		return nil, err
	}

	params := backends.CollModParams{
		Name: collectionName,
	}

	// Parse validator.
	if validatorVal, _ := document.Get("validator"); validatorVal != nil {
		params.SetValidator = true
		switch v := validatorVal.(type) {
		case *types.Document:
			params.Validator = v
		case types.NullType:
			params.Validator = nil
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validator' option must be a document",
				"collMod",
			)
		}
	}

	// Parse validationLevel.
	if validationLevelVal, _ := document.Get("validationLevel"); validationLevelVal != nil {
		level, ok := validationLevelVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationLevel' must be a string ('off', 'strict', or 'moderate')",
				"collMod",
			)
		}
		switch level {
		case "off", "strict", "moderate":
			params.ValidationLevel = level
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationLevel' must be 'off', 'strict', or 'moderate'",
				"collMod",
			)
		}
	}

	// Parse validationAction.
	if validationActionVal, _ := document.Get("validationAction"); validationActionVal != nil {
		action, ok := validationActionVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationAction' must be a string ('error' or 'warn')",
				"collMod",
			)
		}
		switch action {
		case "error", "warn":
			params.ValidationAction = action
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationAction' must be 'error' or 'warn'",
				"collMod",
			)
		}
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := fmt.Sprintf("Invalid namespace specified '%s'", dbName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "collMod")
		}

		return nil, lazyerrors.Error(err)
	}

	if err = db.CollMod(connCtx, &params); err != nil {
		switch {
		case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionDoesNotExist):
			msg := fmt.Sprintf("ns does not exist: %s.%s", dbName, collectionName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNamespaceNotFound, msg, "collMod")
		case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid):
			msg := fmt.Sprintf("Invalid collection name: %s", collectionName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "collMod")
		default:
			return nil, lazyerrors.Error(err)
		}
	}

	return documentOpMsg(
		must.NotFail(types.NewDocument(
			"ok", float64(1),
		)),
	)
}
