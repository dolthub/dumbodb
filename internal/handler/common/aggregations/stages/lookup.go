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
// Pipeline form (with optional let variables):
//
//	{ $lookup: { from: <coll>, let: { <var>: <expr>, ... }, pipeline: [...], as: <array field> } }
type lookup struct {
	from         string
	localField   string         // simple form
	foreignField string         // simple form
	letVars      *types.Document // pipeline form: variable bindings
	pipeline     *types.Array   // pipeline form (may be nil)
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
			letVal, lErr := specDoc.Get("let")
			if lErr != nil {
				return nil, lazyerrors.Error(lErr)
			}

			letDoc, ok := letVal.(*types.Document)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrFailedToParse,
					fmt.Sprintf("$lookup 'let' must be an object, got %s", types.FormatAnyValue(letVal)),
					"$lookup (stage)",
				)
			}

			l.letVars = letDoc
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
			matched, pErr := l.runPipeline(ctx, fromDocs, doc)
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
		// Supports array localField: if localField value is an array, any element match counts.
		for _, doc := range docs {
			localVal := getFieldValue(doc, l.localField)

			matched := make([]*types.Document, 0)

			for _, fromDoc := range fromDocs {
				foreignVal := getFieldValue(fromDoc, l.foreignField)

				if localValArr, isArr := localVal.(*types.Array); isArr {
					// Array localField: match if foreignVal equals any element.
					if arrayContainsValue(localValArr, foreignVal) {
						matched = append(matched, fromDoc.DeepCopy())
					}
				} else {
					if valuesEqual(localVal, foreignVal) {
						matched = append(matched, fromDoc.DeepCopy())
					}
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
// It evaluates let variables from localDoc, substitutes them into the pipeline spec,
// and executes each stage in sequence. Nested $lookup stages are supported.
func (l *lookup) runPipeline(ctx context.Context, fromDocs []*types.Document, localDoc *types.Document) ([]*types.Document, error) {
	// Compute variable bindings from let.
	vars := map[string]any{}

	if l.letVars != nil {
		letIter := l.letVars.Iterator()
		defer letIter.Close()

		for {
			k, v, err := letIter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			vars[k] = evaluateLetExpr(v, localDoc)
		}
	}

	// Build the initial set of documents.
	current := make([]*types.Document, len(fromDocs))
	for i, d := range fromDocs {
		current[i] = d.DeepCopy()
	}

	// Execute each pipeline stage.
	pipeIter := l.pipeline.Iterator()
	defer pipeIter.Close()

	for {
		_, stageVal, err := pipeIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		stageDoc, ok := stageVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrTypeMismatch,
				"Each element of the '$lookup' pipeline must be an object",
				"$lookup (stage)",
			)
		}

		// Substitute let variables into the stage spec.
		substituted := substituteVarsInDoc(stageDoc, vars)

		// Create the stage. $lookup is handled specially to pass the fetcher.
		var stage aggregations.Stage

		if substituted.Command() == "$lookup" {
			stage, err = NewLookupStage(substituted, l.fetcher)
		} else {
			stage, err = NewStage(substituted)
		}

		if err != nil {
			return nil, err
		}

		// Run the stage against the current set of documents.
		stageInput := iterator.Values(iterator.ForSlice(current))
		stageCloser := iterator.NewMultiCloser()

		stageOutput, sErr := stage.Process(ctx, stageInput, stageCloser)
		if sErr != nil {
			stageCloser.Close()
			return nil, sErr
		}

		current, err = iterator.ConsumeValues(stageOutput)
		stageCloser.Close()

		if err != nil {
			return nil, lazyerrors.Error(err)
		}
	}

	return current, nil
}

// evaluateLetExpr evaluates a let variable expression against a local document.
// Expressions of the form "$fieldName" are resolved to the field value from localDoc.
// Non-expression values are returned as-is.
func evaluateLetExpr(expr any, localDoc *types.Document) any {
	s, ok := expr.(string)
	if !ok {
		return expr
	}

	if !strings.HasPrefix(s, "$") {
		return s
	}

	// $$ prefix would be a variable reference — not valid in let values.
	if strings.HasPrefix(s, "$$") {
		return types.Null
	}

	fieldName := strings.TrimPrefix(s, "$")
	if fieldName == "" {
		return types.Null
	}

	val := getFieldValue(localDoc, fieldName)
	if val == nil {
		return types.Null
	}

	return val
}

// substituteVarsInDoc recursively replaces $$varName references in a document
// with the corresponding values from vars.
func substituteVarsInDoc(doc *types.Document, vars map[string]any) *types.Document {
	if len(vars) == 0 {
		return doc
	}

	result := new(types.Document)
	docIter := doc.Iterator()

	defer docIter.Close()

	for {
		k, v, err := docIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			break
		}

		result.Set(k, substituteVarsInValue(v, vars))
	}

	return result
}

// substituteVarsInValue recursively substitutes $$varName references in a value.
func substituteVarsInValue(val any, vars map[string]any) any {
	switch v := val.(type) {
	case string:
		if strings.HasPrefix(v, "$$") {
			varName := strings.TrimPrefix(v, "$$")
			if resolved, ok := vars[varName]; ok {
				return resolved
			}
		}

		return v

	case *types.Document:
		return substituteVarsInDoc(v, vars)

	case *types.Array:
		result := types.MakeArray(v.Len())
		arrIter := v.Iterator()

		defer arrIter.Close()

		for {
			_, elem, err := arrIter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			if err != nil {
				break
			}

			result.Append(substituteVarsInValue(elem, vars))
		}

		return result

	default:
		return val
	}
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

// arrayContainsValue reports whether the array contains a value equal to target.
func arrayContainsValue(arr *types.Array, target any) bool {
	arrIter := arr.Iterator()
	defer arrIter.Close()

	for {
		_, elem, err := arrIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			break
		}

		if valuesEqual(elem, target) {
			return true
		}
	}

	return false
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
