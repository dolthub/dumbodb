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
	"math"
	stdsort "sort"
	"strconv"

	"github.com/cockroachdb/apd/v3"
	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators/accumulators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
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
//	    granularity: <string>  // optional
//	  }
//	}
//
// $bucketAuto automatically determines bucket boundaries to distribute documents evenly
// across a specified number of buckets.
type bucketAuto struct {
	groupBy any
	buckets int32
	output  []bucketOutput
	rounder granularityRounder
}

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

	numBuckets, err := parseBucketAutoBuckets(bucketsVal)
	if err != nil {
		return nil, err
	}

	if err := operators.CheckFieldPaths(groupByVal); err != nil {
		return nil, err
	}
	if err := operators.CheckConstantSubexpressions(groupByVal); err != nil {
		return nil, err
	}

	ba := &bucketAuto{
		groupBy: groupByVal,
		buckets: numBuckets,
	}

	if g, getErr := spec.Get("granularity"); getErr == nil {
		name, ok := g.(string)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoGranularityType,
				"The $bucketAuto 'granularity' field must be a string, but found type: "+handlerparams.AliasFromType(g),
				"$bucketAuto (stage)")
		}
		if ba.rounder, ok = newGranularityRounder(name); !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoUnknownGranularity,
				fmt.Sprintf("Unknown rounding granularity '%s'", name), "$bucketAuto (stage)")
		}
	}

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

		if err := operators.CheckFieldPaths(outputDoc); err != nil {
			return nil, err
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
			if accErr = accumulators.UseBucketAuto(acc); accErr != nil {
				return nil, accErr
			}

			ba.output = append(ba.output, bucketOutput{field: field, accumulator: acc})
		}
	}

	return ba, nil
}

func (ba *bucketAuto) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$bucketAuto")
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
		val, err := evaluateGroupExpr(ba.groupBy, doc)
		if err != nil {
			return nil, err
		}

		keyed = append(keyed, docWithKey{doc: doc, key: val})
	}

	if ba.rounder != nil {
		for _, kd := range keyed {
			if err = checkGranularityValue(kd.key); err != nil {
				return nil, err
			}
		}
	}

	stdsort.SliceStable(keyed, func(i, j int) bool {
		return types.CompareOrder(keyed[i].key, keyed[j].key, types.Ascending) == types.Less
	})

	// mongod fills each bucket with about n/buckets documents, keeps equal
	// values together, and with a granularity moves the boundaries to it.
	total := len(keyed)
	approx := int(math.Round(float64(total) / float64(ba.buckets)))
	if approx < 1 {
		approx = 1
	}

	var res []*types.Document
	var prevMax any
	for i, bucketNum := 0, int32(1); i < total; bucketNum++ {
		start := i
		minKey := keyed[i].key
		if ba.rounder != nil {
			if minKey, err = ba.rounder.roundDown(minKey); err != nil {
				return nil, err
			}
			if prevMax != nil {
				minKey = prevMax
			}
		}
		i++
		if bucketNum == ba.buckets {
			i = total
		} else {
			for c := 1; c < approx && i < total; c++ {
				i++
			}
		}
		maxKey := keyed[i-1].key
		for i < total && types.CompareForAggregation(keyed[i].key, maxKey) == types.Equal {
			i++
		}

		if ba.rounder != nil {
			boundary, err := ba.rounder.roundUp(maxKey)
			if err != nil {
				return nil, err
			}
			for i < total && types.CompareForAggregation(boundary, keyed[i].key) == types.Greater {
				i++
			}
			maxKey = boundary
			if f, _ := numberAsFloat(boundary); f == 0 && i < total {
				if maxKey, err = ba.rounder.roundDown(keyed[i].key); err != nil {
					return nil, err
				}
			}
		} else if i < total {
			maxKey = keyed[i].key
		}
		prevMax = maxKey

		id := must.NotFail(types.NewDocument("min", minKey, "max", maxKey))
		doc := must.NotFail(types.NewDocument("_id", id))

		rawDocs := make([]*types.Document, 0, i-start)
		for _, kd := range keyed[start:i] {
			rawDocs = append(rawDocs, kd.doc)
		}

		if err := appendBucketOutput(doc, ba.output, rawDocs); err != nil {
			return nil, err
		}
		res = append(res, doc)
	}

	result := iterator.Values(iterator.ForSlice(res))
	closer.Add(result)

	return result, nil
}

// parseBucketAutoBuckets returns the number of buckets: a number with a
// positive 32-bit integer value, as mongod requires.
func parseBucketAutoBuckets(v any) (int32, error) {
	f, ok := numberAsFloat(v)
	if !ok {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoBucketsNotNumber,
			"The $bucketAuto 'buckets' field must be a numeric value, but found type: "+handlerparams.AliasFromType(v),
			"$bucketAuto (stage)")
	}
	if f != math.Trunc(f) || f < math.MinInt32 || f > math.MaxInt32 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoBucketsNotInt32,
			"The $bucketAuto 'buckets' field must be representable as a 32-bit integer, but found "+
				strconv.FormatFloat(f, 'g', 6, 64), "$bucketAuto (stage)")
	}
	if f <= 0 {
		return 0, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoBucketsNotPositive,
			fmt.Sprintf("The $bucketAuto 'buckets' field must be greater than 0, but found: %d", int64(f)),
			"$bucketAuto (stage)")
	}
	return int32(f), nil
}

// checkGranularityValue returns mongod's error for a groupBy value that a
// granularity cannot round.
func checkGranularityValue(v any) error {
	f, ok := numberAsFloat(v)
	switch {
	case !ok:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoGranularityNotNumeric,
			"$bucketAuto can specify a 'granularity' with numeric boundaries only, but found a value with type: "+
				handlerparams.AliasFromType(v), "$bucketAuto (stage)")
	case math.IsNaN(f):
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoGranularityNaN,
			"$bucketAuto can specify a 'granularity' with numeric boundaries only, but found a NaN", "$bucketAuto (stage)")
	case f < 0:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketAutoGranularityNegative,
			"$bucketAuto can specify a 'granularity' with non-negative numbers only, but found a negative number",
			"$bucketAuto (stage)")
	}
	return nil
}

// numberAsFloat returns a number as a double. A Decimal128 outside the
// double range becomes an infinity or zero, as in mongod.
func numberAsFloat(v any) (float64, bool) {
	if d, ok := v.(types.Decimal128); ok {
		return decimalAsFloat(aggregations.DecimalToAPD(d)), true
	}
	return toFloat64(v)
}

func decimalAsFloat(d *apd.Decimal) float64 {
	f, err := strconv.ParseFloat(d.String(), 64)
	if err != nil && !errors.Is(err, strconv.ErrRange) {
		return math.NaN()
	}
	return f
}

var (
	_ aggregations.Stage = (*bucketAuto)(nil)
)
