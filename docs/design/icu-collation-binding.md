# ICU Collation Binding (ucol_*) Design

**Issue:** workspace-alp (collation epic)
**Date:** 2026-08-07, revised 2026-08-11
**Status:** Design / Draft -- **design only, no code changes yet**
**Depends on:** the engine decision in
`docs/design/collation-divergence-measurement.md` (adopt ICU, not x/text)
**Blocking decision:** which ICU version to pin (divergence doc section 1b
recommends 57.1). The binding's *code* is version-agnostic, but the version must
be settled before collation-ordered indexes persist sort keys -- after that it is
part of the storage format and spans all history (section 9).
**Settled here:** bundle ICU from vendored source, not prebuilt archives
(section 11a).

## 1. Goal and scope

Replace the `golang.org/x/text/collate` comparator in `internal/collation` with
a binding to ICU's C collation API (`ucol_*`), so DumboDB collation matches
MongoDB's (which is itself ICU) across the full option surface. This doc
specifies the binding: the C surface, the MongoDB-spec-to-ICU-attribute mapping,
the Go API, lifecycle/concurrency, and the migration. It changes no code.

Why a binding at all (established elsewhere, recap): ICU is already linked into
DumboDB via `go-icu-regex` (cgo, `-licui18n -licuuc -licudata`); x/text cannot
express `caseFirst`, `strength` 4/5, `maxVariable`, or `normalization: false`;
and GMS has no reusable ICU collation binding (its engine is per-rune single
weight, MySQL-only). So we add a small `ucol_*` binding modeled on the
conventions `go-icu-regex` already establishes.

## 2. Why this is mostly mechanical

MongoDB's collation document is a direct surface over ICU's collator
attributes. MongoDB parses the same nine fields and calls the same
`ucol_setAttribute` / `ucol_setMaxVariable` we will. So the mapping (section 5)
is near 1:1 and total; there is very little semantic translation to get wrong.
The only genuinely non-mechanical piece is locale validation (section 7).

## 3. Package layout

- `internal/collation/icu` -- the cgo binding. Thin, mirrors the C API:
  handle type `Collator` wrapping `*C.UCollator`, `Open`, `SetAttribute`,
  `SetMaxVariable`, `StrColl`, `GetSortKey`, `Version`, `Close`, plus a
  `UErrorCode` wrapper and a UTF-16 `ucharStr` helper (only where UChar is
  unavoidable). No C/C++ shim file is needed -- unlike `go-icu-regex`'s replace
  loop, every call we need is a single libicu function.
- `internal/collation` -- unchanged public shape. Keeps `Parse`, `Collation`,
  `IsSimple`, `Resolve`, `CaseInsensitive`, and the `Comparator` type with its
  existing methods `CompareStrings(a, b string) int`, `EqualStrings(a, b) bool`,
  `Key(s string) []byte`. Only the comparator's internals change from x/text to
  the `icu` package. Call sites (msg_update, msg_find, etc.) are untouched.

Keeping the `Comparator` contract identical is deliberate: the handler already
threads `collation.Parse(spec).Comparator()` through find/update/delete/sort, so
a same-shape swap means the migration is confined to one package.

## 4. C API surface we bind

From `unicode/ucol.h` (and `uloc.h` for validation):

- `UCollator *ucol_open(const char *loc, UErrorCode *status)` -- open a collator
  for a bare locale (e.g. "de", "sv", "tr"). We never encode options in the
  locale string; every option is set explicitly via attributes, matching how
  MongoDB drives ICU.
- `void ucol_close(UCollator *coll)`.
- `void ucol_setAttribute(UCollator *coll, UColAttribute attr,
  UColAttributeValue value, UErrorCode *status)`.
- `void ucol_setMaxVariable(UCollator *coll, UColReorderCode group,
  UErrorCode *status)` (ICU 53+; present in 57.1 and 72.1).
- `UCollationResult ucol_strcollUTF8(const UCollator *coll, const char *source,
  int32_t sourceLength, const char *target, int32_t targetLength,
  UErrorCode *status)` (ICU 50+). Compares Go strings as UTF-8 directly, with no
  UTF-16 conversion -- the hot compare path. Returns UCOL_LESS(-1) / UCOL_EQUAL(0)
  / UCOL_GREATER(1).
