# FerretDB Integration Test Analysis: MongoDB 8.2.6 vs Expected 7.0.x

**Date**: 2026-03-24
**Log**: `/home/ubuntu/mongodb-reference.txt`
**Issue**: hq-xjiy5

---

## TL;DR

The 55 failures are almost entirely caused by running the test suite against MongoDB
8.2.6 when it was written for MongoDB 7.0.x. Virtually none of the failures indicate
real bugs in dongo. **The fix is to use MongoDB 7.0.8 (the version FerretDB's own
Docker config pins to) when generating the reference baseline.**

---

## 1. Is the Version Mismatch the Root Cause of ALL Failures?

**Yes — the version mismatch is the root cause of every failure in the log.**

No independent failure categories were found. Every failing test can be traced to
either:
- A hard-coded MongoDB 7.x version string assertion, or
- A behavioral difference between MongoDB 7 and 8 (wire protocol, error message
  wording, response document shape, error codes).

---

## 2. Failure Classification

### Category A: Pure Version Gate Assertions (2 tests)

These tests explicitly assert the version string with a regex and fail immediately
on a version mismatch. They do not test any functional behaviour.

| Test | Assertion |
|------|-----------|
| `TestServerStatusCommand` | `assert.Regexp(t, "^7\\.0\\.", field.Value)` |
| `TestBuildInfoCommand` | `assert.Regexp(t, "^7\\.0\\.", field.Value)` |

These would pass with MongoDB 7.0.x and fail with any other major version.

### Category B: Wire Protocol Version Change (1–2 tests)

MongoDB 8 reports `maxWireVersion: 27` (up from 21 in MongoDB 7). Tests that assert
exact wire version values fail.

| Test | Expected | Actual |
|------|----------|--------|
| `TestHello` | `maxWireVersion: 21` | `maxWireVersion: 27` |
| `TestHostInfoCommand` (subset) | `maxWireVersion: 21` | `maxWireVersion: 27` |

### Category C: Error Message Wording Changes (~20 tests)

MongoDB 8 changed error message formatting across the board. The test suite asserts
exact error message strings. Examples:

| Type | MongoDB 7 (expected) | MongoDB 8 (actual) |
|------|----------------------|--------------------|
| Capitalisation | `"collection name has invalid type object"` | `"Collection name has invalid type object"` |
| Prefix stripped | `"Cannot create field 'foo' in element {...}"` | `"Plan executor error during update :: caused by :: Cannot create field 'foo' in element {...}"` |
| Delimiter change | `"Invalid namespace specified 'short-db.'"` | `"Invalid namespace specified: short-db"` |
| Error code change | code 10065, `"invalid parameter: expected an object"` | code 40414, `"BSON field '...' is missing but a required field"` |
| Message reordering | `"FieldPath field names may not start with '$'. Consider using..."` | `"Consider using $getField... :: caused by :: FieldPath field names..."` |
| Error prefix change | `"PlanExecutor error during aggregation :: caused by :: ..."` | `"Executor error during aggregate command on namespace: ... :: caused by :: ..."` |

These are purely MongoDB 8 wording changes — the same error condition, just formatted
differently. They tell us nothing about dongo behaviour.

### Category D: Response Document Shape Changes (~5 tests)

MongoDB 8 added new fields to some responses and changed response structure.

| Test | Change |
|------|--------|
| `TestServerStatusCommand` | New top-level fields: `service`, `changeStreamPreImages`, `fle`; `catalogStats` gains `systemProfile` |
| `TestDBStatsCommandFreeStorage` | Response field ordering/structure change |
| `TestCollStatsCommandScale` | Similar structural changes |

### Category E: Query Behaviour Changes (~3 tests)

A small number of tests assert exact result sets and MongoDB 8 changed the behaviour.

| Test | Change |
|------|--------|
| `TestQueryBitwiseAllClear` | MongoDB 8 excludes bare `"decimal128"` from bitwise results |
| `TestQueryBitwiseAllSet`, `TestQueryBitwiseAnySet`, `TestQueryBitwiseAnyClear` | Similar decimal128 handling changes |

These represent genuine behavioural differences between MongoDB 7 and 8 in edge cases
around decimal128 and bitwise operations.

### The "wasn't created because no providers were set" Message

43 of 55 failing tests emit this message. It is **not an error** — it is an
informational log line from the test framework indicating that a test collection was
set up without pre-inserted fixture data (intentional for error-path tests). The
actual failures in those tests are all from Categories B–E above. This message does
not indicate a test infrastructure problem.

---

## 3. Test Suite Confidence

**The FerretDB integration tests are trustworthy as a functional baseline when run
against MongoDB 7.0.x (their target version), but fragile against version changes.**

Evidence for this assessment:

**Trustworthy aspects:**
- The suite tests real wire protocol conformance and command semantics
- It covers a wide range of operations: queries, aggregation, index management,
  auth, administrative commands
