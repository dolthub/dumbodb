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
	stdsort "sort"
	"strings"

	"github.com/dolthub/dongo/internal/handler/common"
	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

// graphLookup represents the $graphLookup aggregation stage.
//
// It performs a recursive graph traversal, starting from the values produced by
// startWith and following the connectFromField → connectToField edges in the
// "from" collection until no new documents are discovered or maxDepth is reached.
//
// Syntax:
//
//	{ $graphLookup: {
//	    from:                    <collection>,
//	    startWith:               <expression>,
//	    connectFromField:        <string>,
//	    connectToField:          <string>,
//	    as:                      <string>,
//	    maxDepth:                <number>,           // optional; 0-based
//	    depthField:              <string>,           // optional
//	    restrictSearchWithMatch: <document>,         // optional
//	} }
type graphLookup struct {
	from                    string
	startWith               any             // field-path expression ("$field") or literal
	connectFromField        string
	connectToField          string
	as                      string
	maxDepth                int64           // -1 means unlimited
	depthField              string          // empty means not set
	restrictSearchWithMatch *types.Document // nil means no filter
	fetcher                 CollectionFetcher
}

// NewGraphLookupStage creates a new $graphLookup stage with a collection fetcher.
func NewGraphLookupStage(stage *types.Document, fetcher CollectionFetcher) (aggregations.Stage, error) {
	spec, err := stage.Get("$graphLookup")
	if err != nil {
		return nil, err
	}

	specDoc, ok := spec.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("$graphLookup specification must be an object, got %s", types.FormatAnyValue(spec)),
			"$graphLookup (stage)",
		)
	}

	from, err := requireStringField(specDoc, "from", "$graphLookup")
	if err != nil {
		return nil, err
	}

	startWithVal, err := specDoc.Get("startWith")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"$graphLookup requires a 'startWith' option",
			"$graphLookup (stage)",
		)
	}

	connectFromField, err := requireStringField(specDoc, "connectFromField", "$graphLookup")
	if err != nil {
		return nil, err
	}

	connectToField, err := requireStringField(specDoc, "connectToField", "$graphLookup")
	if err != nil {
		return nil, err
	}

	asField, err := requireStringField(specDoc, "as", "$graphLookup")
	if err != nil {
		return nil, err
	}

	gl := &graphLookup{
		from:             from,
		startWith:        startWithVal,
		connectFromField: connectFromField,
		connectToField:   connectToField,
		as:               asField,
		maxDepth:         -1,
		fetcher:          fetcher,
	}

	if specDoc.Has("maxDepth") {
		maxDepthVal, _ := specDoc.Get("maxDepth")

		switch v := maxDepthVal.(type) {
		case int32:
			gl.maxDepth = int64(v)
		case int64:
			gl.maxDepth = v
		case float64:
			gl.maxDepth = int64(v)
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$graphLookup 'maxDepth' must be a number, got %s", types.FormatAnyValue(maxDepthVal)),
				"$graphLookup (stage)",
			)
		}

		if gl.maxDepth < 0 {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				"$graphLookup 'maxDepth' must be non-negative",
				"$graphLookup (stage)",
			)
		}
	}

	if specDoc.Has("depthField") {
		gl.depthField, err = requireStringField(specDoc, "depthField", "$graphLookup")
		if err != nil {
			return nil, err
		}
	}

	if specDoc.Has("restrictSearchWithMatch") {
		rswmVal, _ := specDoc.Get("restrictSearchWithMatch")

		rswmDoc, ok := rswmVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrFailedToParse,
				fmt.Sprintf("$graphLookup 'restrictSearchWithMatch' must be an object, got %s", types.FormatAnyValue(rswmVal)),
				"$graphLookup (stage)",
			)
		}

		gl.restrictSearchWithMatch = rswmDoc
	}

	return gl, nil
}

// requireStringField extracts a required string field from a document, returning a
// structured command error on failure.
func requireStringField(doc *types.Document, field, stageName string) (string, error) {
	val, err := doc.Get(field)
	if err != nil {
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("%s requires a '%s' option", stageName, field),
			stageName+" (stage)",
		)
	}

	s, ok := val.(string)
	if !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			fmt.Sprintf("%s '%s' must be a string, got %s", stageName, field, types.FormatAnyValue(val)),
			stageName+" (stage)",
		)
	}

	return s, nil
}

// Process implements the Stage interface.
func (gl *graphLookup) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	fromDocs, err := gl.fetcher(ctx, gl.from)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	out := make([]*types.Document, 0, len(docs))

	for _, doc := range docs {
		visited, gErr := gl.traverse(doc, fromDocs)
		if gErr != nil {
			return nil, gErr
		}

		newDoc := doc.DeepCopy()
		arr := types.MakeArray(len(visited))

		for _, v := range visited {
			arr.Append(v)
		}

		newDoc.Set(gl.as, arr)
		out = append(out, newDoc)
	}

	result := iterator.Values(iterator.ForSlice(out))
	closer.Add(result)

	return result, nil
}

