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

## Current Goal

The scorecard shows 0/117 tests passing. The immediate goal is to get tests
passing one at a time. Pick ONE failing test, make it pass, verify the
scorecard improves, and push. Small, verified improvements only.
