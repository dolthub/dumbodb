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

	"github.com/dolthub/dongo/internal/types"
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

// ── date part operators ($year, $month, $dayOfMonth, etc.) ──────────────────

// datePartOp is a generic date component extractor.
type datePartOp struct {
	name string
	arg  any
}

// newDatePartOp returns a constructor for the given date part operator.
func newDatePartOp(name string) func(args ...any) (Operator, error) {
	return func(args ...any) (Operator, error) {
		if len(args) != 1 {
			return nil, newOperatorError(ErrArgsInvalidLen, name,
				fmt.Sprintf("Expression %s takes exactly 1 argument. %d were passed in.", name, len(args)))
		}

		return &datePartOp{name: name, arg: args[0]}, nil
	}
}

func (op *datePartOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	t, ok := toTime(v)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, op.name,
			fmt.Sprintf("%s requires a date, got %T", op.name, v))
	}

	switch op.name {
	case "$year":
		return int32(t.Year()), nil
	case "$month":
		return int32(t.Month()), nil
	case "$dayOfMonth":
		return int32(t.Day()), nil
	case "$hour":
		return int32(t.Hour()), nil
	case "$minute":
		return int32(t.Minute()), nil
	case "$second":
		return int32(t.Second()), nil
	case "$millisecond":
		return int32(t.Nanosecond() / 1e6), nil
	case "$dayOfWeek":
		// MongoDB: 1=Sunday ... 7=Saturday
		return int32(t.Weekday()) + 1, nil
	case "$dayOfYear":
		return int32(t.YearDay()), nil
	case "$week":
		// MongoDB $week: week of year (0-53), week starts Sunday
		_, week := t.ISOWeek()
		// Approximate: use ISOWeek then adjust; simpler is day-of-year / 7
		yday := t.YearDay()
		// adjust for day of week of Jan 1
		jan1 := time.Date(t.Year(), 1, 1, 0, 0, 0, 0, time.UTC)
		startDow := int(jan1.Weekday()) // 0=Sunday
		week = (yday + startDow - 1) / 7
		return int32(week), nil
	case "$isoWeek":
		_, week := t.ISOWeek()
		return int32(week), nil
	case "$isoWeekYear":
		year, _ := t.ISOWeek()
		return int32(year), nil
	case "$isoDayOfWeek":
		// ISO: 1=Monday ... 7=Sunday
		dow := int(t.Weekday())
		if dow == 0 {
			dow = 7
		}

		return int32(dow), nil
	}

	return types.Null, nil
}

var _ Operator = (*datePartOp)(nil)

// ── $dateToString ─────────────────────────────────────────────────────────────

// dateToStringOp represents { $dateToString: { date: <expr>, format: <format>, timezone: <tz>, onNull: <val> } }.
type dateToStringOp struct {
	dateArg   any
	format    string
	onNullArg any
}

func newDateToString(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateToString",
			"$dateToString requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateToString",
			"$dateToString requires a document argument")
	}

	dateArg, err := doc.Get("date")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateToString",
			"Missing 'date' parameter to $dateToString")
	}

	format := "%Y-%m-%dT%H:%M:%S.%LZ" // default ISO format
	if fv, err := doc.Get("format"); err == nil {
		if s, ok := fv.(string); ok {
			format = s
		}
	}

	var onNullArg any
	if onNullV, err := doc.Get("onNull"); err == nil {
		onNullArg = onNullV
	}

	return &dateToStringOp{dateArg: dateArg, format: format, onNullArg: onNullArg}, nil
}

func (op *dateToStringOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.dateArg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		if op.onNullArg != nil {
			return evalArgValue(op.onNullArg, doc)
		}

		return types.Null, nil
	}

	t, ok := toTime(v)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateToString",
			fmt.Sprintf("$dateToString requires a date, got %T", v))
	}

	return formatDate(op.format, t), nil
}

