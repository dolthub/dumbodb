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

// Package collation resolves MongoDB collation specs and builds locale-aware
// string comparators from them. It depends only on internal/types so both the
// handler and backend layers can share one implementation.
package collation

import (
	"golang.org/x/text/collate"
	"golang.org/x/text/language"

	"github.com/dolthub/dumbodb/internal/types"
)

// Version mirrors the ICU version MongoDB 8.0 reports in a resolved collation.
// DumboDB targets MongoDB 8.0 parity; the string is echoed verbatim so
// listIndexes and collection info match the oracle.
const Version = "57.1"

// Collation is a parsed MongoDB collation spec. A nil *Collation and the
// "simple" locale both mean binary comparison.
type Collation struct {
	Locale          string
	CaseLevel       bool
	CaseFirst       string
	Strength        int32
	NumericOrdering bool
	Alternate       string
	MaxVariable     string
	Normalization   bool
	Backwards       bool
}

// Parse reads a collation spec document, applying MongoDB's defaults for any
// omitted field. It returns nil when doc is nil.
func Parse(doc *types.Document) *Collation {
	if doc == nil {
		return nil
	}

	c := &Collation{
		CaseFirst:   "off",
		Strength:    3,
		Alternate:   "non-ignorable",
		MaxVariable: "punct",
	}

	if s, ok := getString(doc, "locale"); ok {
		c.Locale = s
	}
	if b, ok := getBool(doc, "caseLevel"); ok {
		c.CaseLevel = b
	}
	if s, ok := getString(doc, "caseFirst"); ok {
		c.CaseFirst = s
	}
	if n, ok := getInt(doc, "strength"); ok {
		c.Strength = n
	}
	if b, ok := getBool(doc, "numericOrdering"); ok {
		c.NumericOrdering = b
	}
	if s, ok := getString(doc, "alternate"); ok {
		c.Alternate = s
	}
	if s, ok := getString(doc, "maxVariable"); ok {
		c.MaxVariable = s
	}
	if b, ok := getBool(doc, "normalization"); ok {
		c.Normalization = b
	}
	if b, ok := getBool(doc, "backwards"); ok {
		c.Backwards = b
	}

	return c
}

// IsSimple reports whether the collation is the binary default, for which no
// locale-aware comparison is needed.
func (c *Collation) IsSimple() bool {
	return c == nil || c.Locale == "" || c.Locale == "simple"
}

// CaseInsensitive reports whether string comparison ignores case (strength 1 or
// 2 without an explicit case level).
func (c *Collation) CaseInsensitive() bool {
	return c != nil && !c.IsSimple() && c.Strength <= 2 && !c.CaseLevel
}

// Resolve renders the full collation document MongoDB reports, filling defaults
// and the ICU version. Returns nil for a simple/absent collation.
func (c *Collation) Resolve() *types.Document {
	if c.IsSimple() {
		return nil
	}
	return must(types.NewDocument(
		"locale", c.Locale,
		"caseLevel", c.CaseLevel,
		"caseFirst", c.CaseFirst,
		"strength", c.Strength,
		"numericOrdering", c.NumericOrdering,
		"alternate", c.Alternate,
		"maxVariable", c.MaxVariable,
		"normalization", c.Normalization,
		"backwards", c.Backwards,
		"version", Version,
	))
}

// Comparator returns a locale-aware string comparator for this collation, or
// nil for a simple/absent collation (callers then use binary comparison). The
// returned value wraps a non-concurrency-safe collator and must be used from a
// single goroutine.
func (c *Collation) Comparator() *Comparator {
	if c.IsSimple() {
		return nil
	}

	var opts []collate.Option
	switch {
	case c.Strength <= 1:
		opts = append(opts, collate.Loose)
	case c.Strength == 2:
		opts = append(opts, collate.IgnoreCase, collate.IgnoreWidth)
	}
	if c.NumericOrdering {
		opts = append(opts, collate.Numeric)
	}

	return &Comparator{col: collate.New(language.Make(c.Locale), opts...)}
}

// Comparator compares strings under a resolved collation. It is not safe for
// concurrent use.
type Comparator struct {
	col *collate.Collator
	buf collate.Buffer
}

// CompareStrings returns -1, 0, or 1 ordering a before, equal to, or after b.
func (cmp *Comparator) CompareStrings(a, b string) int {
	return cmp.col.CompareString(a, b)
}

// EqualStrings reports whether a and b compare equal under the collation.
func (cmp *Comparator) EqualStrings(a, b string) bool {
	return cmp.col.CompareString(a, b) == 0
}

// Key returns the collation sort key for s: two strings equal under the
// collation share a key, and byte order of keys matches collation order.
func (cmp *Comparator) Key(s string) []byte {
	cmp.buf.Reset()
	k := cmp.col.KeyFromString(&cmp.buf, s)
	out := make([]byte, len(k))
	copy(out, k)
	return out
}

func getString(doc *types.Document, key string) (string, bool) {
	v, err := doc.Get(key)
	if err != nil {
		return "", false
	}
	s, ok := v.(string)
	return s, ok
}

func getBool(doc *types.Document, key string) (bool, bool) {
	v, err := doc.Get(key)
	if err != nil {
		return false, false
	}
	b, ok := v.(bool)
	return b, ok
}

func getInt(doc *types.Document, key string) (int32, bool) {
	v, err := doc.Get(key)
	if err != nil {
		return 0, false
	}
	switch n := v.(type) {
	case int32:
		return n, true
	case int64:
		return int32(n), true
	case float64:
		return int32(n), true
	default:
		return 0, false
	}
}

func must(doc *types.Document, err error) *types.Document {
	if err != nil {
		panic(err)
	}
	return doc
}
