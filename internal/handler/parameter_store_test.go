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
	"testing"

	"github.com/stretchr/testify/assert"
)

func TestParameterStore_GetReturnsDefaultWhenUnset(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	v, ok := s.Get("maxTransactionLockRequestTimeoutMillis")
	assert.True(t, ok)
	assert.Equal(t, int64(5), v)

	v, ok = s.Get("quiet")
	assert.True(t, ok)
	assert.Equal(t, false, v)
}

func TestParameterStore_SetThenGetReturnsStoredValue(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	prev, code := s.Set("maxTransactionLockRequestTimeoutMillis", int64(5000))
	assert.Equal(t, ParamSetOK, code)
	assert.Equal(t, int64(5), prev)

	v, ok := s.Get("maxTransactionLockRequestTimeoutMillis")
	assert.True(t, ok)
	assert.Equal(t, int64(5000), v)
}

func TestParameterStore_SetReturnsPreviousValueOnRepeatedSet(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, code := s.Set("quiet", true)
	assert.Equal(t, ParamSetOK, code)

	prev, code := s.Set("quiet", false)
	assert.Equal(t, ParamSetOK, code)
	assert.Equal(t, true, prev)
}

func TestParameterStore_SetUnknownParameterIsRejected(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, code := s.Set("notARealParameter", 1)
	assert.Equal(t, ParamSetUnknown, code)
}

func TestParameterStore_SetNotRuntimeSettableIsRejected(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, code := s.Set("featureCompatibilityVersion", "8.0")
	assert.Equal(t, ParamSetNotRuntime, code)
}

func TestParameterStore_GetUnknownParameterReturnsNotOK(t *testing.T) {
	t.Parallel()
	s := newParameterStore()

	_, ok := s.Get("notARealParameter")
	assert.False(t, ok)
}
