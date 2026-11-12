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

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
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
// isView, when not nil, reports whether a collection is a view; an equality
// $lookup from a view ends the prefix.
func SBEPrefixLen(specs []*types.Document, isView func(string) bool) int {
	end, _ := sbePrefix(specs, isView)
	return end
}

// sbePrefix returns SBEPrefixLen and the number of leading stages mongod could
// push down into SBE at all, which bounds the $lookup stages it runs there.
func sbePrefix(specs []*types.Document, isView func(string) bool) (int, int) {
	var sawSort, sawProject, sawLimit, sawLookup, sawGroup bool
	var project *types.Document
	end := 0
	for i, spec := range specs {
		if spec == nil {
			return end, i
		}
		pushed := sawLookup || sawGroup
		if !sbeCompatibleStage(spec) {
			return end, i
		}
		switch spec.Command() {
		case "$match":
			if sawLimit || sawGroup {
				return end, i
			}
		case "$sort":
			if sawProject || sawLimit || pushed {
				return end, i
			}
			sawSort = true
		case "$limit", "$skip":
			if spec.Command() == "$skip" && isZeroSkip(spec) {
				// mongod drops a $skip of 0.
				continue
			}
			if pushed {
				return end, i
			}
			if sawProject && !sbeProjectPushable(project, true) {
				// mongod moves the $limit or $skip ahead of the $project,
				// which then no longer qualifies.
				return end, i
			}
			sawLimit = true
		case "$project":
			if sawProject || pushed || !sbeProjectPushable(spec, sawSort || sawLimit) {
				return end, i
			}
			sawProject = true
			project = spec
		case "$group":
			sawGroup = true
			end = i + 1
		case "$bucket", "$sortByCount":
			return i + 1, i + 1
		case "$lookup":
			if v, _ := spec.Get("$lookup"); v != nil {
				d, ok := v.(*types.Document)
				if !ok || d.Has("pipeline") {
					return end, i
				}
				if from, _ := d.Get("from"); isView != nil {
					if name, ok := from.(string); ok && isView(name) {
						return end, i
					}
				}
			}
			sawLookup = true
		default:
			return end, i
		}
	}
	return end, len(specs)
}

func isZeroSkip(spec *types.Document) bool {
	n, ok := toFloat64(must.NotFail(spec.Get("$skip")))
	return ok && n == 0
}

// sbeClassicOperators are the expression operators that make mongod 8.0 run
// a stage in its classic engine instead of SBE.
var sbeClassicOperators = map[string]bool{
	"$allElementsTrue": true, "$arrayElemAt": true, "$avg": true, "$binarySize": true, "$convert": true,
	"$filter": true, "$first": true, "$firstN": true, "$getField": true, "$in": true, "$indexOfArray": true,
	"$last": true, "$lastN": true, "$log": true, "$map": true, "$max": true, "$maxN": true, "$median": true,
	"$mergeObjects": true, "$min": true, "$minN": true, "$percentile": true, "$rand": true, "$reduce": true,
	"$replaceAll": true, "$setField": true, "$size": true, "$slice": true, "$split": true, "$stdDevPop": true,
	"$stdDevSamp": true, "$strcasecmp": true, "$strLenCP": true, "$substr": true, "$substrBytes": true,
	"$substrCP": true, "$sum": true, "$toBool": true, "$toDate": true, "$toDecimal": true, "$toDouble": true,
	"$toInt": true, "$toLong": true, "$toObjectId": true, "$toString": true, "$toUUID": true,
	"$unsetField": true, "$type": true, "$zip": true,
}

// sbeClassicAccumulators are the $group accumulators that make mongod run the
// $group in its classic engine.
var sbeClassicAccumulators = map[string]bool{"$median": true, "$percentile": true}

// groupsByIDKeepingOne reports whether a $group groups by "$_id" and keeps
// one value per group ($first, $last, $top, $bottom), which mongod rewrites
// into a stage of its classic engine.
func groupsByIDKeepingOne(spec *types.Document) bool {
	if id, _ := spec.Get("_id"); id != "$_id" {
		return false
	}
	for _, field := range spec.Keys() {
		if field == "_id" {
			continue
		}
		acc, ok := must.NotFail(spec.Get(field)).(*types.Document)
		if !ok || acc.Len() != 1 {
			return false
		}
		switch acc.Command() {
		case "$first", "$last", "$top", "$bottom":
		default:
			return false
		}
	}
	return true
}

