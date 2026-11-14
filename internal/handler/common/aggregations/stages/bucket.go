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
	"strings"

	"github.com/dolthub/dumbodb/internal/collation"
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
	cmp          *collation.Comparator
	// boundaryKeys are the boundaries keyed under cmp.
	boundaryKeys []any
}

// useSBE implements sbeAware.
func (b *bucket) useSBE() error {
	for _, o := range b.output {
		if err := accumulators.UseSBE(o.accumulator); err != nil {
			return err
		}
	}
	return nil
}

type bucketOutput struct {
	field       string
	accumulator accumulators.Accumulator
}

// bucketConstant folds a $bucket boundary or default, as mongod does.
// constant is false when v is not a constant expression; value is nil when it
// folds to the missing value.
func bucketConstant(v any) (value any, constant bool, err error) {
	if v == "$$REMOVE" {
		return nil, true, nil
	}
	if !operators.IsConstantExpression(v) {
		return nil, false, nil
	}
	folded, missing, err := operators.EvalOperand(v, types.MakeDocument(0))
	if err != nil || missing {
		return nil, err == nil, err
	}
	return folded, true, nil
}

// bucketMaxSwitchSize is the largest BSON object mongod builds; it
// desugars $bucket into a $group on a $switch holding the folded boundaries
// and default, which must fit in one.
const bucketMaxSwitchSize = 16*1024*1024 + 16*1024

// bucketSwitchSize is the BSON size of the folded constants of mongod's
// $switch for $bucket: each branch reads its lower and upper boundary and
// returns its lower one.
type bucketSwitchSize struct {
	total    int
	exceeded bool
}

// add charges a folded value that appears n times in the $switch.
func (s *bucketSwitchSize) add(v any, n int) {
	if v != nil {
		s.total += n * types.BSONValueSize(v)
	}
	if s.total > bucketMaxSwitchSize {
		s.exceeded = true
	}
}

func (s *bucketSwitchSize) err() error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBSONObjectTooLarge, fmt.Sprintf(
		"BSONObj size: %d (0x%X) is invalid. Size must be between 0 and %d(16MB)", s.total, s.total, bucketMaxSwitchSize),
		"$bucket (stage)")
}

// boundaryTypeName names a boundary's type; nil is the missing value.
func boundaryTypeName(v any) string {
	if v == nil {
		return "missing"
	}
	return handlerparams.AliasFromType(v)
}

// parseBucketBoundaries returns $bucket's boundaries, which must be at
// least two sorted values of one type.
//
// Once the folded values outgrow mongod's $switch, which it refuses after
// parsing the stage, the remaining ones are folded for their errors only
// and only the lowest and highest boundary are kept, so memory stays
// bounded.
func parseBucketBoundaries(v any, size *bucketSwitchSize) ([]any, error) {
	boundariesArr, ok := v.(*types.Array)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketBoundariesNotArray,
			"The $bucket 'boundaries' field must be an array, but found type: "+handlerparams.AliasFromType(v)+".",
			"$bucket (stage)")
	}

	n := boundariesArr.Len()
	boundaries := make([]any, 0, n)
	var first, prev any
	var orderErr error
	for i := range n {
		elem := must.NotFail(boundariesArr.Get(i))
		v, constant, err := bucketConstant(elem)
		if err != nil {
			return nil, err
		}
		if !constant {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketBoundaryNotConstant,
				"The $bucket 'boundaries' field must be an array of constant values, but found value: "+
					operators.FormatExpression(elem)+".", "$bucket (stage)")
		}
		// mongod checks the order once every boundary has folded.
		if i == 0 {
			first = v
		} else if orderErr == nil {
			orderErr = checkBucketBoundaryOrder(i, prev, v)
		}
		prev = v

		uses := 3
		switch {
		case n < 2:
			uses = 0
		case i == 0:
			uses = 2
		case i == n-1:
			uses = 1
		}
		size.add(v, uses)
		if !size.exceeded {
			boundaries = append(boundaries, v)
		}
	}

	if n < 2 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBucketNotEnoughBoundaries,
			fmt.Sprintf("The $bucket 'boundaries' field must have at least 2 values, but found %d value(s).", n),
			"$bucket (stage)",
		)
	}
	if orderErr != nil {
		return nil, orderErr
	}
	if size.exceeded {
		return []any{first, prev}, nil
	}
	return boundaries, nil
}

