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
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// MergeParams holds parameters for the MergeFunc callback.
type MergeParams struct {
	DBName         string
	CollName       string
	Docs           []*types.Document
	On             []string // match fields; defaults to ["_id"]
	WhenMatched    string   // "merge" | "replace" | "keepExisting" | "fail"
	WhenNotMatched string   // "insert" | "discard" | "fail"
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
		if err = parseMergeInto(v, s); err != nil {
			return nil, err
		}

		if err = parseMergeOn(v, s); err != nil {
			return nil, err
		}

		if err = parseMergeWhenMatched(v, s); err != nil {
			return nil, err
		}

		if err = parseMergeWhenNotMatched(v, s); err != nil {
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

// parseMergeInto extracts the "into" field from the $merge spec document.
func parseMergeInto(spec *types.Document, s *merge) error {
	intoVal, err := spec.Get("into")
	if err != nil {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$merge requires an 'into' field",
			"$merge (stage)",
		)
	}

	switch v := intoVal.(type) {
	case string:
		if v == "" {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				"$merge 'into' is an empty string",
				"$merge (stage)",
			)
		}

		s.targetColl = v

	case *types.Document:
		// optional "db" field
		if dbVal, dbErr := v.Get("db"); dbErr == nil {
			dbStr, ok := dbVal.(string)
			if !ok || dbStr == "" {
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$merge 'into.db' must be a non-empty string",
					"$merge (stage)",
				)
			}

			s.targetDB = dbStr
		}

		// required "coll" field
		collVal, collErr := v.Get("coll")
		if collErr != nil {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$merge 'into' document must have a 'coll' field",
				"$merge (stage)",
			)
		}

		collStr, ok := collVal.(string)
		if !ok || collStr == "" {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				"$merge 'into.coll' is an empty string",
				"$merge (stage)",
			)
		}

		s.targetColl = collStr

	default:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge 'into' must be a string or document, got %T", intoVal),
			"$merge (stage)",
		)
	}

	return nil
}

// parseMergeOn extracts the optional "on" field from the $merge spec document.
func parseMergeOn(spec *types.Document, s *merge) error {
	onVal, err := spec.Get("on")
	if err != nil {
		// "on" is optional; default (_id) already set
		return nil //nolint:nilerr // intentionally ignoring "field not found" error
	}

	switch v := onVal.(type) {
	case string:
		if v == "" {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$merge 'on' field name cannot be empty",
				"$merge (stage)",
			)
		}

		s.on = []string{v}

	case *types.Array:
		fields := make([]string, 0, v.Len())

		for i := range v.Len() {
			elem, elemErr := v.Get(i)
			if elemErr != nil {
				return lazyerrors.Error(elemErr)
			}

			fieldStr, ok := elem.(string)
			if !ok || fieldStr == "" {
				return handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$merge 'on' array elements must be non-empty strings",
					"$merge (stage)",
				)
			}

			fields = append(fields, fieldStr)
		}

		if len(fields) == 0 {
			return handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$merge 'on' array must not be empty",
				"$merge (stage)",
			)
		}

		s.on = fields

	default:
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge 'on' must be a string or array of strings, got %T", onVal),
			"$merge (stage)",
		)
	}

	return nil
}

// parseMergeWhenMatched extracts the optional "whenMatched" field.
func parseMergeWhenMatched(spec *types.Document, s *merge) error {
	val, err := spec.Get("whenMatched")
	if err != nil {
		// optional; default already set
		return nil //nolint:nilerr // intentionally ignoring "field not found" error
	}

	str, ok := val.(string)
	if !ok {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge 'whenMatched' must be a string, got %T", val),
			"$merge (stage)",
		)
	}

	if _, valid := validWhenMatched[str]; !valid {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge 'whenMatched' must be one of: merge, replace, keepExisting, fail; got %q", str),
			"$merge (stage)",
		)
	}

	s.whenMatched = str

	return nil
}

// parseMergeWhenNotMatched extracts the optional "whenNotMatched" field.
func parseMergeWhenNotMatched(spec *types.Document, s *merge) error {
	val, err := spec.Get("whenNotMatched")
	if err != nil {
		// optional; default already set
		return nil //nolint:nilerr // intentionally ignoring "field not found" error
	}

	str, ok := val.(string)
	if !ok {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge 'whenNotMatched' must be a string, got %T", val),
			"$merge (stage)",
		)
	}

	if _, valid := validWhenNotMatched[str]; !valid {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$merge 'whenNotMatched' must be one of: insert, discard, fail; got %q", str),
			"$merge (stage)",
		)
	}

	s.whenNotMatched = str

	return nil
}

// Process implements Stage interface.
//
// It consumes all documents from iter and delegates to the MergeFunc callback.
// Like $out, $merge returns an empty iterator (no documents to the caller).
func (s *merge) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = s.merger(ctx, &MergeParams{
		DBName:         s.targetDB,
		CollName:       s.targetColl,
		Docs:           docs,
		On:             s.on,
		WhenMatched:    s.whenMatched,
		WhenNotMatched: s.whenNotMatched,
	}); err != nil {
		return nil, lazyerrors.Error(err)
	}

	// $merge returns no documents to the client
	empty := iterator.Values(iterator.ForSlice([]*types.Document{}))
	closer.Add(empty)

	return empty, nil
}

// check interfaces
var (
	_ aggregations.Stage = (*merge)(nil)
)
