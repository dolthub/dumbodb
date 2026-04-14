# Parity Scorekeeper Design Doc

**Issue**: do-zhf
**Date**: 2026-03-26
**Status**: Implemented

## Overview

The Parity Scorekeeper is a Gas Town dog plugin that monitors the `parity.yml`
GitHub Actions workflow in `dolthub/dumbodb-parity-tesing`. After each completed
workflow run, it checks the conclusion, files a bd bead on failure, and closes
the bead on recovery.

## Design Decisions

### 1. Architecture: Dog Plugin

**Decision**: Dog plugin (`parity-scorekeeper-dog`) in the town-level plugins directory.

Same rationale as `scorekeeper-dog`: monitoring is recurring, requires judgment
calls, and runs on a cooldown gate (every 30 minutes).

### 2. Scope: Workflow-Level Monitoring

**Decision**: Monitor at the workflow run level (pass/fail), not individual test cases.

The parity workflow tests dumbodb against a reference Dolt implementation. Unlike
the FerretDB scorecard (which produces a per-test artifact), parity.yml reports
overall pass/fail. There's no artifact to parse — just the workflow conclusion.

This keeps the plugin simple: one failure bead per active incident, closed on
recovery.

### 3. State: Minimal JSON State File

**Decision**: State persisted to `$HOME/gt/.scorekeeper/parity-state.json`.

Only tracks the last processed run ID and any open failure bead:
```json
{
  "last_run_id": "12345678",
  "last_run_conclusion": "failure",
  "last_run_at": "2026-03-26T20:00:00Z",
  "last_run_url": "https://github.com/dolthub/dumbodb-parity-tesing/actions/runs/...",
  "open_failure_bead": "do-abc"
}
```

### 4. Token: Dedicated Verify Token

**Decision**: Uses `GH_TOKEN` from `/home/ubuntu/.gh_gt_token_dumbodb_vefify`.

The `dolthub/dumbodb-parity-tesing` repo requires a separate token with access to
workflow run data. This token is explicitly configured rather than relying on the
default `gh` auth.

### 5. Bead Priority: P1 (High)

**Decision**: Failure beads are filed at P1.

Parity failures indicate dumbodb behavior diverges from the reference Dolt
implementation — this is a correctness issue, not a flaky test. P1 ensures it
surfaces quickly.

### 6. Escalation: Mayor at HIGH Severity

**Decision**: Escalate to mayor at HIGH severity on first failure detection.

Parity failures need visibility beyond just a bead. Mayor routing ensures the
right team member sees it promptly.

### 7. Startup Failure Detection

**Decision**: Check workflow logs for "did not start in time" pattern.

If dumbodb fails to start during the parity run, that's a distinct failure mode
from a test failure. The bead description notes this when detected, aiding
diagnosis.

## Implementation

The plugin is at `/home/ubuntu/dumbodb/plugins/parity-scorekeeper-dog/plugin.md`.

It runs as a Dog agent (Claude with AI judgment) on a 30-minute cooldown.
Steps:
1. Load GH token and verify authentication
2. Load state from `parity-state.json`
3. Fetch latest completed parity workflow run
4. Check for dumbodb startup failure in logs
5. On failure: file bead (or update existing) + escalate to mayor
6. On success: close open failure bead if present
7. Persist updated state

## Key Differences from scorekeeper-dog

| Feature | scorekeeper-dog | parity-scorekeeper-dog |
|---------|----------------|------------------------|
| Repo | dolthub/dumbodb | dolthub/dumbodb-parity-tesing |
| Workflow | dumbodb-scorecard.yml | parity.yml |
| Granularity | Per-test tracking | Per-run tracking |
| Skiplist | Yes | No |
| Regression attribution | Yes | No |
| Bead per failure | One per failing test | One per active incident |
| Priority | P2 | P1 |
