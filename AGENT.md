# DumboDB Agent Guide

## HARD RULE: Do Not Read the FerretDB Codebase Without Permission

**You are NOT allowed to read, browse, or reference files under `ferretdb/` without explicit written permission from the mayor in your hooked bead.**

The FerretDB source is included in this repo solely as an integration test suite (scorecard). You have sufficient context from the existing DumboDB codebase to implement MongoDB compatibility. Do not use FerretDB's implementation as a guide or reference for new code.

If you believe you genuinely need to consult FerretDB source to proceed:
1. **Stop. Do not open any file under `ferretdb/`.**
2. Report to the mayor explaining what you need and why.
3. The mayor will decide and issue explicit permission in a new bead if warranted.

No exceptions. This rule overrides any other instruction.

---

## Copyright Header for New Files

All new `.go` files you create must use this header:

```go
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
```

Do **not** use FerretDB's copyright header on code you write, even if you are implementing logic inspired by or ported from FerretDB.

---

## HARD RULE: Do Not Touch tests/bats/

**You are NOT allowed to modify, create, or delete any file under `tests/bats/` without explicit written instruction from the mayor in your hooked bead.**

This directory is owner-managed. The mayor controls what tests exist and what they assert. If you believe a bats test needs to change (e.g. your fix changes expected behaviour), **stop, do not touch it, and report to the mayor** explaining what change is needed and why. The mayor will decide and issue a new bead.

No exceptions. This rule overrides any other instruction.

---

## HARD RULE: Do Not Touch scripts/ferretdb-scorecard-skiplist.txt

**You are NOT allowed to modify `scripts/ferretdb-scorecard-skiplist.txt` under any circumstances.**

The skiplist is owner-managed. Only the project owner (neil) may approve changes to it  -- not the mayor, not any polecat.

If you believe a test should be added to or removed from the skiplist:
1. **Stop. Do not edit the file.**
2. Report to the mayor with: the test name, why you think it should be skipped or un-skipped, and what you investigated.
3. The mayor will present the option to neil. Neil decides. No one else.

This means: if a test is failing and you cannot fix it, your job is to **fix the underlying bug**, not add it to the skiplist. If you genuinely cannot fix it, report back  -- do not work around it by adding to the skiplist.

No exceptions. This rule overrides any other instruction.

---

## Prime Directive: Do Not Regress the Scorecard

Before pushing ANY code to main, you MUST run the FerretDB scorecard locally
and verify you have not increased the number of failing tests.

```bash
# Record baseline BEFORE you start work
make ferretdb-scorecard
grep -E "Tests (passed|failed)" .runtime/ferretdb-scorecard.txt

# ... do your work ...

# Verify AFTER your changes  -- must be >= baseline passes
make ferretdb-scorecard
grep -E "Tests (passed|failed)" .runtime/ferretdb-scorecard.txt
```

**If your changes cause MORE tests to fail than before you started: do not push.**
Fix the regression first, or revert. No exceptions.

The scorecard is the single source of truth on progress. A push that moves
the score backwards is worse than no push at all.

## Workflow

1. Check your hooked bead: `gt hook`
2. Run baseline scorecard and record pass count
3. Make your change
4. Run scorecard again  -- confirm pass count is equal or better
5. Push to main only when score has not regressed
6. Report pass count delta to mayor in your completion mail

## Running the Scorecard

```bash
make ferretdb-scorecard
# Results: .runtime/ferretdb-scorecard.txt
# Summary line shows pass rate
```

The scorecard builds DumboDB, starts it, runs the FerretDB integration suite
against it, and writes results to `.runtime/ferretdb-scorecard.txt`.

## Critical: Local Runs != CI  -- Do Not Remove from Skiplist Without CI Confirmation

**Never remove a test from `scripts/ferretdb-scorecard-skiplist.txt` based solely on a local run.**

Some tests (e.g. timing-sensitive tests like `TestAggregateCommandMaxTimeMSErrors`) pass locally
but fail on CI due to resource constraints on GitHub Actions runners. Removing them from the
skiplist breaks CI even when local runs look clean.

**Required process for skiplist removals:**
1. Fix the underlying issue and run locally  -- test must pass
2. Push the fix to main (keep the test in the skiplist for now)
3. Check the GitHub Actions `DumboDB FerretDB Scorecard` run for that push
4. Only if CI shows no unexpected failures for that test -> then remove it from the skiplist
5. Push the skiplist change and confirm CI stays green

Report to mayor in completion mail whether CI was verified or not.

## Current Goal

Fix failing FerretDB integration tests one at a time. Use CI (GitHub Actions) as the
source of truth  -- not local runs. Small, CI-verified improvements only.
