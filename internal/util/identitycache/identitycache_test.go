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

package identitycache

import (
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

type doc struct{ n int }

func TestCacheGetPut(t *testing.T) {
	var c Cache[doc, string]
	a, b := &doc{1}, &doc{1}

	_, ok := c.Get(a)
	require.False(t, ok)

	c.Put(a, "a")
	v, ok := c.Get(a)
	require.True(t, ok)
	require.Equal(t, "a", v)

	_, ok = c.Get(b)
	require.False(t, ok, "keys are compared by identity, not value")

	runtime.KeepAlive(a)
	runtime.KeepAlive(b)
}

func TestCacheRemovesCollectedKeys(t *testing.T) {
	var c Cache[doc, int]
	for i := 0; i < 100; i++ {
		c.Put(&doc{i}, i)
	}
	require.Eventually(t, func() bool {
		runtime.GC()
		return c.len() == 0
	}, 10*time.Second, 10*time.Millisecond)
}
