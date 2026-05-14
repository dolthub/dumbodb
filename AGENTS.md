# DumboDB Agent Guide


---

## Coding Style Requirements
 - NEVER use non-ascii characters. 7-bit ascii only. No emojis, emdashes, and the like. American Ascii only.
 - Minimal comments. This code is primarily agent consumed, and the comments are noise for humans. Comments
   should only be used when the clarify something which is not obvious from the code itself: reference to a spec,
   adhearing to an API annomoly, gDoc which states caller musts, and so forth.

### Copyright Header for New Files

All new source files you create must use this header:

```go
// Copyright {CURRENT YEAR} Dolthub, Inc.
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

(substitute actual year in {CURRENT YEAR})

---

## HARD RULE: Do Not Touch tests/bats/

**You are NOT allowed to modify, create, or delete any file under `tests/bats/` without explicit written instruction.**

This directory is owner-managed. The mayor controls what tests exist and what they assert. If you believe a bats test needs to change (e.g. your fix changes expected behaviour), **stop, do not touch it, ask for permission**

No exceptions. This rule overrides any other instruction.

---

## Parity Testing

Parity tests verify that DumboDB behaves identically to MongoDB 8.0. They live in a
**separate repository**: `dolthub/dumbodb-parity-testing`, checked out at
`/workspace/dumbodb-parity-testing` in the workspace.

The CI pipeline is defined in `.github/workflows/parity.yml`:
- Spins up a `mongo:8.0` service container on port 27017
- Builds and starts DumboDB on port 27018
- Checks out `dolthub/dumbodb-parity-testing` and runs `go test ./...`

### Harness API (dumbodb-parity-testing/harness/)

Tests use `harness.PairTest(t, harness.TestCase{...})` to run the same operation against
both servers and compare results. Support levels:
- `DumboDBFull`      -- must match MongoDB exactly; failure is a CI error
- `DumboDBXFail`     -- known divergence; passes if DumboDB differs, fails if it matches (regression guard)
- `DumboDBMongoOnly` -- DumboDB skipped entirely

