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

// Package accumulators provides aggregation accumulator operators.
// Accumulators are different from other operators as they perform operations
// on a group of documents rather than a single document.
// They are used only in small subset of available stages (like `$group`).
//
// Accumulators that can be used outside of accumulation with different behaviour (like `$sum`),
// should be stored in both operators and accumulators packages.
package accumulators

import (
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// newAccumulatorFunc is a type for a function that creates an accumulation operator.
// It takes the arguments extracted from the accumulator document.
type newAccumulatorFunc func(args ...any) (Accumulator, error)

// Accumulator is the parsed, immutable spec for one accumulation (e.g.
// {$sum: "$x"}). It is parsed once per $group/$bucket field and creates fresh
// per-group accumulation state via New, so documents are folded incrementally
// rather than buffered.
type Accumulator interface {
	// New returns a fresh, zero-state Accumulation for a single group.
	New() Accumulation
}

// Accumulation folds the documents of a single group and produces the
// accumulated result. It is stateful and not safe for concurrent use; one
// instance is created per group.
type Accumulation interface {
	// Accumulate folds one document into the running state.
	Accumulate(doc *types.Document) error
	// Result returns the accumulated value. Called once after the last Accumulate.
	Result() (any, error)
}

// Accumulate is a helper that folds every document from iter through a fresh
// Accumulation built from acc, then returns the result. It always closes iter.
// Used by stages that must buffer their input for other reasons ($bucket,
// $bucketAuto); $group folds incrementally without it.
func Accumulate(acc Accumulator, iter types.DocumentsIterator) (any, error) {
	defer iter.Close()

	state := acc.New()

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, err
		}

		if err := state.Accumulate(doc); err != nil {
			return nil, err
		}
	}

	return state.Result()
}

func NewAccumulator(stage, key string, value any) (Accumulator, error) {
	accumulation, ok := value.(*types.Document)
	if !ok || accumulation.Len() == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupInvalidAccumulator,
			fmt.Sprintf("The field '%s' must be an accumulator object", key),
			stage+" (stage)",
		)
	}

	// accumulation document contains only one field.
	if accumulation.Len() > 1 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageGroupMultipleAccumulator,
			fmt.Sprintf("The field '%s' must specify one accumulator", key),
			stage+" (stage)",
		)
	}

	operator := accumulation.Command()

	expr := must.NotFail(accumulation.Get(operator))

	var args []any

	switch expr := expr.(type) {
	case *types.Document:
		args = append(args, expr)
	case *types.Array:
		iter := expr.Iterator()
		defer iter.Close()

		for {
			_, v, err := iter.Next()

			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			args = append(args, v)
		}
	default:
		args = append(args, expr)
	}

	newAccumulator, ok := Accumulators[operator]
	if !ok {
		return nil, unknownAccumulatorError(stage, operator, expr)
	}

	if _, isArray := expr.(*types.Array); isArray {
		return nil, unaryOperatorError(operator)
	}

	return newAccumulator(args...)
}

// unimplementedAccumulators are MongoDB 8.0 $group accumulators DumboDB lacks.
var unimplementedAccumulators = map[string]bool{
	"$accumulator": true,
	"$bottom":      true,
	"$bottomN":     true,
	"$firstN":      true,
	"$lastN":       true,
	"$maxN":        true,
	"$median":      true,
	"$minN":        true,
	"$percentile":  true,
	"$top":         true,
	"$topN":        true,
}

// unknownAccumulatorError is the error for an operator Accumulators lacks:
// NotImplemented for a MongoDB accumulator, otherwise MongoDB's error for a
// name $group does not accept. MongoDB registers the window-only covariance
// functions as unary accumulators, so an array operand fails that check
// first.
func unknownAccumulatorError(stage, operator string, operand any) error {
	if unimplementedAccumulators[operator] {
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrNotImplemented,
			fmt.Sprintf("%s accumulator %q is not implemented yet", stage, operator),
			operator+" (accumulator)",
		)
	}

	if _, isArray := operand.(*types.Array); isArray && (operator == "$covariancePop" || operator == "$covarianceSamp") {
		return unaryOperatorError(operator)
	}

	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrStageGroupUnknownOperator,
		fmt.Sprintf("unknown group operator '%s'", operator),
		operator+" (accumulator)",
	)
}

// unaryOperatorError is MongoDB's error for an array operand: every $group
// accumulator takes exactly one.
func unaryOperatorError(operator string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrStageGroupUnaryOperator,
		fmt.Sprintf("The %s accumulator is a unary operator", operator),
		operator+" (accumulator)",
	)
}

// Accumulators maps all aggregation accumulators.
var Accumulators = map[string]newAccumulatorFunc{
	// sorted alphabetically
	"$addToSet":     newAddToSet,
	"$avg":          newAvg,
	"$count":        newCount,
	"$first":        newFirst,
	"$last":         newLast,
	"$max":          newMax,
	"$mergeObjects": newMergeObjectsAccumulator,
	"$min":          newMin,
	"$push":         newPush,
	"$stdDevPop":    newStdDevPop,
	"$stdDevSamp":   newStdDevSamp,
	"$sum":          newSum,
	// please keep sorted alphabetically
}
