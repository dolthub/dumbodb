# FerretDB Integration Test Analysis

---

## Run 3: MongoDB 7.0.8 (target version) — hq-iay1f

**Date**: 2026-03-24
**Log**: `/home/ubuntu/mongodb-reference.txt` (7.0.8 run)
**Issue**: hq-iay1f
**Prior issues**: hq-xjiy5 (8.2.6 run), hq-u4ih8 (7.0.31 run)

### TL;DR

**This run is a corrupted infrastructure failure — not a valid MongoDB 7.0.8 baseline.**

All 132 tests fail, but for one reason: `TestServerStatusCommandStress` (the first
test to execute) exhausted the OS file-descriptor limit (`TooManyFilesOpen`, errno 24),
causing MongoDB to close all open connections and become unreachable. Every subsequent
test failed immediately with `connection refused` — not because of MongoDB 7.0.8
behaviour, but because MongoDB had crashed.

**Outcome**: This log cannot be used as a 7.0.8 baseline. The run must be repeated
with an adequate file-descriptor limit (`ulimit -n 65536` or equivalent before starting
MongoDB and the test suite).

---

### 1. Failure Count and Classification

**132 tests failed. 0 tests passed.**

| Category | Tests | Description |
|----------|-------|-------------|
| A: Root cause — file-descriptor exhaustion | 1 | `TestServerStatusCommandStress` triggers `TooManyFilesOpen` + socket EOF cascade |
| B: Cascading — MongoDB unreachable after crash | 131 | All subsequent tests: `connection refused` to 127.0.0.1:37017 |
| **Total** | **132** | |

There are **no version-gate failures, no auth/SASL failures, no error-message
mismatches, and no behavioural differences** observable in this log. Every single
failure traces back to MongoDB being unreachable.

---

### 2. Root Cause: `TooManyFilesOpen` in TestServerStatusCommandStress

`TestServerStatusCommandStress` is a concurrent stress test that opens many
simultaneous connections. It was the very first test to run. Within its 42-second
window it generated:

1. `(TooManyFilesOpen) 24: Too many open files` — the OS EMFILE limit was hit
2. Multiple `socket was unexpectedly closed: EOF` errors — MongoDB started dropping connections
3. Connection pool cleared repeatedly — the driver gave up on the connection pool

MongoDB then became completely unresponsive. All subsequent tests, including basic
ones like `TestPingCommand` and `TestBuildInfoCommand`, failed immediately:

```
server selection error: server selection timeout, current topology:
  { Type: Unknown, Servers: [{ Addr: 127.0.0.1:37017, Type: Unknown,
    Last error: dial tcp 127.0.0.1:37017: connect: connection refused }] }
```

The test binary ran on macOS/ARM64 (stack traces show `asm_arm64.s` and path
`/Users/neil/Documents/dumbodb/ferretdb/integration/`) — not inside Docker.
macOS defaults to a low per-process file-descriptor limit (256–1024), which a
concurrent stress test easily exhausts.

---

### 3. Are Any Failures Explainable vs. Surprising?

All 132 failures are **explainable** under a single root cause. There are
**no surprising failures** that require investigation.

**Not visible in this log:**
- Whether MongoDB 7.0.8 would pass the non-auth tests (expected: yes)
- Whether auth/SASL tests would still fail (expected: yes — same no-auth
  configuration issue identified in the 7.0.31 run, Category B)
- Whether any genuine 7.0.8 regressions exist (cannot determine)

---

### 4. Comparison Across Runs

| Run | Issue | MongoDB | Failures | Root cause |
|-----|-------|---------|----------|------------|
| 1 | hq-xjiy5 | 8.2.6 | 55 | Version mismatch (8.x vs expected 7.0.x) |
| 2 | hq-u4ih8 | 7.0.31 | 25 | Auth infrastructure + minor-version drift |
| 3 | hq-iay1f | 7.0.8 | 132 | **Infrastructure crash (ulimit EMFILE)** |
| Expected | — | 7.0.8 (Docker) | ~0–10 | Auth/SASL only (no users configured) |

