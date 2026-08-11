# Collation Engine Divergence Measurement Plan

**Issue:** workspace-alp (collation epic)
**Date:** 2026-08-07, revised 2026-08-11
**Status:** Plan / Draft -- **measurement only, no dumbodb code changes**
**Settled:** the engine -- ICU, not x/text (section 1a)
**Recommended, awaiting sign-off:** pin ICU 57.1 (section 1b). This is *not*
deferrable: it becomes permanent, and unrepairable by migration, as soon as
collation-ordered indexes persist sort keys.
**Still open:** the harness itself (phases 1-5) is unbuilt; only Phase 0 has run.

## 1. Goal

DumboDB's collation currently sorts and matches with the pure-Go
`golang.org/x/text/collate` library. MongoDB uses ICU 57.1. Before deciding
whether to keep x/text (with documented risk) or take on an ICU dependency, we
need a *measured* answer to one question:

> For the collation specs our users actually use, how far does
> `x/text/collate` diverge from MongoDB's ICU 57.1 -- in which locales, on which
> options, and does the divergence affect *matching* (equality) or only
> *ordering*?

This plan produces that measurement. It changes no dumbodb code. Its output is a
report that makes the keep-x/text-vs-adopt-ICU call an evidence-based decision.

## 1a. Decision (2026-08-07): adopt ICU, not x/text

Phase 0 is sufficient to decide the engine. `x/text` categorically cannot
express `caseFirst`, `strength` 4/5, `maxVariable`, or `normalization: false`.
Because there is no way to know which collation options a given user relies on,
and silently diverging from MongoDB is a correctness hazard, an engine that
cannot even represent part of the spec is disqualified. DumboDB will back
collation with ICU.

Two findings make this cheaper and sharper than expected:

1. **ICU is already a dependency.** DumboDB already links system libicu via cgo
   through `github.com/dolthub/go-icu-regex` (required directly and transitively
   via Dolt / go-mysql-server; `internal/icu` is in the compiled dependency
   graph, no build-tag gating). Adopting ICU for collation adds no new
   fundamental dependency -- the cgo/libicu build and runtime requirement
   already exists. `go-icu-regex` exposes only regex, so a collation binding to
   ICU's `ucol_*` API still has to be added; that is an implementation task, not
   a dependency decision. (Reuse from GMS was investigated and ruled out -- see
   below.)

   **GMS reuse ruled out (checked 2026-08-07).** go-mysql-server has no ICU
   collation binding: `ucol_*` / ICU collation headers appear nowhere in GMS or
   Dolt (only `go-icu-regex`, regex-only). GMS collations are a self-contained
   pure-Go MySQL implementation driven by generated weight tables
   (`sql/encodings/*.bin`) keyed to MySQL collation IDs, and the entire sort API
   is `CollationSorter func(r rune) int32` -- one weight per rune, walked one
   rune at a time. That model structurally cannot express contractions
   (Czech `ch`, Spanish `ll`), expansions (German `ss`), multi-level weights, or
   MongoDB's `strength`/`caseFirst`/`backwards`/`numericOrdering`/`caseLevel`. It
   models a different collation universe (MySQL) with a weaker engine than
   MongoDB parity needs. So DumboDB must add its own `ucol_*` cgo binding,
   following the pattern `go-icu-regex` already establishes.

2. **The remaining question is which ICU, not whether ICU.** MongoDB pins ICU
   57.1 (CLDR ~29, 2016) for index-key stability. The system ICU here is 72.1
   (CLDR 42, Unicode 15.0). Using the already-linked system ICU gets every
   collation option correct -- the categorical x/text failures vanish -- but
   retains a residual ordering/equality drift from Mongo's 57.1 on tailored
   locales, because the CLDR data moved 13 releases. That drift is far smaller
   than x/text's (x/text was CLDR 23 and missing options); for stable locales
   like `en` at strength 1-2 it is almost certainly nil, and it concentrates in
   the same tailored-locale tail.

