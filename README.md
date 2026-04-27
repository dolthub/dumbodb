# DumboDB

MongoDB-compatible document store backed by [Dolt](https://github.com/dolthub/dolt) — with version control built in.

Connect from any MongoDB client. Commit changes when ready, then query your history:

```js
db.runCommand({ doltLog: 1, limit: 3 })
{
  commits: [
    {
      commitId: 'v9ra3pmi0f6kotj5k3fganpmb3oi9t1k',
      parent1:  'tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b',
      refs:     [ 'HEAD', 'main' ],
      message:  'alice order updated',
      timestamp: ISODate('2026-04-14T17:22:31.000Z'),
      author:   'alice <alice@acme.com>'
    },
    {
      commitId: 'tqq1tn5qs0pns2j2uk5k1b2ufhqt9q3b',
      parent1:  '5vi6e5t3riqpgh6fq0j1pf0r0imuqhsn',
      message:  'initial data',
      timestamp: ISODate('2026-04-14T09:00:00.000Z'),
      author:   'bob <bob@acme.com>'
    },
    {
      commitId: '5vi6e5t3riqpgh6fq0j1pf0r0imuqhsn',
      message:  'Initialize database',
      timestamp: ISODate('2026-04-14T08:55:12.000Z'),
      author:   'DumboDB'
    }
  ],
  ok: 1
}
```

Or diff any two commits to see exactly what changed:

```js
db.runCommand({ doltDiff: 1, from: "HEAD~1", to: "HEAD" })
{
  collections: [
    {
      name: 'orders',
      added:    [],
      removed:  [],
      modified: [
        {
          _id: ObjectId('507f1f77bcf86cd799439011'),
          diff: [
            { type: 'modified', path: 'amount', from: 100, to: 150 }
          ]
        }
      ]
    }
  ],
  ok: 1
}
```

## dolt* commands

Connect via an encoded database name `<db>__d_<branch>` to target a specific branch.

| Command | Description |
|---------|-------------|
| [`doltCommit`](docs/COMMANDS.md#doltcommit) | Commit the current working set with a message and author |
| [`doltBranch`](docs/COMMANDS.md#doltbranch) | Create or delete a branch |
| [`doltMerge`](docs/COMMANDS.md#doltmerge) | Merge a source branch into the current branch; supports abort/continue |
| [`doltCherryPick`](docs/COMMANDS.md#doltcherrypick) | Apply one commit's diff onto the current branch; supports abort/continue |
| [`doltRebase`](docs/COMMANDS.md#doltrebase) | Reapply branch commits onto another branch tip, rewriting history |
| [`doltLog`](docs/COMMANDS.md#doltlog) | Return commit history for the current branch |
| [`doltStatus`](docs/COMMANDS.md#doltstatus) | Show uncommitted changes on the current branch |
| [`doltDiff`](docs/COMMANDS.md#doltdiff) | Document-level diff between two states |
| [`doltReset`](docs/COMMANDS.md#doltreset) | Move branch HEAD to a target commit (soft or hard) |
| [`doltRevert`](docs/COMMANDS.md#doltrevert) | Revert one or more commits, creating a new inverse commit |
| [`doltCurrentBranch`](docs/COMMANDS.md#doltcurrentbranch) | Return the current branch name for this connection |
| [`doltConflicts`](docs/COMMANDS.md#doltconflicts) | List or inspect conflicts from an in-progress merge/cherry-pick/rebase |
| [`doltResolveConflict`](docs/COMMANDS.md#doltresolveconflict) | Resolve a single document conflict (ours / theirs / custom) |

All commands have a `dumbo*` alias (e.g. `dumboCommit`, `dumboMerge`) for environments that filter unknown MongoDB commands.

Full command reference: [docs/COMMANDS.md](docs/COMMANDS.md)

## Build & Run

```bash
git clone --recurse-submodules https://github.com/dolthub/dumbodb
cd dumbodb
make build
# Binary: .runtime/bin/dumbodb

mkdir -p /tmp/dumbodb-data
.runtime/bin/dumbodb --data-dir /tmp/dumbodb-data
# Listens on 127.0.0.1:27017
```

Or build manually:

```bash
go build -o /tmp/dumbodb ./cmd/dumbodb/
```

## Testing

### DumboDB-specific tests

```bash
go test -v ./tests/

# Single test:
go test -v -run TestFind_CursorCleanupOnFilterError ./tests/

# Bats integration tests:
bats tests/bats/
```

The binary is cached at `.runtime/bin/dumbodb` after the first build. Pre-build with `make build`.

### MongoDB parity tests (dolthub/dumbodb-parity-testing)

MongoDB compatibility tests live in a separate repo: **dolthub/dumbodb-parity-testing**.

That repo uses a dual-client harness (`PairTest`) that runs each operation against both a real MongoDB 8 instance and DumboDB, then compares results:

| Label | Meaning |
|-------|---------|
| `DumboDBFull` | Both MongoDB and DumboDB exercised; divergences break CI |
| `DumboDBXFail` | Both exercised; DumboDB divergence recorded but not fatal |
| `DumboDBMongoOnly` | MongoDB only; DumboDB skipped (auth, sharding, GridFS, etc.) |

## Acknowledgements

DumboDB is built on two open-source projects:

- **[FerretDB](https://github.com/FerretDB/FerretDB)** — The wire protocol, connection handling, type system, and handler packages were adapted from FerretDB v1.24.2 (Apache 2.0). See [ACKNOWLEDGEMENTS](ACKNOWLEDGEMENTS) for details.
- **[Dolt](https://github.com/dolthub/dolt)** — The version-controlled storage engine powering every commit, branch, and merge.

### Repo layout

```
cmd/dumbodb/          Server entry point
internal/             DumboDB implementation
tests/                DumboDB-specific regression tests
  bats/               Bats shell integration tests
docs/
  design/             Design documents and architecture notes
  verify/             Manual + automated verification guides
.github/workflows/    CI: go test, bats
```
