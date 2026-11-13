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
	"math"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// densifyMaxGeneratedDocs is mongod's internalQueryMaxAllowedDensifyDocs.
const densifyMaxGeneratedDocs = 500000

type densifyBounds int

const (
	densifyBoundsFull densifyBounds = iota
	densifyBoundsPartition
	densifyBoundsExplicit
)

// densify represents the $densify stage: it fills gaps in a numeric or date
// field with documents holding only that field and the partition fields.
type densify struct {
	field      types.Path
	partitions []types.Path
	step       any
	unit       string
	bounds     densifyBounds
	lower      any
	upper      any
}

func newDensify(stage *types.Document) (aggregations.Stage, error) {
	v := must.NotFail(stage.Get("$densify"))
	spec, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"The $densify stage specification must be an object, found "+handlerparams.AliasFromType(v), "$densify (stage)")
	}

	var fieldVal, rangeVal, partitionsVal any
	for _, key := range spec.Keys() {
		val := must.NotFail(spec.Get(key))
		if _, isNull := val.(types.NullType); isNull {
			// mongod's IDL parser treats a null field as absent.
			continue
		}
		switch key {
		case "field":
			// mongod's IDL parser checks each field's type as it reads it,
			// before it looks for missing ones.
			if _, isString := val.(string); !isString {
				if _, err := stagePath(val, "$densify.field", "$densify (stage)"); err != nil {
					return nil, err
				}
			}
			fieldVal = val
		case "range":
			rangeVal = val
		case "partitionByFields":
			partitionsVal = val
		default:
			return nil, densifyUnknownField("$densify." + key)
		}
	}
	if fieldVal == nil {
		return nil, densifyMissingField("$densify.field")
	}
	if rangeVal == nil {
		return nil, densifyMissingField("$densify.range")
	}

	d := new(densify)
	var err error
	if d.field, err = stagePath(fieldVal, "$densify.field", "$densify (stage)"); err != nil {
		return nil, err
	}

	if partitionsVal != nil {
		arr, ok := partitionsVal.(*types.Array)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				"BSON field '$densify.partitionByFields' is the wrong type '"+handlerparams.AliasFromType(partitionsVal)+
					"', expected type 'array'", "$densify (stage)")
		}
		for i := range arr.Len() {
			p, err := stagePath(must.NotFail(arr.Get(i)), "$densify.partitionByFields", "$densify (stage)")
			if err != nil {
				return nil, err
			}
			if p.String() == d.field.String() {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyPartitionIncludesField,
					"partitionByFields cannot include field that is being densified", "$densify (stage)")
			}
			d.partitions = append(d.partitions, p)
		}
	}

	if err = d.parseRange(rangeVal); err != nil {
		return nil, err
	}
	return d, nil
}

func (d *densify) parseRange(v any) error {
	spec, ok := v.(*types.Document)
	if !ok {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"BSON field '$densify.range' is the wrong type '"+handlerparams.AliasFromType(v)+"', expected type 'object'",
			"$densify (stage)")
	}
	var boundsVal, unitVal any
	for _, key := range spec.Keys() {
		val := must.NotFail(spec.Get(key))
		if _, isNull := val.(types.NullType); isNull && key != "bounds" {
			continue
		}
		switch key {
		case "step":
			d.step = val
		case "bounds":
			boundsVal = val
		case "unit":
			unitVal = val
		default:
			return densifyUnknownField("$densify.range." + key)
		}
	}
	if d.step == nil {
		return densifyMissingField("$densify.range.step")
	}
	if boundsVal == nil {
		return densifyMissingField("$densify.range.bounds")
	}

	switch d.step.(type) {
	case int32, int64, float64, types.Decimal128:
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"BSON field '$densify.range.step' is the wrong type '"+handlerparams.AliasFromType(d.step)+
				"', expected types '[int, double, long, decimal']", "$densify (stage)")
	}
	if types.Compare(d.step, int32(0)) != types.Greater {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyStepNotPositive,
			"The step parameter in a range statement must be a strictly positive numeric value", "$densify (stage)")
	}

	if unitVal != nil {
		unit, ok := unitVal.(string)
		if !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				"BSON field '$densify.range.unit' is the wrong type '"+handlerparams.AliasFromType(unitVal)+
					"', expected type 'string'", "$densify (stage)")
		}
		if !operators.IsTimeUnit(unit) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
				"unknown time unit value: "+unit, "$densify (stage)")
		}
		d.unit = unit
		if _, ok := wholeStep(d.step); !ok {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyDateStepNotWhole,
				"The step parameter in a range statement must be a whole number when densifying a date range",
				"$densify (stage)")
		}
	}

	switch b := boundsVal.(type) {
	case string:
		switch b {
		case "full":
			d.bounds = densifyBoundsFull
		case "partition":
			if len(d.partitions) == 0 {
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyPartitionBoundsWithoutFields,
					"One cannot specify the bounds as 'partition' without specifying a non-empty array of "+
						"partitionByFields. You may have meant to specify 'full' bounds.", "$densify (stage)")
			}
			d.bounds = densifyBoundsPartition
		default:
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsString,
				"Bounds string must either be 'full' or 'partition'", "$densify (stage)")
		}
	case *types.Array:
		return d.parseExplicitBounds(b)
	default:
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsInvalid,
			"The bounds in a range statement must be the string 'full', 'partition', or an ascending array of "+
				"two numbers or two dates", "$densify (stage)")
	}
	return nil
}

