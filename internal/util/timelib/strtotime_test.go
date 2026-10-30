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

package timelib

import (
	"math/rand"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

// allRules tries every rule at every position, as the scanner did before
// rulesByFirstByte.
func allRules(byte) []int {
	out := make([]int, len(rules))
	for i := range out {
		out[i] = i
	}
	return out
}

func TestFirstBytePrefilterIsExact(t *testing.T) {
	t.Parallel()

	inputs := []string{
		"2017-02-08T12:10:40.787Z", "2017-02-08T12:10:40+05:30", "Wed Feb 08 2017 12:10:40 GMT-0500 (EST)",
		"monday next week", "last day of february 2020", "+1 week 2 days", "@1234567890", "tomorrow noon",
		"10/09/2017", "09.10.2017", "2017W061", "2017-039", "1 Jan 2017 12:00 PST", "midnight",
		"third friday of march", "12:00:00.123456", "20170208T121040", "Feb 8, 2017", "2017-02-08 12:10 +0100",
		"yesterday 14:00 Europe/London", "back of 7pm", "front of 7", "\u00B5sec", "-1 month",
	}
	r := rand.New(rand.NewSource(1))
	const alphabet = "0123456789-+:/.,TtZz WEDFMondayJanFebPSTGMTUTC@()\\u00b5\t"
	for i := 0; i < 20000; i++ {
		var b strings.Builder
		for n := r.Intn(30); n > 0; n-- {
			b.WriteByte(alphabet[r.Intn(len(alphabet))])
		}
		inputs = append(inputs, b.String())
	}

	for _, in := range inputs {
		want, wantOK := strToTime(in, allRules)
		got, gotOK := StrToTime(in)
		require.Equal(t, wantOK, gotOK, "%q", in)
		if wantOK {
			assert.Equal(t, want, got, "%q", in)
		}
	}
}

func TestStrToTimeIsLinear(t *testing.T) {
	t.Parallel()

	for name, s := range map[string]string{
		"Dashes":  strings.Repeat("-", 200000),
		"Pluses":  strings.Repeat("+", 200000),
		"Digits":  strings.Repeat("1", 200000),
		"Mondays": strings.Repeat("monday ", 30000),
		"Spaces":  "2021-01-01" + strings.Repeat(" ", 200000) + "10:00",
	} {
		t.Run(name, func(t *testing.T) {
			t.Parallel()

			start := time.Now()
			_, _ = StrToTime(s)
			assert.Less(t, time.Since(start), 2*time.Second)
		})
	}
}