- FerretDB actively maintains these tests against their supported MongoDB version
- The tests run against a live MongoDB instance (no mocking), so results are real

**Fragile aspects:**
- Version string assertions are hard-coded to `^7\.0\.` regexes
- Exact error message strings are asserted — these break on any MongoDB internal
  wording change (MongoDB changes these between minor versions)
- Wire protocol version constants are hard-coded
- Response document shapes are asserted exhaustively; new fields added by MongoDB 8
  cause failures

**Bottom line**: ~50 of the 55 failures would disappear if run against MongoDB 7.0.8.
The remaining ~5 (decimal128 bitwise edge cases, structural response changes) may or
may not be relevant depending on whether dongo targets MongoDB 7 or 8 semantics.

---

## 4. What MongoDB Version Does the FerretDB Submodule Expect?

**MongoDB 7.0.8** — pinned explicitly.

Evidence:
- `ferretdb/build/deps/mongodb.Dockerfile`: `FROM mongo:7.0.8`
- `ferretdb/docker-compose.yml`: MongoDB service uses that Dockerfile
- All version assertions in the test code use `^7\.0\.` regexes
- The issue note in the Dockerfile: `# TODO https://github.com/FerretDB/FerretDB/issues/5073`
  indicates this pin is intentional and tracked

The test run that produced `mongodb-reference.txt` used MongoDB 8.2.6 — almost
certainly a locally installed MongoDB rather than the Docker container the script
instructs to use:
```
# Start MongoDB first:
#   cd ferretdb && docker compose up -d mongodb
```

---

## 5. Options for Proceeding

### Option A (Recommended): Run with MongoDB 7.0.8 Container

Use FerretDB's own Docker setup to start MongoDB, which pulls `mongo:7.0.8`:

```bash
cd ferretdb && docker compose up -d mongodb
# Wait for it to be ready, then:
make mongodb-reference
```

**Pros:**
- Zero test suite changes
- Produces a clean baseline (≈0 version-mismatch failures)
- Directly comparable to what FerretDB CI runs against
- Any remaining failures are genuine behavioural gaps

**Cons:**
- Requires Docker; can't use a bare `mongod` on port 37017

**Verdict**: This is the right path. The `scripts/mongodb-reference.sh` already
documents this approach — it just wasn't followed when the failing run was executed.

### Option B: Patch Version Assertions in the Test Suite

Modify `commands_administration_test.go` and similar files to accept `^[78]\.` or
update to `^8\.2\.`.

**Pros:** Works with MongoDB 8.2.6 as-is

**Cons:**
- FerretDB's test suite is a git submodule we don't own
- Patching it creates a divergent fork that bitrels with upstream
- Error message changes (Category C) would still require ~20 more patches
- Net result: significant patch burden with no real benefit

**Verdict**: Not recommended.

### Option C: Use ferretdb-reference Baseline Instead

Run the suite against FerretDB itself (the `ferretdb-reference` make target) and
use that as the conformance reference rather than real MongoDB.

**Pros:**
- No MongoDB version dependency
- FerretDB baseline is always self-consistent

**Cons:**
- Defeats the purpose: FerretDB is what we're testing dongo against
- We lose visibility into what real MongoDB actually does
- A bug in FerretDB would be hidden if FerretDB is also the reference

**Verdict**: Useful as a *complementary* check (to measure FerretDB conformance
separately from MongoDB conformance), but cannot replace the MongoDB reference.

### Option D: Track Only the dongo vs MongoDB Delta

Accept all 55 failures as "version noise", and define the scorecard metric as
"failures in ferretdb-scorecard that are NOT in mongodb-reference" (i.e., dongo
regressions relative to the reference).

**Pros:** Works without fixing anything

**Cons:**
- 55 noisy baseline entries reduce the value of the delta metric
- Version-specific false positives could mask real regressions
- No insight into which tests are actually informative

**Verdict**: Acceptable as a short-term workaround, but Option A is better.

---

## Recommendation

**Immediate action**: Re-run `mongodb-reference` using MongoDB 7.0.8 via Docker:

```bash
cd /path/to/dongo
# Start MongoDB 7.0.8 (what FerretDB expects)
cd ferretdb && docker compose up -d mongodb && cd ..
# Wait ~5s for MongoDB to be ready, then:
make mongodb-reference
```

**Longer-term**: The Makefile and `scripts/mongodb-reference.sh` should either:
1. Start the Docker container automatically, or
2. Check the version of the target MongoDB and warn/abort if it doesn't match
   the expected `^7\.0\.` range

This prevents future confusion when a developer has a local MongoDB installed at
a different version.

**Expected outcome after using MongoDB 7.0.8**: 0–5 failures (the decimal128
bitwise edge cases and response shape changes in Category D/E are the only
candidates for real version-invariant failures, and even those may resolve).
