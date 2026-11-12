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
	if d.field, err = densifyPath(fieldVal, "$densify.field"); err != nil {
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
			p, err := densifyPath(must.NotFail(arr.Get(i)), "$densify.partitionByFields")
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
	if !lowerDate && (fmt.Sprintf("%T", lower) != fmt.Sprintf("%T", d.step) || fmt.Sprintf("%T", upper) != fmt.Sprintf("%T", d.step)) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyBoundsStepType,
			"Upper bound, lower bound, and step must all have the same type", "$densify (stage)")
	}
	d.bounds, d.lower, d.upper = densifyBoundsExplicit, lower, upper
	return nil
}

func densifyPath(v any, field string) (types.Path, error) {
	s, ok := v.(string)
	if !ok {
		return types.Path{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			"BSON field '"+field+"' is the wrong type '"+handlerparams.AliasFromType(v)+"', expected type 'string'",
			"$densify (stage)")
	}
	for _, part := range strings.Split(s, ".") {
		if strings.HasPrefix(part, "$") {
			return types.Path{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFieldPathInvalidName,
				"Consider using $getField or $setField for a field path with '.' or '$'. :: caused by :: "+
					"FieldPath field names may not start with '$', given '"+s+"'.", "$densify (stage)")
		}
	}
	p, err := types.NewPathFromString(s)
	if err != nil {
		return types.Path{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrFailedToParse,
			"Invalid field path "+s, "$densify (stage)")
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

func (d *densify) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$densify")
	if err != nil {
		return nil, err
	}

	sortDoc := types.MakeDocument(len(d.partitions) + 1)
	for _, p := range d.partitions {
		sortDoc.Set(p.String(), int32(1))
	}
	sortDoc.Set(d.field.String(), int32(1))
	if err = common.SortDocuments(docs, sortDoc); err != nil {
		return nil, err
	}

	var globalMin, globalMax any
	for _, doc := range docs {
		v, ok, err := d.fieldValue(doc)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		if globalMin == nil || types.CompareOrder(v, globalMin, types.Ascending) == types.Less {
			globalMin = v
		}
		if globalMax == nil || types.CompareOrder(v, globalMax, types.Ascending) == types.Greater {
			globalMax = v
		}
	}

	gen := &densifyGenerator{d: d}
	var out []*types.Document
	for start := 0; start < len(docs); {
		end := start + 1
		for end < len(docs) && d.samePartition(docs[start], docs[end]) {
			end++
		}
		if out, err = gen.densifyPartition(out, docs[start:end], globalMin, globalMax); err != nil {
			return nil, err
		}
		start = end
	}

	res := iterator.Values(iterator.ForSlice(out))
	closer.Add(res)
	return res, nil
}

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

func (d *densify) samePartition(a, b *types.Document) bool {
	for _, p := range d.partitions {
		av, aErr := a.GetByPath(p)
		bv, bErr := b.GetByPath(p)
		if (aErr == nil) != (bErr == nil) {
			return false
		}
		if aErr == nil && types.CompareForAggregation(av, bv) != types.Equal {
			return false
		}
	}
	return true
}

type densifyGenerator struct {
	d         *densify
	generated int
}

// densifyPartition appends one partition's documents, sorted by the field, to
// out with the documents that fill its gaps.
func (g *densifyGenerator) densifyPartition(out, docs []*types.Document, globalMin, globalMax any) ([]*types.Document, error) {
	d := g.d
	var cur, last any
	inclusive := true
	switch d.bounds {
	case densifyBoundsFull:
		cur, last = globalMin, globalMax
	case densifyBoundsPartition:
		for _, doc := range docs {
			if v, ok, _ := d.fieldValue(doc); ok {
				if cur == nil {
					cur = v
				}
				last = v
			}
		}
	case densifyBoundsExplicit:
		cur, last, inclusive = d.lower, d.upper, false
	}

	emitUntil := func(v any, orEqual bool) error {
		for cur != nil {
			c := types.CompareOrder(cur, v, types.Ascending)
			if c == types.Greater || (c == types.Equal && !orEqual) {
				return nil
			}
			if d.bounds == densifyBoundsExplicit && types.CompareOrder(cur, last, types.Ascending) != types.Less {
				return nil
			}
			if err := g.emit(&out, docs[0], cur); err != nil {
				return err
			}
			var err error
			if cur, err = g.advance(cur); err != nil {
				return err
			}
		}
		return nil
	}

	for _, doc := range docs {
		v, ok, err := d.fieldValue(doc)
		if err != nil {
			return nil, err
		}
		if ok && cur != nil {
			if err = emitUntil(v, false); err != nil {
				return nil, err
			}
			if types.CompareOrder(cur, v, types.Ascending) == types.Equal {
				if cur, err = g.advance(cur); err != nil {
					return nil, err
				}
			}
		}
		out = append(out, doc)
	}
	if cur != nil && last != nil {
		if err := emitUntil(last, inclusive); err != nil {
			return nil, err
		}
	}
	return out, nil
}

// emit appends a generated document with value v and the partition fields of
// tmpl.
func (g *densifyGenerator) emit(out *[]*types.Document, tmpl *types.Document, v any) error {
	g.generated++
	if g.generated > densifyMaxGeneratedDocs {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrDensifyTooManyDocs,
			fmt.Sprintf("Generated %d documents in $densify, which is over the limit of %d. "+
				"Increase the 'internalQueryMaxAllowedDensifyDocs' parameter to allow more generated documents",
				g.generated, densifyMaxGeneratedDocs), "$densify (stage)")
	}
	doc := types.MakeDocument(len(g.d.partitions) + 1)
	for _, p := range g.d.partitions {
		if v, err := tmpl.GetByPath(p); err == nil {
			must.NoError(doc.SetByPath(p, v))
		}
	}
	must.NoError(doc.SetByPath(g.d.field, v))
	*out = append(*out, doc)
	return nil
}

func (g *densifyGenerator) advance(cur any) (any, error) {
	if t, ok := cur.(time.Time); ok {
		n, _ := wholeStep(g.d.step)
		return operators.DateAddUTC(t, g.d.unit, n)
	}
	return aggregations.SumNumbers(cur, g.d.step), nil
}

var _ aggregations.Stage = (*densify)(nil)
