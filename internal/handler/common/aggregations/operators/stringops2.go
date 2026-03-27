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
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/dolthub/dongo/internal/types"
)

// ── $strLenBytes ─────────────────────────────────────────────────────────────

type strLenBytesOp struct{ arg any }

func newStrLenBytes(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$strLenBytes",
			fmt.Sprintf("Expression $strLenBytes takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &strLenBytesOp{arg: args[0]}, nil
}

func (op *strLenBytesOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	s, ok := v.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$strLenBytes",
			fmt.Sprintf("$strLenBytes requires a string, got %T", v))
	}

	return int32(len(s)), nil
}

var _ Operator = (*strLenBytesOp)(nil)

// ── $strLenCP ─────────────────────────────────────────────────────────────────

type strLenCPOp struct{ arg any }

func newStrLenCP(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$strLenCP",
			fmt.Sprintf("Expression $strLenCP takes exactly 1 argument. %d were passed in.", len(args)))
	}

	return &strLenCPOp{arg: args[0]}, nil
}

func (op *strLenCPOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == types.Null {
		return types.Null, nil
	}

	s, ok := v.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$strLenCP",
			fmt.Sprintf("$strLenCP requires a string, got %T", v))
	}

	return int32(utf8.RuneCountInString(s)), nil
}

var _ Operator = (*strLenCPOp)(nil)

// ── $substr (alias for $substrBytes) ─────────────────────────────────────────

// substrOp represents { $substr: [ <string>, <start>, <length> ] }.
type substrOp struct {
	strArg    any
	startArg  any
	lengthArg any
}

func newSubstr(args ...any) (Operator, error) {
	if len(args) != 3 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$substr",
			fmt.Sprintf("Expression $substr takes exactly 3 arguments. %d were passed in.", len(args)))
	}

	return &substrOp{strArg: args[0], startArg: args[1], lengthArg: args[2]}, nil
}

func (op *substrOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.strArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return "", nil
	}

	s, ok := sv.(string)
	if !ok {
		return "", nil
	}

	startV, err := evalArgValue(op.startArg, doc)
	if err != nil {
		return nil, err
	}

	lenV, err := evalArgValue(op.lengthArg, doc)
	if err != nil {
		return nil, err
	}

	start := int(toFloat64(startV))
	length := int(toFloat64(lenV))

	bytes := []byte(s)
	n := len(bytes)

	if start < 0 {
		start = 0
	}

	if start >= n {
		return "", nil
	}

	if length < 0 {
		return string(bytes[start:]), nil
	}

	end := start + length
	if end > n {
		end = n
	}

	return string(bytes[start:end]), nil
}

var _ Operator = (*substrOp)(nil)

// ── $substrCP ─────────────────────────────────────────────────────────────────

// substrCPOp represents { $substrCP: [ <string>, <start>, <length> ] } (Unicode code points).
type substrCPOp struct {
	strArg    any
	startArg  any
	lengthArg any
}

func newSubstrCP(args ...any) (Operator, error) {
	if len(args) != 3 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$substrCP",
			fmt.Sprintf("Expression $substrCP takes exactly 3 arguments. %d were passed in.", len(args)))
	}

	return &substrCPOp{strArg: args[0], startArg: args[1], lengthArg: args[2]}, nil
}

func (op *substrCPOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.strArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return "", nil
	}

	s, ok := sv.(string)
	if !ok {
		return "", nil
	}

	startV, err := evalArgValue(op.startArg, doc)
	if err != nil {
		return nil, err
	}

	lenV, err := evalArgValue(op.lengthArg, doc)
	if err != nil {
		return nil, err
	}

	start := int(toFloat64(startV))
	length := int(toFloat64(lenV))

	runes := []rune(s)
	n := len(runes)

	if start < 0 {
		start = 0
	}

	if start >= n {
		return "", nil
	}

	if length < 0 {
		return string(runes[start:]), nil
	}

	end := start + length
	if end > n {
		end = n
	}

	return string(runes[start:end]), nil
}

var _ Operator = (*substrCPOp)(nil)

