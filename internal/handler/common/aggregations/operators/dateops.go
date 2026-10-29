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

package operators

import (
	"fmt"
	"strings"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
)

// toTime converts a value to time.Time. Returns false if conversion is not possible.
func toTime(v any) (time.Time, bool) {
	switch val := v.(type) {
	case time.Time:
		return val.UTC(), true
	case int64:
		return time.Unix(0, val*int64(time.Millisecond)).UTC(), true
	case int32:
		return time.Unix(0, int64(val)*int64(time.Millisecond)).UTC(), true
	case float64:
		return time.Unix(0, int64(val)*int64(time.Millisecond)).UTC(), true
	}

	return time.Time{}, false
}

func toInt64(v any) int64 {
	switch n := v.(type) {
	case int32:
		return int64(n)
	case int64:
		return n
	case float64:
		return int64(n)
	}

	return 0
}

// dateDiffOp represents { $dateDiff: { startDate: <expr>, endDate: <expr>, unit: <string> } }.
type dateDiffOp struct {
	startDateArg any
	endDateArg   any
	unitArg      any
}

func newDateDiff(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			"$dateDiff requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			"$dateDiff requires a document argument")
	}

	startDateArg, err := doc.Get("startDate")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			"Missing 'startDate' parameter to $dateDiff")
	}

	endDateArg, err := doc.Get("endDate")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			"Missing 'endDate' parameter to $dateDiff")
	}

	unitArg, err := doc.Get("unit")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			"Missing 'unit' parameter to $dateDiff")
	}

	return &dateDiffOp{startDateArg: startDateArg, endDateArg: endDateArg, unitArg: unitArg}, nil
}

func (op *dateDiffOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.startDateArg, doc)
	if err != nil {
		return nil, err
	}

	ev, err := evalArgValue(op.endDateArg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(sv) || isNullish(ev) {
		return types.Null, nil
	}

	start, ok := toTime(sv)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			fmt.Sprintf("$dateDiff startDate must be a date, got %T", sv))
	}

	end, ok := toTime(ev)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			fmt.Sprintf("$dateDiff endDate must be a date, got %T", ev))
	}

	uv, err := evalArgValue(op.unitArg, doc)
	if err != nil {
		return nil, err
	}

	unit, ok := uv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			"$dateDiff unit must be a string")
	}

	diff, err := diffDateUnit(start, end, unit)
	if err != nil {
		return nil, err
	}

	return diff, nil
}

func diffDateUnit(start, end time.Time, unit string) (int64, error) {
	switch strings.ToLower(unit) {
	case "millisecond":
		return end.UnixMilli() - start.UnixMilli(), nil
	case "second":
		return int64(end.Sub(start).Seconds()), nil
	case "minute":
		return int64(end.Sub(start).Minutes()), nil
	case "hour":
		return int64(end.Sub(start).Hours()), nil
	case "day":
		// Count calendar day boundaries crossed (truncate both to midnight).
		startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
		endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, end.Location())
		return int64(endDay.Sub(startDay).Hours() / 24), nil
	case "week":
		// Count calendar week boundaries crossed.
		startDay := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, start.Location())
		endDay := time.Date(end.Year(), end.Month(), end.Day(), 0, 0, 0, 0, end.Location())
		return int64(endDay.Sub(startDay).Hours() / (24 * 7)), nil
	case "month":
		years := int64(end.Year() - start.Year())
		months := int64(end.Month() - start.Month())
		total := years*12 + months
		// Adjust if end day < start day (incomplete month)
		if end.Day() < start.Day() {
			total--
		}

		return total, nil
	case "quarter":
		months, err := diffDateUnit(start, end, "month")
		if err != nil {
			return 0, err
		}

		return months / 3, nil
	case "year":
		years := int64(end.Year() - start.Year())
		// Adjust if end month/day < start month/day (incomplete year)
		startMD := time.Date(0, start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
		endMD := time.Date(0, end.Month(), end.Day(), 0, 0, 0, 0, time.UTC)

		if endMD.Before(startMD) {
			years--
		}

		return years, nil
	default:
		return 0, newOperatorError(ErrArgsInvalidLen, "$dateDiff",
			fmt.Sprintf("unknown date unit: %s", unit))
	}
}

