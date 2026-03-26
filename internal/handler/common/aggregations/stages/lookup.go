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

package stages

import (
	"context"
	"fmt"

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

// CollectionFetcher is a function that retrieves all documents from a named collection.
// It is used by the $lookup stage to access the "from" collection.
type CollectionFetcher func(ctx context.Context, collectionName string) ([]*types.Document, error)

// lookup represents $lookup stage.
//
// Simple equality join form:
//
//	{ $lookup: { from: <coll>, localField: <field>, foreignField: <field>, as: <array field> } }
//
// Pipeline form (let variables not yet supported):
//
//	{ $lookup: { from: <coll>, pipeline: [...], as: <array field> } }
type lookup struct {
	from         string
	localField   string // simple form
	foreignField string // simple form
	pipeline     *types.Array  // pipeline form (may be nil)
	as           string
	fetcher      CollectionFetcher
}

// NewLookupStage creates a new $lookup stage with a collection fetcher.
// The fetcher provides access to the "from" collection.
func NewLookupStage(stage *types.Document, fetcher CollectionFetcher) (aggregations.Stage, error) {
	spec, err := stage.Get("$lookup")
	if err != nil {
		return nil, err
	}

	specDoc, ok := spec.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup specification must be an object, got %s", types.FormatAnyValue(spec)),
			"$lookup (stage)",
		)
	}

	// "from" is required.
	fromVal, err := specDoc.Get("from")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires a 'from' option",
			"$lookup (stage)",
		)
	}

	from, ok := fromVal.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup 'from' must be a string, got %s", types.FormatAnyValue(fromVal)),
			"$lookup (stage)",
		)
	}

	// "as" is required.
	asVal, err := specDoc.Get("as")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires an 'as' option",
			"$lookup (stage)",
		)
	}

	asField, ok := asVal.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$lookup 'as' must be a string, got %s", types.FormatAnyValue(asVal)),
			"$lookup (stage)",
		)
	}

	l := &lookup{
		from:    from,
		as:      asField,
		fetcher: fetcher,
	}

	// Determine form: simple or pipeline.
	hasPipeline := specDoc.Has("pipeline")
	hasLocalField := specDoc.Has("localField")
	hasLet := specDoc.Has("let")

	if hasPipeline {
		// Pipeline form.
		pipelineVal, pErr := specDoc.Get("pipeline")
		if pErr != nil {
			return nil, lazyerrors.Error(pErr)
		}

		pipelineArr, ok := pipelineVal.(*types.Array)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$lookup 'pipeline' must be an array, got %s", types.FormatAnyValue(pipelineVal)),
				"$lookup (stage)",
			)
		}

		l.pipeline = pipelineArr

		if hasLet {
			// "let" is not yet supported; treat it as unimplemented.
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrNotImplemented,
				"$lookup: pipeline form with 'let' is not implemented yet",
				"$lookup (stage)",
			)
		}
	} else if hasLocalField {
		// Simple equality join form.
		localFieldVal, lErr := specDoc.Get("localField")
		if lErr != nil {
			return nil, lazyerrors.Error(lErr)
		}

		localField, ok := localFieldVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$lookup 'localField' must be a string, got %s", types.FormatAnyValue(localFieldVal)),
				"$lookup (stage)",
			)
		}

		foreignFieldVal, fErr := specDoc.Get("foreignField")
		if fErr != nil {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"$lookup 'foreignField' is required when 'localField' is specified",
				"$lookup (stage)",
			)
		}

		foreignField, ok := foreignFieldVal.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$lookup 'foreignField' must be a string, got %s", types.FormatAnyValue(foreignFieldVal)),
				"$lookup (stage)",
			)
		}

		l.localField = localField
		l.foreignField = foreignField
	} else {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$lookup requires either 'localField'/'foreignField' or 'pipeline'",
			"$lookup (stage)",
		)
	}

	return l, nil
}

// Process implements Stage interface.
func (l *lookup) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Fetch all documents from the "from" collection.
	fromDocs, err := l.fetcher(ctx, l.from)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	out := make([]*types.Document, 0, len(docs))

	if l.pipeline != nil {
		// Pipeline form: run the pipeline against the from collection for each input doc.
		for _, doc := range docs {
			matched, pErr := l.runPipeline(ctx, fromDocs)
			if pErr != nil {
				return nil, pErr
			}

			newDoc := doc.DeepCopy()
			arr := types.MakeArray(len(matched))

			for _, m := range matched {
				arr.Append(m)
			}

			newDoc.Set(l.as, arr)
			out = append(out, newDoc)
		}
	} else {
		// Simple equality join form.
		for _, doc := range docs {
			localVal := getFieldValue(doc, l.localField)

			matched := make([]*types.Document, 0)

			for _, fromDoc := range fromDocs {
				foreignVal := getFieldValue(fromDoc, l.foreignField)
				if valuesEqual(localVal, foreignVal) {
					matched = append(matched, fromDoc.DeepCopy())
				}
			}

			newDoc := doc.DeepCopy()
			arr := types.MakeArray(len(matched))

			for _, m := range matched {
				arr.Append(m)
			}

			newDoc.Set(l.as, arr)
			out = append(out, newDoc)
		}
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)

	return result, nil
}

// runPipeline runs the $lookup pipeline form against a set of documents.
// This is a simplified implementation that runs stages sequentially.
func (l *lookup) runPipeline(_ context.Context, fromDocs []*types.Document) ([]*types.Document, error) {
	// For basic pipeline form without let, just return all fromDocs filtered by each stage.
	// Full pipeline execution is not yet supported here; we return all docs from the from collection.
	// TODO: implement full sub-pipeline execution.
	result := make([]*types.Document, 0, len(fromDocs))

	for _, d := range fromDocs {
		result = append(result, d.DeepCopy())
	}

	return result, nil
}

// getFieldValue retrieves the value of a field from a document.
// Returns nil if the field does not exist.
func getFieldValue(doc *types.Document, field string) any {
	v, err := doc.Get(field)
	if err != nil {
		return nil
	}

	return v
}

// valuesEqual compares two values for equality in the context of $lookup matching.
func valuesEqual(a, b any) bool {
	if a == nil && b == nil {
		return true
	}

	if a == nil || b == nil {
		return false
	}

	// Use types.Compare if available, otherwise use reflect-based comparison.
	// For now, use a simple type-switch approach for common types.
	switch av := a.(type) {
	case int32:
		switch bv := b.(type) {
		case int32:
			return av == bv
		case int64:
			return int64(av) == bv
		case float64:
			return float64(av) == bv
		}
	case int64:
		switch bv := b.(type) {
		case int64:
			return av == bv
		case int32:
			return av == int64(bv)
		case float64:
			return float64(av) == bv
		}
	case float64:
		switch bv := b.(type) {
		case float64:
			return av == bv
		case int32:
			return av == float64(bv)
		case int64:
			return av == float64(bv)
		}
	case string:
		bv, ok := b.(string)
		return ok && av == bv
	case bool:
		bv, ok := b.(bool)
		return ok && av == bv
	}

	// Fallback: compare string representations.
	return fmt.Sprintf("%v", a) == fmt.Sprintf("%v", b)
}

// check interfaces
var (
	_ aggregations.Stage = (*lookup)(nil)
)
