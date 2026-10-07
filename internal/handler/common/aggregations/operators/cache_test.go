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
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestCachedOperatorIsReusedPerDocument(t *testing.T) {
	spec := must.NotFail(types.NewDocument("$add", must.NotFail(types.NewArray("$a", int32(1)))))
	op1, err := cachedOperator(spec)
	require.NoError(t, err)
	op2, err := cachedOperator(spec)
	require.NoError(t, err)
	require.Same(t, op1, op2)

	other, err := cachedOperator(must.NotFail(types.NewDocument("$add", must.NotFail(types.NewArray("$a", int32(1))))))
	require.NoError(t, err)
	require.NotSame(t, op1, other)

	_, err = cachedOperator(must.NotFail(types.NewDocument("$noSuchOperator", int32(1))))
	require.Error(t, err)
}

func TestFieldExpressionIsReused(t *testing.T) {
	e1, err := fieldExpression("$a.b")
	require.NoError(t, err)
	e2, err := fieldExpression("$a.b")
	require.NoError(t, err)
	require.Same(t, e1, e2)
}

func TestReusedExprEvaluatesEachDocument(t *testing.T) {
	// {$expr: {$eq: [{$add: [{$multiply: ["$a", 2]}, "$b"]}, "$want"]}}
	spec := must.NotFail(types.NewDocument("$expr", must.NotFail(types.NewDocument("$eq", must.NotFail(types.NewArray(
		must.NotFail(types.NewDocument("$add", must.NotFail(types.NewArray(
			must.NotFail(types.NewDocument("$multiply", must.NotFail(types.NewArray("$a", int32(2))))),
			"$b",
		)))),
		"$want",
	))))))
	op, err := NewExpr(spec, "$expr")
	require.NoError(t, err)

	for i := int32(0); i < 50; i++ {
		doc := must.NotFail(types.NewDocument("a", i, "b", int32(3), "want", 2*i+3+i%2))
		got, err := op.Process(doc)
		require.NoError(t, err)
		require.Equal(t, i%2 == 0, got, "document %d", i)
	}
}
