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
	"strings"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func (h *Handler) MsgCreate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = common.RejectUnknownFields(document,
		"capped",
		"size",
		"max",
		"storageEngine",
		"validator",
		"validationLevel",
		"validationAction",
		"indexOptionDefaults",
		"viewOn",
		"pipeline",
		"collation",
		"expireAfterSeconds",
		"timeseries",
		"clusteredIndex",
		"changeStreamPreAndPostImages",
		"autoIndexId",
		"temp",
		"flags",
		"idIndex",
		"encryptedFields",
		"recordIdsReplicated",
	); err != nil {
		return nil, err
	}

	ignoredFields := []string{
		"autoIndexId",
		"storageEngine",
		"indexOptionDefaults",
		"writeConcern",
		"comment",
	}
	common.Ignored(document, h.L, ignoredFields...)

	command := document.Command()

	dbName, err := common.GetRequiredParam[string](document, "$db")
	if err != nil {
		return nil, err
	}

	if err = enforceWritableRootish(dbName); err != nil {
		return nil, err
	}

	collectionName, err := common.GetRequiredParam[string](document, command)
	if err != nil {
		return nil, err
	}

	params := backends.CreateCollectionParams{
		Name: collectionName,
	}

	// hasExplicitOptions tracks whether the caller specified any create options.
	// If an already-existing collection is created with explicit options, it's an error.
	var hasExplicitOptions bool

	if v, _ := document.Get("capped"); v != nil {
		capped, err := handlerparams.GetBoolOptionalParam("capped", v)
		if err != nil {
			return nil, err
		}
		if capped {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidOptions,
				"capped collections are not supported by DumboDB",
				"create",
			)
		}
	}

	// TTL is rejected rather than silently ignored: a wall-clock sweeper that
	// deletes data conflicts with version control (a historical checkout would
	// lose its still-live-at-the-time documents). Covers both regular and
	// time-series collections, which both carry a top-level expireAfterSeconds.
	if v, _ := document.Get("expireAfterSeconds"); v != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrInvalidOptions,
			"TTL (expireAfterSeconds) is not supported by DumboDB",
			"create",
		)
	}

	if sizeVal, _ := document.Get("size"); sizeVal != nil {
		// size was provided without capped=true  -- still counts as explicit options.
		hasExplicitOptions = true
	}

	if tsVal, _ := document.Get("timeseries"); tsVal != nil {
		tsDoc, ok := tsVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'timeseries' option must be a document",
				"create",
			)
		}
		hasExplicitOptions = true
		params.IsTimeSeries = true

		timeFieldVal, _ := tsDoc.Get("timeField")
		if timeFieldVal == nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'timeField' option must be set",
				"create",
			)
		}
		timeField, ok := timeFieldVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'timeField' option must be a string",
				"create",
			)
		}
		params.TimeField = timeField

		if metaFieldVal, _ := tsDoc.Get("metaField"); metaFieldVal != nil {
			metaField, ok := metaFieldVal.(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"'metaField' option must be a string",
					"create",
				)
			}
			params.MetaField = metaField
		}

		if granularityVal, _ := tsDoc.Get("granularity"); granularityVal != nil {
			granularity, ok := granularityVal.(string)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"'granularity' option must be a string",
					"create",
				)
			}
			switch granularity {
			case "seconds", "minutes", "hours":
				params.Granularity = granularity
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"'granularity' option must be 'seconds', 'minutes', or 'hours'",
					"create",
				)
			}
		}
	}

	if viewOnVal, _ := document.Get("viewOn"); viewOnVal != nil {
		viewOn, ok := viewOnVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'viewOn' option must be a string",
				"create",
			)
		}
		params.ViewOn = viewOn
		hasExplicitOptions = true

		if pipelineVal, _ := document.Get("pipeline"); pipelineVal != nil {
			pipeline, ok := pipelineVal.(*types.Array)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"'pipeline' option must be an array",
					"create",
				)
			}
			params.ViewPipeline = pipeline
		}
	}

	if validatorVal, _ := document.Get("validator"); validatorVal != nil {
		hasExplicitOptions = true
		validatorDoc, ok := validatorVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validator' option must be a document",
				"create",
			)
		}
		params.Validator = validatorDoc
	}

	if collationVal, _ := document.Get("collation"); collationVal != nil {
		hasExplicitOptions = true
		collationDoc, ok := collationVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'collation' must be a document",
				"create",
			)
		}
		if loc := collation.Parse(collationDoc).Locale; !collation.LocaleAccepted(loc) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				fmt.Sprintf("Field 'locale' is invalid in: { locale: %q }", loc),
				"create",
			)
		}
		params.Collation = collationDoc
	}

	if validationLevelVal, _ := document.Get("validationLevel"); validationLevelVal != nil {
		level, ok := validationLevelVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationLevel' must be a string ('off', 'strict', or 'moderate')",
				"create",
			)
		}
		switch level {
		case "off", "strict", "moderate":
			params.ValidationLevel = level
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationLevel' must be 'off', 'strict', or 'moderate'",
				"create",
			)
		}
	}

	if validationActionVal, _ := document.Get("validationAction"); validationActionVal != nil {
		action, ok := validationActionVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationAction' must be a string ('error' or 'warn')",
				"create",
			)
		}
		switch action {
		case "error", "warn":
			params.ValidationAction = action
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validationAction' must be 'error' or 'warn'",
				"create",
			)
		}
	}

	if collectionName == "" {
		msg := fmt.Sprintf("Invalid namespace specified '%s.'", dbName)
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "create")
	}

	if strings.ContainsRune(collectionName, '\x00') {
		msg := "namespaces cannot have embedded null characters"
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "create")
	}

	if strings.HasPrefix(collectionName, ".") {
		msg := fmt.Sprintf("Collection names cannot start with '.': %s", collectionName)
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "create")
	}

	const maxNamespaceLen = 255
	ns := dbName + "." + collectionName
	if len(ns) > maxNamespaceLen {
		msg := fmt.Sprintf("Fully qualified namespace is too long. Namespace: %s Max: %d", ns, maxNamespaceLen)
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "create")
	}

	db, err := h.b.Database(dbName)
	if err != nil {
		if backends.ErrorCodeIs(err, backends.ErrorCodeDatabaseNameIsInvalid) {
			msg := invalidDatabaseNameMsg(dbName, collectionName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "create")
		}

		return nil, lazyerrors.Error(err)
	}

	if params.ViewOn != "" {
		if verr := validateViewChainAcyclic(connCtx, db, collectionName, params.ViewOn); verr != nil {
			return nil, verr
		}
	}

	err = db.CreateCollection(connCtx, &params)

	switch {
	case err == nil:
		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"ok", float64(1),
			)),
		)

	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionNameIsInvalid):
		msg := fmt.Sprintf("Invalid collection name: %s", collectionName)
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidNamespace, msg, "create")

	case backends.ErrorCodeIs(err, backends.ErrorCodeCollectionAlreadyExists):
		// MongoDB 8.0: idempotent OK for a no-options plain-collection create
		// over an existing plain collection outside a txn; NamespaceExists (48)
		// inside a txn, with explicit options, or when a view is involved on
		// either side (creating a collection over a view, or vice versa).
		ci := conninfo.Get(connCtx)
		existingIsView := false
		existingUUID := ""
		if info, verr := lookupCollectionInfo(connCtx, db, collectionName); verr == nil && info != nil {
			existingIsView = info.IsView
			existingUUID = info.UUID
		}
		if hasExplicitOptions || ci.InTransaction() || existingIsView {
			if ci.InTransaction() {
				h.AbortPendingTransaction(connCtx)
			}
			// Creating a view over an existing collection reports the existing
			// collection's UUID, matching MongoDB's message (only the UUID value
			// differs). Other collisions keep the plain message.
			msg := fmt.Sprintf("Collection %s.%s already exists.", dbName, collectionName)
			if params.ViewOn != "" && !existingIsView && existingUUID != "" {
				msg = fmt.Sprintf("namespace %s.%s already exists, but with different options: { uuid: UUID(\"%s\") }", dbName, collectionName, existingUUID)
			}
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNamespaceExists, msg, "create")
		}

		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"ok", float64(1),
			)),
		)

	default:
		return nil, common.TranslateBackendWriteError(err)
	}
}

// invalidDatabaseNameMsg returns the MongoDB-compatible error message for an invalid database name.
func invalidDatabaseNameMsg(dbName, collectionName string) string {
	if len(dbName) > 63 {
		return fmt.Sprintf("db name must be at most 63 characters, found: %d", len(dbName))
	}

	if strings.ContainsRune(dbName, '.') {
		return fmt.Sprintf("'.' is an invalid character in a db name: %s", dbName)
	}

	if strings.ContainsRune(dbName, '$') {
		return fmt.Sprintf("Invalid namespace: %s.%s", dbName, collectionName)
	}

	return fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, collectionName)
}