// checkBucketBoundaryOrder returns mongod's error for boundaries i-1 and i
// that are not of one type or not ascending.
func checkBucketBoundaryOrder(i int, prev, cur any) error {
	if prev == nil || cur == nil || !types.SameTypeClass(prev, cur) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketBoundaryTypes,
			fmt.Sprintf("All values in the the 'boundaries' option to $bucket must have the same type. "+
				"Found conflicting types %s and %s.", boundaryTypeName(prev), boundaryTypeName(cur)),
			"$bucket (stage)")
	}
	if types.CompareOrder(prev, cur, types.Ascending) != types.Less {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketBoundariesOrder,
			fmt.Sprintf("The 'boundaries' option to $bucket must be sorted, but elements %d and %d are not in "+
				"ascending order (%s is not less than %s).", i-1, i, types.FormatAnyValue(prev), types.FormatAnyValue(cur)),
			"$bucket (stage)")
	}
	return nil
}

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

	// mongod checks each field in the order the specification gives them,
	// and only then that groupBy and boundaries are present.
	var groupByVal, defaultVal any
	var boundaries []any
	var hasDefault bool
	var outputDoc *types.Document
	var size bucketSwitchSize
	for _, key := range spec.Keys() {
		val := must.NotFail(spec.Get(key))
		switch key {
		case "groupBy":
			if !isGroupExpression(val) {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketGroupBy,
					"The $bucket 'groupBy' field must be defined as a $-prefixed path or an expression, but found: "+
						types.FormatAnyValue(val)+".", "$bucket (stage)")
			}
			groupByVal = val
		case "boundaries":
			if boundaries, err = parseBucketBoundaries(val, &size); err != nil {
				return nil, err
			}
		case "default":
			var constant bool
			if defaultVal, constant, err = bucketConstant(val); err != nil {
				return nil, err
			}
			size.add(defaultVal, 1)
			if !constant {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketDefaultNotConstant,
					"The $bucket 'default' field must be a constant expression, but found: "+operators.FormatExpression(val)+".",
					"$bucket (stage)")
			}
			hasDefault = defaultVal != nil
		case "output":
			if outputDoc, ok = val.(*types.Document); !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketOutputNotObject,
					"The $bucket 'output' field must be an object, but found type: "+handlerparams.AliasFromType(val)+".",
					"$bucket (stage)")
			}
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketUnknownOption,
				"Unrecognized option to $bucket: "+key+".", "$bucket (stage)")
		}
	}
	if groupByVal == nil || boundaries == nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBucketMissingBoundaries,
			"$bucket requires 'groupBy' and 'boundaries' to be specified.",
			"$bucket (stage)",
		)
	}

	if hasDefault {
		if err := checkBucketDefaultOutside(defaultVal, boundaries); err != nil {
			return nil, err
		}
	}

	// mongod then builds its $group on a $switch, which must fit in a BSON
	// object, and parses that group's expressions.
	if size.exceeded {
		return nil, size.err()
	}

	if err := operators.CheckFieldPaths(groupByVal); err != nil {
		return nil, err
	}
	if err := operators.CheckConstantSubexpressions(groupByVal); err != nil {
		return nil, err
	}

	var output []bucketOutput
	if outputDoc != nil {
		if err := operators.CheckFieldPaths(outputDoc); err != nil {
			return nil, err
		}
		for _, field := range outputDoc.Keys() {
			acc, err := accumulators.NewAccumulator("$bucket", field, must.NotFail(outputDoc.Get(field)))
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			output = append(output, bucketOutput{field: field, accumulator: acc})
		}
	}

	b := &bucket{
		groupBy:      groupByVal,
		boundaries:   boundaries,
		hasDefault:   hasDefault,
		defaultValue: defaultVal,
		output:       output,
		cmp:          operators.CollationOf(stage),
	}
	b.boundaryKeys = make([]any, len(boundaries))
	for i, boundary := range boundaries {
		b.boundaryKeys[i] = b.cmp.KeyValue(boundary)
	}

	return b, nil
}

// checkBucketDefaultOutside returns mongod's error for a $bucket default
// that falls within the boundaries.
func checkBucketDefaultOutside(defaultVal any, boundaries []any) error {
	lower, upper := boundaries[0], boundaries[len(boundaries)-1]
	if types.SameTypeClass(defaultVal, lower) &&
		types.CompareOrder(defaultVal, lower, types.Ascending) != types.Less &&
		types.CompareOrder(defaultVal, upper, types.Ascending) == types.Less {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketDefaultInRange,
			"The $bucket 'default' field must be less than the lowest boundary or greater than or equal to "+
				"the highest boundary.", "$bucket (stage)")
	}
	return nil
}