**Retargeting the measurement.** The harness below is not wasted: its candidate
side switches from x/text to the ICU we would actually ship, and it now answers
the residual question -- how far is that ICU from Mongo's pinned 57.1? Section 1b
covers why the version is now a free choice rather than a host-constrained one,
and what criterion decides it.

## 1b. The version pin (2026-08-11): recommend 57.1, and decide before keys land

**The earlier A-vs-B framing is superseded.** A prior draft posed this as "A. ship
the already-linked system ICU (72.1)" vs "B. pin 57.1 exactly," with B the heavy
option because it meant building 2016-era ICU beside the system copy. That is
obsolete: the binding now bundles ICU in a dedicated module from vendored source
(`icu-collation-binding.md` section 11a), so the host's ICU constrains nothing
and bundling costs the same whichever version we pick. The version is a free
parameter. What remains is choosing its value -- and that is *not* settled.

**Criterion.** Matching MongoDB's version is not a goal in itself. It matters only
where the two databases would yield observably different results. The question is
therefore not "which ICU does Mongo use" but "where does version V diverge from
Mongo, and is that region one real workloads touch?"

**Where collation is observable.** Divergence is not confined to presentation:

- ordering of `sort` / `$sort` results on a collated field;
- equality in `find`, `$match`, and range bounds;
- **accept/reject decisions of a collated unique index** -- this changes which
  documents exist, and surfaces as an error rather than as a reordering;
- dedup in `distinct`, `$group` string `_id`, and `$addToSet`;
- the echoed `version` label in a resolved collation -- a protocol question,
  separable from behavior (`icu-collation-binding.md` section 10).

The first four are behavioral and are what the harness measures. The unique-index
row carries the most weight, being the one that changes persisted state.

**Why 57.1 is not merely "a version."** It is the only choice whose divergence
from Mongo is zero by construction. Every other version has a divergence set D
that must be measured and then explicitly accepted. That asymmetry is real and
should not be argued away.

**What 57.1 costs.** ICU 57.1 is Unicode 8.0, CLDR ~29 (verified from the
`release-57-1` headers: `U_ICU_VERSION "57.1"`, `U_UNICODE_VERSION "8.0"`).
Current ICU 78.3 is Unicode 17.0. Pinning 57.1 freezes DumboDB on 2015 Unicode
permanently: characters assigned in Unicode 9.0 through 17.0 get no tailored
collation weights and fall back to implicit code-point-derived ordering. It also
means shipping a 2016 C++ codebase that must be audited against a decade of
subsequent ICU security fixes.

**The tension to decide, stated plainly.** Mongo 8.0 is *also* 57.1, so it has
the same blind spot for post-2015 characters. Matching 57.1 therefore means
faithfully reproducing MongoDB's staleness rather than avoiding it: on a string
containing a recently assigned character, 57.1 yields parity-with-a-worse-answer
while a modern ICU yields a better answer that diverges. Parity and correctness
point in opposite directions in exactly that cell, and no measurement resolves
it -- it is a product judgment about what DumboDB is for. The harness's job is to
size the rest of the matrix, where the two goals do not conflict, so that this
judgment applies to as small a region as possible.

**This cannot be deferred.** An earlier revision of this section claimed the
choice stayed reversible because DumboDB persists no ICU sort keys today. That
was wrong -- it mistook the current unoptimized state for a stable one. Removing
the full-scan on collated reads (`workspace-alp.15`) *is* the goal, and the fix is
collation-ordered indexes, which persist ICU sort keys by design. At that point
the version becomes part of the storage format, and in a version-controlled store
it spans all of history: chunk dedup and structural sharing assume key encoding is
identical across branches and across time, and immutable history means no
migration can repair index trees inside existing commits. See
`icu-collation-binding.md` section 9. The pin must therefore be chosen before
collation-ordered indexes ship, which makes it a decision for now, not later.

**Recommendation: pin 57.1.** The reasoning, in order of weight:

1. **Asymmetry under permanence.** 57.1 has divergence zero by construction; any
   other version has a permanent, currently unmeasured divergence set D. When a
   decision cannot be revised, a guaranteed zero beats an unmeasured risk.
2. **The criterion above, applied honestly, points here.** Pinning 57.1 inherits
   Mongo's Unicode 8.0 blind spot and any CLDR 29 tailoring bug that later CLDR
   releases fixed. But Mongo 8.0 carries those same defects, so *no workload that
   works on MongoDB is lost by reproducing them*. The staleness costs nothing in
   compatibility terms; it costs only the ability to be better than Mongo, which
   is a different goal than the one this criterion states.
3. **A permanent divergence would corrode the parity suite.** Choosing a modern
   ICU gives the harness a set of known-failing collation cells that no fix can
   ever close. That suite is the project's primary quality signal, and permanent
   expected-failures erode it.
4. **It converts this measurement into a conformance test.** Under a 57.1 pin,
   candidate and oracle run the *same* ICU, so the expected result is exact
   agreement and any disagreement is a bug in our spec-to-attribute mapping
   (`icu-collation-binding.md` section 5). Pass/fail and actionable, rather than
   an open-ended "how far apart are we" number.
5. **The cost of old code is mitigated by the packaging decision already taken.**
   Vendored source (binding doc section 11a) can be patched, so security fixes can
   be backported into the vendored tree; a system library or prebuilt archive
   could not be. This makes "2016 C++" an audit-and-patch obligation rather than
   an unpatchable exposure -- but the obligation is real and must be explicit: a
   CVE review of the vendored subset (`ucol_*`, the collation data loader, and
   their dependencies) before shipping, with a named owner for backports.

**What would overturn this.** Stated so the decision is falsifiable rather than
inherited:

- If DumboDB's goal is to be a *better* document database rather than a
  MongoDB-compatible one, correctness-first wins and a modern ICU is right. That
  is a product-strategy call and should be made explicitly, not by default.
- If MongoDB itself un-pins 57.1, the parity target moves; revisit before keys
  land.
- If the CVE review finds exposure in 57.1's collation subset that cannot
  reasonably be backported.

**Prerequisites before collation-ordered indexes ship,** whichever version wins:
the ICU version stamped into index metadata and enforced at merge (binding doc
section 9), and the conformance harness green.

## 2. Facts already established (do not re-derive)

- **Version skew is real and large.** `collate.CLDRVersion == "23"` (March
  2013), `collate.UnicodeVersion == "6.2.0"` (2012). ICU 57.1 bundles roughly
  CLDR 29 (2016). Collation weights and locale tailorings changed across those
  releases, so divergence on tailored locales is expected, not hypothetical.
- **MongoDB pins ICU 57.1** deliberately for index-key stability, and reports
  `version: "57.1"` in a resolved collation. The oracle is therefore a fixed
  target across mongod 8.0.x patch versions.
- **x/text's entire tuning surface is small:** flags `IgnoreCase`,
  `IgnoreDiacritics`, `IgnoreWidth`, `Loose`, `Force`, `Numeric`; plus
  `OptionsFromTag(tag)` (reads BCP-47 `-u-` extensions) and `Reorder(...)`.
  MongoDB exposes nine collation fields. Most do not have a direct x/text flag.
- **Current parity coverage is confined to the region where the two engines
  agree most:** every collation test uses `locale: "en"` at strength 1 or 2.
  There is no test at strength 3+, no non-`en` locale, and no test of
  `caseFirst`/`backwards`/`alternate`/`numericOrdering`/`caseLevel`. Green
  parity today is therefore not evidence about the divergence surface -- nothing
  exercises it.

## 3. Measurement design: isolate the library from the handler

The question is about the *library*, not DumboDB's current wiring. DumboDB's
handler is already known to be lossy (it honors only `strength` and
`numericOrdering` and silently ignores the other six fields). Routing the
measurement through DumboDB would conflate two different divergences:

