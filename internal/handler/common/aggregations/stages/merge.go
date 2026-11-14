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

package stages

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// MergeBatchSize is the most documents $merge passes to its MergeFunc at
// once; mergeBatchBytes bounds a batch's total BSON size.
const (
	MergeBatchSize  = 1000
	mergeBatchBytes = 16 * 1024 * 1024
)

type MergeParams struct {
	DBName         string
	CollName       string
	Docs           []*types.Document
	On             []string // match fields; defaults to ["_id"]
	WhenMatched    string   // "merge" | "replace" | "keepExisting" | "fail" | "pipeline"
	WhenNotMatched string   // "insert" | "discard" | "fail"

	// ApplyWhenMatched, for WhenMatched "pipeline", parses the pipeline for
	// Docs[i] and returns the matched document existing updated by it, or
	// nil when existing is nil.
	ApplyWhenMatched func(ctx context.Context, existing *types.Document, i int) (*types.Document, error)
}

// MergeFunc is a callback that merges pipeline results into the named collection.
type MergeFunc func(ctx context.Context, params *MergeParams) error

// merge represents $merge stage.
//
// String shorthand (writes to the current database):
//
//	{ $merge: "<collection>" }
//
// Full document form:
//
//	{ $merge: {
//	    into: "<collection>" | { db: "<db>", coll: "<coll>" },
//	    on: "<field>" | ["<field>", ...],       // optional, default "_id"
//	    whenMatched:    "merge" | "replace" | "keepExisting" | "fail",   // default "merge"
//	    whenNotMatched: "insert" | "discard" | "fail",                    // default "insert"
//	} }
//
// $merge must be the last stage in the pipeline.
type merge struct {
	targetDB       string
	targetColl     string
	on             []string
	whenMatched    string
	whenNotMatched string
	merger         MergeFunc

	// whenMatchedPipeline, when whenMatched is "pipeline", updates each
	// matched document; let holds the variables it reads besides new.
	whenMatchedPipeline *types.Array
	let                 *types.Document
}

// validWhenMatched and validWhenNotMatched list accepted option values.
var validWhenMatched = map[string]struct{}{
	"merge":        {},
	"replace":      {},
	"keepExisting": {},
	"fail":         {},
}

var validWhenNotMatched = map[string]struct{}{
	"insert":  {},
	"discard": {},
	"fail":    {},
}

// NewMergeStage validates and creates a new $merge stage.
//
// sourceDB is used as the default target database.
// merger is called during Process to persist the documents.
func NewMergeStage(stage *types.Document, sourceDB string, merger MergeFunc) (aggregations.Stage, error) {
	spec, err := stage.Get("$merge")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	s := &merge{
		targetDB:       sourceDB,
		on:             []string{"_id"},
		whenMatched:    "merge",
		whenNotMatched: "insert",
		merger:         merger,
	}

	switch v := spec.(type) {
	case string:
		if v == "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				"$merge 'into' is an empty string",
				"$merge (stage)",
			)
		}

		s.targetColl = v

	case *types.Document:
		if err = parseMergeSpec(v, s); err != nil {
			return nil, err
		}

	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge requires a string or document argument, got %T", spec),
			"$merge (stage)",
		)
	}

	if s.targetColl == "" {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$merge must specify a target collection via 'into'",
			"$merge (stage)",
		)
	}

	return s, nil
}

