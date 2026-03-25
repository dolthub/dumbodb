# Dongo Agent Guide

## Prime Directive: Do Not Regress the Scorecard

Before pushing ANY code to main, you MUST run the FerretDB scorecard locally
and verify you have not increased the number of failing tests.

```bash
# Record baseline BEFORE you start work
make ferretdb-scorecard
grep -E "Tests (passed|failed)" .runtime/ferretdb-scorecard.txt

# ... do your work ...

# Verify AFTER your changes — must be >= baseline passes
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
4. Run scorecard again — confirm pass count is equal or better
5. Push to main only when score has not regressed
6. Report pass count delta to mayor in your completion mail

## Running the Scorecard

```bash
make ferretdb-scorecard
# Results: .runtime/ferretdb-scorecard.txt
# Summary line shows pass rate
```

The scorecard builds Dongo, starts it, runs the FerretDB integration suite
against it, and writes results to `.runtime/ferretdb-scorecard.txt`.

## Critical: Local Runs ≠ CI — Do Not Remove from Skiplist Without CI Confirmation

**Never remove a test from `scripts/ferretdb-scorecard-skiplist.txt` based solely on a local run.**

Some tests (e.g. timing-sensitive tests like `TestAggregateCommandMaxTimeMSErrors`) pass locally
but fail on CI due to resource constraints on GitHub Actions runners. Removing them from the
skiplist breaks CI even when local runs look clean.

**Required process for skiplist removals:**
1. Fix the underlying issue and run locally — test must pass
2. Push the fix to main (keep the test in the skiplist for now)
3. Check the GitHub Actions `Dongo FerretDB Scorecard` run for that push
4. Only if CI shows no unexpected failures for that test → then remove it from the skiplist
5. Push the skiplist change and confirm CI stays green

Report to mayor in completion mail whether CI was verified or not.

## Current Goal

Fix failing FerretDB integration tests one at a time. Use CI (GitHub Actions) as the
source of truth — not local runs. Small, CI-verified improvements only.
