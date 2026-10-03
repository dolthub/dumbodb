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

package common

import (
	"testing"
	"weak"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestValidatedExpr_ReusedPerFilterDocument(t *testing.T) {
	exprFilter := func(value int32) *types.Document {
		return must.NotFail(types.NewDocument("$eq", must.NotFail(types.NewArray("$a", value))))
	}

	f1, f2 := exprFilter(1), exprFilter(2)
	op1, err := validatedExpr(f1)
	require.NoError(t, err)
	again, err := validatedExpr(f1)
	require.NoError(t, err)
	require.Same(t, op1, again)

	op2, err := validatedExpr(f2)
	require.NoError(t, err)
	require.NotSame(t, op1, op2)

	match, err := filterExprOperator(must.NotFail(types.NewDocument("a", int32(2))), f2)
	require.NoError(t, err)
	require.True(t, match)
	match, err = filterExprOperator(must.NotFail(types.NewDocument("a", int32(2))), f1)
	require.NoError(t, err)
	require.False(t, match)

	_, err = validatedExpr(must.NotFail(types.NewDocument("$noSuchOperator", int32(1))))
	require.Error(t, err)

	match, err = filterExprOperator(must.NotFail(types.NewDocument("a", int32(2))), true)
	require.NoError(t, err)
	require.True(t, match, "a non-document $expr value is evaluated without caching")
}

// The $expr case of filterOperator must hit the cache across documents.
func TestFilterOperatorExpr_ValidatesOncePerFilter(t *testing.T) {
	exprValue := must.NotFail(types.NewDocument("$eq", must.NotFail(types.NewArray("$a", int32(1)))))
	for i := int32(0); i < 3; i++ {
		_, err := filterOperator(must.NotFail(types.NewDocument("a", i)), "$expr", exprValue, nil)
		require.NoError(t, err)
	}
	op, ok := validatedExprs.Load(weak.Make(exprValue))
	require.True(t, ok, "the expression was not cached")
	again, err := validatedExpr(exprValue)
	require.NoError(t, err)
	require.Same(t, op, again)
}