var _ Operator = (*dateDiffOp)(nil)

// dateTruncOp represents { $dateTrunc: { date: <expr>, unit: <string>, binSize: <number> } }.
// Truncates a date to the start of the specified time unit.
type dateTruncOp struct {
	dateArg    any
	unitArg    any
	binSizeArg any // optional, defaults to 1
}

func newDateTrunc(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			"$dateTrunc requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			"$dateTrunc requires a document argument")
	}

	dateArg, err := doc.Get("date")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			"Missing 'date' parameter to $dateTrunc")
	}

	unitArg, err := doc.Get("unit")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			"Missing 'unit' parameter to $dateTrunc")
	}

	op := &dateTruncOp{dateArg: dateArg, unitArg: unitArg}

	if binSizeArg, err := doc.Get("binSize"); err == nil {
		op.binSizeArg = binSizeArg
	}

	return op, nil
}

func (op *dateTruncOp) Process(doc *types.Document) (any, error) {
	dv, err := evalArgValue(op.dateArg, doc)
	if err != nil {
		return nil, err
	}

	if isNullish(dv) {
		return types.Null, nil
	}

	t, ok := toTime(dv)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			fmt.Sprintf("$dateTrunc 'date' must be a date, got %T", dv))
	}

	uv, err := evalArgValue(op.unitArg, doc)
	if err != nil {
		return nil, err
	}

	unit, ok := uv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			"$dateTrunc 'unit' must be a string")
	}

	binSize := int64(1)
	if op.binSizeArg != nil {
		bsv, err := evalArgValue(op.binSizeArg, doc)
		if err != nil {
			return nil, err
		}

		if !isNullish(bsv) {
			binSize = int64(toFloat64(bsv))
		}
	}

	var truncated time.Time

	switch strings.ToLower(unit) {
	case "year":
		year := int64(t.Year())
		year = (year / binSize) * binSize
		truncated = time.Date(int(year), 1, 1, 0, 0, 0, 0, time.UTC)
	case "quarter":
		month := int(t.Month())
		quarterStart := ((month - 1) / 3) * 3 + 1
		truncated = time.Date(t.Year(), time.Month(quarterStart), 1, 0, 0, 0, 0, time.UTC)
	case "month":
		month := int64(t.Month()) + int64(t.Year()-1)*12
		month = (month/binSize)*binSize + 1
		year := (month - 1) / 12
		m := (month-1)%12 + 1
		truncated = time.Date(int(year), time.Month(m), 1, 0, 0, 0, 0, time.UTC)
	case "week":
		weekday := int(t.Weekday())
		if weekday == 0 {
			weekday = 7 // Sunday -> 7 for ISO week
		}
		truncated = time.Date(t.Year(), t.Month(), t.Day()-weekday+1, 0, 0, 0, 0, time.UTC)
	case "day":
		day := t.Unix() / 86400
		day = (day / binSize) * binSize
		truncated = time.Unix(day*86400, 0).UTC()
	case "hour":
		hours := t.Unix() / 3600
		hours = (hours / binSize) * binSize
		truncated = time.Unix(hours*3600, 0).UTC()
	case "minute":
		minutes := t.Unix() / 60
		minutes = (minutes / binSize) * binSize
		truncated = time.Unix(minutes*60, 0).UTC()
	case "second":
		seconds := t.Unix()
		seconds = (seconds / binSize) * binSize
		truncated = time.Unix(seconds, 0).UTC()
	case "millisecond":
		ms := t.UnixMilli()
		ms = (ms / binSize) * binSize
		truncated = time.UnixMilli(ms).UTC()
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateTrunc",
			fmt.Sprintf("$dateTrunc unknown unit: %s", unit))
	}

	return truncated, nil
}

var _ Operator = (*dateTruncOp)(nil)
