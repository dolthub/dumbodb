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

// Command collationprobe is the Phase 0 expressiveness probe for the collation
// divergence measurement (docs/design/collation-divergence-measurement.md). For
// each MongoDB collation field it builds a minimal witness -- a pair of strings
// differing only at the weight level that field controls -- and reports whether
// golang.org/x/text/collate responds to the corresponding knob. A field the
// library does not respond to is INEXPRESSIBLE and therefore ICU-only.
//
// It talks to no database; it only exercises x/text/collate.
package main

import (
	"fmt"
	"strings"

	"golang.org/x/text/collate"
	"golang.org/x/text/language"
)

// Witness strings use \u escapes so this source stays 7-bit ASCII.
const (
	cafe      = "cafe"
	cafeAcute = "caf\u00e9" // cafe with acute e
	cafeUpper = "CAFE"
	ringPre   = "\u00e5"   // a-with-ring, precomposed
	ringDecom = "a\u030a"    // a + combining ring above
	aUmlaut   = "\u00e4"     // a-umlaut
	oUmlaut   = "\u00f6"     // o-umlaut
	coteBase  = "cote"
	coteAcute = "cot\u00e9"       // cote with acute e
	coteCirc  = "c\u00f4te"       // cote with circumflex o
	coteBoth  = "c\u00f4t\u00e9" // circumflex o + acute e
)

func order(c *collate.Collator, in []string) []string {
	out := append([]string(nil), in...)
	c.SortStrings(out)
	return out
}

func eq(c *collate.Collator, a, b string) bool {
	return c.CompareString(a, b) == 0
}

