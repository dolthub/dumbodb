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
	"math"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestExistsOperandTrue(t *testing.T) {
	const decimalExponentBias = uint64(6176) << 49
	for _, tc := range []struct {
		name string
		v    any
		want bool
	}{
		{"true", true, true},
		{"false", false, false},
		{"int 1", int32(1), true},
		{"int 0", int32(0), false},
		{"int -1", int32(-1), true},
		{"long 0", int64(0), false},
		{"long 1", int64(1), true},
		{"double 0.5", 0.5, true},
		{"double 0", 0.0, false},
		{"double -0", math.Copysign(0, -1), false},
		{"double NaN", math.NaN(), true},
		{"decimal 0", types.Decimal128{H: decimalExponentBias}, false},
		{"decimal 0E+3", types.Decimal128{H: decimalExponentBias + 3<<49}, false},
		{"decimal -0", types.Decimal128{H: 1<<63 | decimalExponentBias}, false},
		{"decimal 1", types.Decimal128{H: decimalExponentBias, L: 1}, true},
		{"decimal NaN", types.Decimal128{H: 0x7c00000000000000}, true},
		{"decimal Inf", types.Decimal128{H: 0x7800000000000000}, true},
		{"decimal non-canonical", types.Decimal128{H: 0x6000000000000000}, false},
		{"null", types.Null, false},
		{"undefined", types.Undefined, false},
		{"string", "x", true},
		{"empty string", "", true},
		{"document", types.MakeDocument(0), true},
		{"array", must.NotFail(types.NewArray()), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, existsOperandTrue(tc.v))
		})
	}
}