- `int32_t ucol_getSortKey(const UCollator *coll, const UChar *source,
  int32_t sourceLength, uint8_t *result, int32_t resultLength)` -- binary sort
  key. No UTF-8 variant, so `Key()` converts to UTF-16 first (the `ucharStr`
  helper). Two-call idiom: size probe, then fill.
- `const char *ucol_getLocaleByType(const UCollator *coll, ULocDataLocaleType,
  UErrorCode *status)` -- used for locale validation (section 7).
- `void ucol_getVersion(const UCollator *coll, UVersionInfo info)` -- collator
  version; informational only (section 10).

Error handling follows `go-icu-regex`: a `UErrorCode` starts at
`U_ZERO_ERROR (0)`; `> 0` is failure (`U_FAILURE`), `< 0` is a warning
(`U_USING_FALLBACK_WARNING`, `U_USING_DEFAULT_WARNING`) -- and the warnings are
exactly what locale validation inspects.

## 5. MongoDB spec -> ICU attribute mapping (total)

| MongoDB field | ICU attribute / call | Values |
|---|---|---|
| `locale` | `ucol_open(loc, ...)` | bare locale string; validated (sec 7) |
| `strength` | `UCOL_STRENGTH` | 1..5 -> `UCOL_PRIMARY` / `SECONDARY` / `TERTIARY` / `QUATERNARY` / `IDENTICAL` |
| `caseLevel` | `UCOL_CASE_LEVEL` | `UCOL_ON` / `UCOL_OFF` |
| `caseFirst` | `UCOL_CASE_FIRST` | "off"/"upper"/"lower" -> `UCOL_OFF` / `UCOL_UPPER_FIRST` / `UCOL_LOWER_FIRST` |
| `numericOrdering` | `UCOL_NUMERIC_COLLATION` | `UCOL_ON` / `UCOL_OFF` |
| `alternate` | `UCOL_ALTERNATE_HANDLING` | "non-ignorable"/"shifted" -> `UCOL_NON_IGNORABLE` / `UCOL_SHIFTED` |
| `maxVariable` | `ucol_setMaxVariable` | "punct"/"space" -> `UCOL_REORDER_CODE_PUNCTUATION` / `UCOL_REORDER_CODE_SPACE` |
| `normalization` | `UCOL_NORMALIZATION_MODE` | `UCOL_ON` / `UCOL_OFF` |
| `backwards` | `UCOL_FRENCH_COLLATION` | `UCOL_ON` / `UCOL_OFF` |

`Parse` already fills MongoDB's defaults (strength 3, caseFirst off, alternate
non-ignorable, maxVariable punct, the bools false). Those defaults are applied
as explicit `setAttribute` calls too, so the collator never relies on
locale-data defaults that could differ from MongoDB's declared defaults.

## 6. Go API and the comparator

`internal/collation.Comparator` keeps its three methods. Internally it holds an
`*icu.Collator` instead of an `*collate.Collator`:

- `CompareStrings(a, b string) int` -> `icu.StrColl(coll, a, b)` via
  `ucol_strcollUTF8`. Returns -1/0/1.
- `EqualStrings(a, b string) bool` -> `CompareStrings == 0`.
- `Key(s string) []byte` -> UTF-16 encode `s`, `ucol_getSortKey` (size probe +
  fill), return the bytes. Sort keys order bytewise the same as the collator
  orders strings, so they remain usable for collated index storage / sorted
  iteration.

`Collation.Comparator()` still returns `nil` for a simple/absent collation, so
callers keep their existing "nil comparator means binary compare" fast path.
`collCompare`, `collCompareOrderOp`, and `lessFuncCollated` in
`internal/handler/common/collation_compare.go` need no change -- they call the
same three methods.

## 7. Locale validation (the one non-mechanical part)

