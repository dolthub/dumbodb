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
	"slices"
	"strings"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
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
		"mergeMode",
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

	// mongod reports an existing collection before an invalid clusteredIndex,
	// so that error waits for the create to find no collection.
	clusteredErr := validateClusteredIndex(document)
	if clusteredErr != nil && !h.collectionExists(connCtx, dbName, collectionName) {
		return nil, clusteredErr
	}
	// The clusteredIndex a collection was created with, or none, which a
	// create over it must give again.
	params.ClusteredIndexName = requestedClusteredIndexName(document)

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
			if err := aggregations.CheckPipelineLength(pipeline.Len()); err != nil {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrPipelineTooLong,
					fmt.Sprintf("Invalid pipeline for view %s.%s :: caused by :: Pipeline length must be no longer than 1000 stages.", dbName, collectionName),
					"create")
			}
			params.ViewPipeline = pipeline
		}
	}

	// A validator differs from the existing collection's only if it is not
	// the same one, which the existing-collection check compares.
	if validatorVal, _ := document.Get("validator"); validatorVal != nil {
		validatorDoc, ok := validatorVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"'validator' option must be a document",
				"create",
			)
		}
		if err := common.ValidateValidatorExpressions(validatorDoc); err != nil {
			return nil, err
		}
		params.Validator = validatorDoc
	}

	if collationVal, _ := document.Get("collation"); collationVal != nil {
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

	if mergeModeVal, _ := document.Get("mergeMode"); mergeModeVal != nil {
		mode, err := parseMergeMode(mergeModeVal, "create")
		if err != nil {
			return nil, err
		}
		params.MergeMode = mode
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
		if verr := validateViewDependenciesAcyclic(connCtx, db, collectionName, params.ViewOn, params.ViewPipeline, "create"); verr != nil {
			return nil, verr
		}
	}

	if clusteredErr != nil {
		if info, ierr := lookupCollectionInfo(connCtx, db, collectionName); ierr != nil || info == nil {
			return nil, clusteredErr
		}
		err = backends.NewError(backends.ErrorCodeCollectionAlreadyExists, nil)
	} else {
		err = db.CreateCollection(connCtx, &params)
	}

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
		// MongoDB 8.0: a create over an existing namespace succeeds outside a
		// transaction when it asks for the options the namespace was created
		// with; otherwise it is NamespaceExists (48), naming what differs.
		ci := conninfo.Get(connCtx)
		existing, verr := lookupCollectionInfo(connCtx, db, collectionName)
		if verr != nil || existing == nil {
			existing = &backends.CollectionInfo{}
		}
		if msg, differs := createConflict(dbName, collectionName, &params, existing, hasExplicitOptions, clusteredErr != nil); differs || ci.InTransaction() {
			if ci.InTransaction() {
				h.AbortPendingTransaction(connCtx)
				msg = fmt.Sprintf("Collection %s.%s already exists.", dbName, collectionName)
			}
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrNamespaceExists, msg, "create")
		}

		return documentOpMsg(
			must.NotFail(types.NewDocument(
				"ok", float64(1),
			)),
		)

	default:
		return nil, common.TranslateBackendWriteError(connCtx, err)
	}
}

// collectionExists reports whether dbName holds collectionName.
func (h *Handler) collectionExists(ctx context.Context, dbName, collectionName string) bool {
	db, err := h.b.Database(dbName)
	if err != nil {
		return false
	}
	info, err := lookupCollectionInfo(ctx, db, collectionName)
	return err == nil && info != nil
}

// createConflict returns mongod's NamespaceExists message for a create of
// params over existing, and whether the two differ. A view must name the same
// source and pipeline; a collection the same options, compared as stored:
// an option given only at one create differs even when it is the default.
func createConflict(dbName, collName string, params *backends.CreateCollectionParams, existing *backends.CollectionInfo,
	otherOptions, clusteredInvalid bool,
) (string, bool) {
	ns := dbName + "." + collName
	switch {
	case existing.IsView:
		requested := dbName
		if params.ViewOn != "" {
			requested = dbName + "." + params.ViewOn
		}
		if params.ViewOn != existing.ViewOn {
			return fmt.Sprintf("namespace %s already exists, but is a view on %s.%s rather than %s", ns, dbName, existing.ViewOn, requested), true
		}
		if !sameBSON(viewPipeline(params.ViewPipeline), viewPipeline(existing.ViewPipeline)) {
			return fmt.Sprintf("namespace %s already exists, but with pipeline %s rather than %s", ns,
				formatPipeline(existing.ViewPipeline), formatPipeline(params.ViewPipeline)), true
		}
		if !sameOptionalCollation(params.Collation, existing.Collation) {
			return fmt.Sprintf("namespace %s already exists, but with collation: %s rather than %s", ns,
				conflictCollation(existing.Collation, true), conflictCollation(params.Collation, false)), true
		}
		return "", false
	case params.ViewOn != "":
		return fmt.Sprintf("namespace %s already exists, but with different options: { uuid: UUID(\"%s\") }", ns, existing.UUID), true
	}

	differs := otherOptions || clusteredInvalid || params.ClusteredIndexName != existing.ClusteredIndexName ||
		!sameBSON(params.Validator, existing.Validator) ||
		params.ValidationLevel != existing.ValidationLevel || params.ValidationAction != existing.ValidationAction ||
		!sameOptionalCollation(params.Collation, existing.Collation)
	if !differs {
		return "", false
	}
	options := fmt.Sprintf("uuid: UUID(\"%s\")", existing.UUID)
	if existing.ClusteredIndexName != "" {
		options += fmt.Sprintf(", clusteredIndex: { v: 2, key: { _id: 1 }, name: \"%s\", unique: true }", existing.ClusteredIndexName)
	}
	if existing.Validator.Len() > 0 {
		options += ", validator: " + types.FormatAnyValue(existing.Validator)
	}
	if existing.ValidationLevel != "" {
		options += fmt.Sprintf(", validationLevel: %q", existing.ValidationLevel)
	}
	if existing.ValidationAction != "" {
		options += fmt.Sprintf(", validationAction: %q", existing.ValidationAction)
	}
	if !collation.Parse(existing.Collation).IsSimple() {
		options += ", collation: " + resolvedCollationString(existing.Collation)
	}
	return fmt.Sprintf("namespace %s already exists, but with different options: { %s }", ns, options), true
}

