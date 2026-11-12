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
	"strings"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// mongod 8.0 (internalQueryFrameworkControl: trySbeRestricted) runs a
// pipeline's $group stages in its SBE engine, together with the stages before
// them that its find layer absorbs. Expression errors raised there carry SBE's
// codes and messages rather than the classic engine's.

// SBEPrefixLen returns how many leading stages of a top-level pipeline mongod
// runs in SBE: through its last $group, $bucket or $sortByCount when every
// stage before it can be pushed down with it, else 0.
func SBEPrefixLen(specs []*types.Document) int {
	var sawSort, sawProject, sawLimit, sawLookup, sawGroup bool
	end := 0
	for i, spec := range specs {
		if spec == nil {
			return end
		}
		pushed := sawLookup || sawGroup
		switch spec.Command() {
		case "$match":
			if sawLimit || sawGroup {
				return end
			}
		case "$sort":
			if sawProject || sawLimit || pushed {
				return end
			}
			sawSort = true
		case "$limit", "$skip":
			if pushed {
				return end
			}
			sawLimit = true
		case "$project":
			if sawProject || pushed || !sbeProjectPushable(spec, sawSort) {
				return end
			}
			sawProject = true
		case "$group":
			sawGroup = true
			end = i + 1
		case "$bucket", "$sortByCount":
			return i + 1
		case "$lookup":
			if v, _ := spec.Get("$lookup"); v != nil {
				if d, ok := v.(*types.Document); !ok || d.Has("pipeline") {
					return end
				}
			}
			sawLookup = true
		default:
			return end
		}
	}
	return end
}

// sbeSpecs returns the stage documents of pipeline, nil for a non-document.
func sbeSpecs(pipeline *types.Array) []*types.Document {
	if pipeline == nil {
		return nil
	}
	specs := make([]*types.Document, pipeline.Len())
	for i := range specs {
		specs[i], _ = must.NotFail(pipeline.Get(i)).(*types.Document)
	}
	return specs
}

// sbeProjectPushable reports whether mongod's find layer absorbs a $project:
// not an exclusion, and after a $sort only an inclusion.
func sbeProjectPushable(spec *types.Document, afterSort bool) bool {
	v, _ := spec.Get("$project")
	proj, ok := v.(*types.Document)
	if !ok {
		return false
	}
	keys := proj.Keys()
	included := false
	for i, val := range proj.Values() {
		switch val := val.(type) {
		case bool, int32, int64, float64, types.Decimal128:
			if isProjectionTruthy(val) {
				included = true
			} else if keys[i] != "_id" {
				return false
			}
		default:
			if afterSort {
				return false
			}
			included = true
		}
	}
	return included
}

func isProjectionTruthy(v any) bool {
	switch v := v.(type) {
	case bool:
		return v
	case int32:
		return v != 0
	case int64:
		return v != 0
	case float64:
		return v != 0
	case types.Decimal128:
		return types.Compare(v, int32(0)) != types.Equal
	}
	return false
}

// WrapSBEStages makes the first n stages report expression errors as mongod's
// SBE engine does.
func WrapSBEStages(pipeline []aggregations.Stage, n int) {
	for i := 0; i < n && i < len(pipeline); i++ {
		pipeline[i] = &sbeStage{inner: pipeline[i]}
	}
}

type sbeStage struct {
	inner aggregations.Stage
}

func (s *sbeStage) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	out, err := s.inner.Process(ctx, iter, closer)
	if err != nil {
		return nil, sbeError(err)
	}
	res := &sbeIterator{iter: out}
	closer.Add(res)
	return res, nil
}

type sbeIterator struct {
	iter types.DocumentsIterator
}

func (it *sbeIterator) Next() (struct{}, *types.Document, error) {
	_, doc, err := it.iter.Next()
	if err != nil {
		return struct{}{}, nil, sbeError(err)
	}
	return struct{}{}, doc, nil
}

func (it *sbeIterator) Close() {
	it.iter.Close()
}

// sbeError returns err with SBE's code and message when SBE reports the same
// failure differently, as for math operators.
func sbeError(err error) error {
	if errors.Is(err, iterator.ErrIteratorDone) {
		return err
	}
	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) || cmdErr.Info() == nil {
		return err
	}
	op := cmdErr.Info().Argument
	code, msg, ok := sbeEquivalent(cmdErr.Code(), op, cmdErr.Err().Error())
	if !ok {
		return err
	}
	return handlererrors.NewCommandErrorMsgWithArgument(code, msg, op)
}

// sbeBoundedTrigOps are the trigonometric operators with a restricted domain,
// whose non-numeric error SBE reports under its own code.
var sbeBoundedTrigOps = map[string]bool{
	"$sin": true, "$cos": true, "$tan": true, "$asin": true, "$acos": true, "$acosh": true, "$atanh": true,
}