func (b *bucket) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$bucket")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	// Indices 0..len(boundaries)-2 are the regular buckets and
	// len(boundaries)-1 the default one. As mongod's $group does, each
	// document is accumulated as soon as its bucket is known.
	numBuckets := len(b.boundaries) - 1
	buckets := make([]*bucketState, numBuckets+1)

	for _, doc := range docs {
		val, err := evaluateGroupExpr(b.groupBy, doc)
		if err != nil {
			return nil, err
		}

		bucketIdx := b.findBucket(val)
		if bucketIdx < 0 {
			if !b.hasDefault {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(
					handlererrors.ErrBucketNoBranch,
					"$switch could not find a matching branch for an input, and no default was specified.",
					"$bucket (stage)",
				)
			}
			bucketIdx = numBuckets
		}

		if buckets[bucketIdx] == nil {
			buckets[bucketIdx] = newBucketState(b.output, b.cmp)
		}
		if err = buckets[bucketIdx].add(doc); err != nil {
			return nil, err
		}
	}

	var res []*types.Document
	for i, state := range buckets {
		if state == nil {
			continue
		}
		id := b.defaultValue
		if i < numBuckets {
			id = b.boundaries[i]
		}
		doc := must.NotFail(types.NewDocument("_id", id))
		if err = state.appendTo(doc); err != nil {
			return nil, err
		}
		res = append(res, doc)
	}

	result := iterator.Values(iterator.ForSlice(res))
	closer.Add(result)

	return result, nil
}

// bucketState accumulates one bucket's documents.
type bucketState struct {
	output []bucketOutput
	accs   []accumulators.Accumulation
	count  int32
}

func newBucketState(output []bucketOutput, cmp *collation.Comparator) *bucketState {
	s := &bucketState{output: output, accs: make([]accumulators.Accumulation, len(output))}
	for i, out := range output {
		s.accs[i] = accumulators.NewAccumulation(out.accumulator, cmp)
	}
	return s
}

func (s *bucketState) add(doc *types.Document) error {
	s.count++
	for _, acc := range s.accs {
		if err := acc.Accumulate(doc); err != nil {
			return processGroupStageError(err)
		}
	}
	return nil
}

// appendTo sets the bucket's outputs on doc, or a count when there are none.
func (s *bucketState) appendTo(doc *types.Document) error {
	if len(s.output) == 0 {
		doc.Set("count", s.count)
		return nil
	}
	for i, out := range s.output {
		val, err := s.accs[i].Result()
		if err != nil {
			return processGroupStageError(err)
		}
		doc.Set(out.field, val)
	}
	return nil
}

// findBucket returns the bucket index for a value, or -1 if out of range.
func (b *bucket) findBucket(val any) int {
	bounds := b.boundaryKeys
	key := b.cmp.KeyValue(val)

	// Binary search for the bucket where boundaries[i] <= val < boundaries[i+1].
	lo, hi := 0, len(bounds)-2

	// First check if val is below the first boundary or at/above the last.
	if types.CompareOrder(key, bounds[0], types.Ascending) == types.Less {
		return -1
	}

	if types.CompareOrder(key, bounds[len(bounds)-1], types.Ascending) != types.Less {
		return -1
	}

	for lo <= hi {
		mid := (lo + hi) / 2
		cmpLower := types.CompareOrder(key, bounds[mid], types.Ascending)
		cmpUpper := types.CompareOrder(key, bounds[mid+1], types.Ascending)

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
func appendBucketOutput(doc *types.Document, output []bucketOutput, cmp *collation.Comparator, groupDocs []*types.Document) error {
	if len(output) == 0 {
		doc.Set("count", int32(len(groupDocs)))
		return nil
	}

	for _, out := range output {
		iter := iterator.Values(iterator.ForSlice(groupDocs))
		val, err := accumulators.Accumulate(out.accumulator, iter, cmp)
		if err != nil {
			return processGroupStageError(err)
		}

		doc.Set(out.field, val)
	}
	return nil
}

var (
	_ aggregations.Stage = (*bucket)(nil)
)

// isGroupExpression reports whether v is a $-prefixed path or an operator
// expression, as $bucket's groupBy and $sortByCount require.
func isGroupExpression(v any) bool {
	switch v := v.(type) {
	case string:
		return strings.HasPrefix(v, "$")
	case *types.Document:
		return v.Len() > 0 && strings.HasPrefix(v.Keys()[0], "$")
	}
	return false
}
