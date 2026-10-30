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
	"errors"
	"fmt"
	"strings"

	"github.com/dlclark/regexp2"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
	"github.com/dolthub/dumbodb/internal/util/pcre"
)

// regexOp represents $regexFind, $regexFindAll and $regexMatch:
// { <op>: { input: <expr>, regex: <expr>, options: <expr> } }.
type regexOp struct {
	opName                string
	input, regex, options any
	hasOptions            bool

	// constant is set when regex and options are constants, which MongoDB
	// validates and compiles up front.
	constant *regexState
}

// regexState is ExpressionRegex::RegexExecutionState.
type regexState struct {
	input     []rune
	hasInput  bool
	pattern   *string
	options   *string
	re        *regexp2.Regexp
	startRune int
}

func (s *regexState) nullish() bool {
	return !s.hasInput || s.pattern == nil
}

// newRegex follows CommonRegexParse and ExpressionRegex::optimize.
func newRegex(opName string) func(args ...any) (Operator, error) {
	return func(args ...any) (Operator, error) {
		spec, ok := args[0].(*types.Document)
		if len(args) != 1 || !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexNotObject,
				fmt.Sprintf("%s expects an object of named arguments but found: %s", opName, handlerparams.AliasFromType(args[0])), opName)
		}

		op := &regexOp{opName: opName}
		var hasInput, hasRegex bool
		iter := spec.Iterator()
		defer iter.Close()
		for {
			key, value, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			if err != nil {
				return nil, err
			}
			switch key {
			case "input":
				op.input, hasInput = value, true
			case "regex":
				op.regex, hasRegex = value, true
			case "options":
				op.options, op.hasOptions = value, true
			default:
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexUnknownField,
					fmt.Sprintf("%s found an unknown argument: %s", opName, key), opName)
			}
		}
		if !hasInput {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexNoInput,
				opName+" requires 'input' parameter", opName)
		}
		if !hasRegex {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexNoRegex,
				opName+" requires 'regex' parameter", opName)
		}

		if isConstantOperand(op.regex) && (!op.hasOptions || isConstantOperand(op.options)) {
			options := any(types.Null)
			if op.hasOptions {
				options = op.options
			}
			op.constant = new(regexState)
			if err := op.extractRegexAndOptions(op.constant, op.regex, options); err != nil {
				return nil, err
			}
			if err := op.compile(op.constant); err != nil {
				return nil, err
			}
		}
		return op, nil
	}
}

// buildState is ExpressionRegex::buildInitialState.
func (op *regexOp) buildState(doc *types.Document) (*regexState, error) {
	input, err := evalArgValue(op.input, doc)
	if err != nil {
		return nil, err
	}
	regex, err := evalArgValue(op.regex, doc)
	if err != nil {
		return nil, err
	}
	options := any(types.Null)
	if op.hasOptions {
		if options, err = evalArgValue(op.options, doc); err != nil {
			return nil, err
		}
	}

	state := new(regexState)
	if op.constant != nil {
		*state = *op.constant
	}

	switch input := input.(type) {
	case string:
		state.input, state.hasInput = []rune(input), true
	default:
		if !isNullish(input) {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexInputType,
				op.opName+" needs 'input' to be of type string", op.opName)
		}
	}

	if op.constant == nil {
		if err = op.extractRegexAndOptions(state, regex, options); err != nil {
			return nil, err
		}
		if err = op.compile(state); err != nil {
			return nil, err
		}
	}
	return state, nil
}