// sbeCompatibleStage reports whether every expression of a stage, and for a
// grouping stage every accumulator, can run in mongod's SBE engine.
func sbeCompatibleStage(spec *types.Document) bool {
	name := spec.Command()
	v, _ := spec.Get(name)
	switch name {
	case "$group":
		d, ok := v.(*types.Document)
		if !ok {
			return true
		}
		if groupsByIDKeepingOne(d) {
			return false
		}
		for _, field := range d.Keys() {
			val := must.NotFail(d.Get(field))
			if acc, ok := val.(*types.Document); ok && field != "_id" && acc.Len() == 1 {
				if sbeClassicAccumulators[acc.Command()] {
					return false
				}
				if !sbeCompatibleExpression(must.NotFail(acc.Get(acc.Command()))) {
					return false
				}
				continue
			}
			if !sbeCompatibleExpression(val) {
				return false
			}
		}
		return true
	case "$bucket":
		d, ok := v.(*types.Document)
		if !ok {
			return true
		}
		if out, _ := d.Get("output"); out != nil {
			if !sbeCompatibleStage(must.NotFail(types.NewDocument("$group", out))) {
				return false
			}
		}
		g, _ := d.Get("groupBy")
		return sbeCompatibleExpression(g)
	case "$project", "$sortByCount":
		return sbeCompatibleExpression(v)
	case "$match":
		d, ok := v.(*types.Document)
		return !ok || sbeCompatibleFilter(d)
	}
	return true
}

// sbeCompatibleFilter checks the $expr expressions of a query filter.
func sbeCompatibleFilter(filter *types.Document) bool {
	for _, k := range filter.Keys() {
		val := must.NotFail(filter.Get(k))
		switch k {
		case "$expr":
			if !sbeCompatibleExpression(val) {
				return false
			}
		case "$and", "$or", "$nor":
			arr, ok := val.(*types.Array)
			if !ok {
				continue
			}
			for i := range arr.Len() {
				if d, ok := must.NotFail(arr.Get(i)).(*types.Document); ok && !sbeCompatibleFilter(d) {
					return false
				}
			}
		case "$jsonSchema":
			return false
		default:
			if d, ok := val.(*types.Document); ok && !sbeCompatiblePredicate(d) {
				return false
			}
		}
	}
	return true
}

// sbeClassicQueryOperators are the query operators that make mongod 8.0 run
// a $match in its classic engine.
var sbeClassicQueryOperators = map[string]bool{
	"$bitsAllSet": true, "$bitsAnySet": true, "$bitsAllClear": true, "$bitsAnyClear": true,
}

// sbeCompatiblePredicate checks the operators of a field's predicate.
func sbeCompatiblePredicate(pred *types.Document) bool {
	for _, op := range pred.Keys() {
		if sbeClassicQueryOperators[op] {
			return false
		}
		if inner, ok := must.NotFail(pred.Get(op)).(*types.Document); ok {
			switch op {
			case "$not":
				if !sbeCompatiblePredicate(inner) {
					return false
				}
			case "$elemMatch":
				if !sbeCompatiblePredicate(inner) || !sbeCompatibleFilter(inner) {
					return false
				}
			}
		}
	}
	return true
}