// ── $trim ─────────────────────────────────────────────────────────────────────

// trimOp represents { $trim: { input: <expr>, chars: <chars-expr> } }.
type trimOp struct {
	inputArg any
	charsArg any
	trimLeft bool
	trimRight bool
}

func newTrim(args ...any) (Operator, error) {
	return newTrimOp("$trim", true, true, args...)
}

func newLtrim(args ...any) (Operator, error) {
	return newTrimOp("$ltrim", true, false, args...)
}

func newRtrim(args ...any) (Operator, error) {
	return newTrimOp("$rtrim", false, true, args...)
}

func newTrimOp(name string, left, right bool, args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, name,
			fmt.Sprintf("Expression %s takes exactly 1 argument. %d were passed in.", name, len(args)))
	}

	// Handle both document form and direct string form.
	var inputArg any
	var charsArg any

	switch v := args[0].(type) {
	case *types.Document:
		input, err := v.Get("input")
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, name,
				fmt.Sprintf("Missing 'input' parameter to %s", name))
		}

		inputArg = input

		if charsV, err := v.Get("chars"); err == nil {
			charsArg = charsV
		}
	default:
		inputArg = args[0]
	}

	return &trimOp{inputArg: inputArg, charsArg: charsArg, trimLeft: left, trimRight: right}, nil
}

func (op *trimOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.inputArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return types.Null, nil
	}

	s, ok := sv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$trim",
			fmt.Sprintf("$trim requires a string, got %T", sv))
	}

	cutset := " \t\n\r" // default whitespace

	if op.charsArg != nil {
		cv, err := evalArgValue(op.charsArg, doc)
		if err != nil {
			return nil, err
		}

		if cv != types.Null {
			if cs, ok := cv.(string); ok {
				cutset = cs
			}
		}
	}

	if op.trimLeft && op.trimRight {
		return strings.Trim(s, cutset), nil
	} else if op.trimLeft {
		return strings.TrimLeft(s, cutset), nil
	}

	return strings.TrimRight(s, cutset), nil
}

var _ Operator = (*trimOp)(nil)

// ── $split ────────────────────────────────────────────────────────────────────

// splitOp represents { $split: [ <string-expr>, <delimiter-expr> ] }.
type splitOp struct {
	strArg       any
	delimiterArg any
}

func newSplit(args ...any) (Operator, error) {
	if len(args) != 2 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$split",
			fmt.Sprintf("Expression $split takes exactly 2 arguments. %d were passed in.", len(args)))
	}

	return &splitOp{strArg: args[0], delimiterArg: args[1]}, nil
}

func (op *splitOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.strArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return types.Null, nil
	}

	s, ok := sv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$split",
			fmt.Sprintf("$split requires a string, got %T", sv))
	}

	dv, err := evalArgValue(op.delimiterArg, doc)
	if err != nil {
		return nil, err
	}

	if dv == types.Null {
		return types.Null, nil
	}

	delimiter, ok := dv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$split",
			fmt.Sprintf("$split delimiter must be a string, got %T", dv))
	}

	parts := strings.Split(s, delimiter)
	arr := types.MakeArray(len(parts))

	for _, p := range parts {
		arr.Append(p)
	}

	return arr, nil
}

var _ Operator = (*splitOp)(nil)

// ── $indexOfBytes ─────────────────────────────────────────────────────────────

// indexOfBytesOp represents { $indexOfBytes: [ <string>, <substring>, <start>, <end> ] }.
type indexOfBytesOp struct {
	strArg       any
	substringArg any
	startArg     any
	endArg       any
}

func newIndexOfBytes(args ...any) (Operator, error) {
	switch len(args) {
	case 2:
		return &indexOfBytesOp{strArg: args[0], substringArg: args[1]}, nil
	case 3:
		return &indexOfBytesOp{strArg: args[0], substringArg: args[1], startArg: args[2]}, nil
	case 4:
		return &indexOfBytesOp{strArg: args[0], substringArg: args[1], startArg: args[2], endArg: args[3]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$indexOfBytes",
			fmt.Sprintf("Expression $indexOfBytes takes 2, 3, or 4 arguments. %d were passed in.", len(args)))
	}
}

