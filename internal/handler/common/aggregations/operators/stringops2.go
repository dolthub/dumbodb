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
	"unicode/utf8"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

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

	if isNullish(v) {
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

	if isNullish(v) {
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

// -- $substr (alias for $substrBytes) -----------------------------------------

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
	startV, err := evalArgValue(op.startArg, doc)
	if err != nil {
		return nil, err
	}
	lenV, err := evalArgValue(op.lengthArg, doc)
	if err != nil {
		return nil, err
	}

	s, err := coerceToString(sv)
	if err != nil {
		return nil, err
	}
	return substrBytes("$substrBytes", s, startV, lenV)
}

var _ Operator = (*substrOp)(nil)

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
	startV, err := evalArgValue(op.startArg, doc)
	if err != nil {
		return nil, err
	}
	lenV, err := evalArgValue(op.lengthArg, doc)
	if err != nil {
		return nil, err
	}
	s, err := coerceToString(sv)
	if err != nil {
		return nil, err
	}

	if !isNumber(startV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPStartNotNumeric,
			"$substrCP: starting index must be a numeric type (is BSON type "+handlerparams.AliasFromType(startV)+")", "$substrCP")
	}
	start, ok := integralInt(startV)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPStartNotInt,
			"$substrCP: starting index cannot be represented as a 32-bit integral value: "+scalarString(startV), "$substrCP")
	}
	if !isNumber(lenV) {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPLengthNotNumeric,
			"$substrCP: length must be a numeric type (is BSON type "+handlerparams.AliasFromType(lenV)+")", "$substrCP")
	}
	length, ok := integralInt(lenV)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPLengthNotInt,
			"$substrCP: length cannot be represented as a 32-bit integral value: "+scalarString(lenV), "$substrCP")
	}
	if length < 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPLengthNegative,
			"$substrCP: length must be a nonnegative integer.", "$substrCP")
	}
	if start < 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPStartNegative,
			"$substrCP: the starting index must be nonnegative integer.", "$substrCP")
	}

	invalid := handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSubstrCPInvalidUTF8,
		"$substrCP: invalid UTF-8 string", "$substrCP")
	startByte := 0
	for range start {
		if startByte >= len(s) {
			return "", nil
		}
		if isUTF8Continuation(s[startByte]) || utf8CodePointLength(s[startByte]) > 4 {
			return nil, invalid
		}
		startByte += utf8CodePointLength(s[startByte])
	}
	endByte := startByte
	for i := 0; i < length && endByte < len(s); i++ {
		if isUTF8Continuation(s[endByte]) || utf8CodePointLength(s[endByte]) > 4 {
			return nil, invalid
		}
		endByte += utf8CodePointLength(s[endByte])
	}
	return s[startByte:min(endByte, len(s))], nil
}

var _ Operator = (*substrCPOp)(nil)

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

	if isNullish(sv) {
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

		if !isNullish(cv) {
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

	if isNullish(sv) {
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

	if isNullish(dv) {
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
	if isNullish(sv) {
		return types.Null, nil
	}
	s, ok := sv.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOfBytesNotString,
			"$indexOfBytes requires a string as the first argument, found: "+handlerparams.AliasFromType(sv), "$indexOfBytes")
	}
	tokenV, err := evalArgValue(op.substringArg, doc)
	if err != nil {
		return nil, err
	}
	token, ok := tokenV.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOfBytesTokenString,
			"$indexOfBytes requires a string as the second argument, found: "+handlerparams.AliasFromType(tokenV), "$indexOfBytes")
	}

	start, end := 0, len(s)
	if op.startArg != nil {
		stv, err := evalArgValue(op.startArg, doc)
		if err != nil {
			return nil, err
		}
		if start, err = requireIndex(stv, "$indexOfBytes", "starting index"); err != nil {
			return nil, err
		}
	}
	if op.endArg != nil {
		ev, err := evalArgValue(op.endArg, doc)
		if err != nil {
			return nil, err
		}
		if end, err = requireIndex(ev, "$indexOfBytes", "ending index"); err != nil {
			return nil, err
		}
		end = min(end, len(s))
	}

	if start > len(s) || end < start {
		return int32(-1), nil
	}
	idx := strings.Index(s[start:end], token)
	if idx < 0 {
		return int32(-1), nil
	}
	return int32(start + idx), nil
}

var _ Operator = (*indexOfBytesOp)(nil)

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
	if isNullish(sv) {
		return types.Null, nil
	}
	s, ok := sv.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOfCPNotString,
			"$indexOfCP requires a string as the first argument, found: "+handlerparams.AliasFromType(sv), "$indexOfCP")
	}
	tokenV, err := evalArgValue(op.substringArg, doc)
	if err != nil {
		return nil, err
	}
	token, ok := tokenV.(string)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOfCPTokenString,
			"$indexOfCP requires a string as the second argument, found: "+handlerparams.AliasFromType(tokenV), "$indexOfCP")
	}

	start := 0
	if op.startArg != nil {
		stv, err := evalArgValue(op.startArg, doc)
		if err != nil {
			return nil, err
		}
		if start, err = requireIndex(stv, "$indexOfCP", "starting index"); err != nil {
			return nil, err
		}
	}

	// Count code points, noting the byte where the search starts; past the
	// end it stays 0, as in mongod.
	codePoints, startByte := 0, 0
	for b := 0; b < len(s); codePoints++ {
		if codePoints == start {
			startByte = b
		}
		if isUTF8Continuation(s[b]) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIndexOfCPBadUTF8,
				"$indexOfCP found bad UTF-8 in the input", "$indexOfCP")
		}
		b += utf8CodePointLength(s[b])
	}

	end := codePoints
	if op.endArg != nil {
		ev, err := evalArgValue(op.endArg, doc)
		if err != nil {
			return nil, err
		}
		if end, err = requireIndex(ev, "$indexOfCP", "ending index"); err != nil {
			return nil, err
		}
		end = min(end, codePoints)
	}

	if end < start {
		return int32(-1), nil
	}
	if startByte == 0 && s == "" && token == "" {
		return int32(0), nil
	}
	for b, cp := startByte, start; cp < end; cp++ {
		if b+len(token) <= len(s) && s[b:b+len(token)] == token {
			return int32(cp), nil
		}
		b += utf8CodePointLength(s[b])
	}
	return int32(-1), nil
}

var _ Operator = (*indexOfCPOp)(nil)
