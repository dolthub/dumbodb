// Copyright 2021 FerretDB Inc.
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
