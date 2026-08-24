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

	// set records which fields the spec explicitly provided. A field that was
	// not provided keeps the opened locale's ICU tailoring instead of being
	// forced to a MongoDB spec default -- MongoDB resolves a locale's tailored
	// caseFirst/backwards/alternate for any field the caller omitted, and only
	// overrides fields the caller set.
	set providedFields
}

// providedFields tracks which collation attributes a spec explicitly set, so
// buildCollator can leave the rest at the locale's CLDR tailoring.
type providedFields struct {
	caseLevel       bool
	caseFirst       bool
	strength        bool
	numericOrdering bool
	alternate       bool
	maxVariable     bool
	normalization   bool
	backwards       bool
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
		c.set.caseLevel = true
	}
	if s, ok := getString(doc, "caseFirst"); ok {
		c.CaseFirst = s
		c.set.caseFirst = true
	}
	if n, ok := getInt(doc, "strength"); ok {
		c.Strength = n
		c.set.strength = true
	}
	if b, ok := getBool(doc, "numericOrdering"); ok {
		c.NumericOrdering = b
		c.set.numericOrdering = true
	}
	if s, ok := getString(doc, "alternate"); ok {
		c.Alternate = s
		c.set.alternate = true
	}
	if s, ok := getString(doc, "maxVariable"); ok {
		c.MaxVariable = s
		c.set.maxVariable = true
	}
	if b, ok := getBool(doc, "normalization"); ok {
		c.Normalization = b
		c.set.normalization = true
	}
	if b, ok := getBool(doc, "backwards"); ok {
		c.Backwards = b
		c.set.backwards = true
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
// and the ICU version. The backwards field reflects the locale's resolved
// tailoring -- fr_CA turns it on even when the spec did not -- which is what
// MongoDB surfaces here; the other fields report the spec-with-defaults, which
// is also what MongoDB reports (it does not surface tailored caseFirst/alternate
// in this document). Returns nil for a simple/absent collation.
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
		"backwards", c.resolvedBackwards(),
		"version", Version,
	))
}

var (
	backwardsMu    sync.RWMutex
	backwardsCache = map[string]bool{}
)

// resolvedBackwards reports the collator's effective French/backwards setting,
// which a locale's tailoring can turn on (fr_CA) even when the spec did not.
// MongoDB surfaces this resolved value in the reported collation, so DumboDB
// matches it here. Cached per resolved collator.
func (c *Collation) resolvedBackwards() bool {
	key := c.collatorCacheKey()

	backwardsMu.RLock()
	v, ok := backwardsCache[key]
	backwardsMu.RUnlock()
	if ok {
		return v
	}

	v = c.Backwards
	if a, err := c.buildCollator().GetAttribute(icu4c.FrenchCollation); err == nil {
		v = a == icu4c.On
	}
	backwardsMu.Lock()
	backwardsCache[key] = v
	backwardsMu.Unlock()
	return v
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
	key := c.collatorCacheKey()

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

// collatorCacheKey extends cacheKey with which fields the spec explicitly
// provided. Two specs with identical resolved values but different provided
// sets -- e.g. {locale:"da"} (caseFirst tailored to upper) versus
// {locale:"da", caseFirst:"off"} (pinned off) -- build different collators and
// must not share a cache entry.
func (c *Collation) collatorCacheKey() string {
	s := c.set
	return fmt.Sprintf("%s|p%t%t%t%t%t%t%t%t", c.cacheKey(),
		s.strength, s.caseLevel, s.caseFirst, s.numericOrdering,
		s.alternate, s.maxVariable, s.normalization, s.backwards)
}

var (
	validMu    sync.RWMutex
	validCache = map[string]error{}
)

// Validate reports whether an operation's collation spec is one MongoDB accepts,
// returning a BadValue-style error otherwise. It replicates MongoDB's rule that
// the resolved caseFirst/backwards -- which a locale's tailoring can set even
// when the caller did not (e.g. Danish caseFirst=upper, French Canadian
// backwards=on) -- must not conflict with the strength. A nil or simple spec is
// always valid. Results are cached per resolved collator.
func Validate(doc *types.Document) error {
	c := Parse(doc)
	if c == nil || c.IsSimple() {
		return nil
	}
	key := c.collatorCacheKey()

	validMu.RLock()
	e, ok := validCache[key]
	validMu.RUnlock()
	if ok {
		return e
	}

	e = c.validateResolved()
	validMu.Lock()
	validCache[key] = e
	validMu.Unlock()
	return e
}

// validateResolved opens the collator and checks its resolved attributes against
// MongoDB's strength rules. GetAttribute reflects the locale tailoring plus any
// overrides, so a Danish collator reports caseFirst=upper here even when the
// spec never set it. If any attribute cannot be read it does not reject.
func (c *Collation) validateResolved() error {
	col := c.buildCollator()
	caseFirst, e1 := col.GetAttribute(icu4c.CaseFirst)
	caseLevel, e2 := col.GetAttribute(icu4c.CaseLevel)
	strength, e3 := col.GetAttribute(icu4c.Strength)
	backwards, e4 := col.GetAttribute(icu4c.FrenchCollation)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return nil
	}

	lowStrength := strength == icu4c.Primary || strength == icu4c.Secondary
	if caseFirst != icu4c.Off && caseLevel != icu4c.On && lowStrength {
		return fmt.Errorf("'caseFirst' is invalid unless 'caseLevel' is on or 'strength' is greater than 2")
	}
	if backwards == icu4c.On && strength == icu4c.Primary {
		return fmt.Errorf("'backwards' is invalid with 'strength' of 1")
	}
	return nil
}

// buildCollator opens an ICU collator for the locale and overrides only the
// attributes the spec explicitly set (c.set); every other attribute keeps the
// opened locale's CLDR tailoring. This matches MongoDB: a locale like Danish
// (caseFirst=upper), French Canadian (backwards=on), or Thai (alternate=shifted)
// carries tailored attribute defaults that must survive when the caller does not
// override them. The locale is validated upstream (Accepted); if an open ever
// fails it falls back to the root collator so comparison never panics. Attribute
// values are always valid, so setAttribute errors are ignored.
func (c *Collation) buildCollator() *icu4c.Collator {
	col, err := icu4c.Open(c.Locale)
	if err != nil {
		col, _ = icu4c.Open("")
	}

	if c.set.strength {
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
	}
	if c.set.caseLevel {
		_ = col.SetAttribute(icu4c.CaseLevel, onOff(c.CaseLevel))
	}
	if c.set.caseFirst {
		_ = col.SetAttribute(icu4c.CaseFirst, caseFirst(c.CaseFirst))
	}
	if c.set.numericOrdering {
		_ = col.SetAttribute(icu4c.NumericCollation, onOff(c.NumericOrdering))
	}
	if c.set.alternate {
		_ = col.SetAttribute(icu4c.AlternateHandling, alternate(c.Alternate))
	}
	if c.set.normalization {
		_ = col.SetAttribute(icu4c.NormalizationMode, onOff(c.Normalization))
	}
	if c.set.backwards {
		_ = col.SetAttribute(icu4c.FrenchCollation, onOff(c.Backwards))
	}
	if c.set.maxVariable {
		if c.MaxVariable == "space" {
			_ = col.SetMaxVariable(icu4c.ReorderSpace)
		} else {
			_ = col.SetMaxVariable(icu4c.ReorderPunctuation)
		}
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
