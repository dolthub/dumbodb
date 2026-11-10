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

package dolt

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestSimpleIDEquality(t *testing.T) {
	operatorShaped := must.NotFail(types.NewDocument("$in", must.NotFail(types.NewArray(int32(1)))))
	literalDocument := must.NotFail(types.NewDocument("a", int32(1)))
	for _, tc := range []struct {
		name   string
		filter any
		want   any
		ok     bool
	}{
		{name: "bare scalar", filter: int32(5), want: int32(5), ok: true},
		{name: "bare document", filter: literalDocument, want: literalDocument, ok: true},
		{name: "eq scalar", filter: must.NotFail(types.NewDocument("$eq", int32(5))), want: int32(5), ok: true},
		{name: "eq operator-shaped literal", filter: must.NotFail(types.NewDocument("$eq", operatorShaped)), want: operatorShaped, ok: true},
		{name: "operator", filter: operatorShaped},
		{name: "eq with other operator", filter: must.NotFail(types.NewDocument("$eq", int32(5), "$ne", int32(6)))},
		{name: "eq array", filter: must.NotFail(types.NewDocument("$eq", must.NotFail(types.NewArray(int32(1)))))},
		{name: "eq null", filter: must.NotFail(types.NewDocument("$eq", types.Null))},
		{name: "eq regex", filter: must.NotFail(types.NewDocument("$eq", types.Regex{Pattern: "^x"}))},
		{name: "bare array", filter: must.NotFail(types.NewArray(int32(1)))},
		{name: "bare null", filter: types.Null},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := simpleIDEquality(must.NotFail(types.NewDocument("_id", tc.filter)))
			require.Equal(t, tc.ok, ok)
			if tc.ok {
				require.Equal(t, types.Equal, types.Compare(got, tc.want))
			}
		})
	}
	_, ok := simpleIDEquality(must.NotFail(types.NewDocument("v", int32(1))))
	require.False(t, ok)
}