// parseMergeSpec parses the $merge specification document, checking its
// fields in order as mongod's IDL parser does, then the whenMatched pipeline
// and the let variables.
func parseMergeSpec(spec *types.Document, s *merge) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$merge (stage)")
	}

	var hasInto bool
	values := spec.Values()
	for i, key := range spec.Keys() {
		v := values[i]
		var err error
		switch key {
		case "into":
			hasInto = true
			err = parseMergeInto(v, s)
		case "on":
			err = parseMergeOn(v, s)
		case "let":
			switch let := v.(type) {
			case types.NullType:
			case *types.Document:
				s.let = let
			default:
				err = fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field '$merge.let' is the wrong type '%s', expected type 'object'", handlerparams.AliasFromType(v)))
			}
		case "whenMatched":
			err = parseMergeWhenMatched(v, s)
		case "whenNotMatched":
			switch mode := v.(type) {
			case types.NullType:
			case string:
				if _, valid := validWhenNotMatched[mode]; !valid {
					err = fail(handlererrors.ErrBadValue, fmt.Sprintf(
						"Enumeration value '%s' for field '$merge.whenNotMatched' is not a valid value.", mode))
				}
				s.whenNotMatched = mode
			default:
				err = fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field '$merge.whenNotMatched' is the wrong type '%s', expected type 'string'", handlerparams.AliasFromType(v)))
			}
		default:
			err = fail(handlererrors.ErrIDLUnknownField, fmt.Sprintf("BSON field '$merge.%s' is an unknown field.", key))
		}
		if err != nil {
			return err
		}
	}
	if !hasInto {
		return fail(handlererrors.ErrMissingField, "BSON field '$merge.into' is missing but a required field")
	}
	if (s.whenMatched == "keepExisting" || s.whenMatched == "fail") && s.whenNotMatched != "insert" {
		return fail(handlererrors.ErrMergeModesUnsupported, fmt.Sprintf(
			"Combination of $merge modes 'whenMatched: %s' and 'whenNotMatched: %s' is not supported", s.whenMatched, s.whenNotMatched))
	}

	seen := make(map[string]bool, len(s.on))
	for _, field := range s.on {
		if err := checkFieldPathArgument(field, "$merge"); err != nil {
			return err
		}
		if seen[field] {
			return fail(handlererrors.ErrMergeOnDuplicateField, fmt.Sprintf("Found a duplicate field '%s'", field))
		}
		seen[field] = true
	}

	if s.whenMatchedPipeline != nil {
		if err := LiteParsePipeline(s.whenMatchedPipeline); err != nil {
			return err
		}
	}

	if s.let == nil {
		return nil
	}
	if s.whenMatchedPipeline == nil {
		return fail(handlererrors.ErrMergeLetWithoutPipeline,
			fmt.Sprintf("Cannot use 'let' variables with 'whenMatched: %s' mode", s.whenMatched))
	}
	if newVar, err := s.let.Get("new"); err == nil && newVar != "$$ROOT" {
		return fail(handlererrors.ErrMergeLetNew,
			"'let' may not define a value for the reserved 'new' variable other than '$$ROOT'")
	}
	letValues := s.let.Values()
	for i, name := range s.let.Keys() {
		if name == "new" {
			continue
		}
		if err := checkMergeLetExpression(letValues[i]); err != nil {
			return err
		}
		if err := operators.CheckUserVariableName(name); err != nil {
			return err
		}
	}
	return nil
}