func (d *densify) parseExplicitBounds(b *types.Array) error {
	if b.Len() != 2 {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsLength,
			"A bounding array in a range statement must have exactly two elements", "$densify (stage)")
	}
	lower, upper := must.NotFail(b.Get(0)), must.NotFail(b.Get(1))
	_, lowerDate := lower.(time.Time)
	_, upperDate := upper.(time.Time)
	if !lowerDate && !isDensifyNumber(lower) || !upperDate && !isDensifyNumber(upper) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsType,
			"Explicit bounds must be numeric or dates", "$densify (stage)")
	}
	if lowerDate != upperDate || types.CompareOrder(lower, upper, types.Ascending) == types.Greater {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsOrder,
			"A bounding array must be an ascending array of either two dates or two numbers", "$densify (stage)")
	}
	if lowerDate && d.unit == "" {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyDateBoundsWithoutUnit,
			"A bounding array of dates must specify a unit", "$densify (stage)")
	}
	if !lowerDate && d.unit != "" {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyNumericBoundsWithUnit,
			"Numeric bounds may not have unit parameter", "$densify (stage)")
	}
	if !lowerDate && (!sameNumberType(lower, d.step) || !sameNumberType(upper, d.step)) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsStepType,
			"Upper bound, lower bound, and step must all have the same type", "$densify (stage)")
	}
	d.bounds, d.lower, d.upper = densifyBoundsExplicit, lower, upper
	return nil
}

// stagePath parses the field path v of the option field of a stage.
func stagePath(v any, field, stage string) (types.Path, error) {
	s, ok := v.(string)
	if !ok {
		return types.Path{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"BSON field '"+field+"' is the wrong type '"+handlerparams.AliasFromType(v)+"', expected type 'string'",
			stage)
	}
	for _, part := range strings.Split(s, ".") {
		if strings.HasPrefix(part, "$") {
			return types.Path{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathInvalidName,
				"Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "+
					"FieldPath field names may not start with '$', given '"+s+"'.", stage)
		}
	}
	p, err := types.NewPathFromString(s)
	if err != nil {
		return types.Path{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"Invalid field path "+s, stage)
	}
	return p, nil
}

func densifyUnknownField(name string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
		"BSON field '"+name+"' is an unknown field.", "$densify (stage)")
}

func densifyMissingField(name string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrMissingField,
		"BSON field '"+name+"' is missing but a required field", "$densify (stage)")
}

func isDensifyNumber(v any) bool {
	switch v.(type) {
	case int32, int64, float64, types.Decimal128:
		return true
	}
	return false
}

func sameNumberType(a, b any) bool {
	switch a.(type) {
	case int32:
		_, ok := b.(int32)
		return ok
	case int64:
		_, ok := b.(int64)
		return ok
	case float64:
		_, ok := b.(float64)
		return ok
	case types.Decimal128:
		_, ok := b.(types.Decimal128)
		return ok
	}
	return false
}

// wholeStep returns step as an int64 when it is a whole number.
func wholeStep(step any) (int64, bool) {
	switch s := step.(type) {
	case int32:
		return int64(s), true
	case int64:
		return s, true
	case float64:
		if s != math.Trunc(s) || math.IsInf(s, 0) {
			return 0, false
		}
		return int64(s), true
	case types.Decimal128:
		return aggregations.Decimal128ToInt64(s)
	}
	return 0, false
}