func (op *indexOfBytesOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.strArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return types.Null, nil
	}

	s, ok := sv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$indexOfBytes",
			fmt.Sprintf("$indexOfBytes requires a string, got %T", sv))
	}

	subV, err := evalArgValue(op.substringArg, doc)
	if err != nil {
		return nil, err
	}

	sub, ok := subV.(string)
	if !ok {
		return int32(-1), nil
	}

	bytes := []byte(s)
	n := len(bytes)
	start := 0
	end := n

	if op.startArg != nil {
		stv, err := evalArgValue(op.startArg, doc)
		if err != nil {
			return nil, err
		}

		if stv != types.Null {
			start = int(toFloat64(stv))
		}
	}

	if op.endArg != nil {
		ev, err := evalArgValue(op.endArg, doc)
		if err != nil {
			return nil, err
		}

		if ev != types.Null {
			end = int(toFloat64(ev))
		}
	}

	if start < 0 {
		start = 0
	}

	if end > n {
		end = n
	}

	if start >= end {
		return int32(-1), nil
	}

	idx := strings.Index(string(bytes[start:end]), sub)
	if idx < 0 {
		return int32(-1), nil
	}

	return int32(start + idx), nil
}

var _ Operator = (*indexOfBytesOp)(nil)

// ── $indexOfCP ────────────────────────────────────────────────────────────────

// indexOfCPOp represents { $indexOfCP: [ <string>, <substring>, <start>, <end> ] } (code points).
type indexOfCPOp struct {
	strArg       any
	substringArg any
	startArg     any
	endArg       any
}

func newIndexOfCP(args ...any) (Operator, error) {
	switch len(args) {
	case 2:
		return &indexOfCPOp{strArg: args[0], substringArg: args[1]}, nil
	case 3:
		return &indexOfCPOp{strArg: args[0], substringArg: args[1], startArg: args[2]}, nil
	case 4:
		return &indexOfCPOp{strArg: args[0], substringArg: args[1], startArg: args[2], endArg: args[3]}, nil
	default:
		return nil, newOperatorError(ErrArgsInvalidLen, "$indexOfCP",
			fmt.Sprintf("Expression $indexOfCP takes 2, 3, or 4 arguments. %d were passed in.", len(args)))
	}
}

func (op *indexOfCPOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.strArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return types.Null, nil
	}

	s, ok := sv.(string)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$indexOfCP",
			fmt.Sprintf("$indexOfCP requires a string, got %T", sv))
	}

	subV, err := evalArgValue(op.substringArg, doc)
	if err != nil {
		return nil, err
	}

	sub, ok := subV.(string)
	if !ok {
		return int32(-1), nil
	}

	runes := []rune(s)
	n := len(runes)
	start := 0
	end := n

	if op.startArg != nil {
		stv, err := evalArgValue(op.startArg, doc)
		if err != nil {
			return nil, err
		}

		if stv != types.Null {
			start = int(toFloat64(stv))
		}
	}

	if op.endArg != nil {
		ev, err := evalArgValue(op.endArg, doc)
		if err != nil {
			return nil, err
		}

		if ev != types.Null {
			end = int(toFloat64(ev))
		}
	}

	if start < 0 {
		start = 0
	}

	if end > n {
		end = n
	}

	if start >= end {
		return int32(-1), nil
	}

	subStr := string([]rune(sub))
	haystack := string(runes[start:end])
	idx := strings.Index(haystack, subStr)

	if idx < 0 {
		return int32(-1), nil
	}

	// Convert byte index back to rune index.
	return int32(start + utf8.RuneCountInString(haystack[:idx])), nil
}

var _ Operator = (*indexOfCPOp)(nil)

// ── $regexMatch ───────────────────────────────────────────────────────────────

// regexMatchOp represents { $regexMatch: { input: <expr>, regex: <regex>, options: <opts> } }.
type regexMatchOp struct {
	inputArg   any
	regexArg   any
	optionsArg any
}