// checkMergeLetExpression returns the error mongod reports parsing a $merge
// let value: a bad field path, an undefined variable, or an operator that does
// not parse.
func checkMergeLetExpression(expr any) error {
	if err := operators.CheckFieldPaths(expr); err != nil {
		return err
	}
	switch v := expr.(type) {
	case *types.Document:
		if operators.IsOperator(v) {
			return operators.CheckExpressionShape(v)
		}
		for _, field := range v.Values() {
			if err := checkMergeLetExpression(field); err != nil {
				return err
			}
		}
	case *types.Array:
		for i := range v.Len() {
			if err := checkMergeLetExpression(must.NotFail(v.Get(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

// parseMergeInto parses the $merge "into" field.
func parseMergeInto(into any, s *merge) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$merge (stage)")
	}

	switch v := into.(type) {
	case string:
		if v == "" {
			return fail(handlererrors.ErrMergeIntoEmpty, "$merge 'into' field cannot be an empty string")
		}
		s.targetColl = v

	case *types.Document:
		values := v.Values()
		for i, key := range v.Keys() {
			var dst *string
			switch key {
			case "db":
				dst = &s.targetDB
			case "coll":
				dst = &s.targetColl
			default:
				return fail(handlererrors.ErrIDLUnknownField, fmt.Sprintf("BSON field 'into.%s' is an unknown field.", key))
			}
			switch name := values[i].(type) {
			case types.NullType:
			case string:
				if name != "" {
					*dst = name
				}
			default:
				return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field 'into.%s' is the wrong type '%s', expected type 'string'", key, handlerparams.AliasFromType(values[i])))
			}
		}
		if s.targetColl == "" {
			return fail(handlererrors.ErrMergeIntoNoColl,
				"$merge 'into' field must specify a 'coll' that is not empty, null or undefined")
		}

	default:
		return fail(handlererrors.ErrMergeIntoType,
			"$merge 'into' field  must be either a string or an object, but found "+handlerparams.AliasFromType(into))
	}

	return checkTargetNamespace(s.targetDB, s.targetColl, "$merge")
}

// parseMergeOn parses the $merge "on" field.
func parseMergeOn(on any, s *merge) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$merge (stage)")
	}

	switch v := on.(type) {
	case string:
		s.on = []string{v}

	case *types.Array:
		if v.Len() == 0 {
			return fail(handlererrors.ErrMergeOnEmpty, "If explicitly specifying $merge 'on', must include at least one field")
		}
		fields := make([]string, 0, v.Len())
		for i := range v.Len() {
			elem := must.NotFail(v.Get(i))
			field, ok := elem.(string)
			if !ok {
				return fail(handlererrors.ErrMergeOnElementType,
					"$merge 'on' array elements must be strings, but found "+handlerparams.AliasFromType(elem))
			}
			fields = append(fields, field)
		}
		s.on = fields

	default:
		return fail(handlererrors.ErrMergeOnType,
			"$merge 'on' field  must be either a string or an array of strings, but found "+handlerparams.AliasFromType(on))
	}

	return nil
}

// parseMergeWhenMatched parses the $merge "whenMatched" field: a mode, or a
// pipeline that updates the matched document.
func parseMergeWhenMatched(whenMatched any, s *merge) error {
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$merge (stage)")
	}

	switch v := whenMatched.(type) {
	case string:
		if _, valid := validWhenMatched[v]; !valid {
			return fail(handlererrors.ErrBadValue, fmt.Sprintf("Enumeration value '%s' for field 'whenMatched' is not a valid value.", v))
		}
		s.whenMatched = v
		s.whenMatchedPipeline = nil

	case *types.Array:
		for i := range v.Len() {
			if _, ok := must.NotFail(v.Get(i)).(*types.Document); !ok {
				return fail(handlererrors.ErrTypeMismatch, "Each element of the 'pipeline' array must be an object")
			}
		}
		s.whenMatched = "pipeline"
		s.whenMatchedPipeline = v

	default:
		return fail(handlererrors.ErrMergeWhenMatchedType,
			"$merge 'whenMatched' field  must be either a string or an array, but found "+handlerparams.AliasFromType(whenMatched))
	}

	return nil
}

// Process implements Stage interface.
//
// It reads iter in batches of at most MergeBatchSize documents and
// mergeBatchBytes, passing each to the MergeFunc callback as it fills. Like
// $out, $merge returns an empty iterator (no documents to the caller).
func (s *merge) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	defer iter.Close()

	var batch []*types.Document
	var batchVars []map[string]any
	var batchBytes int
	flush := func() error {
		if len(batch) == 0 {
			return nil
		}
		params := &MergeParams{
			DBName:         s.targetDB,
			CollName:       s.targetColl,
			Docs:           batch,
			On:             s.on,
			WhenMatched:    s.whenMatched,
			WhenNotMatched: s.whenNotMatched,
		}
		if s.whenMatchedPipeline != nil {
			vars := batchVars
			params.ApplyWhenMatched = func(ctx context.Context, existing *types.Document, i int) (*types.Document, error) {
				return s.applyWhenMatched(ctx, existing, vars[i])
			}
		}
		err := s.merger(ctx, params)
		batch, batchVars, batchBytes = nil, nil, 0
		return err
	}

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if err = s.checkWrittenDocument(doc); err != nil {
			return nil, err
		}

		if s.whenMatchedPipeline != nil {
			vars, err := s.evalLet(doc)
			if err != nil {
				return nil, err
			}
			batchVars = append(batchVars, vars)
		}

		batch = append(batch, doc)
		batchBytes += doc.BSONSize()
		if len(batch) >= MergeBatchSize || batchBytes >= mergeBatchBytes {
			if err = flush(); err != nil {
				return nil, lazyerrors.Error(err)
			}
		}
	}

	if err := flush(); err != nil {
		return nil, lazyerrors.Error(err)
	}

	// $merge returns no documents to the client
	empty := iterator.Values(iterator.ForSlice([]*types.Document{}))
	closer.Add(empty)

	return empty, nil
}

// evalLet returns the variables the whenMatched pipeline reads for the
// incoming document doc: new, and each let variable whose value is not
// missing.
func (s *merge) evalLet(doc *types.Document) (map[string]any, error) {
	vars := map[string]any{"new": doc}
	if s.let == nil {
		return vars, nil
	}
	values := s.let.Values()
	for i, name := range s.let.Keys() {
		if name == "new" {
			continue
		}
		v, missing, err := operators.EvalOperand(values[i], doc)
		if err != nil {
			return nil, err
		}
		if !missing {
			vars[name] = v
		}
	}
	return vars, nil
}