1. handler drops an option it could have honored (a DumboDB bug), and
2. x/text genuinely cannot match ICU (the library ceiling).

We want (2) in isolation. So the harness compares:

- **Oracle:** real MongoDB (ICU 57.1). For a given collation spec and corpus,
  insert the corpus, run `find({}).sort({s:1}).collation(spec)`, and read back
  the resulting order. Equality classes are read with an equality query per
  distinct string (or `$group` under the collation).
- **Candidate:** `x/text/collate` invoked directly, using the *best possible*
  mapping of the MongoDB spec to x/text options -- not DumboDB's current mapping.
  We are measuring the library's ceiling, so the candidate side must try
  `OptionsFromTag` and every applicable flag, not just what the handler wires
  today.

DumboDB is not in the loop. The harness lives in the parity repo (it already
imports `x/text` transitively) as a standalone, non-default measurement (build
tag or separate package) so it does not run in the normal parity sweep.

## 4. Phase 0 -- expressiveness partition (do this first)

Before measuring divergence, partition MongoDB's nine collation fields into
what x/text can represent at all. For a field x/text cannot express, divergence
is 100 percent by construction and no measurement is needed -- it is ICU-only if
that field matters.

Empirically probe (Phase 0 is code that prints a table, not assumptions):

| MongoDB field | Candidate x/text path | Expressible? |
|---|---|---|
| `locale` | `language.Make`; check `collate.Supported()` and whether `Make` silently substituted a different tag | probe |
| `strength` 1/2/3 | `Loose` / `IgnoreCase`+`IgnoreDiacritics` combos | likely yes |
| `strength` 4/5 (quaternary/identical) | none | **no** |
| `numericOrdering` | `Numeric` | yes |
| `caseFirst` (off/upper/lower) | `-u-kf-` via `OptionsFromTag` | probe |
| `caseLevel` | `-u-kc-` via `OptionsFromTag` | probe |
| `alternate` (non-ignorable/shifted) | `-u-ka-` via `OptionsFromTag` | probe |
| `maxVariable` (punct/space) | none obvious | probe |
| `backwards` (French accents) | `-u-kb-` via `OptionsFromTag` | probe |
| `normalization` | not exposed | probe |

The `OptionsFromTag` rows are the real unknown: BCP-47 `-u-` extensions (`kf`,
`kc`, `ka`, `kb`, `kn`, `ks`) may or may not flow through given x/text's CLDR-23
vintage. Phase 0 resolves each empirically by building a collator from a tagged
locale, running a corpus that the extension is supposed to reorder, and checking
whether the order actually changed. Output: each field labeled EXPRESSIBLE or
INEXPRESSIBLE, with a one-line note on how it was determined.

Fields labeled INEXPRESSIBLE are pruned from the divergence matrix and go
straight to the decision as "ICU required if this option must be supported."

## 4a. Phase 0 results (measured 2026-08-07)

Ran `cmd/collationprobe` against `x/text/collate` (`CLDRVersion=23`,
`UnicodeVersion=6.2.0`, 95 supported locales). Each field's verdict comes from a
minimal witness that isolates the weight level the field controls; the tag-based
fields were driven through `OptionsFromTag` with a BCP-47 `-u-` extension.

| MongoDB field | Verdict | How determined |
|---|---|---|
| `locale` | EXPRESSIBLE | `en` vs `sv` reorder `a-ring/a-umlaut/o-umlaut` (sv sorts them after `z`) |
| `strength` 1/2/3 | EXPRESSIBLE | cafe/cafe-acute/CAFE reproduce all three equality levels |
| `strength` 4/5 | **INEXPRESSIBLE** | no quaternary/identical option exists |
| `numericOrdering` | EXPRESSIBLE | `Numeric` flag and `-u-kn-true` both give `a2 < a10` |
| `caseFirst` | **INEXPRESSIBLE** | `-u-kf-upper` and `-u-kf-lower` produce identical order |
| `caseLevel` | EXPRESSIBLE | `-u-ks-level1` makes cafe==CAFE; adding `-u-kc-true` re-separates them |
| `alternate: shifted` | EXPRESSIBLE | `-u-ka-shifted` makes `black-bird` == `blackbird` |
| `maxVariable` | **INEXPRESSIBLE** | `-u-kv-punct` vs `-u-kv-space` produce identical results (kv ignored) |
| `backwards` | EXPRESSIBLE | `-u-kb-true` swaps the middle two of the French cote quartet |
| `normalization` | **ALWAYS-ON** | precomposed a-ring == decomposed a+ring by default; cannot be turned off |