// traverse performs a breadth-first recursive graph traversal for one input document.
//
// It starts with the values produced by evaluating startWith against doc, then
// repeatedly looks up documents in fromDocs where connectToField matches a queued
// value, and queues those documents' connectFromField values for the next iteration.
//
// Cycle prevention: each connectToField search-value is only processed once, so
// cycles in the graph terminate naturally.
func (gl *graphLookup) traverse(doc *types.Document, fromDocs []*types.Document) ([]*types.Document, error) {
	// Sort fromDocs by _id ascending before traversal so that when multiple documents
	// match at the same BFS depth level, they are discovered in a deterministic order
	// that matches MongoDB's natural (insertion-order) collection scan.
	sorted := make([]*types.Document, len(fromDocs))
	copy(sorted, fromDocs)
	stdsort.SliceStable(sorted, func(i, j int) bool {
		idI, errI := sorted[i].Get("_id")
		idJ, errJ := sorted[j].Get("_id")
		if errI != nil || errJ != nil {
			return false
		}
		return types.CompareOrder(idI, idJ, types.Ascending) == types.Less
	})
	fromDocs = sorted

	// searchedKeys guards against re-processing the same connectToField search value.
	searchedKeys := make(map[string]bool)

	// resultIDs deduplicates result documents by their _id (or connectToField value).
	resultIDs := make(map[string]bool)

	var results []*types.Document

	frontier := evaluateStartWith(gl.startWith, doc)
	currentDepth := int64(0)

	for len(frontier) > 0 {
		if gl.maxDepth >= 0 && currentDepth > gl.maxDepth {
			break
		}

		var nextFrontier []any

		for _, searchVal := range frontier {
			searchKey := fmt.Sprintf("%v", searchVal)
			if searchedKeys[searchKey] {
				continue
			}

			searchedKeys[searchKey] = true

			for _, fromDoc := range fromDocs {
				toVal := getFieldValue(fromDoc, gl.connectToField)
				if !valuesEqual(toVal, searchVal) {
					continue
				}

				if gl.restrictSearchWithMatch != nil {
					matched, mErr := common.FilterDocument(fromDoc, gl.restrictSearchWithMatch)
					if mErr != nil || !matched {
						continue
					}
				}

				docID := docIdentityKey(fromDoc)
				if resultIDs[docID] {
					continue
				}

				resultIDs[docID] = true

				result := fromDoc.DeepCopy()
				if gl.depthField != "" {
					result.Set(gl.depthField, int32(currentDepth))
				}

				results = append(results, result)

				// Enqueue the connectFromField value(s) for the next depth level.
				fromVal := getFieldValue(fromDoc, gl.connectFromField)
				if fromVal != nil {
					if fromArr, ok := fromVal.(*types.Array); ok {
						nextFrontier = append(nextFrontier, arrayElements(fromArr)...)
					} else {
						nextFrontier = append(nextFrontier, fromVal)
					}
				}
			}
		}

		frontier = nextFrontier
		currentDepth++
	}

	return results, nil
}

// evaluateStartWith evaluates the startWith expression against an input document,
// returning the initial set of values for the graph frontier.
//
// "$fieldName" references are resolved from doc; arrays are flattened one level;
// all other values are used as-is.
func evaluateStartWith(startWith any, doc *types.Document) []any {
	switch sw := startWith.(type) {
	case string:
		if strings.HasPrefix(sw, "$") {
			fieldName := strings.TrimPrefix(sw, "$")
			val := getFieldValue(doc, fieldName)

			if val == nil {
				return nil
			}

			if arr, ok := val.(*types.Array); ok {
				return arrayElements(arr)
			}

			return []any{val}
		}

		return []any{sw}

	case *types.Array:
		result := make([]any, 0, sw.Len())
		arrIter := sw.Iterator()

		defer arrIter.Close()

		for {
			_, elem, err := arrIter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			if err != nil {
				break
			}

			result = append(result, evaluateStartWith(elem, doc)...)
		}

		return result

	default:
		if startWith == nil {
			return nil
		}

		return []any{startWith}
	}
}

// arrayElements returns the elements of a types.Array as a []any slice.
func arrayElements(arr *types.Array) []any {
	result := make([]any, 0, arr.Len())
	arrIter := arr.Iterator()

	defer arrIter.Close()

	for {
		_, v, err := arrIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			break
		}

		result = append(result, v)
	}

	return result
}

// docIdentityKey returns a string key that uniquely identifies a document.
// It prefers the _id field; falls back to a formatted representation.
func docIdentityKey(doc *types.Document) string {
	id, err := doc.Get("_id")
	if err == nil {
		return fmt.Sprintf("_id:%v", id)
	}

	return fmt.Sprintf("doc:%v", doc)
}

// check interfaces
var (
	_ aggregations.Stage = (*graphLookup)(nil)
)
