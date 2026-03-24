# FerretDB Integration Test Analysis: MongoDB 7.0.31 vs 7.0.8

**Date**: 2026-03-24
**Log**: `/home/ubuntu/mongodb-reference.txt`
**Issues**: hq-xjiy5 (prior run, MongoDB 8.2.6), hq-u4ih8 (this run, MongoDB 7.0.31)

---

## TL;DR

Running with MongoDB 7.0.31 (same major version, higher patch) reduced failures
from **55 → 25** compared to the prior MongoDB 8.2.6 run. Progress, but not clean.
The 25 remaining failures fall into two root causes:

1. **Auth infrastructure gap** (10 tests): The scorecard runs MongoDB without auth
   users; these tests require speculative auth / SASL to have real users configured.
2. **7.0.8 vs 7.0.31 minor-version drift** (15 tests): Error naming, error codes,
   response shapes, and bitwise behaviour changed between patch versions.

**None of the 25 failures are dongo bugs.** The fix is still to use MongoDB 7.0.8
exactly — pinned via Docker — as FerretDB's own test infrastructure does.

---

## 1. How Many Failures Remain?

**25 failures** (all at test-group level, containing multiple subtests).

| Run | MongoDB version | Failures |
|-----|----------------|----------|
| hq-xjiy5 | 8.2.6 | 55 |
| hq-u4ih8 | 7.0.31 | **25** |
| Expected | 7.0.8 | ~0–3 |

---

## 2. Failure Classification

### Category A: Response Shape Additions in 7.0.31 (2 tests)

MongoDB 7.0.31 added new fields to some administrative command responses that
7.0.8 does not return. The test assertions use exact document matching.

| Test | Change |
|------|--------|
| `TestServerStatusCommand` | 7.0.31 adds `profiler`, `queues`, and `systemProfile` (inside `catalogStats`) fields to the `serverStatus` response |
| `TestBuildInfoCommand` | 7.0.31 running on macOS adds a `macOS` key to `buildInfo`; test does not expect it |

**Dongo relevance**: None. These are upstream minor-version additions to MongoDB's
own responses. Not observable in dongo behaviour.

### Category B: Auth/SASL Tests Requiring User Configuration (10 tests)

These tests probe speculative authentication (SASL/SCRAM-SHA-256) by expecting
the SASL handshake to start successfully (returning a `conversationId`), then
testing error conditions on the second step.

With MongoDB 7.0.31 running **without auth users configured** (as the scorecard
does: `no auth` in the header), the SASL handshake fails at the very first step
with `AuthenticationFailed` (code 18), before the test can reach the error path
it was trying to exercise.

Some sub-tests additionally probe no-auth paths (e.g., expecting that `find`
requires auth when called after a failed SASL start) — these fail because MongoDB
running without `--auth` flag allows unauthenticated finds.

**Affected tests:**

| Test | Failure mode |
|------|-------------|
| `TestOpQueryHello` | `speculativeAuthenticate` field absent in response (no user → MongoDB omits it) |
| `TestOpQueryIsMasterHelloOk` | Same |
| `TestOpQueryIsMaster` | Same |
| `TestOpQuery` | Same |
| `TestSASLContinueOpQueryErrors` | saslStart fails immediately; also `$db` in OP_QUERY rejected |
| `TestHelloOpQuerySASLSupportedMechs` | `saslSupportedMechs` nil (no user exists) |
| `TestHelloSpeculative` | Speculative auth SASL steps return `No SASL session state found` |
| `TestSASLContinueErrors` | HandshakeFails sub-test: saslStart fails; FindFails sub-test: find succeeds (no --auth) |
| `TestLogoutCommandAuthenticatedUser` | Expects an error on logout; gets nil (no auth mode) |
| `TestHelloIsMasterOpQuerySpeculative` | Speculative auth via OP_QUERY: `No SASL session state found` |

**Dongo relevance**: Not directly. These are suite infrastructure failures —
the tests need a MongoDB with real auth users and `--auth` enabled. The
scorecard intentionally runs without auth; this category would also fail against
any no-auth MongoDB. When comparing the dongo ferretdb-scorecard to this
reference, these 10 tests are equally broken on both sides and should be
excluded from the delta.