Two behavioral notes beyond the table:

- **Unknown locales are not rejected.** `language.Make("zz-nonsense")` yields the
  `und` (root) tag silently. MongoDB rejects an unknown locale with an error.
  Any x/text-backed implementation must validate the locale against
  `collate.Supported()` itself, or it will silently sort under root where Mongo
  would have errored.
- **`OptionsFromTag` honors more than the six exported flags.** `ks`, `kc`,
  `ka`, `kb`, `kn` all took effect; only `kf` and `kv` were ignored. So x/text's
  reachable surface is wider than the flag list suggests -- and wider than
  DumboDB's current handler, which wires only `strength` and `numericOrdering`.

### Partition and consequences

- **Hard ICU-only (x/text cannot represent it at all):** `strength` 4/5,
  `caseFirst`, `maxVariable`, and `normalization: false`. If any of these must be
  supported faithfully, x/text is disqualified for that spec regardless of
  divergence measurements -- there is nothing to measure.
- **Expressible, divergence-to-be-measured (Phases 1+):** `locale`, `strength`
  1/2/3, `numericOrdering`, `caseLevel`, `alternate: shifted`, `backwards`. For
  these x/text produces an answer; whether that answer matches ICU 57.1 is the
  CLDR-23-vs-CLDR-29 question the later phases quantify.

This prunes the divergence matrix: the four hard-ICU-only rows drop out, and the
measurement focuses on the six expressible fields, concentrated on tailored
locales where CLDR 23 and 29 are most likely to disagree.

## 5. Phase 1 -- the divergence harness

A single Go test (parity repo, gated so it is not part of the normal sweep) that
for each `(locale, spec, corpus)` triple:

1. builds the oracle order + equality classes from Mongo under `spec`;
2. builds the candidate order + equality classes from x/text under the best
   mapping of `spec`;
3. records both, computes the metrics in section 8, and appends one row to a
   report artifact (a checked-in `.md` or `.json`).

It must record the mongod version and the ICU version string it reports, and
x/text's `CLDRVersion`/`UnicodeVersion`, so the report is self-describing and
reproducible. It is a measurement, not an assertion: rows do not fail the build;
they populate a report a human reads.

## 6. Phase 2 -- the corpus (adversarial, per phenomenon)

Random strings mostly agree between engines and hide the tail. Use a small
curated corpus targeting known ICU tailoring points, one group per phenomenon:

- **Case order:** `a A b B z Z` (tertiary tie-break and `caseFirst`).
- **Diacritic / secondary:** `cafe cafe-acute resume resume-acute naive
  naive-diaeresis`.
- **French backwards accents:** `cote cote-acute cote-circumflex
  cote-acute-circumflex` (the canonical backwards-secondary example).
- **German umlaut:** `a a-umlaut ae af z` (standard treats a-umlaut near a;
  phonebook treats it as ae).
- **Swedish:** `a z a-ring a-umlaut o-umlaut` (should end ... z then the three
  extras, a major tailoring vs a-near-a).
- **Turkish dotted/dotless i:** `i dotless-i I I-dot` at strength 1 and 2.
- **Czech:** `h ch i` (ch sorts after h as a unit).
- **Spanish:** modern vs traditional `ch`/`ll` handling.
- **Numeric:** `a1 a2 a10 a100`.
- **Punctuation / shifted:** `"black-bird" "blackbird" "black bird"`.
- **CJK canary:** a few Han characters (pinyin vs stroke; likely large
  divergence or inexpressible -- a canary, not a target).