var sbeUnboundedTrigOps = map[string]bool{
	"$atan": true, "$sinh": true, "$cosh": true, "$tanh": true, "$asinh": true,
	"$degreesToRadians": true, "$radiansToDegrees": true,
}

var sbeMathNonNumeric = map[string]handlererrors.ErrorCode{
	"$abs":   handlererrors.ErrSBEAbsNonNumeric,
	"$ceil":  handlererrors.ErrSBECeilNonNumeric,
	"$floor": handlererrors.ErrSBEFloorNonNumeric,
	"$exp":   handlererrors.ErrSBEExpNonNumeric,
	"$ln":    handlererrors.ErrSBELnNonNumeric,
	"$log10": handlererrors.ErrSBELog10NonNumeric,
	"$sqrt":  handlererrors.ErrSBESqrtNonNumeric,
}

func sbeEquivalent(code handlererrors.ErrorCode, op, msg string) (handlererrors.ErrorCode, string, bool) {
	switch code {
	case handlererrors.ErrBadValue:
		if op == "$divide" {
			return handlererrors.ErrSBEDivideByZero, msg, true
		}
	case handlererrors.ErrTypeMismatch:
		switch op {
		case "$divide":
			return handlererrors.ErrSBEDivideNonNumeric, "$divide only supports numeric types", true
		case "$subtract":
			return handlererrors.ErrSBESubtractNonNumeric, "Only numbers and dates are allowed in an $subtract expression. " +
				"To subtract a number from a date, the date must be the first argument.", true
		case "$add":
			return handlererrors.ErrSBEAddNonNumeric, "only numbers and dates are allowed in an $add expression", true
		case "$multiply":
			return handlererrors.ErrSBEMultiplyNonNumeric, "only numbers are allowed in an $multiply expression", true
		}
	case handlererrors.ErrModByZero, handlererrors.ErrModByZeroDecimal:
		return handlererrors.ErrSBEModByZero, "can't $mod by zero", true
	case handlererrors.ErrModNonNumeric:
		return handlererrors.ErrSBEModNonNumeric, "$mod only supports numeric types", true
	case handlererrors.ErrMathNonNumeric:
		switch {
		case sbeMathNonNumeric[op] != 0:
			return sbeMathNonNumeric[op], op + " only supports numeric types", true
		case sbeBoundedTrigOps[op]:
			return handlererrors.ErrSBEBoundedTrigNonNumeric, op + " supports only numeric types", true
		case sbeUnboundedTrigOps[op]:
			return handlererrors.ErrSBETrigNonNumeric, op + " supports only numeric types", true
		}
	case handlererrors.ErrAbsOfLongMin:
		return handlererrors.ErrSBEAbsOfLongMin, msg, true
	case handlererrors.ErrLnNonPositive:
		return handlererrors.ErrSBELnNonPositive, "$ln's argument must be a positive number", true
	case handlererrors.ErrLog10NonPositive:
		return handlererrors.ErrSBELog10NonPositive, "$log10's argument must be a positive number", true
	case handlererrors.ErrSqrtNegative:
		return handlererrors.ErrSBESqrtNegative, msg, true
	case handlererrors.ErrPowZeroNegativeExponent:
		return handlererrors.ErrSBEPowZeroNegative, "$pow cannot raise 0 to a negative exponent", true
	case handlererrors.ErrPowNonNumericBase, handlererrors.ErrPowNonNumericExponent:
		return handlererrors.ErrSBEPowNonNumeric, "$pow only supports numeric types", true
	case handlererrors.ErrRoundNonNumeric:
		return handlererrors.ErrSBERoundNonNumeric, op + " only supports numeric types", true
	case handlererrors.ErrRoundPlaceNotIntegral, handlererrors.ErrRoundPlaceOutOfRange, handlererrors.ErrCannotConvertToLong:
		if op == "$round" || op == "$trunc" {
			return handlererrors.ErrSBERoundPlace, op + ` requires "place" argument to be an integer between -20 and 100`, true
		}
	case handlererrors.ErrTrigOutOfBounds:
		const marker = "value must be in "
		if i := strings.LastIndex(msg, marker); i >= 0 {
			bounds := strings.ReplaceAll(msg[i+len(marker):], ",", ", ")
			return handlererrors.ErrSBETrigOutOfBounds, "Cannot apply " + op + ", value must be in " + bounds, true
		}
	case handlererrors.ErrNumericFirstArgument, handlererrors.ErrNumericSecondArgument:
		if op == "$atan2" {
			return handlererrors.ErrSBEAtan2NonNumeric, "$atan2 supports only numeric types", true
		}
	}
	return 0, "", false
}

var (
	_ aggregations.Stage = (*sbeStage)(nil)
)
