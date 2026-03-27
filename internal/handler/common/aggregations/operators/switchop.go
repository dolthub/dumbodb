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
	"errors"

	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
)

// switchBranch holds a case/then pair for $switch.
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
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "$switch requires a document argument")
	}

	doc, ok := args[0].(*types.Document)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "$switch requires a document argument")
	}

	branchesV, err := doc.Get("branches")
	if err != nil {
		return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "Missing 'branches' parameter to $switch")
	}

	branchesArr, ok := branchesV.(*types.Array)
	if !ok {
		return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "'branches' must be an array")
	}

	var branches []switchBranch

	iter := branchesArr.Iterator()
	defer iter.Close()

	for {
		_, v, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, err
		}

		branchDoc, ok := v.(*types.Document)
		if !ok {
			return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "each branch must be a document")
		}

		caseExpr, err := branchDoc.Get("case")
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "Missing 'case' in branch")
		}

		thenExpr, err := branchDoc.Get("then")
		if err != nil {
			return nil, newOperatorError(ErrArgsInvalidLen, "$switch", "Missing 'then' in branch")
		}

		branches = append(branches, switchBranch{caseExpr: caseExpr, thenExpr: thenExpr})
	}

	var defaultArg any
	if dv, err := doc.Get("default"); err == nil {
		defaultArg = dv
	}

	return &switchOp{branches: branches, defaultArg: defaultArg}, nil
}

func (op *switchOp) Process(doc *types.Document) (any, error) {
	for _, branch := range op.branches {
		caseResult, err := evalArgValue(branch.caseExpr, doc)
		if err != nil {
			return nil, err
		}

		if !isFalsy(caseResult) {
			return evalArgValue(branch.thenExpr, doc)
		}
	}

	if op.defaultArg != nil {
		return evalArgValue(op.defaultArg, doc)
	}

	return nil, newOperatorError(ErrArgsInvalidLen, "$switch",
		"$switch could not find a matching branch")
}

var _ Operator = (*switchOp)(nil)