Run 3 is a regression in **run quality**, not MongoDB quality. The test environment
was not configured correctly for a high-concurrency stress suite.

---

### 5. Recommendation: Rerun with Correct Environment

**Immediate fix — raise file-descriptor limit before running:**

```bash
ulimit -n 65536
cd ferretdb && docker compose up -d mongodb
# Wait ~5s, then:
make mongodb-reference
```

**Or add to the test script (`scripts/mongodb-reference.sh`):**

```bash
# Raise fd limit before running integration tests
ulimit -n 65536 2>/dev/null || true
```

**Why Docker matters here:** Running MongoDB inside Docker with `--ulimit nofile=65536:65536`
(or via the compose file) isolates the fd limit from the host macOS default. The
previous runs that produced 55 and 25 failures presumably had MongoDB running in Docker
with adequate limits; this run had MongoDB running directly on the macOS host.

**Expected outcome after fix**: ~0–10 failures (auth/SASL infrastructure only),
matching the projection from the 7.0.31 analysis.

---

### 6. No New Issues Filed

This analysis does not surface any dumbodb-specific bugs or new actionable items
beyond what was identified in the 7.0.31 run (hq-u4ih8). The auth/SASL
infrastructure gap remains the only known non-version failure category.

The delta methodology from hq-u4ih8 still applies unchanged: failures present
in `ferretdb-scorecard` (dumbodb) but **absent** in a clean 7.0.8 reference run
are genuine dumbodb-vs-MongoDB gaps.

---

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

**None of the 25 failures are dumbodb bugs.** The fix is still to use MongoDB 7.0.8
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

**DumboDB relevance**: None. These are upstream minor-version additions to MongoDB's
own responses. Not observable in dumbodb behaviour.

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

**DumboDB relevance**: Not directly. These are suite infrastructure failures —
the tests need a MongoDB with real auth users and `--auth` enabled. The
scorecard intentionally runs without auth; this category would also fail against
any no-auth MongoDB. When comparing the dumbodb ferretdb-scorecard to this
reference, these 10 tests are equally broken on both sides and should be
excluded from the delta.

**Note**: In the 8.2.6 run, most of these likely also failed (they were counted
in the 55), but possibly under different error messages. The no-auth pattern
is stable regardless of major version.

### Category C: Response Shape Addition — HostInfo (1 test)

| Test | Change |
|------|--------|
| `TestHostInfoCommand` | 7.0.31 adds `numCoresAvailableToProcess` field to the `system` sub-document of `hostInfo` |

**DumboDB relevance**: None. Minor kernel metadata field not present in 7.0.8.

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

**DumboDB relevance**: Low. The error code is the same; only the symbolic name
changed. dumbodb should target the same code (40414). If dumbodb uses `Location40414`,
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

**DumboDB relevance**: Medium for the `TestQueryBadFindType` code change. dumbodb
should match 7.0.8 behaviour (code 73, `InvalidNamespace`) for namespace type
validation. If dumbodb returns code 2 (`BadValue`) for these cases it would match
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

**DumboDB relevance**: High if dumbodb aims to match 7.0.8 bitwise semantics. If
dumbodb returns the same results as 7.0.31 (excluding bare decimal128), it would
pass tests against this reference but differ from the 7.0.8 target. This is
a genuine bitwise decimal128 edge case worth tracking.

---

## 3. Summary by Root Cause

| Root Cause | Tests | DumboDB-relevant? |
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

## 7. Delta Methodology for DumboDB Scoring

When comparing `ferretdb-scorecard` (dumbodb) against `mongodb-reference` (7.0.31
for now), the meaningful metric is failures present in ferretdb-scorecard **but
absent** in mongodb-reference. The 25 baseline failures are noise, not bugs.

Once 7.0.8 is in use as the reference, the delta will be much cleaner:
- Any remaining failures in `ferretdb-scorecard` not in `mongodb-reference`
  are genuine dumbodb-vs-MongoDB gaps.
- The bitwise decimal128 category (F) is worth watching specifically — if dumbodb
  passes those tests, it confirms dumbodb matches 7.0.8 semantics for decimal128
  bitwise operations.
