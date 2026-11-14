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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type switchBranch struct {
	caseExpr any
	thenExpr any
}

// switchOp represents { $switch: { branches: [ { case: <expr>, then: <expr> }, ... ], default: <expr> } }.
type switchOp struct {
	branches   []switchBranch
	defaultArg any
}

func newSwitch(args ...any) (Operator, error) {
	var doc *types.Document
	if len(args) == 1 {
		doc, _ = args[0].(*types.Document)
	}
	if doc == nil {
		found := "array"
		if len(args) == 1 {
			found = handlerparams.AliasFromType(args[0])
		}
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchNotObject,
			"$switch requires an object as an argument, found: "+found, "$switch")
	}

	op := new(switchOp)
	for _, k := range doc.Keys() {
		v := must.NotFail(doc.Get(k))
		switch k {
		case "branches":
			arr, ok := v.(*types.Array)
			if !ok {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchBranchesNotArray,
					"$switch expected an array for 'branches', found: "+handlerparams.AliasFromType(v), "$switch")
			}
			for i := range arr.Len() {
				branch, err := parseSwitchBranch(must.NotFail(arr.Get(i)))
				if err != nil {
					return nil, err
				}
				op.branches = append(op.branches, branch)
			}
		case "default":
			op.defaultArg = v
		default:
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchUnknownArgument,
				"$switch found an unknown argument: "+k, "$switch")
		}
	}

	if len(op.branches) == 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchNoBranches,
			"$switch requires at least one branch", "$switch")
	}

	return op, nil
}

func parseSwitchBranch(v any) (switchBranch, error) {
	doc, ok := v.(*types.Document)
	if !ok {
		return switchBranch{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchBranchNotObject,
			"$switch expected each branch to be an object, found: "+handlerparams.AliasFromType(v), "$switch")
	}

	var branch switchBranch
	var hasCase, hasThen bool
	for _, k := range doc.Keys() {
		switch k {
		case "case":
			branch.caseExpr, hasCase = must.NotFail(doc.Get(k)), true
		case "then":
			branch.thenExpr, hasThen = must.NotFail(doc.Get(k)), true
		default:
			return switchBranch{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchBranchUnknownArgument,
				"$switch found an unknown argument to a branch: "+k, "$switch")
		}
	}

	switch {
	case !hasCase:
		return switchBranch{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchBranchMissingCase,
			"$switch requires each branch have a 'case' expression", "$switch")
	case !hasThen:
		return switchBranch{}, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrSwitchBranchMissingThen,
			"$switch requires each branch have a 'then' expression.", "$switch")
	}

	return branch, nil
}

func (op *switchOp) Process(doc *types.Document) (any, error) {
	for _, branch := range op.branches {
		caseResult, err := evalArgValue(branch.caseExpr, doc)
		if err != nil {
			return nil, err
		}

		if !isFalsy(caseResult) {
			return evalBranch(branch.thenExpr, doc)
		}
	}

	if op.defaultArg != nil {
		return evalBranch(op.defaultArg, doc)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBucketNoBranch,
		"$switch could not find a matching branch for an input, and no default was specified.", "$switch")
}

var _ Operator = (*switchOp)(nil)