func newRegexMatch(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexMatch",
			"$regexMatch requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexMatch",
			"$regexMatch requires a document argument")
	}

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexMatch",
			"Missing 'input' parameter to $regexMatch")
	}

	regexArg, err := doc.Get("regex")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexMatch",
			"Missing 'regex' parameter to $regexMatch")
	}

	var optionsArg any
	if optV, err := doc.Get("options"); err == nil {
		optionsArg = optV
	}

	return &regexMatchOp{inputArg: inputArg, regexArg: regexArg, optionsArg: optionsArg}, nil
}

func compileRegex(regexArg, optionsArg any, doc *types.Document) (*regexp.Regexp, error) {
	rv, err := evalArgValue(regexArg, doc)
	if err != nil {
		return nil, err
	}

	pattern, ok := rv.(string)
	if !ok {
		return nil, fmt.Errorf("regex must be a string")
	}

	opts := ""
	if optionsArg != nil {
		ov, err := evalArgValue(optionsArg, doc)
		if err != nil {
			return nil, err
		}

		if os, ok := ov.(string); ok {
			opts = os
		}
	}

	// Convert MongoDB regex options to Go RE2 flags.
	flags := "(?s)"
	if strings.Contains(opts, "i") {
		flags += "(?i)"
	}

	if strings.Contains(opts, "x") {
		// extended — strip whitespace/comments (not supported in RE2, skip)
	}

	if strings.Contains(opts, "m") {
		flags += "(?m)"
	}

	re, err := regexp.Compile(flags + pattern)
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexMatch",
			fmt.Sprintf("Invalid regex: %s", err))
	}

	return re, nil
}

func (op *regexMatchOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.inputArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return false, nil
	}

	s, ok := sv.(string)
	if !ok {
		return false, nil
	}

	re, err := compileRegex(op.regexArg, op.optionsArg, doc)
	if err != nil {
		return nil, err
	}

	return re.MatchString(s), nil
}

var _ Operator = (*regexMatchOp)(nil)

// ── $regexFind ────────────────────────────────────────────────────────────────

// regexFindOp represents { $regexFind: { input: <expr>, regex: <regex>, options: <opts> } }.
type regexFindOp struct {
	inputArg   any
	regexArg   any
	optionsArg any
}

func newRegexFind(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexFind",
			"$regexFind requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexFind",
			"$regexFind requires a document argument")
	}

	inputArg, err := doc.Get("input")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexFind",
			"Missing 'input' parameter to $regexFind")
	}

	regexArg, err := doc.Get("regex")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$regexFind",
			"Missing 'regex' parameter to $regexFind")
	}

	var optionsArg any
	if optV, err := doc.Get("options"); err == nil {
		optionsArg = optV
	}

	return &regexFindOp{inputArg: inputArg, regexArg: regexArg, optionsArg: optionsArg}, nil
}

func (op *regexFindOp) Process(doc *types.Document) (any, error) {
	sv, err := evalArgValue(op.inputArg, doc)
	if err != nil {
		return nil, err
	}

	if sv == types.Null {
		return types.Null, nil
	}

	s, ok := sv.(string)
	if !ok {
		return types.Null, nil
	}

	re, err := compileRegex(op.regexArg, op.optionsArg, doc)
	if err != nil {
		return nil, err
	}

	loc := re.FindStringSubmatchIndex(s)
	if loc == nil {
		return types.Null, nil
	}

	match := s[loc[0]:loc[1]]

	// Build captures array.
	captures := types.MakeArray(0)

	for i := 2; i < len(loc); i += 2 {
		if loc[i] < 0 {
			captures.Append(types.Null)
		} else {
			captures.Append(s[loc[i]:loc[i+1]])
		}
	}

	result, err := types.NewDocument(
		"match", match,
		"idx", int32(utf8.RuneCountInString(s[:loc[0]])),
		"captures", captures,
	)
	if err != nil {
		return nil, err
	}

	return result, nil
}

var _ Operator = (*regexFindOp)(nil)
