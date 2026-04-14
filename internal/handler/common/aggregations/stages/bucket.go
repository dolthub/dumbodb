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

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators/accumulators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// bucket represents $bucket stage.
//
//	{
//	  $bucket: {
//	    groupBy: <expression>,
//	    boundaries: [ <lowerbound1>, <lowerbound2>, ... ],
//	    default: <literal>,  // optional
//	    output: {            // optional
//	      <field1>: { <accumulator1>: <expression1> },
//	      ...
//	    }
//	  }
//	}
//
// $bucket categorizes incoming documents into groups (buckets) based on a specified expression
// and bucket boundaries, and outputs a document per each bucket.
type bucket struct {
	groupBy      any
	boundaries   []any
	defaultValue any // nil means no default
	hasDefault   bool
	output       []bucketOutput
}

// bucketOutput holds an output field name and its accumulator.
type bucketOutput struct {
	field       string
	accumulator accumulators.Accumulator
}

// newBucket creates a new $bucket stage.
func newBucket(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$bucket")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"Argument to $bucket stage must be a document",
			"$bucket (stage)",
		)
	}

	// groupBy is required.
	groupByVal, err := spec.Get("groupBy")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrFailedToParse,
			"The $bucket stage specification must have a 'groupBy' field",
			"$bucket (stage)",
		)
	}

	// boundaries is required.
	boundariesVal, err := spec.Get("boundaries")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBucketMissingBoundaries,
			"$bucket requires 'groupBy' and 'boundaries' to be specified.",
			"$bucket (stage)",
		)
	}

	boundariesArr, ok := boundariesVal.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"The $bucket stage specification requires 'boundaries' to be an array",
			"$bucket (stage)",
		)
	}

	if boundariesArr.Len() < 2 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBucketNotEnoughBoundaries,
			fmt.Sprintf("The $bucket 'boundaries' field must have at least 2 values, but found %d value(s).", boundariesArr.Len()),
			"$bucket (stage)",
		)
	}

	boundaries := make([]any, boundariesArr.Len())
	for i := 0; i < boundariesArr.Len(); i++ {
		bv := must.NotFail(boundariesArr.Get(i))
		boundaries[i] = bv
	}

	// Validate boundaries are sorted ascending and comparable.
	for i := 1; i < len(boundaries); i++ {
		cmp := types.CompareOrder(boundaries[i-1], boundaries[i], types.Ascending)
		if cmp != types.Less {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"The values of 'boundaries' must be sorted in ascending order",
				"$bucket (stage)",
			)
		}
	}

	b := &bucket{
		groupBy:    groupByVal,
		boundaries: boundaries,
	}

	// default is optional.
	if defaultVal, getErr := spec.Get("default"); getErr == nil {
		b.hasDefault = true
		b.defaultValue = defaultVal
	}

	// output is optional.
	if outputVal, getErr := spec.Get("output"); getErr == nil {
		outputDoc, ok := outputVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$bucket 'output' field must be a document",
				"$bucket (stage)",
			)
		}

		outputIter := outputDoc.Iterator()
		defer outputIter.Close()

		for {
			field, accVal, iterErr := outputIter.Next()
			if errors.Is(iterErr, iterator.ErrIteratorDone) {
				break
			}

			if iterErr != nil {
				return nil, lazyerrors.Error(iterErr)
			}

			acc, err := accumulators.NewAccumulator("$bucket", field, accVal)
			if err != nil {
				return nil, lazyerrors.Error(err)
			}

			b.output = append(b.output, bucketOutput{field: field, accumulator: acc})
		}
	}

	return b, nil
}

// Process implements Stage interface.
func (b *bucket) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// bucketGroups maps bucket index to documents.
	// Indices 0..len(boundaries)-2 are the regular buckets.
	// Index len(boundaries)-1 is the default bucket.
	numBuckets := len(b.boundaries) - 1
	groups := make([][]*types.Document, numBuckets+1)

	for _, doc := range docs {
		val, evalErr := evaluateGroupExpr(b.groupBy, doc)
		if evalErr != nil {
			val = types.Null
		}

		bucketIdx := b.findBucket(val)

		if bucketIdx < 0 {
			// out of range — goes to default
			if !b.hasDefault {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBadValue,
					fmt.Sprintf(
						"$bucket could not find a matching branch for value %s, and no default was set",
						types.FormatAnyValue(val),
					),
					"$bucket (stage)",
				)
			}

			groups[numBuckets] = append(groups[numBuckets], doc)
		} else {
			groups[bucketIdx] = append(groups[bucketIdx], doc)
		}
	}

	// Build output documents.
	var res []*types.Document

	for i := 0; i < numBuckets; i++ {
		if len(groups[i]) == 0 {
			continue
		}

		doc := must.NotFail(types.NewDocument("_id", b.boundaries[i]))
		appendBucketOutput(doc, b.output, groups[i])
		res = append(res, doc)
	}

	// Default bucket.
	if b.hasDefault && len(groups[numBuckets]) > 0 {
		doc := must.NotFail(types.NewDocument("_id", b.defaultValue))
		appendBucketOutput(doc, b.output, groups[numBuckets])
		res = append(res, doc)
	}

	result := iterator.Values(iterator.ForSlice(res))
	closer.Add(result)

	return result, nil
}

// findBucket returns the bucket index for a value, or -1 if out of range.
func (b *bucket) findBucket(val any) int {
	// Binary search for the bucket where boundaries[i] <= val < boundaries[i+1].
	lo, hi := 0, len(b.boundaries)-2

	// First check if val is below the first boundary or at/above the last.
	if types.CompareOrder(val, b.boundaries[0], types.Ascending) == types.Less {
		return -1
	}

	if types.CompareOrder(val, b.boundaries[len(b.boundaries)-1], types.Ascending) != types.Less {
		return -1
	}

	for lo <= hi {
		mid := (lo + hi) / 2
		cmpLower := types.CompareOrder(val, b.boundaries[mid], types.Ascending)
		cmpUpper := types.CompareOrder(val, b.boundaries[mid+1], types.Ascending)

		if cmpLower != types.Less && cmpUpper == types.Less {
			return mid
		}

		if cmpLower == types.Less {
			hi = mid - 1
		} else {
			lo = mid + 1
		}
	}

	return -1
}

// appendBucketOutput applies optional output accumulators to a bucket document.
// If no output is defined, adds a default "count" field with the document count.
func appendBucketOutput(doc *types.Document, output []bucketOutput, groupDocs []*types.Document) {
	if len(output) == 0 {
		doc.Set("count", int32(len(groupDocs)))
		return
	}

	for _, out := range output {
		iter := iterator.Values(iterator.ForSlice(groupDocs))
		val, err := out.accumulator.Accumulate(iter)

		if err != nil {
			val = types.Null
		}

		doc.Set(out.field, val)
	}
}

// check interfaces
var (
	_ aggregations.Stage = (*bucket)(nil)
)
