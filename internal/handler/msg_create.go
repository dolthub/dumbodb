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
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MsgCreate implements `create` command.
//
// The passed context is canceled when the client connection is closed.
func (h *Handler) MsgCreate(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	document, err := opMsgDocument(msg)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	ignoredFields := []string{
		"autoIndexId",
		"storageEngine",
		"indexOptionDefaults",
		"writeConcern",
		"comment",
		"expireAfterSeconds",
		"collation",
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

	var capped bool
	if v, _ := document.Get("capped"); v != nil {
		capped, err = handlerparams.GetBoolOptionalParam("capped", v)
		if err != nil {
			return nil, err
		}
	}

	if capped {
		hasExplicitOptions = true

		size, _ := document.Get("size")
		if _, ok := size.(types.NullType); size == nil || ok {
			msg := "the 'size' field is required when 'capped' is true"
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrInvalidOptions, msg, "create")
		}

		params.CappedSize, err = handlerparams.GetValidatedNumberParamWithMinValue(document.Command(), "size", size, 1)
		if err != nil {
			return nil, err
		}

		// Match MongoDB: enforce a 4096-byte minimum for capped collections and
		// round larger sizes up to the next 256-byte multiple.
		params.CappedSize = normalizeCappedSize(params.CappedSize)

		if max, _ := document.Get("max"); max != nil {
			params.CappedDocuments, err = handlerparams.GetValidatedNumberParamWithMinValue(document.Command(), "max", max, 0)
			if err != nil {
				return nil, err
			}
		}
	} else if sizeVal, _ := document.Get("size"); sizeVal != nil {
		// size was provided without capped=true  -- still counts as explicit options.
		hasExplicitOptions = true
	}

	// Parse time series options.
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

	// Parse view options (viewOn + pipeline).
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

		// Parse optional pipeline.
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

	// Parse schema validation options.
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

	// Validate collection name with MongoDB-compatible error messages before calling backend.
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

	// Check fully qualified namespace length (db.collection must be <= 255 bytes).
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
		// MongoDB 7.0+ returns success when creating an already-existing collection with the
		// same options (idempotent). With different options (e.g., size), it returns a
		// NamespaceExists error.
		if hasExplicitOptions {
			msg := fmt.Sprintf("Collection %s.%s already exists.", dbName, collectionName)
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNamespaceExists, msg, "create")
		}

		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"ok", float64(1),
			)),
		)

	default:
		return nil, lazyerrors.Error(err)
	}
}

// invalidDatabaseNameMsg returns the MongoDB-compatible error message for an invalid database name.
func invalidDatabaseNameMsg(dbName, collectionName string) string {
	// Too long.
	if len(dbName) > 63 {
		return fmt.Sprintf("db name must be at most 63 characters, found: %d", len(dbName))
	}

	// Contains a dot.
	if strings.ContainsRune(dbName, '.') {
		return fmt.Sprintf("'.' is an invalid character in a db name: %s", dbName)
	}

	// Contains a dollar sign.
	if strings.ContainsRune(dbName, '$') {
		return fmt.Sprintf("Invalid namespace: %s.%s", dbName, collectionName)
	}

	// Other invalid characters (slash, backslash, space, null, etc.).
	return fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, collectionName)
}

// normalizeCappedSize applies MongoDB's rounding rules to a capped collection
// size: a minimum of 4096 bytes, and sizes above that rounded up to the next
// 256-byte multiple.
func normalizeCappedSize(size int64) int64 {
	const (
		minSize   = int64(4096)
		alignment = int64(256)
	)

	if size < minSize {
		return minSize
	}

	if rem := size % alignment; rem != 0 {
		size += alignment - rem
	}

	return size
}