All corpus strings are stored as explicit code points (no reliance on source
encoding) to keep the file 7-bit ASCII per repo rules; the doc names them in
words above.

## 7. Phase 3 -- the locale x spec matrix

Locales chosen because each stresses a distinct tailoring:

`en` (baseline -- divergence here would be alarming), `de` (+ `de` phonebook via
`-u-co-phonebk` if expressible), `sv`, `tr`, `fr`, `es`, `cs`, and one CJK
canary (`zh`).

Specs per locale: strength 1, 2, 3; then, only for the fields Phase 0 marked
EXPRESSIBLE, add `numericOrdering`, `caseFirst: upper/lower`, `backwards`,
`alternate: shifted`. Skip inexpressible combinations (already decided).

Keep the first run tractable: Latin tailoring set + one CJK canary is enough to
answer the likelihood question. Broaden later if the decision needs it.

## 8. Phase 4 -- metrics (split matching from ordering)

DumboDB uses collation for two distinct things, with different risk profiles:

- **Equality classes** -- which strings compare *equal*. This drives
  case-insensitive `find`, `count`, `distinct`, and unique-index dedup. A
  divergence here is **high severity**: wrong query results, wrong unique-index
  rejection/acceptance.
- **Order** -- the ranking among *unequal* strings. This drives `sort`. A
  divergence here is lower severity for matching-heavy use, high for sort-heavy
  use.

Per `(locale, spec)` record:

- equality-class match vs ICU (set-equal partition?) -- boolean, plus the
  specific strings that changed class;
- order match among unequal strings -- boolean; if not, the Kendall tau distance
  (count of discordant pairs) and the explicit list of flipped adjacent pairs,
  so a divergence is legible, not just a number;
- whether x/text silently substituted a different locale tag (from Phase 0's
  `Make` check) -- a silent fallback is itself a divergence.

## 9. Phase 5 -- verdict taxonomy and decision output

Each `(locale, spec)` cell gets one verdict:

- **INEXPRESSIBLE** -- x/text cannot represent the spec (from Phase 0). ICU
  required if this spec must be supported.
- **EQUIVALENT** -- identical equality classes and identical order vs ICU 57.1.
- **ORDER-ONLY DIVERGENCE** -- equality classes match; order differs. Safe for
  matching and unique indexes; risky for sort.
- **EQUALITY DIVERGENCE** -- disagreement on which strings are equal. High
  severity; breaks find/count/distinct/unique.

The report's headline is the aggregate over the region our users actually need
(today: `en` / `simple` / strength-2 case-insensitive matching for
parse-server): is it EQUIVALENT, and how many cells outside that region diverge
and how badly. That is the direct answer to "how likely is the divergence."

## 10. Deliverables

1. Phase 0 expressiveness table (nine fields labeled, with method notes).
2. The gated divergence harness in the parity repo.
3. The corpus and the locale x spec matrix as data.
4. A checked-in divergence report (matrix of verdicts + flipped-pair details +
   engine version stamps).
5. A one-paragraph recommendation grounded in 1-4. The engine half is already
   answered (ICU -- section 1a), so what the report must deliver is the version
   pin: the measured divergence set for a modern candidate ICU against the Mongo
   57.1 oracle, and whether it falls outside the region real workloads touch
   (section 1b).

## 11. Non-goals

- No dumbodb code changes. This measures the library; it does not fix the
  handler's lossy option mapping (tracked separately under workspace-alp).
- Not an ICU integration spike. The engine call landed on ICU, and the "how" now
  lives in `icu-collation-binding.md`.
- No end-to-end DumboDB-vs-Mongo parity tests for the empty chart rows yet.
  Those are worth writing once the engine is chosen; the corpus here is designed
  to be reused for them. Writing them now would test the handler, not answer the
  engine question.
