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

// OutWriter is a callback that replaces all documents in the named collection
// with the provided documents.  It should drop the collection if it exists,
// recreate it, and insert the documents atomically.
//
// dbName is the target database; collName is the target collection.
type OutWriter func(ctx context.Context, dbName, collName string, docs []*types.Document) error

// out represents $out stage.
//
// String form (writes to the current database):
//
//	{ $out: "<collection>" }
//
// Document form (allows cross-database output):
//
//	{ $out: { db: "<db>", coll: "<collection>" } }
//
// $out must be the last stage in an aggregation pipeline.
// It replaces the target collection's contents with the pipeline results.
type out struct {
	targetDB   string
	targetColl string
	writer     OutWriter
}

// NewOutStage validates and creates a new $out stage.
//
// sourceDB is the database in which the aggregate command is being run;
// it is used as the default target database when the stage spec is a plain string.
// writer is called during Process to persist the documents.
func NewOutStage(stage *types.Document, sourceDB string, writer OutWriter) (aggregations.Stage, error) {
	spec, err := stage.Get("$out")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	s := &out{writer: writer, targetDB: sourceDB}

	switch v := spec.(type) {
	case string:
		if v == "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				"$out 'coll' is an empty string",
				"$out (stage)",
			)
		}

		s.targetColl = v

	case *types.Document:
		// optional "db" field
		if dbVal, dbErr := v.Get("db"); dbErr == nil {
			dbStr, ok := dbVal.(string)
			if !ok || dbStr == "" {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					"$out 'db' must be a non-empty string",
					"$out (stage)",
				)
			}

			s.targetDB = dbStr
		}

		// required "coll" field
		collVal, collErr := v.Get("coll")
		if collErr != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$out must specify a 'coll' field when given as a document",
				"$out (stage)",
			)
		}

		collStr, ok := collVal.(string)
		if !ok || collStr == "" {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrInvalidNamespace,
				"$out 'coll' is an empty string",
				"$out (stage)",
			)
		}

		s.targetColl = collStr

	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("$out requires a string or document argument, got %T", spec),
			"$out (stage)",
		)
	}

	return s, nil
}

// Process implements Stage interface.
//
// It consumes all documents from iter, writes them to the target collection via
// the OutWriter callback, and returns an empty iterator (the aggregate command
// returns no documents to the caller when $out is used).
func (s *out) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if err = s.writer(ctx, s.targetDB, s.targetColl, docs); err != nil {
		return nil, lazyerrors.Error(err)
	}

	// $out returns no documents to the client
	empty := iterator.Values(iterator.ForSlice([]*types.Document{}))
	closer.Add(empty)

	return empty, nil
}

var (
	_ aggregations.Stage = (*out)(nil)
)