MongoDB rejects an unknown locale with `BadValue (2)` ("Field 'locale' is
invalid ..."). ICU's `ucol_open` does not fail on an unknown locale -- it falls
back toward root and reports the fallback through the status warning
(`U_USING_DEFAULT_WARNING` when it fell all the way to root,
`U_USING_FALLBACK_WARNING` for a partial match) and via
`ucol_getLocaleByType(UCOL_VALID_LOCALE)`.

First-cut rule: after `ucol_open`, if the status is `U_USING_DEFAULT_WARNING`
(root fallback) for a non-empty, non-"root"/"simple" requested locale, treat the
locale as invalid and return MongoDB's `BadValue`. This catches the silent-root
behavior that bit x/text.

Open question: MongoDB maintains its own explicit list of accepted locales and
does not accept every ICU locale keyword form. Matching that list exactly (vs.
"whatever ICU recognizes") is a fidelity choice to settle -- likely by pinning
the MongoDB-accepted locale set as data and validating against it before
`ucol_open`. Deferred to the parity phase; the warning-based rule is the
starting point.

We pass only the bare locale to `ucol_open` and set all options via attributes;
we never accept BCP-47 `-u-` keyword forms in the locale string (MongoDB does
not either). This keeps validation and option handling separate and predictable.

## 8. Lifecycle and concurrency

A `UCollator` must be closed (`ucol_close`) and is comparatively expensive to
open (locale data load). Two facts shape the design:

- Opening per query would allocate/free C memory on every find/update. The same
  small set of collation specs recurs heavily (an index's collation reused
  across many operations).
- ICU collators are immutable once configured; `ucol_strcollUTF8` and
  `ucol_getSortKey` take a `const UCollator *` and are safe for concurrent use
  from multiple goroutines as long as no attribute is set after construction.

So: a process-wide **collator cache** keyed by the canonical resolved spec
(the output of `Resolve()` serialized deterministically). The cache builds a
collator once (open + all setAttribute + validate + freeze by never mutating
again) and hands out non-owning `Comparator` handles that share it. The cache
owns the lifetime; callers never `Close`. This has three payoffs: call sites
stay exactly as they are (no `defer cmp.Close()` churn), C allocations are
amortized, and concurrent reads are safe.

Backstop: the binding's `Collator` still registers a `runtime.Cleanup`
finalizer calling `ucol_close`, so a collator that somehow escapes the cache is
not leaked. Concurrency-safety of `ucol_strcollUTF8` on a shared const collator
must be confirmed on the ICU version we ship (holds for 57.1 and 72.1); if it
ever does not, the fallback is a small `sync.Pool` of per-goroutine collators
per spec.

## 9. Sort keys, index persistence, and version pinning

**Assessed 2026-08-07, conclusion corrected 2026-08-11.** DumboDB persists no ICU
sort keys *today* -- but that is a consequence of collated reads being unoptimized,
which is a known defect we intend to fix, not a property to design around. The
earlier reading of this section ("pinning is cheap insurance, the version stays
reversible") was wrong: it mistook the current unoptimized state for a stable one.

Evidence for the current state, traced through the collated-index code:

- `Comparator.Key()` (the ICU sort-key generator) is **dead code** -- called
  nowhere in the repo.
- What a collated index persists is the collation **spec**, not keys:
  `dolt/index_persist.go` stores `CollationBSONHex` (the `{locale, strength,
  ...}` document as BSON).
- On-disk index entries store **raw values**: `extractIndexKey` returns
  `doc.Get(field)` verbatim, so the prolly-map is ordered by binary encoding,
  not collation order.
- Collated uniqueness is an **O(N) live value scan**: `scanUniqueConflict`
  iterates all rows, re-reads each document, and compares live via
  `indexKeysEqualColl -> cmp.EqualStrings`. No stored key participates.
- Collated queries **disable every index-accelerated path**: `!params.Collated`
  gates the secondary-index lookup, the `_id` point lookup, and the byte
  prefilter, so collated reads full-scan and filter live through the collator.

That describes only today's state, and today's state is not the target. The
current arrangement bought version-flexibility at a price nobody intends to keep
paying: collated unique is an O(N) scan per insert and every collated query
full-scans. Fixing that is the *goal* (`workspace-alp.15`), and the fix is
collation-ordered indexes -- storing entries in ICU sort-key order for O(log N)
collated range and unique lookups, as MongoDB does. That makes
`Comparator.Key()` live by design. So the honest reading is not "pinning is
optional"; it is **pinning is mandatory, and the deadline is whenever
collation-ordered indexes land.**

**Why this binds harder in DumboDB than in MongoDB.** For MongoDB an ICU change
is a rebuild: drop the index, rebuild it under the new collator, move on. DumboDB
cannot do that, because its index storage model assumes key encoding is
*deterministic across branches and across time*:

- `secondary-index-structural-sharing.md` B1 ("same doc, same bytes") has two
  branches inserting the same doc produce **byte-identical leaf chunks** so the
  chunk store deduplicates them. Its stated reason is that "key encoding is
  branch-independent." ICU sort keys are branch-independent only while every
  writer shares one ICU version.
- P2 ("small writes share storage with the previous version") and B3 ("merged
  indexes share storage with both parents") likewise depend on chunk addresses
  matching across time and across merge parents.

So an ICU version change does not merely invalidate the live index. It makes
chunks written before the change un-shareable with chunks written after
(every collated index rewrites in full, permanently, since history is retained),
and it makes two branches whose writes straddle the change **incomparable**: a
merge would be combining two trees under different sort orders. Worse, history is
immutable -- rebuilding the tip does not and cannot repair the index trees inside
existing commits, so old commits keep old-ordered trees forever. There is no
migration that fixes this after the fact. The ICU version is therefore part of
the storage format, spanning all of history, not a per-index rebuildable detail.

**Design obligations that follow.** Once keys are persisted:

1. The ICU version must be recorded in index metadata alongside the existing
   `CollationBSONHex`, so a mismatch is *detected* rather than silently
   producing wrong answers.
2. A version mismatch between merge parents must fail loudly, not merge.
3. An ICU version change must be treated as a storage-format break -- a new
   index identity, not an in-place upgrade.

**Sequencing consequence: the version must be chosen before this ships, not
after.** Because the choice is permanent once keys land, and because it cannot be
corrected by any later migration, it cannot be deferred to an implementation
detail. See section 1b of the divergence doc, which now carries a recommendation
rather than an open question.

## 10. The version-echo tension

`listIndexes` echoes `version: "57.1"` and a parity test pins it against the
mongod under test. We keep echoing MongoDB's compatibility label ("57.1") even
if the linked ICU is 72.1 -- the string is a MongoDB-reported constant, not
something ICU emits. That is honest about the *protocol* (we match Mongo's
echo) while the *behavior* tracks the linked ICU. The behavioral gap between the
echoed label and the linked engine is precisely what the divergence measurement
quantifies; this binding must not pretend the label implies 57.1 behavior.

Under the recommended 57.1 pin this tension dissolves: label and behavior
converge, and the echo stops being a compatibility fiction. The tension only
needs managing if a modern ICU is chosen instead -- in which case the honest
options are to keep echoing "57.1" as a protocol constant while documenting that
behavior differs, or to echo the real version and accept a parity failure on the
field. That choice belongs with the version decision, not here.

## 11. Build and platform

Copy `go-icu-regex`'s cgo directives into `internal/collation/icu`:

```
// #cgo !windows LDFLAGS: -licui18n -licuuc -licudata
// #cgo icu_static CPPFLAGS: -DU_STATIC_IMPLEMENTATION
// #cgo windows,icu_static LDFLAGS: -lsicuin -lsicuuc -lsicudt
// #cgo windows,!icu_static LDFLAGS: -licuin -licuuc -licudt
// #include "unicode/ucol.h"
// #include "unicode/uloc.h"
// #include <stdlib.h>
```

`CGO_ENABLED=1` is already required by the existing `go-icu-regex` link, so this
adds no new build constraint. Cross-compilation and static-link stories are
inherited from that dependency, not created here.

**Partly superseded by section 11a.** Those directives link the *host's* ICU,
which is what `go-icu-regex` wants but not something a pinned collation engine can
accept. Once collation moves to the bundled `go-icu-collate` module, its link
flags come from that module's vendored build rather than from `-licui18n` against
the system copy. What survives from this section is the `#include` surface and the
fact that cgo is already a given.

## 11a. Packaging: bundling ICU (spiked 2026-08-08, decided 2026-08-11)

Any pinned version requires bundling: the host ICU is whatever the OS ships (72.1
on this box) and is not a version we control, so linking it -- what
`go-icu-regex` does -- cannot pin anything. We therefore bundle ICU in a
dedicated module (proposed `dolthub/go-icu-collate`, mirroring the `gozstd` /
`go-icu-regex` house style).

Bundling *decouples* the version choice from the host; it does not make that
choice. Which version to pin is open and tracked in section 1b of the divergence
doc. Everything below is about packaging mechanism and holds for any version.

Two non-WASM packaging options were spiked against real ICU 57.1 on this box
(GCC 12.2, 16 cores). WASM/wazero is excluded (known wazero deadlock history).

**Provenance caveat (2026-08-11):** the spike's build tree and artifacts no longer
exist on the dev box, so the figures below are recorded results from that session
rather than currently reproducible measurements. They are consistent enough to
choose a direction on; re-spike before leaning on any individual number.

- **"source"** -- vendor ICU 57.1 `common`+`i18n` C++ source; cgo compiles it at
  the consumer's build (the `gozstd` model: no prebuilt artifacts).
- **"library"** -- prebuild static `lib*.a` per platform; the module ships/links
  those.

Measured results:

| Metric | source | library |
|---|---|---|
| Cold build cost | ~48s (full static ICU build, all data) | ~7s (link prebuilt `.a` + cgo glue) |
| Warm / incremental (Go build cache) | 0s | 0s |
| Repo ships | ICU C++ source (~17.5 MB for common+i18n) | prebuilt `.a` per GOOS/GOARCH |
| Per-platform maintenance | none (compiles anywhere with a toolchain) | a build matrix you own |
| Wrapper packaging effort | fiddly (see below) | trivial (run ICU configure+make, ship `.a`) |
| cgo required | yes | yes |
| Pins 57.1 | yes (verified: linked binary reports 57.1) | yes (verified) |

Both were validated end to end: a Go cgo program linked **ICU 57.1** (while the
system has 72.1), fully statically (`ldd` shows no dynamic ICU), and produced
correct collation -- secondary `Alice`==`alice`, primary `cafe`==`cafe-acute`,
secondary `cafe`!=`cafe-acute`, tertiary `a`<`b`.

Key measured facts that shape the choice:

- **Build time is a non-issue.** A *full* ICU 57.1 static build is ~48s on 16
  cores, clean on GCC 12 with `-std=c++11`. The Go build cache makes it 0s after
  the first build; the 48s is a cold-CI cost only. So "source" is not slow, and
  the ~48s-vs-7s gap is small in absolute terms and disappears with caching.
- **The data trims hard.** The full `icudt57l.dat` is 25 MB, but the core UCA
  collation table is ~164 KB and per-locale tailorings are KB each -- the 25 MB
  is ~99% non-collation (formatting, timezones, transliteration). A
  collation-only data blob is low-single-digit MB, shrinking the library-path
  payload and both binaries. Data trimming applies to both options.
- **Static archives (untrimmed):** `libicuuc.a` 3.8 MB, `libicui18n.a`
  (contains `ucol`) 7.4 MB, `libicudata.a` 25 MB. Final-link dead-code
  elimination drops unreferenced members, so binary bloat is far below the `.a`
  sizes even before data trimming.
- **Public headers are portable.** `common/unicode/platform.h` has zero autoconf
  substitutions and detects the platform via static `#ifdef`, so vendored
  headers need no per-platform generation.

The asymmetry is **wrapper packaging effort**, and it is smaller than first
assessed. cgo compiles only the source files in a package directory, not subdirs,
so a "source" wrapper must present ICU's sources as flat cgo package(s). But
re-checked against the `release-57-1` tree (2026-08-11), `common/` and `i18n/`
are *already flat*: 182 and 198 `.c`/`.cpp` files respectively, each with exactly
one subdirectory (`unicode/`, headers only, which needs nothing but an include
path). So the "flatten ~380 files" step is a copy of two flat directories, not a
tree rewrite. The file count was right; the difficulty was overstated. The real
one-time work in the source path is generating the trimmed collation data blob.
The "library" wrapper is still less work up front -- run ICU's configure+make,
ship the `.a` -- at the cost of owning a per-platform build matrix and hosting
binary archives.

**Decision (2026-08-11): source.** Auditability decides it, not build time.
Vendored C++ source is reviewable in-tree, scannable by the same dependency and
static-analysis tooling as the rest of the repo, and reproducible from source by
anyone building DumboDB. Prebuilt `.a` archives are opaque blobs a reader must
take on trust and tooling cannot inspect -- a worse supply-chain posture for a
dependency we intend to pin for the lifetime of the storage format, and worse
still if the pinned ICU turns out to be old enough to need its own CVE audit
(divergence doc section 1b). The build-time case for "library" is weak on the
recorded numbers anyway (~48s cold, 0s cached), and the two paths produce
identical runtime behavior. This trades a one-time data-generation task for
permanent auditability.

Coexistence: a bundled static ICU (collation) and the system's dynamic ICU
(regex, via `go-icu-regex`) share one process safely regardless of which two
versions they are -- ICU version-suffixes its symbols (`ucol_open_57` vs
`uregex_open_72`). The spike confirmed this concretely: its binary linked 57.1
statically against a host carrying 72.1, with no symbol conflict.

## 12. Migration from x/text

1. Add `internal/collation/icu` (this binding) with unit tests.
2. Reimplement `Comparator` internals on `icu.Collator`; keep method signatures.
3. Add the collator cache; wire `Collation.Comparator()` to it.
4. **De-lossy the handler.** Today only `strength` and `numericOrdering` reach
   the comparator; the mapping (section 5) now honors all nine fields. Confirm
   each field flows from the parsed spec into `setAttribute`.
5. Remove the `golang.org/x/text/collate` + `language` imports from
   `internal/collation` (they may remain elsewhere; check before dropping from
   go.mod).
6. Locale validation: return `BadValue` for unknown locales (section 7),
   matching Mongo, where today x/text silently used root.

Each step builds and passes the affected module's tests before commit, per repo
rules.

Step 0, before any of the above: the ICU version is settled explicitly (divergence
doc section 1b). Steps 1-6 do not depend on which version wins, but shipping
collation-ordered indexes afterwards does, and by then it is unrepairable
(section 9). Do not let the vendored module's initial checkout be the thing that
decides it.

## 13. Testing

- **Unit witnesses:** the Phase 0 minimal-pair witnesses (cafe/cafe-acute/CAFE for
  strength, a/A for caseFirst, the French cote quartet for backwards, etc.)
  become table tests asserting the ICU comparator's compare/equality results --
  now expecting *correct* behavior for the options x/text could not express.
- **Parity corpus:** the tailored-locale corpus from the divergence plan becomes
  end-to-end DumboDB-vs-Mongo parity tests, populating the empty chart rows
  (non-en locales, strength 3/4/5, caseFirst, backwards, numericOrdering,
  invalid-locale rejection). These lock fidelity once the engine is in.
- **Concurrency:** a race test hammering one cached collator from many
  goroutines via CompareStrings/Key.

## 14. Risks and open questions

1. **Concurrency safety of shared const collators** -- believed safe on 57.1/72.1;
   must be confirmed, with the `sync.Pool` fallback ready.
2. **Exact MongoDB locale acceptance list** vs. ICU's recognized set (section 7).
3. **Sort-key persistence** and its coupling to the ICU version -- the highest
   consequence item here, and not deferrable (section 9). Collation-ordered
   indexes are the goal, they persist ICU sort keys, and in a version-controlled
   store that makes the ICU version part of the storage format across all
   history, unrepairable by migration.
4. **Which ICU version to pin** -- recommended 57.1 (divergence doc section 1b),
   awaiting sign-off. The risk is one of sequencing: this must be an explicit
   decision made before keys land, never something an implementation detail
   settles by default.
5. **Collation-only data trimming.** With the source packaging path (section 11a),
   generating a trimmed ICU data blob is the main one-time task and the main place
   to get it wrong: dropping a locale's tailoring silently degrades it to root
   ordering rather than failing loudly. Needs a test that every locale MongoDB
   accepts still collates as tailored after trimming.
6. **strength 5 (identical)** ties down to code-point comparison; confirm ICU's
   identical-level behavior matches Mongo's for equal-primary strings.
7. **Cache growth** -- specs are low-cardinality in practice, but the cache
   should bound or evict rather than grow unboundedly if a client sends many
   distinct specs.

## 15. Non-goals

- Deciding which ICU version to pin -- owned by the divergence doc (section 1b),
  which recommends 57.1. Not a non-goal in the sense of "someone else's problem
  later": it is a prerequisite for collation-ordered indexes, and section 9
  explains why it cannot be corrected afterwards. This binding's code compiles
  against any version.
- A pure-Go fallback. `go-icu-regex` has a pure-Go regex fallback; collation has
  no comparable pure-Go equal (that was the whole x/text finding), so a
  cgo-only collation path with no fallback is accepted here.
- Reworking how indexes store keys -- only surfacing the version coupling.
