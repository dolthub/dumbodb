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

package handler

import (
	"math"
	"testing"

	"github.com/stretchr/testify/assert"

	"github.com/dolthub/dumbodb/internal/types"
)

func TestParameterStore_GetReturnsDefaultWhenUnset(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	v, ok := s.Get("maxTransactionLockRequestTimeoutMillis")
	assert.True(t, ok)
	assert.Equal(t, int32(5), v)

	v, ok = s.Get("quiet")
	assert.True(t, ok)
	assert.Equal(t, false, v)
}

func TestParameterStore_SetThenGetReturnsStoredValue(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	prev, code, _ := s.Set("maxTransactionLockRequestTimeoutMillis", int64(5000))
	assert.Equal(t, ParamSetOK, code)
	assert.Equal(t, int32(5), prev)

	v, ok := s.Get("maxTransactionLockRequestTimeoutMillis")
	assert.True(t, ok)
	assert.Equal(t, int32(5000), v)
}

func TestParameterStore_SetReturnsPreviousValueOnRepeatedSet(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, code, _ := s.Set("quiet", true)
	assert.Equal(t, ParamSetOK, code)

	prev, code, _ := s.Set("quiet", false)
	assert.Equal(t, ParamSetOK, code)
	assert.Equal(t, true, prev)
}

func TestParameterStore_SetUnknownParameterIsRejected(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, code, _ := s.Set("notARealParameter", 1)
	assert.Equal(t, ParamSetUnknown, code)
}

func TestParameterStore_SetNotRuntimeSettableIsRejected(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, code, _ := s.Set("featureCompatibilityVersion", "8.0")
	assert.Equal(t, ParamSetNotRuntime, code)
}

func TestParameterStore_SetCoercesToParameterType(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name   string
		param  string
		value  any
		want   any
		reason string
	}{
		{"IntFromDouble", "maxTransactionLockRequestTimeoutMillis", -7.9, int32(-7), ""},
		{"IntFromDecimal", "maxTransactionLockRequestTimeoutMillis", types.Decimal128{H: 0x303e000000000000, L: 79}, int32(7), ""},
		{"IntFromBool", "maxTransactionLockRequestTimeoutMillis", true, int32(1), ""},
		{"IntOutOfBounds", "maxTransactionLockRequestTimeoutMillis", int64(1) << 40, nil, "Out of bounds coercing to integral value"},
		{"IntFromNaN", "maxTransactionLockRequestTimeoutMillis", math.NaN(), nil, "Unable to coerce NaN/Inf to integral type"},
		{"IntFromString", "maxTransactionLockRequestTimeoutMillis", "7", nil, "Unable to coerce value to integral type"},
		{"BoolFromString", "quiet", "false", true, ""},
		{"BoolFromNull", "quiet", types.Null, false, ""},
		{"BoolFromZero", "quiet", float64(0), false, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			s := newParameterStore()

			_, code, reason := s.Set(tc.param, tc.value)
			assert.Equal(t, tc.reason, reason)
			if tc.reason != "" {
				assert.Equal(t, ParamSetBadValue, code)
				return
			}
			assert.Equal(t, ParamSetOK, code)
			v, _ := s.Get(tc.param)
			assert.Equal(t, tc.want, v)
		})
	}
}

func TestParameterStore_GetUnknownParameterReturnsNotOK(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, ok := s.Get("notARealParameter")
	assert.False(t, ok)
}
