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

// Package collation resolves MongoDB collation specs and builds ICU-backed
// string comparators from them, via the vendored, pinned ICU in
// github.com/dolthub/go-icu-collation.
package collation

import (
	"fmt"
	"sync"

	"github.com/dolthub/go-icu-collation/icu4c"

	"github.com/dolthub/dumbodb/internal/types"
)

// Version is the ICU version DumboDB collation runs on, echoed in resolved
// collation specs (listIndexes, collection info). It is the real linked ICU, not
// MongoDB's pinned "57.1".
var Version = icu4c.Version()

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

// Effective returns the collation document that governs an operation: the
// operation's own collation if it specified one, else the collection's default
// (nil meaning simple/binary). This is MongoDB's precedence: an explicit
// operation collation wins -- including opting down to {locale:"simple"} -- and
// only an absent operation collation falls through to the collection default.
func Effective(opCollation, defaultCollation *types.Document) *types.Document {
	if opCollation != nil {
		return opCollation
	}
	return defaultCollation
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

// Identity returns a canonical string identifying this collation for equality
// comparison: two collations are equal iff their normalized specs match on every
// semantic field (version, which is engine metadata, is excluded). A
// simple/absent collation returns "" -- the binary default. Used to decide
// whether two indexes on the same key are the same index.
func (c *Collation) Identity() string {
	if c.IsSimple() {
		return ""
	}
	return c.cacheKey()
}

// Comparator compares strings under a resolved collation via a shared,
// immutable ICU collator. Its methods are safe for concurrent use.
type Comparator struct {
	col *icu4c.Collator
}

// CompareStrings returns -1, 0, or 1 ordering a before, equal to, or after b.
func (cmp *Comparator) CompareStrings(a, b string) int {
	return cmp.col.Compare(a, b)
}

// EqualStrings reports whether a and b compare equal under the collation.
func (cmp *Comparator) EqualStrings(a, b string) bool {
	return cmp.col.Compare(a, b) == 0
}

// Key returns the collation sort key for s: two strings equal under the
// collation share a key, and byte order of keys matches collation order.
func (cmp *Comparator) Key(s string) []byte {
	return cmp.col.SortKey(s)
}

var (
	cmpMu    sync.RWMutex
	cmpCache = map[string]*Comparator{}
)

// Comparator returns a shared comparator for this collation, or nil for a
// simple/absent collation (callers then use binary comparison). Comparators are
// cached process-wide by resolved spec; the underlying ICU collator is immutable
// and concurrency-safe, so one instance serves every caller.
func (c *Collation) Comparator() *Comparator {
	if c.IsSimple() {
		return nil
	}
	key := c.cacheKey()

	cmpMu.RLock()
	cmp := cmpCache[key]
	cmpMu.RUnlock()
	if cmp != nil {
		return cmp
	}

	cmpMu.Lock()
	defer cmpMu.Unlock()
	if cmp := cmpCache[key]; cmp != nil {
		return cmp
	}
	cmp = &Comparator{col: c.buildCollator()}
	cmpCache[key] = cmp
	return cmp
}

func (c *Collation) cacheKey() string {
	return fmt.Sprintf("%s|%d|%t|%s|%t|%s|%s|%t|%t",
		c.Locale, c.Strength, c.CaseLevel, c.CaseFirst, c.NumericOrdering,
		c.Alternate, c.MaxVariable, c.Normalization, c.Backwards)
}

// buildCollator opens an ICU collator and maps every collation field onto its
// ICU attribute. The locale is validated upstream (Accepted); if an open ever
// fails it falls back to the root collator so comparison never panics. Attribute
// values are always valid, so setAttribute errors are ignored.
func (c *Collation) buildCollator() *icu4c.Collator {
	col, err := icu4c.Open(c.Locale)
	if err != nil {
		col, _ = icu4c.Open("")
	}

	var strength icu4c.AttributeValue
	switch c.Strength {
	case 1:
		strength = icu4c.Primary
	case 2:
		strength = icu4c.Secondary
	case 4:
		strength = icu4c.Quaternary
	case 5:
		strength = icu4c.Identical
	default:
		strength = icu4c.Tertiary
	}
	_ = col.SetAttribute(icu4c.Strength, strength)
	_ = col.SetAttribute(icu4c.CaseLevel, onOff(c.CaseLevel))
	_ = col.SetAttribute(icu4c.CaseFirst, caseFirst(c.CaseFirst))
	_ = col.SetAttribute(icu4c.NumericCollation, onOff(c.NumericOrdering))
	_ = col.SetAttribute(icu4c.AlternateHandling, alternate(c.Alternate))
	_ = col.SetAttribute(icu4c.NormalizationMode, onOff(c.Normalization))
	_ = col.SetAttribute(icu4c.FrenchCollation, onOff(c.Backwards))
	if c.MaxVariable == "space" {
		_ = col.SetMaxVariable(icu4c.ReorderSpace)
	} else {
		_ = col.SetMaxVariable(icu4c.ReorderPunctuation)
	}
	return col
}

func onOff(b bool) icu4c.AttributeValue {
	if b {
		return icu4c.On
	}
	return icu4c.Off
}

func caseFirst(v string) icu4c.AttributeValue {
	switch v {
	case "upper":
		return icu4c.UpperFirst
	case "lower":
		return icu4c.LowerFirst
	default:
		return icu4c.Off
	}
}

func alternate(v string) icu4c.AttributeValue {
	if v == "shifted" {
		return icu4c.Shifted
	}
	return icu4c.NonIgnorable
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