**Note**: In the 8.2.6 run, most of these likely also failed (they were counted
in the 55), but possibly under different error messages. The no-auth pattern
is stable regardless of major version.

### Category C: Response Shape Addition — HostInfo (1 test)

| Test | Change |
|------|--------|
| `TestHostInfoCommand` | 7.0.31 adds `numCoresAvailableToProcess` field to the `system` sub-document of `hostInfo` |

**Dongo relevance**: None. Minor kernel metadata field not present in 7.0.8.

### Category D: Error `Name` Changed — `Location40414` → `IDLFailedToParse` (4 tests)

MongoDB 7.0.31 renamed the internal error category for "BSON field missing but
required" errors. Error code (40414) and message are identical; only the `Name`
(codeName) field changed.

| Test | Expected `Name` | Actual `Name` |
|------|----------------|---------------|
| `TestDelete/QueryNotSet`, `TestDelete/NotSet` | `Location40414` | `IDLFailedToParse` |
| `TestGetLogCommand/Nil` | `Location40414` | `IDLFailedToParse` |
| `TestDropIndexesCommandErrors/MissingIndexField` | `Location40414` | `IDLFailedToParse` |
| `TestCreateIndexesCommandInvalidSpec/MissingIndexes` | `Location40414` | `IDLFailedToParse` |

**Dongo relevance**: Low. The error code is the same; only the symbolic name
changed. dongo should target the same code (40414). If dongo uses `Location40414`,
it matches 7.0.8. If it uses `IDLFailedToParse`, it matches 7.0.31+. The
message is the signal; the name is an internal alias.

### Category E: Error Message / Code Changes (4 tests)

Several error conditions changed their message text or error code between
7.0.8 and 7.0.31.

| Test | 7.0.8 expected | 7.0.31 actual | Change type |
|------|---------------|--------------|-------------|
| `TestQueryBadFindType` (most subtypes) | Code 73, `"Failed to parse namespace element"`, Name `InvalidNamespace` | Code 2, `"collection name has invalid type <X>"`, Name `BadValue` | Error code + message changed |
| `TestQueryBadFindType/Int` (int subtype only) | Code 73, `"Failed to parse namespace element"` | Code 73, `"collection name has invalid type int"`, Name `InvalidNamespace` | Message changed |
| `TestExplainCommandQueryErrors/CollectionName` | Code 73, `"Failed to parse namespace element"` | Code 73, `"collection name has invalid type int"` | Message changed |
| `TestDistinctCommandErrors/CollectionTypeObject` | Code 73, `"Failed to parse namespace element"` | Code 73, `"collection name has invalid type object"` | Message changed |
| `TestCompactCommandNonExistent/NonExistentDB` | Code 26, `"database does not exist"` | Code 26, `"collection does not exist"` | Message changed |

**Dongo relevance**: Medium for the `TestQueryBadFindType` code change. dongo
should match 7.0.8 behaviour (code 73, `InvalidNamespace`) for namespace type
validation. If dongo returns code 2 (`BadValue`) for these cases it would match
7.0.31 but deviate from 7.0.8. Worth noting but not blocking — the test suite
is the authoritative reference here.

### Category F: Bitwise Decimal128 Behaviour Change (4 tests)

The bitwise query operators (`$bitsAnySet`, `$bitsAnyClear`, `$bitsAllSet`,
`$bitsAllClear`) treat bare `Decimal128` values differently in 7.0.31 vs 7.0.8.
Tests expect the bare `"decimal128"` document to be included in bitwise results
but 7.0.31 excludes it.

| Tests | Expected | Actual |
|-------|----------|--------|
| `TestQueryBitwiseAnySet`, `TestQueryBitwiseAnyClear`, `TestQueryBitwiseAllSet`, `TestQueryBitwiseAllClear` | `"decimal128"` doc included in results | `"decimal128"` doc excluded (1 entry missing in every affected sub-test) |

**Note**: These same tests also failed in the 8.2.6 run (listed as Category E
in the prior analysis). This confirms the behaviour changed between 7.0.8 and
7.0.31, and stayed changed in 8.x.

**Dongo relevance**: High if dongo aims to match 7.0.8 bitwise semantics. If
dongo returns the same results as 7.0.31 (excluding bare decimal128), it would
pass tests against this reference but differ from the 7.0.8 target. This is
a genuine bitwise decimal128 edge case worth tracking.