// extractRegexAndOptions is ExpressionRegex::_extractRegexAndOptions.
func (op *regexOp) extractRegexAndOptions(state *regexState, regex, options any) error {
	if _, isRegex := regex.(types.Regex); !isRegex && !isNullish(regex) {
		if _, isString := regex.(string); !isString {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexType,
				op.opName+" needs 'regex' to be of type string or regex", op.opName)
		}
	}
	if _, isString := options.(string); !isString && !isNullish(options) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexOptionsType,
			op.opName+" needs 'options' to be of type string", op.opName)
	}

	switch regex := regex.(type) {
	case types.Regex:
		if regex.Options != "" && !isNullish(options) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexOptionsTwice,
				op.opName+": found regex option(s) specified in both 'regex' and 'option' fields", op.opName)
		}
		state.pattern = &regex.Pattern
		if regex.Options != "" {
			state.options = &regex.Options
		}
	case string:
		state.pattern = &regex
	}
	if s, ok := options.(string); ok {
		state.options = &s
	}

	if state.pattern != nil && strings.ContainsRune(*state.pattern, 0) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexPatternNull,
			op.opName+": regular expression cannot contain an embedded null byte", op.opName)
	}
	if state.options != nil && strings.ContainsRune(*state.options, 0) {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexOptionsNull,
			op.opName+": regular expression options cannot contain an embedded null byte", op.opName)
	}
	return nil
}

// compile is ExpressionRegex::_compile with pcre_util::flagsToOptions.
func (op *regexOp) compile(state *regexState) error {
	if state.pattern == nil {
		return nil
	}

	var opts pcre.Options
	if state.options != nil {
		for _, flag := range *state.options {
			switch flag {
			case 'i':
				opts.CaseInsensitive = true
			case 'm':
				opts.Multiline = true
			case 's':
				opts.DotAll = true
			case 'x':
				opts.Extended = true
			case 'u':
			default:
				return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadRegexOption,
					fmt.Sprintf("%s invalid flag in regex options: %c", op.opName, flag), op.opName)
			}
		}
	}

	re, err := pcre.Compile(*state.pattern, opts)
	if err != nil {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexInvalid,
			fmt.Sprintf("Invalid Regex in %s: %s", op.opName, err), op.opName)
	}
	state.re = re
	return nil
}

// execute is ExpressionRegex::execute.
func (op *regexOp) execute(state *regexState) (*regexp2.Match, error) {
	m, err := state.re.FindRunesMatchStartingAt(state.input, state.startRune)
	if err != nil {
		// regexp2 fails only on its match timeout, MongoDB's PCRE2 match
		// limit (-47); its message would include the whole input.
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRegexExecution,
			fmt.Sprintf("Error occurred while executing the regular expression in %s. Result code: -47", op.opName), op.opName)
	}
	return m, nil
}

// nextMatch is ExpressionRegex::nextMatch; it returns nil when nothing
// matches.
func (op *regexOp) nextMatch(state *regexState) (*types.Document, string, error) {
	m, err := op.execute(state)
	if err != nil || m == nil {
		return nil, "", err
	}
	state.startRune = m.Index

	groups := m.Groups()
	captures := types.MakeArray(len(groups) - 1)
	for _, g := range groups[1:] {
		if len(g.Captures) == 0 {
			captures.Append(types.Null)
		} else {
			captures.Append(g.String())
		}
	}

	match := m.String()
	return must.NotFail(types.NewDocument("match", match, "idx", int32(m.Index), "captures", captures)), match, nil
}

// Process follows ExpressionRegexFind, ExpressionRegexFindAll and
// ExpressionRegexMatch::evaluate.
func (op *regexOp) Process(doc *types.Document) (any, error) {
	state, err := op.buildState(doc)
	if err != nil {
		return nil, err
	}

	switch op.opName {
	case "$regexMatch":
		if state.nullish() {
			return false, nil
		}
		m, err := op.execute(state)
		return m != nil, err

	case "$regexFind":
		if state.nullish() {
			return types.Null, nil
		}
		match, _, err := op.nextMatch(state)
		if err != nil || match == nil {
			return types.Null, err
		}
		return match, nil
	}

	output := types.MakeArray(0)
	if state.nullish() {
		return output, nil
	}
	for {
		match, matched, err := op.nextMatch(state)
		if err != nil {
			return nil, err
		}
		if match == nil {
			break
		}
		output.Append(match)

		// An empty match moves on one code point; otherwise matches do not
		// overlap.
		if matched == "" {
			if state.startRune >= len(state.input) {
				break
			}
			state.startRune++
		} else {
			state.startRune += len([]rune(matched))
		}
		if state.startRune >= len(state.input) {
			break
		}
	}
	return output, nil
}

var _ Operator = (*regexOp)(nil)
