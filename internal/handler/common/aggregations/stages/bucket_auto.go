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

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/common/aggregations/operators/accumulators"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
	"github.com/dolthub/dongo/internal/util/must"
)

// bucketAuto represents $bucketAuto stage.
//
//	{
//	  $bucketAuto: {
//	    groupBy: <expression>,
//	    buckets: <number>,
//	    output: {           // optional
//	      <field1>: { <accumulator1>: <expression1> },
//	      ...
//	    },
//	    granularity: <string>  // optional, not yet implemented
//	  }
//	}
//
// $bucketAuto automatically determines bucket boundaries to distribute documents evenly
// across a specified number of buckets.
type bucketAuto struct {
	groupBy  any
	buckets  int32
	output   []bucketOutput
}

// newBucketAuto creates a new $bucketAuto stage.
func newBucketAuto(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$bucketAuto")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"Argument to $bucketAuto stage must be a document",
			"$bucketAuto (stage)",
		)
	}

	// groupBy is required.
	groupByVal, err := spec.Get("groupBy")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBucketAutoMissingRequiredFields,
			"$bucketAuto requires 'groupBy' and 'buckets' to be specified",
			"$bucketAuto (stage)",
		)
	}

	// buckets is required.
	bucketsVal, err := spec.Get("buckets")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBucketAutoMissingRequiredFields,
			"$bucketAuto requires 'groupBy' and 'buckets' to be specified",
			"$bucketAuto (stage)",
		)
	}

	var numBuckets int32

	switch bv := bucketsVal.(type) {
	case int32:
		numBuckets = bv
	case int64:
		numBuckets = int32(bv)
	case float64:
		numBuckets = int32(bv)
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("The $bucketAuto stage requires 'buckets' to be a number, got %s", types.FormatAnyValue(bucketsVal)),
			"$bucketAuto (stage)",
		)
	}

	if numBuckets <= 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"The $bucketAuto stage requires 'buckets' to be a positive number",
			"$bucketAuto (stage)",
		)
	}

	ba := &bucketAuto{
		groupBy: groupByVal,
		buckets: numBuckets,
	}

	// granularity is optional — not yet implemented, ignore it.

	// output is optional.
	if outputVal, getErr := spec.Get("output"); getErr == nil {
		outputDoc, ok := outputVal.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$bucketAuto 'output' field must be a document",
				"$bucketAuto (stage)",
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

			acc, accErr := accumulators.NewAccumulator("$bucketAuto", field, accVal)
			if accErr != nil {
				return nil, lazyerrors.Error(accErr)
			}

			ba.output = append(ba.output, bucketOutput{field: field, accumulator: acc})
		}
	}

	return ba, nil
}

// Process implements Stage interface.
func (ba *bucketAuto) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if len(docs) == 0 {
		result := iterator.Values(iterator.ForSlice([]*types.Document{}))
		closer.Add(result)

		return result, nil
	}

	// Evaluate the groupBy expression for each document, keeping only non-null comparable values.
	type docWithKey struct {
		doc *types.Document
		key any
	}

	keyed := make([]docWithKey, 0, len(docs))

	for _, doc := range docs {
		val, evalErr := evaluateGroupExpr(ba.groupBy, doc)
		if evalErr != nil {
			val = types.Null
		}

		keyed = append(keyed, docWithKey{doc: doc, key: val})
	}

	// Sort by key ascending for bucketing.
	stdsort.SliceStable(keyed, func(i, j int) bool {
		return types.CompareOrder(keyed[i].key, keyed[j].key, types.Ascending) == types.Less
	})

	numBuckets := int(ba.buckets)
	total := len(keyed)

	if numBuckets > total {
		numBuckets = total
	}

	// Distribute documents into numBuckets buckets as evenly as possible.
	// Each bucket i gets docs [start[i], start[i+1]).
	bucketSize := total / numBuckets
	remainder := total % numBuckets

	var res []*types.Document
	pos := 0

	for i := 0; i < numBuckets; i++ {
		size := bucketSize
		if i < remainder {
			size++
		}

		bucketDocs := keyed[pos : pos+size]
		pos += size

		minKey := bucketDocs[0].key
		var maxKey any

		if i == numBuckets-1 {
			// Last bucket: max is the last element's key.
			maxKey = bucketDocs[len(bucketDocs)-1].key
		} else {
			// Max is the first element of the next bucket (not included in this one).
			maxKey = keyed[pos].key
		}

		id := must.NotFail(types.NewDocument("min", minKey, "max", maxKey))
		doc := must.NotFail(types.NewDocument("_id", id))

		rawDocs := make([]*types.Document, len(bucketDocs))
		for j, kd := range bucketDocs {
			rawDocs[j] = kd.doc
		}

		appendBucketOutput(doc, ba.output, rawDocs)
		res = append(res, doc)
	}

	result := iterator.Values(iterator.ForSlice(res))
	closer.Add(result)

	return result, nil
}

// check interfaces
var (
	_ aggregations.Stage = (*bucketAuto)(nil)
)
