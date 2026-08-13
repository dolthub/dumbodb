# ICU Collation

DumboDB compares and orders strings under MongoDB collations using ICU, the
library MongoDB's collation is built on. This document specifies how that works:
the ICU dependency, the cgo binding, the collation-spec-to-ICU mapping, the
comparator and its lifecycle, locale validation, and how collated indexes store
keys.

Status: specified, not yet implemented. `internal/collation` currently uses a
`golang.org/x/text/collate` approximation; this replaces it with ICU.

## 1. The ICU engine and its dependency

Collation is backed by ICU's C collation API (`ucol_*`). ICU is required because
MongoDB's collation is ICU and no pure-Go library reproduces it: `x/text/collate`
cannot express `caseFirst`, `strength` 4/5, `maxVariable`, or
`normalization: false`; go-mysql-server's collations are a single-weight MySQL
engine that cannot express contractions, expansions, or MongoDB's option surface.

ICU ships in a dedicated, purpose-built Go module, `dolthub/go-icu-collation`:

- It vendors ICU C source and builds it statically via cgo. The vendored version
  is the latest ICU (78.3 / Unicode 17.0), pinned for the lifetime of the
  storage format.
- It binds only the `ucol_*` collation API DumboDB needs.
- It is independent of `go-icu-regex`. That module links the *system* ICU
  (unpinnable) and binds only the regex API, so it cannot serve collation. cgo
  linking ICU is already proven in this build by go-icu-regex, so collation adds
  no new fundamental build constraint -- but it brings its own ICU.

Pinning the latest ICU gives DumboDB current Unicode support rather than freezing
on MongoDB's 2015-era ICU 57.1. Because a collated index's sort keys are fixed by
the ICU that built them, each index records its ICU version (section 6); a later
build can link more than one ICU version and serve each index with the collator
that produced it, adopting a newer ICU without rewriting existing indexes.

## 2. Packages

- `internal/collation/icu` -- the cgo binding. A `Collator` handle wrapping
  `*C.UCollator`, with `Open`, `SetAttribute`, `SetMaxVariable`, `StrColl`,
  `GetSortKey`, `Version`, and `Close`, plus a `UErrorCode` wrapper and a UTF-16
  helper for the sort-key path. Each call is a single libicu function; no C shim
  is needed.
- `internal/collation` -- the collation API the handler uses: `Parse`,
  `Collation`, `IsSimple`, `Resolve`, `CaseInsensitive`, and a `Comparator` type
  with `CompareStrings(a, b string) int`, `EqualStrings(a, b) bool`, and
  `Key(s string) []byte`. The comparator's internals call the `icu` package.

The handler threads `collation.Parse(spec).Comparator()` through
find/update/delete/sort; those call sites do not change with the engine.

## 3. Collation spec to ICU attributes

MongoDB's collation document is a direct surface over ICU collator attributes.
Each field maps to one ICU call:

| MongoDB field | ICU attribute / call | Values |
|---|---|---|
| `locale` | `ucol_open(loc, ...)` | bare locale string; validated (section 5) |
| `strength` | `UCOL_STRENGTH` | 1..5 -> PRIMARY / SECONDARY / TERTIARY / QUATERNARY / IDENTICAL |
| `caseLevel` | `UCOL_CASE_LEVEL` | ON / OFF |
| `caseFirst` | `UCOL_CASE_FIRST` | off/upper/lower -> OFF / UPPER_FIRST / LOWER_FIRST |
| `numericOrdering` | `UCOL_NUMERIC_COLLATION` | ON / OFF |
| `alternate` | `UCOL_ALTERNATE_HANDLING` | non-ignorable/shifted -> NON_IGNORABLE / SHIFTED |
| `maxVariable` | `ucol_setMaxVariable` | punct/space -> REORDER_CODE_PUNCTUATION / REORDER_CODE_SPACE |
| `normalization` | `UCOL_NORMALIZATION_MODE` | ON / OFF |
| `backwards` | `UCOL_FRENCH_COLLATION` | ON / OFF |

`Parse` fills MongoDB's defaults (strength 3, caseFirst off, alternate
non-ignorable, maxVariable punct, booleans false). Every option, including the
defaults, is set explicitly with `ucol_setAttribute`, so the collator never
depends on ICU locale-data defaults that could differ from MongoDB's. The locale
is passed bare to `ucol_open`; options are never encoded as BCP-47 `-u-` keywords
in the locale string, matching how MongoDB drives ICU.

## 4. Comparator and lifecycle

`Collation.Comparator()` returns nil for a simple/absent collation, and callers
then use binary comparison. Otherwise it returns a `Comparator` backed by an ICU
collator:

- `CompareStrings(a, b)` -> `ucol_strcollUTF8`, comparing Go strings as UTF-8
  directly (no UTF-16 conversion). Returns -1/0/1.
- `EqualStrings(a, b)` -> `CompareStrings == 0`.
- `Key(s)` -> `ucol_getSortKey` (UTF-16 encode, size-probe then fill). Sort keys
  order bytewise the same way the collator orders strings, so they back collated
  index storage.