// Process densifies documents as mongod does: it reads them in the order of
// its sort (the field alone under full bounds, else the partitions and then
// the field), keeps the next value to generate for each partition, and fills
// each gap when the document that ends it arrives. Documents are generated as
// the stage is read.
func (d *densify) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$densify")
	if err != nil {
		return nil, err
	}

	sortSpec := types.MakeDocument(len(d.partitions) + 1)
	if d.bounds != densifyBoundsFull {
		for _, p := range d.partitions {
			sortSpec.Set(p.String(), int32(1))
		}
	}
	sortSpec.Set(d.field.String(), int32(1))
	if err = common.SortDocuments(docs, sortSpec); err != nil {
		return nil, err
	}

	it := &densifyIterator{d: d, docs: docs, keys: map[string][]int{}}
	for _, doc := range docs {
		v, ok, err := d.fieldValue(doc)
		if err != nil {
			return nil, err
		}
		if ok && d.bounds == densifyBoundsFull {
			if it.min == nil {
				it.min = v
			}
			it.max = v
		}
	}
	if d.bounds == densifyBoundsExplicit && len(d.partitions) == 0 {
		// Explicit bounds are filled even without input documents.
		it.states = append(it.states, &densifyPartitionState{next: d.lower})
	}
	closer.Add(it)
	return it, nil
}

// densifyPartitionState is a partition's next value to generate and its
// first document, whose partition values the documents generated at the end
// copy.
type densifyPartitionState struct {
	key   *types.Document
	next  any
	first *types.Document
	// rawMin is set while next is the full bounds' minimum as given, which a
	// fill before the next document of the partition converts to the step's
	// type; the fill at the end of the input does not.
	rawMin bool
}

type densifyIterator struct {
	d         *densify
	docs      []*types.Document
	pos       int
	min, max  any
	keys      map[string][]int
	states    []*densifyPartitionState
	generated int

	gen       *densifyPartitionState
	genUntil  any
	genOrEq   bool
	genTmpl   *types.Document
	pending   *types.Document
	finishing int
}

func (it *densifyIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}
	d := it.d
	for {
		if it.gen != nil {
			if doc, err := it.nextGenerated(); doc != nil || err != nil {
				return unused, doc, err
			}
			it.gen = nil
		}
		if it.pending != nil {
			doc := it.pending
			it.pending = nil
			return unused, doc, nil
		}
		if it.pos < len(it.docs) {
			doc := it.docs[it.pos]
			it.pos++
			if err := it.arrive(doc); err != nil {
				return unused, nil, err
			}
			continue
		}
		if it.finishing >= len(it.states) {
			return unused, nil, iterator.ErrIteratorDone
		}
		state := it.states[it.finishing]
		it.finishing++
		if state.next == nil {
			continue
		}
		switch d.bounds {
		case densifyBoundsFull:
			it.gen, it.genUntil, it.genOrEq, it.genTmpl = state, it.max, true, state.first
		case densifyBoundsExplicit:
			it.gen, it.genUntil, it.genOrEq, it.genTmpl = state, d.upper, false, state.first
		case densifyBoundsPartition:
		}
	}
}

// arrive queues doc and the documents generated before it.
func (it *densifyIterator) arrive(doc *types.Document) error {
	d := it.d
	it.pending = doc
	v, ok, err := d.fieldValue(doc)
	if err != nil || !ok {
		return err
	}
	state, isNew := it.partition(doc)
	if state.first == nil {
		state.first = doc
	}
	if state.rawMin {
		state.next, state.rawMin = it.minInStepType(), false
	}
	if isNew {
		switch d.bounds {
		case densifyBoundsPartition:
			state.next = v
		case densifyBoundsExplicit:
			state.next = d.lower
		case densifyBoundsFull:
			if types.CompareOrder(v, it.min, types.Ascending) == types.Equal {
				// The partition starts at the minimum itself, which passes
				// through; filling continues from its value.
				state.next = v
				break
			}
			afterMin, err := it.advance(it.min)
			if err != nil {
				return err
			}
			if types.CompareOrder(v, it.min, types.Ascending) == types.Greater &&
				types.CompareOrder(v, afterMin, types.Ascending) != types.Greater {
				// mongod passes a new partition's first value through when it is
				// at most one step above the minimum, and fills from the minimum
				// later.
				state.next, state.rawMin = it.min, true
				return nil
			}
			state.next = it.minInStepType()
		}
	}
	it.gen, it.genUntil, it.genOrEq, it.genTmpl = state, v, false, doc
	return nil
}

// nextGenerated returns the next document the current fill generates, or nil
// when the fill is done, after which it moves the partition past the value
// that ended it.
func (it *densifyIterator) nextGenerated() (*types.Document, error) {
	d, state := it.d, it.gen
	cur := state.next
	c := types.CompareOrder(cur, it.genUntil, types.Ascending)
	inBounds := d.bounds != densifyBoundsExplicit || types.CompareOrder(cur, d.upper, types.Ascending) == types.Less
	if inBounds && (c == types.Less || (c == types.Equal && it.genOrEq)) {
		doc, err := it.generate(it.genTmpl, cur)
		if err != nil {
			return nil, err
		}
		if state.next, err = it.advance(cur); err != nil {
			return nil, err
		}
		return doc, nil
	}
	if c == types.Equal && !it.genOrEq {
		var err error
		if state.next, err = it.advance(cur); err != nil {
			return nil, err
		}
	}
	return nil, nil
}

