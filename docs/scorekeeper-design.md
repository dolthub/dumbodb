# Scorekeeper Design Doc

**Issue**: hq-252ly
**Date**: 2026-03-24
**Status**: Implemented

## Overview

The Scorekeeper is a Gas Town dog plugin that monitors FerretDB integration test
results from the `docudolt-scorecard` GitHub Actions workflow. After each push to
`main`, it fetches test results, compares against the previous run, tracks
progress via bd issues, and flags regressions.

## Design Decisions

### 1. Architecture: Dog Plugin

**Decision**: Dog plugin (`scorekeeper-dog`) in the town-level plugins directory.

**Alternatives considered:**
- *Dedicated polecat formula*: Polecats are short-lived workers for specific issues.
  Monitoring is recurring and continuous — a patrol/dog pattern is the right fit.
- *Deacon patrol step*: The deacon handles lightweight periodic checks. Scorekeeper
  involves downloading artifacts and making judgment calls — better as a dog plugin
  with AI reasoning capability.
- *GH Actions webhook*: Would require a public webhook endpoint. Polling via `gh` CLI
  is simpler, reliable, and doesn't need infrastructure changes.

**Dog plugin**: Runs periodically (every 30 minutes), checks for new scorecard runs,
processes only runs newer than the last processed run.

### 2. State Persistence: JSON State File

**Decision**: State persisted to `$HOME/gt/.scorekeeper/state.json` (local runtime).

**Format:**
```json
{
  "last_run_id": 23516466950,
  "last_run_sha": "abc123",
  "last_run_at": "2026-03-24T23:05:16Z",
  "tests": {
    "TestFoo": {"status": "pass", "streak": 3},
    "TestBar": {"status": "fail", "issue": "do-42", "since_sha": "def456"}
  }
}
```

**Alternatives considered:**
- *bd issues only*: No separate state file, discover everything from bd issues.
  Problem: querying all issues by label is slow; bd is Dolt-backed and creates
  persistent commits on every write. Better to use bd only for the tracking
  issues themselves.
- *Dolt table*: Overkill for a single-rig plugin; adds schema migration complexity.
- *JSON file in repo*: Would require commits to track state, polluting git history.

**Local runtime file** avoids Dolt overhead and git noise while surviving reboots.

### 3. Fetching GH Actions Results: `gh` CLI Polling

**Decision**: Poll `gh api repos/dolthub/docudolt/actions/workflows/<id>/runs` to
find the latest completed scorecard run, then download the artifact.

**Flow:**
1. Find latest run newer than `last_run_id` with `conclusion: success` or `failure`
2. List artifacts for that run, find `ferretdb-scorecard`
3. Download and unzip the artifact
4. Parse `--- PASS:` and `--- FAIL:` lines from `ferretdb-scorecard.txt`

**Why polling**: The GH Actions workflow uploads a `ferretdb-scorecard` artifact.
The `gh` CLI can download it directly without needing webhook infrastructure.

### 4. Issue Management: One bd Issue Per Failing Test

**Decision**: One bd issue per failing test, in the docudolt rig, with label
`scorekeeper`.

**Title format**: `test-fail: TestFoo`
**Labels**: `scorekeeper,category:test-failure`
**Prefix**: `do-` (docudolt rig)

When a test transitions **failing → passing**: close the bd issue with reason
`test now passing as of <sha>`.

When a test transitions **passing → failing** (regression): create a new bd
issue and nudge the polecat whose commit likely caused it.

**Issue lifecycle:**
- Created: when a test appears in `--- FAIL:` and no open issue exists
- Updated: add SHA of run where it continues failing (periodic note)
- Closed: when test moves to `--- PASS:`

### 5. Regression Attribution: git log + Author Matching

**Decision**: When a regression is detected (test newly failing), look at the
last 3 commits to `main` and identify the most likely culprit commit.

**Method:**
```bash
# Get recent commits with author info
git -C <docudolt-repo-path> log --oneline -5 --format="%H %ae %s" origin/main
```

Match commit author email against known polecats. Polecats use their name
in commit messages (e.g., `feat: add foo (hq-xyz)`). The message includes
the bead ID in parentheses — use that to identify which polecat was working.

**Nudge** the polecat session if it still exists:
```bash
gt nudge docudolt/polecats/<name> "Regression: TestFoo started failing. Likely from your commit <sha>: '<msg>'. Please investigate."
```

If polecat session is gone, create a bd issue flagging the suspected commit.

## Implementation

The plugin is at `/home/ubuntu/docudolt/plugins/scorekeeper-dog/plugin.md`.

It runs as a Dog agent (Claude with AI judgment) on a 30-minute cooldown.
Steps:
1. Check for new completed scorecard runs
2. Download and parse test results
3. Compute diff vs previous state
4. Update state file
5. Create/close bd issues for changed tests
6. Nudge for regressions

## Seeding

On first run (no prior state), the plugin creates bd issues for all currently
failing tests in the most recent completed scorecard run. This provides an
initial baseline for the docudolt rig's failing tests.

The bead description notes seeding depends on `hq-dwzhz` (the GH Action that
uploads scorecard artifacts) landing on `main` — that bead is now closed, so
seeding can proceed on the first plugin run.