// applyWhenMatched parses the whenMatched pipeline with vars bound and
// returns existing updated by it, or nil for a nil existing. Like mongod,
// which parses it for each document it writes, a pipeline that does not parse
// is an error only once a document reaches $merge.
func (s *merge) applyWhenMatched(ctx context.Context, existing *types.Document, vars map[string]any) (*types.Document, error) {
	pipelineStages, err := newUpdatePipeline(substituteVarsInPipeline(s.whenMatchedPipeline, vars))
	if err != nil {
		var malformed *common.MalformedPipelineError
		if errors.As(err, &malformed) {
			return nil, malformed.Err
		}
		return nil, err
	}
	if existing == nil {
		return nil, nil
	}

	closer := iterator.NewMultiCloser()
	defer closer.Close()

	var iter types.DocumentsIterator = iterator.Values(iterator.ForSlice([]*types.Document{existing.DeepCopy()}))
	closer.Add(iter)
	for _, stage := range pipelineStages {
		if iter, err = stage.Process(ctx, iter, closer); err != nil {
			return nil, err
		}
	}
	_, updated, err := iter.Next()
	if err != nil {
		return nil, err
	}

	id, err := existing.Get("_id")
	if err != nil {
		return updated, nil
	}
	newID, err := updated.Get("_id")
	if err != nil {
		withID := must.NotFail(types.NewDocument("_id", id))
		values := updated.Values()
		for i, key := range updated.Keys() {
			withID.Set(key, values[i])
		}
		return withID, nil
	}
	if types.Compare(newID, id) != types.Equal {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrImmutableField,
			"$merge failed to update the matching document, did you attempt to modify the _id or the shard key? :: caused by :: "+
				"After applying the update, the (immutable) field '_id' was found to have been altered to _id: "+types.FormatAnyValue(newID),
			"$merge")
	}
	return updated, nil
}

var (
	_ aggregations.Stage = (*merge)(nil)
)

// MergeTarget returns the target namespace and 'on' fields of a $merge stage.
func MergeTarget(stage aggregations.Stage) (db, coll string, on []string, ok bool) {
	m, ok := stage.(*merge)
	if !ok {
		return "", "", nil, false
	}
	return m.targetDB, m.targetColl, m.on, true
}

// checkWrittenDocument returns mongod's error for a document $merge cannot
// write: an array 'on' field, or an _id that cannot be stored.
func (s *merge) checkWrittenDocument(doc *types.Document) error {
	if err := storedKeysError(doc); err != nil {
		return err
	}
	on := s.on
	if len(on) == 0 {
		on = []string{"_id"}
	}
	for _, field := range on {
		v, found, throughArray := mergeOnValue(doc, field)
		switch v.(type) {
		case *types.Array:
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMergeOnFieldArray,
				fmt.Sprintf("$merge write error: 'on' field '%s' is an array", field), "$merge")
		case types.NullType, types.UndefinedType:
			found = false
		}
		// A missing _id is generated on insert.
		if throughArray || (!found && (field != "_id" || doc.Has("_id"))) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMergeOnFieldMissing,
				fmt.Sprintf("$merge write error: 'on' field '%s' cannot be missing, null, undefined or an array", field), "$merge")
		}
	}
	id, err := doc.Get("_id")
	if err != nil {
		return nil
	}
	if d, ok := id.(*types.Document); ok && d.Len() > 0 && strings.HasPrefix(d.Keys()[0], "$") {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
			"unknown operator: "+d.Keys()[0], "$merge")
	}
	return storedIDError(id)
}

// mergeOnValue returns the value at a dotted 'on' field, whether it exists,
// and whether the path runs through an array before its last component.
func mergeOnValue(doc *types.Document, field string) (v any, found, throughArray bool) {
	parts := strings.Split(field, ".")
	var cur any = doc
	for i, part := range parts {
		d, ok := cur.(*types.Document)
		if !ok {
			_, isArray := cur.(*types.Array)
			return nil, false, isArray && i > 0
		}
		next, err := d.Get(part)
		if err != nil {
			return nil, false, false
		}
		cur = next
	}
	return cur, true, false
}

// storedIDError returns mongod's error for an _id that cannot be stored.
func storedIDError(id any) error {
	var ve *types.ValidationError
	if err := types.ValidateID(id); !errors.As(err, &ve) {
		return err
	}
	code := handlererrors.ErrInvalidID
	if ve.Code() == types.ErrDollarPrefixedID {
		code = handlererrors.ErrDollarPrefixedFieldName
	}
	return handlererrors.NewCommandErrorMsgWithArgument(code, ve.Error(), "_id")
}