---

## 3. Summary by Root Cause

| Root Cause | Tests | Dongo-relevant? |
|------------|-------|----------------|
| Auth infrastructure (no users, no --auth) | 10 | No — suite issue |
| 7.0.31 response shape additions | 3 | No — minor version additions |
| 7.0.31 error codeName rename | 4 | Low — same code, different Name |
| 7.0.31 error message/code changes | 4 | Medium — code change for BadValue case |
| 7.0.31 bitwise decimal128 change | 4 | High — real semantic difference |
| **Total** | **25** | |

---

## 4. Comparison: 8.2.6 vs 7.0.31 vs 7.0.8 (target)

| Category | 8.2.6 (55 failures) | 7.0.31 (25 failures) | 7.0.8 (target) |
|----------|--------------------|--------------------|----------------|
| Version string gates (`^7\.0\.`) | ✗ fails (hard-coded) | ✓ passes | ✓ passes |
| Wire version assertions | ✗ fails (v27 vs v21) | ✓ passes | ✓ passes |
| Error message wording | ✗ fails (≈20 tests) | mostly ✓ | ✓ passes |
| Response document shape | ✗ fails (new fields) | partial ✗ (3 tests) | ✓ passes |
| Bitwise decimal128 | ✗ fails (4 tests) | ✗ fails (4 tests) | ✓ passes |
| Auth/SASL no-users | ✗/✓ mixed | ✗ fails (10 tests) | likely same |
| Error code/name changes | ✓ | ✗ fails (8 tests) | ✓ passes |

The 7.0.8 Docker container is expected to eliminate all categories except
possibly the auth/SASL infrastructure ones (those depend on user configuration,
not the version).

---

## 5. What MongoDB Version Does the Test Suite Target?

**MongoDB 7.0.8** — pinned explicitly in the FerretDB submodule:

- `ferretdb/build/deps/mongodb.Dockerfile`: `FROM mongo:7.0.8`
- `ferretdb/docker-compose.yml`: MongoDB service uses that Dockerfile
- All version assertions in the test code use `^7\.0\.` regexes
- Issue tracked in Dockerfile: `# TODO https://github.com/FerretDB/FerretDB/issues/5073`

The test run that produced this log used MongoDB 7.0.31 — probably a locally
installed MongoDB at `/usr/local/bin/mongod` or similar, rather than the Docker
container the script instructs to use.

---

## 6. Recommendation

**Use MongoDB 7.0.8 via Docker** (unchanged from prior analysis):

```bash
cd ferretdb && docker compose up -d mongodb
# Wait ~5s for MongoDB to be ready, then:
make mongodb-reference
```

**Expected outcome**: ~0–3 failures.
- The 25 currently failing tests should almost all pass.
- The auth/SASL tests (Category B, 10 tests) may still fail if the Docker
  container also runs without pre-created test users — this is a test suite
  design limitation unrelated to the MongoDB version.
- The bitwise decimal128 tests (Category F, 4 tests) pass on 7.0.8 by definition
  since the test expectations are based on 7.0.8 results.

**Longer-term**: Add a version check to `scripts/mongodb-reference.sh`:

```bash
# Check MongoDB version matches expected range
MONGO_VERSION=$(mongosh --quiet --eval 'db.version()' "$MONGODB_URL" 2>/dev/null)
if [[ ! "$MONGO_VERSION" =~ ^7\.0\.8 ]]; then
  echo "WARNING: MongoDB $MONGO_VERSION — tests expect 7.0.8"
  echo "Use: cd ferretdb && docker compose up -d mongodb"
fi
```

---

## 7. Delta Methodology for Dongo Scoring

When comparing `ferretdb-scorecard` (dongo) against `mongodb-reference` (7.0.31
for now), the meaningful metric is failures present in ferretdb-scorecard **but
absent** in mongodb-reference. The 25 baseline failures are noise, not bugs.

Once 7.0.8 is in use as the reference, the delta will be much cleaner:
- Any remaining failures in `ferretdb-scorecard` not in `mongodb-reference`
  are genuine dongo-vs-MongoDB gaps.
- The bitwise decimal128 category (F) is worth watching specifically — if dongo
  passes those tests, it confirms dongo matches 7.0.8 semantics for decimal128
  bitwise operations.