// formatDate converts a time.Time to a string using MongoDB's date format specifiers.
func formatDate(format string, t time.Time) string {
	replacements := map[string]string{
		"%Y": fmt.Sprintf("%04d", t.Year()),
		"%m": fmt.Sprintf("%02d", t.Month()),
		"%d": fmt.Sprintf("%02d", t.Day()),
		"%H": fmt.Sprintf("%02d", t.Hour()),
		"%M": fmt.Sprintf("%02d", t.Minute()),
		"%S": fmt.Sprintf("%02d", t.Second()),
		"%L": fmt.Sprintf("%03d", t.Nanosecond()/1e6),
		"%j": fmt.Sprintf("%03d", t.YearDay()),
		"%u": fmt.Sprintf("%d", func() int {
			d := int(t.Weekday())
			if d == 0 {
				d = 7
			}

			return d
		}()),
		"%w": fmt.Sprintf("%d", int(t.Weekday())+1),
		"%%": "%",
	}

	result := format
	for k, v := range replacements {
		result = strings.ReplaceAll(result, k, v)
	}

	return result
}

var _ Operator = (*dateToStringOp)(nil)

// ── $dateAdd ──────────────────────────────────────────────────────────────────

// dateAddOp represents { $dateAdd: { startDate: <expr>, unit: <string>, amount: <number> } }.
type dateAddOp struct {
	startDateArg any
	unitArg      any
	amountArg    any
}

func newDateAdd(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			"$dateAdd requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			"$dateAdd requires a document argument")
	}

	startDateArg, err := doc.Get("startDate")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			"Missing 'startDate' parameter to $dateAdd")
	}

	unitArg, err := doc.Get("unit")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			"Missing 'unit' parameter to $dateAdd")
	}

	amountArg, err := doc.Get("amount")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			"Missing 'amount' parameter to $dateAdd")
	}

	return &dateAddOp{startDateArg: startDateArg, unitArg: unitArg, amountArg: amountArg}, nil
}

func (op *dateAddOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.startDateArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return types.Null, nil
	}

	t, ok := toTime(sv)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			fmt.Sprintf("$dateAdd startDate must be a date, got %T", sv))
	}

	uv, err := evalArgValue(op.unitArg, doc)
	if err != nil {
		return nil, err
	}

	unit, ok := uv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			"$dateAdd unit must be a string")
	}

	av, err := evalArgValue(op.amountArg, doc)
	if err != nil {
		return nil, err
	}

	amount := toInt64(av)

	return addDateUnit(t, unit, amount)
}

// addDateUnit adds the given amount of the specified date unit to t.
func addDateUnit(t time.Time, unit string, amount int64) (time.Time, error) {
	switch strings.ToLower(unit) {
	case "year":
		return t.AddDate(int(amount), 0, 0), nil
	case "quarter":
		return t.AddDate(0, int(amount*3), 0), nil
	case "month":
		return t.AddDate(0, int(amount), 0), nil
	case "week":
		return t.Add(time.Duration(amount) * 7 * 24 * time.Hour), nil
	case "day":
		return t.AddDate(0, 0, int(amount)), nil
	case "hour":
		return t.Add(time.Duration(amount) * time.Hour), nil
	case "minute":
		return t.Add(time.Duration(amount) * time.Minute), nil
	case "second":
		return t.Add(time.Duration(amount) * time.Second), nil
	case "millisecond":
		return t.Add(time.Duration(amount) * time.Millisecond), nil
	default:
		return time.Time{}, newOperatorError(ErrArgsInvalidLen, "$dateAdd",
			fmt.Sprintf("unknown date unit: %s", unit))
	}
}

// toInt64 converts a numeric value to int64.
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

var _ Operator = (*dateAddOp)(nil)

// ── $dateDiff ─────────────────────────────────────────────────────────────────

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

	if sv == types.Null || ev == types.Null {
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

// diffDateUnit computes the integer difference between two dates in the specified unit.
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

// ── $dateTrunc ────────────────────────────────────────────────────────────────

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

	if dv == types.Null {
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

		if bsv != types.Null {
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
			weekday = 7 // Sunday → 7 for ISO week
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