func sbeCompatibleExpression(v any) bool {
	switch v := v.(type) {
	case *types.Array:
		for i := range v.Len() {
			if !sbeCompatibleExpression(must.NotFail(v.Get(i))) {
				return false
			}
		}
	case *types.Document:
		if v.Command() == "$literal" {
			return true
		}
		for _, k := range v.Keys() {
			if sbeClassicOperators[k] || !sbeCompatibleExpression(must.NotFail(v.Get(k))) {
				return false
			}
		}
	}
	return true
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
			if common.IsTruthy(val) {
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

// MarkSBELookups has each equality $lookup that mongod runs in SBE apply
// SBE's limit on one document's matches.
func MarkSBELookups(pipeline []aggregations.Stage, specs []*types.Document, isView func(string) bool) {
	_, pushable := sbePrefix(specs, isView)
	for i := 0; i < pushable && i < len(pipeline); i++ {
		if l, ok := pipeline[i].(*lookup); ok {
			l.sbe = true
		}
	}
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

// sbeAware is implemented by stages that validate differently when mongod
// runs them in SBE.
type sbeAware interface {
	useSBE() error
}

func (s *sbeStage) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	if a, ok := s.inner.(sbeAware); ok {
		if err := a.useSBE(); err != nil {
			iter.Close()
			return nil, err
		}
	}
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
	if !errors.As(err, &cmdErr) {
		return err
	}
	var op string
	if cmdErr.Info() != nil {
		op = cmdErr.Info().Argument
	}
	var ctxErr *operators.OperatorContextError
	if errors.As(err, &ctxErr) {
		if _, mapped := sbeOperatorErrors[sbeOperatorError{ctxErr.Op, cmdErr.Code()}]; mapped || sbeDatePartOperators[ctxErr.Op] {
			op = ctxErr.Op
		}
	}
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

// sbeOperatorErrors maps an operator's input error to the code and message
// SBE reports for it.
var sbeOperatorErrors = map[sbeOperatorError]struct {
	code handlererrors.ErrorCode
	msg  string
}{
	{"$switch", 40066}:                          {handlererrors.ErrSBEBucketNoBranch, "$switch could not find a matching branch for an input, and no default was specified."},
	{"$concat", 16702}:                          {handlererrors.ErrSBEInput7158201, "$concat supports only strings"},
	{"$strLenBytes", 34473}:                     {handlererrors.ErrSBEInput5155800, "$strLenBytes requires a string argument"},
	{"$dateToString", 16006}:                    {handlererrors.ErrSBEInput4997901, "$dateToString parameter 'date' must be coercible to date"},
	{"$range", 34445}:                           {handlererrors.ErrSBEInput7157712, "$range only supports numeric types for end"},
	{"$range", 34446}:                           {handlererrors.ErrSBEInput7157715, "$range end must be representable as a 32-bit integer"},
	{"$toUpper", 16007}:                         {handlererrors.ErrSBEInput7158200, "$toUpper input type is not supported"},
	{"$toLower", 16007}:                         {handlererrors.ErrSBEInput7158200, "$toLower input type is not supported"},
	{"$trim", 50699}:                            {handlererrors.ErrSBEInput5156302, "$trim input expression must be a string"},
	{"$ltrim", 50699}:                           {handlererrors.ErrSBEInput5156302, "$ltrim input expression must be a string"},
	{"$rtrim", 50699}:                           {handlererrors.ErrSBEInput5156302, "$rtrim input expression must be a string"},
	{"$trim", 50700}:                            {handlererrors.ErrSBEInput5156303, "$trim chars expression must be a string if provided"},
	{"$ltrim", 50700}:                           {handlererrors.ErrSBEInput5156303, "$ltrim chars expression must be a string if provided"},
	{"$rtrim", 50700}:                           {handlererrors.ErrSBEInput5156303, "$rtrim chars expression must be a string if provided"},
	{"$indexOfBytes", 40091}:                    {handlererrors.ErrSBEInput7158007, "$indexOfBytes string must resolve to a string or null"},
	{"$indexOfCP", 40093}:                       {handlererrors.ErrSBEInput7158007, "$indexOfCP string must resolve to a string or null"},
	{"$regexMatch", 51104}:                      {handlererrors.ErrSBEInput5073401, "$regexMatch: input must be of type string"},
	{"$regexFind", 51104}:                       {handlererrors.ErrSBEInput5073401, "$regexFind: input must be of type string"},
	{"$regexFindAll", 51104}:                    {handlererrors.ErrSBEInput5073401, "$regexFindAll: input must be of type string"},
	{"$replaceOne", 51746}:                      {handlererrors.ErrSBEInput7158302, "$replaceOne requires that 'input' be a string"},
	{"$dateAdd", 5166403}:                       {handlererrors.ErrSBEInput7157904, "$dateAdd must have startDate argument convertable to date"},
	{"$dateSubtract", 5166403}:                  {handlererrors.ErrSBEInput7157904, "$dateSubtract must have startDate argument convertable to date"},
	{"$dateDiff", 5166307}:                      {handlererrors.ErrSBEInput7157921, "$dateDiff parameter 'startDate' must be coercible to date"},
	{"$dateTrunc", 5439012}:                     {handlererrors.ErrSBEInput7157932, "$dateTrunc parameter 'date' must be coercible to date"},
	{"$dateToParts", 16006}:                     {handlererrors.ErrSBEInput7157915, "$dateToParts date must have the format of a date"},
	{"$dateFromParts", 40515}:                   {handlererrors.ErrSBEInput7157917, "'year' must evaluate to an integer"},
	{"$concatArrays", 28664}:                    {handlererrors.ErrSBEInput7158000, "$concatArrays only supports arrays"},
	{"$setUnion", 16020}:                        {handlererrors.ErrSBEInput7158100, "All operands of $setUnion must be arrays."},
	{"$setDifference", 16020}:                   {handlererrors.ErrSBEInput7158100, "All operands of $setDifference must be arrays."},
	{"$setEquals", 16020}:                       {handlererrors.ErrSBEInput7158100, "All operands of $setEquals must be arrays."},
	{"$setIntersection", 16020}:                 {handlererrors.ErrSBEInput7158100, "All operands of $setIntersection must be arrays."},
	{"$setIsSubset", 16020}:                     {handlererrors.ErrSBEInput7158100, "All operands of $setIsSubset must be arrays."},
	{"$reverseArray", 16020}:                    {handlererrors.ErrSBEInput7158002, "$reverseArray argument must be an array"},
	{"$objectToArray", 16020}:                   {handlererrors.ErrSBEInput5153215, "$objectToArray requires an object input"},
	{"$arrayToObject", 40386}:                   {handlererrors.ErrSBEInput5153200, "$arrayToObject requires an array input"},
	{"$sortArray", 2942504}:                     {handlererrors.ErrSBEInput7158001, "$sortArray input argument must be an array"},
	{"$anyElementTrue", 16020}:                  {handlererrors.ErrSBEInput7158300, "$anyElementTrue's argument must be an array"},
	{"$tsSecond", handlererrors.ErrBadValue}:    {handlererrors.ErrSBEInput7157900, "$tsSecond expects argument of type timestamp"},
	{"$tsIncrement", handlererrors.ErrBadValue}: {handlererrors.ErrSBEInput7157901, "$tsIncrement expects argument of type timestamp"},
	{"$bsonSize", 31393}:                        {handlererrors.ErrSBEInput7158301, "$bsonSize requires a document input"},
}

type sbeOperatorError struct {
	op   string
	code handlererrors.ErrorCode
}

// sbeDatePartOperators report a non-date input under one SBE code.
var sbeDatePartOperators = map[string]bool{
	"$year": true, "$month": true, "$dayOfMonth": true, "$dayOfWeek": true, "$dayOfYear": true, "$hour": true,
	"$minute": true, "$second": true, "$millisecond": true, "$week": true, "$isoWeek": true, "$isoWeekYear": true,
	"$isoDayOfWeek": true,
}

func sbeEquivalent(code handlererrors.ErrorCode, op, msg string) (handlererrors.ErrorCode, string, bool) {
	if e, ok := sbeOperatorErrors[sbeOperatorError{op, code}]; ok {
		return e.code, e.msg, true
	}
	if code == handlererrors.ErrBucketNoBranch {
		return handlererrors.ErrSBEBucketNoBranch,
			"$switch could not find a matching branch for an input, and no default was specified.", true
	}
	if code == handlererrors.ErrCannotConvertToDate && sbeDatePartOperators[op] {
		return handlererrors.ErrSBEInput5157904, op[1:] + " parameter 'date' must be coercible to date", true
	}
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
	case handlererrors.ErrAddTwoDates:
		return handlererrors.ErrSBEAddTwoDates, "only one date allowed in an $add expression", true
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
