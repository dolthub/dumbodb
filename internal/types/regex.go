// Copyright 2021 FerretDB Inc.
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
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/dlclark/regexp2"

	"github.com/dolthub/dumbodb/internal/util/pcre"
)

// Matcher is implemented by compiled regular expressions returned by Regex.Compile.
type Matcher interface {
	MatchString(s string) bool
	String() string
}

// filterMatchTimeout bounds one match; regexp2 backtracks and can take
// exponential time. A match that runs out of time counts as no match.
const filterMatchTimeout = 100 * time.Millisecond

type regexp2Matcher struct {
	re *regexp2.Regexp
}

func (m *regexp2Matcher) MatchString(s string) bool {
	matched, _ := m.re.MatchString(s)
	return matched
}

func (m *regexp2Matcher) String() string {
	return m.re.String()
}

// Regex represents BSON type Regex.
type Regex struct {
	Pattern string
	Options string
}

// Compile compiles the regex as PCRE2 does for mongod, with the same
// translation the expression operators use. Options other than i, m, s and
// x are ignored.
func (r Regex) Compile() (Matcher, error) {
	re, err := pcre.Compile(r.Pattern, pcre.Options{
		CaseInsensitive: strings.ContainsRune(r.Options, 'i'),
		Multiline:       strings.ContainsRune(r.Options, 'm'),
		DotAll:          strings.ContainsRune(r.Options, 's'),
		Extended:        strings.ContainsRune(r.Options, 'x'),
	})
	if err != nil {
		return nil, fmt.Errorf("Regular expression is invalid: %w", err)
	}
	re.MatchTimeout = filterMatchTimeout
	return &regexp2Matcher{re: re}, nil
}

func (r Regex) LogValue() slog.Value {
	return slogValue(r, 1)
}

var (
	_ slog.LogValuer = Regex{}
)