Collators are cached process-wide in a standard bounded cache keyed by the
canonical resolved spec. A collator is built once (open, then set every
attribute) and never mutated afterward. ICU guarantees const collator operations
are thread-safe once a collator is fully configured, and `ucol_strcollUTF8` /
`ucol_getSortKey` take a `const UCollator *`, so one cached collator serves every
goroutine concurrently -- no per-goroutine pool is needed. The cache owns the
lifetime and hands out non-owning comparator handles, so call sites need no
`Close` and C allocations are amortized; a `runtime.Cleanup` finalizer backstops
any collator that escapes the cache. Specs are low-cardinality in practice -- an
application typically uses one or two collations -- so the standard cache needs
no special eviction.

## 5. Locale validation

MongoDB rejects an unknown locale with `BadValue`. ICU does not fail on an
unknown locale -- it falls back toward root and reports the fallback through the
status warning (`U_USING_DEFAULT_WARNING` for a full root fallback). A non-empty,
non-"simple" locale that ICU resolves only to root is treated as invalid and
returns `BadValue`, matching MongoDB.

The compatibility floor is MongoDB's accepted collation-locale set -- the 109 ICU
locale IDs MongoDB 8.0 supports, captured as validation data and asserted by
parity tests (each must be accepted and collate as tailored, not degraded to
root):

```
af, sq, am, ar, hy, as, az, be, bn, bs, bs_Cyrl, bg, my, ca, chr, zh, zh_Hant,
hr, cs, da, nl, dz, en, en_US, en_US_POSIX, eo, et, ee, fo, fil, fi, fr, fr_CA,
gl, ka, de, de_AT, el, gu, ha, haw, he, hi, hu, is, ig, smn, id, ga, it, ja, kl,
kn, kk, km, kok, ko, ky, lkt, lo, lv, ln, lt, dsb, lb, mk, ms, ml, mt, mr, mn,
ne, se, nb, nn, or, om, ps, fa, fa_AF, pl, pt, pa, ro, ru, sr, sr_Latn, si, sk,
sl, es, sw, sv, ta, te, th, bo, to, tr, uk, hsb, ur, ug, vi, wae, cy, yi, yo, zu
```

DumboDB may additionally accept locales its newer ICU recognizes but MongoDB's
57.1 does not. That superset is intentional and harmless: DumboDB accepts every
locale MongoDB accepts, and only ever accepts extras MongoDB would have rejected.

## 6. Collated indexes: sort keys and ICU version

A collated index stores ICU sort keys (`Comparator.Key`) so collated range and
unique lookups are O(log N), as in MongoDB. Those keys are fixed by the ICU that
built them.

Alongside its collation spec, each collated index records the ICU version that
built its keys -- the same value `listIndexes` reports. This makes every index
self-describing and lets multiple ICU versions coexist: a build linking more than
one ICU serves each index with the collator that produced its keys. The ICU
version is therefore part of the storage format. Because history is immutable and
content-addressed storage shares chunks across branches and across time, the
version is never changed in place -- only recorded per index and coexisted.

`listIndexes` echoes DumboDB's real ICU version, not MongoDB's `57.1` constant.

## 7. Build

The binding compiles against the `go-icu-collation` module's vendored, statically
linked ICU. Its C surface is just:

```
// #include "unicode/ucol.h"
// #include "unicode/uloc.h"
// #include <stdlib.h>
```

Link flags come from the module's vendored build. `CGO_ENABLED=1` is already
required in this build. The bundled static ICU (collation) and the system's
dynamic ICU (regex, via go-icu-regex) coexist in one process because ICU
version-suffixes its symbols (`ucol_open_78` vs `uregex_open_72`).

## 8. Testing

Collation correctness is a combinatorial claim, so the suite *enumerates* the
option space rather than sampling it. MongoDB 8.0 is the oracle for the parity
layer; nothing hardcodes an expected order. The suite has three layers: a fast
server-less witness layer (8.7), the authoritative parity grid against MongoDB
(8.1-8.5), and sort-key/index invariants (8.6).

### 8.1 The option space

The nine fields and the values the grid enumerates:

| Field | Values | Count |
|---|---|---|
| `locale` | the 109 accepted IDs (section 5), plus invalid probes | 109 (+n) |
| `strength` | 1, 2, 3, 4, 5 | 5 |
| `caseLevel` | false, true | 2 |
| `caseFirst` | off, upper, lower | 3 |
| `numericOrdering` | false, true | 2 |
| `alternate` | non-ignorable, shifted | 2 |
| `maxVariable` | punct, space | 2 |
| `normalization` | false, true | 2 |
| `backwards` | false, true | 2 |

The per-locale product is 5*2*3*2*2*2*2*2 = 960 specs; across 109 locales,
~105,000 cells. "Every permutation" is defined against this grid: the generator
emits it in full, minus the provable-no-op prunes of 8.3, and every emitted cell
runs on every change.

### 8.2 Corpus