func same(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// tagCollator builds a collator whose behavior is driven by the BCP-47 -u-
// extension in the given locale string, e.g. "en-u-kn-true". OptionsFromTag
// pulls the collation settings out of the tag; New applies the base locale.
func tagCollator(loc string) *collate.Collator {
	t := language.Make(loc)
	return collate.New(t, collate.OptionsFromTag(t))
}

func line(field, verdict, note string) {
	fmt.Printf("%-22s %-14s %s\n", field, verdict, note)
}

func main() {
	fmt.Printf("x/text/collate CLDRVersion=%s UnicodeVersion=%s Supported=%d locales\n\n",
		collate.CLDRVersion, collate.UnicodeVersion, len(collate.Supported()))
	fmt.Printf("%-22s %-14s %s\n", "FIELD", "VERDICT", "EVIDENCE")
	fmt.Println(strings.Repeat("-", 78))

	probeLocale()
	probeStrength()
	probeStrength45()
	probeNumeric()
	probeCaseFirst()
	probeCaseLevel()
	probeAlternate()
	probeMaxVariable()
	probeBackwards()
	probeNormalization()
}

// locale: does the tailoring change ordering between root/en and a locale with
// a known reordering (Swedish sorts a-ring, a-umlaut, o-umlaut after z)?
func probeLocale() {
	corpus := []string{"z", ringPre, aUmlaut, oUmlaut, "a"}
	en := order(collate.New(language.Make("en")), corpus)
	sv := order(collate.New(language.Make("sv")), corpus)

	unknown := language.Make("zz-nonsense")

	if same(en, sv) {
		line("locale", "SUSPECT", fmt.Sprintf("en==sv order %v; tailoring may be inert", sv))
	} else {
		line("locale", "EXPRESSIBLE", fmt.Sprintf("en=%v sv=%v (differ)", en, sv))
	}
	fmt.Printf("%-22s %-14s unknown 'zz-nonsense' -> tag %q\n", "  locale.fallback", "", unknown.String())
}

// strength 1/2/3: the cafe/cafeAcute/CAFE equality pattern must hit all three
// MongoDB levels: L1 all equal; L2 case equal, accent distinct; L3 all distinct.
func probeStrength() {
	root := language.Make("en")
	l1 := collate.New(root, collate.Loose)      // primary only
	l2 := collate.New(root, collate.IgnoreCase) // + secondary (accents)
	l3 := collate.New(root)                     // tertiary default

	p1 := eq(l1, cafe, cafeAcute) && eq(l1, cafe, cafeUpper)
	p2 := !eq(l2, cafe, cafeAcute) && eq(l2, cafe, cafeUpper)
	p3 := !eq(l3, cafe, cafeAcute) && !eq(l3, cafe, cafeUpper)

	if p1 && p2 && p3 {
		line("strength 1/2/3", "EXPRESSIBLE", "L1 all=; L2 case=,accent!=; L3 all!= reproduced")
	} else {
		line("strength 1/2/3", "PARTIAL", fmt.Sprintf("L1ok=%v L2ok=%v L3ok=%v", p1, p2, p3))
	}
}

// strength 4/5 (quaternary/identical): x/text exposes no such option.
func probeStrength45() {
	line("strength 4/5", "INEXPRESSIBLE", "no quaternary/identical option in x/text")
}

// numericOrdering: a2 < a10 only when numbers are compared as numbers.
func probeNumeric() {
	root := language.Make("en")
	base := order(collate.New(root), []string{"a10", "a2"})
	num := order(collate.New(root, collate.Numeric), []string{"a10", "a2"})
	tag := order(tagCollator("en-u-kn-true"), []string{"a10", "a2"})

	switch {
	case !same(base, num) && same(num, tag):
		line("numericOrdering", "EXPRESSIBLE", fmt.Sprintf("flag & -u-kn agree: %v", num))
	case !same(base, num):
		line("numericOrdering", "EXPRESSIBLE", fmt.Sprintf("flag=%v tag=%v (flag works)", num, tag))
	default:
		line("numericOrdering", "SUSPECT", fmt.Sprintf("no change: %v", num))
	}
}

// caseFirst: upper vs lower must swap the order of a/A at tertiary strength.
func probeCaseFirst() {
	up := order(tagCollator("en-u-kf-upper"), []string{"a", "A"})
	lo := order(tagCollator("en-u-kf-lower"), []string{"a", "A"})
	if !same(up, lo) {
		line("caseFirst", "EXPRESSIBLE", fmt.Sprintf("kf-upper=%v kf-lower=%v", up, lo))
	} else {
		line("caseFirst", "INEXPRESSIBLE", fmt.Sprintf("kf-upper==kf-lower==%v (ignored)", up))
	}
}

// caseLevel: at primary strength (case normally ignored), kc-true must make
// case significant again -> cafe != CAFE.
func probeCaseLevel() {
	baseEq := eq(tagCollator("en-u-ks-level1"), cafe, cafeUpper)
	kcEq := eq(tagCollator("en-u-ks-level1-kc-true"), cafe, cafeUpper)
	if baseEq && !kcEq {
		line("caseLevel", "EXPRESSIBLE", "ks-level1: cafe==CAFE; +kc-true: distinct")
	} else {
		line("caseLevel", "INEXPRESSIBLE", fmt.Sprintf("ks-level1 eq=%v +kc-true eq=%v (no effect)", baseEq, kcEq))
	}
}

// alternate=shifted: punctuation becomes ignorable -> blackbird == black-bird.
func probeAlternate() {
	baseEq := eq(collate.New(language.Make("en")), "blackbird", "black-bird")
	shiftedEq := eq(tagCollator("en-u-ka-shifted"), "blackbird", "black-bird")
	if !baseEq && shiftedEq {
		line("alternate=shifted", "EXPRESSIBLE", "non-ignorable: distinct; shifted: equal")
	} else {
		line("alternate=shifted", "INEXPRESSIBLE", fmt.Sprintf("baseEq=%v shiftedEq=%v (no effect)", baseEq, shiftedEq))
	}
}

// maxVariable: only meaningful under shifted. punct makes only punctuation
// ignorable; space makes spaces ignorable too. Witness a space vs no space.
func probeMaxVariable() {
	// A hyphen is ignorable when maxVariable includes punctuation (punct) but
	// not when only spaces are variable (space); a bare space cannot tell the
	// two apart since it is ignorable in both.
	punctEq := eq(tagCollator("en-u-ka-shifted-kv-punct"), "black-bird", "blackbird")
	spaceEq := eq(tagCollator("en-u-ka-shifted-kv-space"), "black-bird", "blackbird")
	if punctEq != spaceEq {
		line("maxVariable", "EXPRESSIBLE", fmt.Sprintf("kv-punct eq=%v kv-space eq=%v", punctEq, spaceEq))
	} else {
		line("maxVariable", "INEXPRESSIBLE", fmt.Sprintf("kv-punct eq=%v kv-space eq=%v (no distinction)", punctEq, spaceEq))
	}
}

// backwards: French secondary-from-the-end reorders the cote quartet's middle
// two (coteAcute vs coteCirc) relative to forward comparison.
func probeBackwards() {
	corpus := []string{coteBase, coteAcute, coteCirc, coteBoth}
	fwd := order(collate.New(language.Make("fr")), corpus)
	bwd := order(tagCollator("fr-u-kb-true"), corpus)
	if !same(fwd, bwd) {
		line("backwards", "EXPRESSIBLE", fmt.Sprintf("fwd=%v bwd=%v", fwd, bwd))
	} else {
		line("backwards", "INEXPRESSIBLE", fmt.Sprintf("kb-true==forward==%v (ignored)", fwd))
	}
}

// normalization: precomposed a-ring vs decomposed a+ring. Report whether they
// are already equal (normalization effectively always on) or toggled by kk.
func probeNormalization() {
	baseEq := eq(collate.New(language.Make("en")), ringPre, ringDecom)
	kkEq := eq(tagCollator("en-u-kk-true"), ringPre, ringDecom)
	switch {
	case baseEq:
		line("normalization", "ALWAYS-ON", "precomposed==decomposed by default")
	case !baseEq && kkEq:
		line("normalization", "EXPRESSIBLE", "off: distinct; kk-true: equal")
	default:
		line("normalization", "SUSPECT", "decomposed never equal precomposed")
	}
}