// sameOptionalCollation reports whether two create collations, either nil
// for none, are the same; the simple collation is none.
func sameOptionalCollation(a, b *types.Document) bool {
	aSimple, bSimple := collation.Parse(a).IsSimple(), collation.Parse(b).IsSimple()
	if aSimple || bSimple {
		return aSimple == bSimple
	}
	return sameCollation(a, b)
}

// conflictCollation prints a collation in a view conflict as mongod does:
// {} for none, the stored one in full, the requested one without its
// backwards and version fields.
func conflictCollation(c *types.Document, stored bool) string {
	if collation.Parse(c).IsSimple() {
		return "{}"
	}
	full := resolvedCollationString(c)
	if stored {
		return full
	}
	if i := strings.Index(full, ", backwards:"); i >= 0 {
		return full[:i] + " }"
	}
	return full
}

// viewPipeline is a view's pipeline, an empty one for none.
func viewPipeline(p *types.Array) *types.Array {
	if p == nil {
		return types.MakeArray(0)
	}
	return p
}

// formatPipeline prints a view pipeline as mongod does in a create conflict:
// as a document keyed by stage index.
func formatPipeline(p *types.Array) string {
	p = viewPipeline(p)
	if p.Len() == 0 {
		return "{}"
	}
	var b strings.Builder
	b.WriteString("{ ")
	for i := 0; i < p.Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		fmt.Fprintf(&b, "%d: %s", i, types.FormatAnyValue(must.NotFail(p.Get(i))))
	}
	b.WriteString(" }")
	return b.String()
}

// sameBSON reports whether a and b, either nil for none, are equal as mongod
// compares stored options: fields in order, numbers by value.
func sameBSON(a, b any) bool {
	ad, aDoc := a.(*types.Document)
	bd, bDoc := b.(*types.Document)
	if aDoc || bDoc {
		if ad.Len() == 0 || bd.Len() == 0 {
			return ad.Len() == bd.Len()
		}
		if !aDoc || !bDoc || !slices.Equal(ad.Keys(), bd.Keys()) {
			return false
		}
		for _, k := range ad.Keys() {
			if !sameBSON(must.NotFail(ad.Get(k)), must.NotFail(bd.Get(k))) {
				return false
			}
		}
		return true
	}
	aa, aArr := a.(*types.Array)
	ba, bArr := b.(*types.Array)
	if aArr || bArr {
		if !aArr || !bArr || aa.Len() != ba.Len() {
			return false
		}
		for i := 0; i < aa.Len(); i++ {
			if !sameBSON(must.NotFail(aa.Get(i)), must.NotFail(ba.Get(i))) {
				return false
			}
		}
		return true
	}
	return types.Compare(a, b) == types.Equal
}

// invalidDatabaseNameMsg returns the MongoDB-compatible error message for an invalid database name.
func invalidDatabaseNameMsg(dbName, collectionName string) string {
	baseName, rootish := backends.SplitEncodedDBName(dbName)

	if len(baseName) > backends.MaxDatabaseNameBytes {
		return fmt.Sprintf("db name must be at most %d bytes, found: %d", backends.MaxDatabaseNameBytes, len(baseName))
	}

	if len(rootish) > backends.MaxRootishBytes {
		return fmt.Sprintf("rootish must be at most %d bytes, found: %d", backends.MaxRootishBytes, len(rootish))
	}

	if strings.ContainsRune(dbName, '.') {
		return fmt.Sprintf("'.' is an invalid character in a db name: %s", dbName)
	}

	if strings.ContainsRune(dbName, '$') {
		return fmt.Sprintf("Invalid namespace: %s.%s", dbName, collectionName)
	}

	return fmt.Sprintf("Invalid namespace specified '%s.%s'", dbName, collectionName)
}