An option is observable only if the input exercises it, so the corpus is curated
per phenomenon -- one group per weight level and per tailoring -- and stored as
explicit code points (source stays 7-bit ASCII):

- case (tertiary + caseFirst): `a A b B`
- accent / secondary: `cafe`, cafe-with-acute, `resume`, resume-with-acute
- French backwards: the cote / cote-acute / cote-circumflex / cote-acute-
  circumflex quartet
- German: `a`, a-umlaut, `ae`, `z` (standard vs phonebook)
- Swedish: `a z`, a-ring, a-umlaut, o-umlaut
- Turkish dotted/dotless i: `i`, dotless-i, `I`, I-with-dot
- Czech: `h`, `ch`, `i` (ch collates as a unit)
- Spanish: `ch`, `ll` (traditional vs modern)
- numeric: `a1 a2 a10 a100`
- variable / shifted: `black-bird`, `blackbird`, `black bird`
- normalization: precomposed vs NFD-decomposed forms of one accented string
- quaternary: strings equal through tertiary differing only in a variable element
- identical: strings equal through quaternary differing only by code point
- CJK canary: a few Han characters

Each group is attached to the cells whose options it can actually move (8.3), so
no cell runs a corpus that cannot reveal its option.

### 8.3 Redundancy pruning (proved, not sampled)

The goal is a full run of the *behaviorally distinct* cells, not of 112,000
duplicates. Each prune is a rule stating when an option is a provable no-op, and
each rule is guarded by a witness cell asserting the no-op actually holds -- so
pruning is verified, never assumed:

- `maxVariable` is inert unless `alternate = shifted` -> fix `maxVariable = punct`
  when `alternate = non-ignorable`.
- `caseFirst` and `caseLevel` act on the case distinction -> `caseFirst` is inert
  at strength 1-2 unless `caseLevel = true`.
- `backwards` reorders the secondary (accent) level -> inert at strength 1.
- quaternary differences appear only at strength >= 4 (chiefly with
  `alternate = shifted`); the identical level only at strength 5.
- `numericOrdering` and `normalization` move only inputs with digits or combining
  sequences -> enabled together with the numeric and normalization corpus groups.

After pruning, the distinct grid is a few thousand cells per locale family --
runnable on every change while still exercising every option value in every
context where it has an effect.

### 8.4 Observables and the parity grid

MongoDB exposes the comparator only through operations, so each cell is read as
two observables, computed from both servers and diffed:

- **equality partition** -- the grouping of corpus strings that compare equal,
  read via `distinct`, a unique index, and pairwise equality `find`. Highest
  severity: it drives find/count/distinct and unique-index accept/reject, and the
  unique case changes persisted state.
- **total order** -- the full sort order of the corpus under `find().sort()` with
  the collation. Divergence is reported as the Kendall tau distance plus the
  explicit flipped adjacent pairs, so a failure is legible, not just a boolean.

Each cell is one `harness.PairTest`: seed the attached corpus into both servers
under the spec, read both observables from each, diff. A cell is `Full` where
DumboDB matches MongoDB and `XFail` where it diverges; a diverging cell records
the exact strings whose class or rank differ. Until the ICU engine lands,
everything past today's case-fold region is `XFail`; each option that starts
matching flips its cells `Full`. The grid is both the contract and the progress
bar.

### 8.5 Locale coverage

All 109 accepted locales run the tailoring corpus at strengths 1-3: each must be
accepted and yield its tailored order, not root. Invalid-locale probes assert
`BadValue`. The heavily-tailored locales (`tr`, `cs`, `es`, `sv`, `de` phonebook,
`zh`, ...) are where CLDR tailoring is stressed hardest and divergence from
MongoDB's older ICU is most likely -- those cells are expected to need the
per-index ICU-version story (section 6), and are labeled as such, not silently
failed.

### 8.6 Sort keys and collated indexes

Collated indexes persist ICU sort keys, so two invariants are tested directly on
the comparator, over every corpus pair:

- key order equals compare order:
  `sign(bytes.Compare(Key(a), Key(b))) == sign(CompareStrings(a, b))`.
- equal strings share a key:
  `EqualStrings(a, b) == bytes.Equal(Key(a), Key(b))`.

At the index level, the collated unique index's accept/reject sequence is diffed
against MongoDB (the persisted-state observable of 8.4), and each index's echoed
ICU version is checked.

### 8.7 Witness layer and concurrency

A fast, server-less layer asserts known UCA facts per option, so a regression is
caught before the full grid runs: strengths 1/2/3 on cafe/cafe-acute/CAFE;
caseFirst upper vs lower on `a`/`A`; backwards on the French quartet;
numericOrdering on `a2`/`a10`; caseLevel re-separating case at strength 2;
`alternate = shifted` collapsing `black-bird` == `blackbird`, and strength 4
separating them again. These encode the expected linguistic result, not
ICU-against-ICU.

A `-race` test drives one cached collator from many goroutines through
`CompareStrings` and `Key`, confirming the shared-const-collator design
(section 4).
