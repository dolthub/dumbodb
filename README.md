# DumboDB

MongoDB-compatible document store backed by [Dolt](https://github.com/dolthub/dolt) — with version control built in.

## What makes it different

Connect with any MongoDB client and you get a full version history for your data:

```js
db.runCommand({ doltLog: 1, limit: 3 })
{
  branch: 'main',
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

Those commits came from this:

```js
use mydb
db.orders.insertOne({ customer: "alice", amount: 100 })
db.orders.insertOne({ customer: "bob",   amount: 200 })
db.runCommand({ doltCommit: 1, message: "initial data", author: "bob <bob@acme.com>" })

db.orders.updateOne({ customer: "alice" }, { $set: { amount: 150 } })
db.runCommand({ doltCommit: 1, message: "alice order updated", author: "alice <alice@acme.com>" })
```

Branch, merge, diff, cherry-pick, revert — all over the standard MongoDB wire protocol. No schema changes. No migration tools.

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

## dolt* commands

Connect via an encoded database name `<db>__d_<branch>` to target a specific branch.

| Command | Description |
|---------|-------------|
| `doltCommit` | Commit the current working set with a message and author |
| `doltBranch` | Create or delete a branch |
| `doltMerge` | Merge a source branch into the current branch; supports abort/continue |
| `doltCherryPick` | Apply one commit's diff onto the current branch; supports abort/continue |
| `doltRebase` | Reapply branch commits onto another branch tip, rewriting history |
| `doltLog` | Return commit history for the current branch |
| `doltStatus` | Show uncommitted changes on the current branch |
| `doltDiff` | Document-level diff between two states |
| `doltReset` | Move branch HEAD to a target commit (soft or hard) |
| `doltCurrentBranch` | Return the current branch name for this connection |
| `doltConflicts` | List or inspect conflicts from an in-progress merge/cherry-pick/rebase |
| `doltResolveConflict` | Resolve a single document conflict (ours / theirs / custom) |

All commands have a `dumbo*` alias (e.g. `dumboCommit`, `dumboMerge`) for environments that filter unknown MongoDB commands.

Full command reference: [docs/COMMANDS.md](docs/COMMANDS.md) *(coming soon — see [do-rnjg])*

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

### FerretDB integration tests

The `ferretdb/` directory is a Git submodule containing the [FerretDB](https://github.com/FerretDB/FerretDB) integration test suite. Initialize it once:

```bash
git submodule update --init --recursive
```

| Make target | Target | Purpose |
|---|---|---|
| `ferretdb-scorecard` | DumboDB | DumboDB's current pass rate |
| `ferretdb-compat` | DumboDB vs MongoDB | Diff DumboDB against MongoDB |
| `mongodb-reference` | Real MongoDB | Gold-standard baseline |
| `ferretdb-reference` | FerretDB | FerretDB baseline |

```bash
make ferretdb-scorecard
# Results: .runtime/ferretdb-scorecard.txt
```

### Repo layout

```
cmd/dumbodb/          Server entry point
internal/             DumboDB implementation
tests/                DumboDB-specific regression tests
  bats/               Bats shell integration tests
docs/
  design/             Design documents and architecture notes
  verify/             Manual + automated verification guides
ferretdb/             FerretDB integration test suite (submodule)
.github/workflows/    CI: scorecard, parity, bats
```
