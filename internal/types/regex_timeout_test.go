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

package types

import (
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// Filter regexes run on a backtracking engine, so a pattern with
// catastrophic backtracking is cut off by a time limit. Running out of time
// is an error, never a result (workspace-8tj.21).
func TestRegexMatchIsTimeBounded(t *testing.T) {
	m, err := Regex{Pattern: "^(?=(a+)+$)"}.Compile()
	require.NoError(t, err)

	input := strings.Repeat("a", 40) + "!"
	done := make(chan error, 1)
	go func() {
		_, err := m.MatchString(input)
		done <- err
	}()

	select {
	case err := <-done:
		require.Error(t, err)
	case <-time.After(10 * time.Second):
		t.Fatal("regex match did not finish within 10s")
	}
}

func TestRegexLookaheadMatches(t *testing.T) {
	m, err := Regex{Pattern: "^(?=.*foo)bar"}.Compile()
	require.NoError(t, err)
	matched, err := m.MatchString("barfoo")
	require.NoError(t, err)
	require.True(t, matched)
	matched, err = m.MatchString("barbaz")
	require.NoError(t, err)
	require.False(t, matched)
}

func TestRegexCompileIsCached(t *testing.T) {
	a, err := Regex{Pattern: "^ab+c", Options: "im"}.Compile()
	require.NoError(t, err)
	b, err := Regex{Pattern: "^ab+c", Options: "mi"}.Compile()
	require.NoError(t, err)
	require.Same(t, a, b)

	c, err := Regex{Pattern: "^ab+c"}.Compile()
	require.NoError(t, err)
	require.NotSame(t, a, c)

	_, err = Regex{Pattern: "("}.Compile()
	require.Error(t, err)
	_, err = Regex{Pattern: "("}.Compile()
	require.Error(t, err)
}