// partition returns the state of doc's partition, keyed by the partition
// fields' values, in which a missing value differs from null.
func (it *densifyIterator) partition(doc *types.Document) (*densifyPartitionState, bool) {
	if len(it.d.partitions) == 0 && len(it.states) > 0 {
		return it.states[0], it.states[0].next == nil
	}
	key := types.MakeDocument(len(it.d.partitions))
	for j, p := range it.d.partitions {
		if v, err := doc.GetByPath(p); err == nil {
			key.Set(fmt.Sprintf("p%d", j), v)
		}
	}
	hash, _ := canonicalGroupKey(key)
	for _, i := range it.keys[hash] {
		if types.CompareForAggregation(it.states[i].key, key) == types.Equal {
			return it.states[i], false
		}
	}
	state := &densifyPartitionState{key: key}
	it.keys[hash] = append(it.keys[hash], len(it.states))
	it.states = append(it.states, state)
	return state, true
}

// generate returns a document with value v and the partition values of tmpl,
// which may be nil. Like mongod, it shares the values with tmpl.
func (it *densifyIterator) generate(tmpl *types.Document, v any) (*types.Document, error) {
	d := it.d
	for _, p := range d.partitions {
		if pathsOverlap(p, d.field) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyGeneratorIncludesField,
				"DocGenerator cannot include field that is being densified", "$densify (stage)")
		}
	}
	it.generated++
	if it.generated > densifyMaxGeneratedDocs {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyTooManyDocs,
			fmt.Sprintf("Generated %d documents in $densify, which is over the limit of %d. "+
				"Increase the 'internalQueryMaxAllowedDensifyDocs' parameter to allow more generated documents",
				it.generated, densifyMaxGeneratedDocs), "$densify (stage)")
	}
	doc := types.MakeDocument(len(d.partitions) + 1)
	for _, p := range d.partitions {
		if tmpl == nil {
			break
		}
		if pv, err := tmpl.GetByPath(p); err == nil {
			// A partition path below another partition's scalar is skipped.
			_ = doc.SetByPath(p, pv)
		}
	}
	if err := doc.SetByPath(d.field, v); err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyGeneratorIncludesField,
			"DocGenerator cannot include field that is being densified", "$densify (stage)")
	}
	return doc, nil
}

func (it *densifyIterator) Close() {}

// fieldValue returns doc's densify value, reporting false for a missing or
// null value, which $densify passes through.
func (d *densify) fieldValue(doc *types.Document) (any, bool, error) {
	v, err := doc.GetByPath(d.field)
	if err != nil {
		return nil, false, nil
	}
	if _, isNull := v.(types.NullType); isNull {
		return nil, false, nil
	}
	_, isDate := v.(time.Time)
	switch {
	case isDate && d.unit == "":
		return nil, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyValueUnitMismatch,
			"Encountered date densify value in collection when step does not have a date unit.", "$densify (stage)")
	case isDensifyNumber(v) && d.unit != "":
		return nil, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyValueUnitMismatch,
			"Encountered numeric densify value in collection when step has a date unit.", "$densify (stage)")
	case !isDate && !isDensifyNumber(v):
		return nil, false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyFieldType,
			"Densify field type must be numeric or a date", "$densify (stage)")
	}
	return v, true, nil
}

// pathsOverlap reports whether one of a and b is a prefix of the other.
func pathsOverlap(a, b types.Path) bool {
	as, bs := a.String(), b.String()
	return as == bs || strings.HasPrefix(as, bs+".") || strings.HasPrefix(bs, as+".")
}

// minInStepType returns the minimum a new partition's fill starts from under
// full bounds: mongod adds step times zero, which widens it to the step's type
// and gives it a decimal step's exponent.
func (it *densifyIterator) minInStepType() any {
	if _, isDate := it.min.(time.Time); isDate {
		return it.min
	}
	switch step := it.d.step.(type) {
	case float64:
		return aggregations.SumNumbers(it.min, float64(0))
	case int64:
		return aggregations.SumNumbers(it.min, int64(0))
	case types.Decimal128:
		negated := step
		negated.H ^= 1 << 63
		return aggregations.SumNumbers(it.min, aggregations.SumNumbers(step, negated))
	}
	return it.min
}

func (it *densifyIterator) advance(cur any) (any, error) {
	if t, ok := cur.(time.Time); ok {
		n, _ := wholeStep(it.d.step)
		return operators.DateAddUTC(t, it.d.unit, n)
	}
	return aggregations.SumNumbers(cur, it.d.step), nil
}

var _ aggregations.Stage = (*densify)(nil)
