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

// Patterns Go's engine rejects as Perl syntax (lookaround) fall back to a
// backtracking engine. Before the fix it had no time limit, so a pattern with
// catastrophic backtracking pinned a CPU for exponential time per document.
func TestRegexFallbackMatchIsTimeBounded(t *testing.T) {
	m, err := Regex{Pattern: "^(?=(a+)+$)"}.Compile()
	require.NoError(t, err)

	input := strings.Repeat("a", 40) + "!"
	done := make(chan bool, 1)
	go func() { done <- m.MatchString(input) }()

	select {
	case matched := <-done:
		require.False(t, matched)
	case <-time.After(10 * time.Second):
		t.Fatal("fallback regex match did not finish within 10s")
	}
}

func TestRegexFallbackStillMatches(t *testing.T) {
	m, err := Regex{Pattern: "^(?=.*foo)bar"}.Compile()
	require.NoError(t, err)
	require.True(t, m.MatchString("barfoo"))
	require.False(t, m.MatchString("barbaz"))
}
