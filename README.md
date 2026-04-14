# DumboDB

DumboDB is a MongoDB-compatible server backed by [Dolt](https://github.com/dolthub/dolt).

## Build

```bash
git clone --recurse-submodules https://github.com/dolthub/dumbodb
cd dumbodb
make build
# Binary: .runtime/bin/dumbodb
```

Or manually:

```bash
go build -o /tmp/dumbodb ./cmd/dumbodb/
```

## Run

```bash
mkdir -p /tmp/dumbodb-data
.runtime/bin/dumbodb --data-dir /tmp/dumbodb-data
```

The server listens on `127.0.0.1:27017` by default.

## Insert a document

With the server running, use the `mongosh` shell or any MongoDB client:

```bash
mongosh mongodb://127.0.0.1:27017/
```

```js
use mydb
db.mycollection.insertOne({ name: "hello", value: 42 })
db.mycollection.find()
```

Or with `mongosh` one-liner:

```bash
mongosh --eval 'db.test.insertOne({x:1})' mongodb://127.0.0.1:27017/testdb
```

## Test suites

### MongoDB parity tests (dolthub/dumbodb-parity-testing)

MongoDB compatibility tests live in a separate repository: **dolthub/dumbodb-parity-testing**.

That repo uses a dual-client harness (`PairTest`) that runs each operation against
both a real MongoDB 8 instance and DumboDB, then compares the results. Tests are
labelled with three support levels:

| Label | Meaning |
|-------|---------|
| `DumboDBFull` | Both MongoDB and DumboDB are exercised; divergences break CI |
| `DumboDBXFail` | Both are exercised; DumboDB divergence is recorded but not fatal |
| `DumboDBMongoOnly` | MongoDB only; DumboDB skipped (auth, sharding, GridFS, etc.) |

**Policy**: `tests/` in this repo is for dumbodb-specific tests only. MongoDB
compatibility tests belong in dolthub/dumbodb-parity-testing.

### DumboDB-specific regression tests (tests/)

The `tests/` package contains regression tests for dumbodb-internal behaviors
that have no MongoDB equivalent — things like internal resource management,
cursor lifecycle, and implementation-specific edge cases. DumboDB is built and
started automatically by the test harness.

```bash
# Run with verbose output
go test -v ./tests/

# Run a single test
go test -v -run TestFind_CursorCleanupOnFilterError ./tests/
```

The binary is cached at `.runtime/bin/dumbodb` after the first build.
Pre-build it explicitly with:

```bash
make build
# or: go build -o .runtime/bin/dumbodb ./cmd/dumbodb/
```

### Repo layout

```
tests/                      DumboDB-specific regression tests
  query_test.go             Test harness + regression tests
  bats/                     Bats shell integration tests (owner-managed, do not edit)
cmd/dumbodb/                  DumboDB server entry point
internal/                   DumboDB implementation
.github/workflows/
  dumbodb-scorecard.yml       FerretDB scorecard CI
  mongodb-reference.yml     MongoDB reference baseline CI
  bats.yml                  Bats shell test CI
```

---

## Run FerretDB integration tests

The `ferretdb/` directory is a Git submodule containing the [FerretDB](https://github.com/FerretDB/FerretDB)
integration test suite.  Initialize it once:

```bash
git submodule update --init --recursive
```

### Test targets overview

| Make target | Target under test | Purpose |
|---|---|---|
| `ferretdb-scorecard` | DumboDB | DumboDB's current pass rate |
| `ferretdb-compat` | DumboDB vs MongoDB | Diff DumboDB against MongoDB behavior |
| `mongodb-reference` | Real MongoDB | Gold-standard baseline |
| `ferretdb-reference` | FerretDB itself | FerretDB baseline |

Run the reference targets to determine whether a failure is a **dumbodb-specific
regression** or a **known FerretDB/MongoDB limitation**.  If `mongodb-reference`
also fails a test, it is not a dumbodb bug.

### DumboDB scorecard

```bash
make ferretdb-scorecard
# Results: .runtime/ferretdb-scorecard.txt
```

Builds DumboDB, starts it on `127.0.0.1:27017`, runs the full FerretDB integration
suite with `-target-backend=mongodb` (DumboDB speaks the MongoDB wire protocol), and
writes results to `.runtime/ferretdb-scorecard.txt`.

**Understanding failures:**

- **Real dumbodb bugs** — features not yet implemented (expected at this stage).
- **Compat tests** — files named `*_compat_test.go` compare the target against a
  second MongoDB instance.  Without `-compat-url` they are automatically skipped
  (not failed) — a `SKIP` in the output is not a failure.

To run a single test manually:

```bash
cd ferretdb/integration
go test -count=1 -timeout=60s -tags=ferretdb_dev -v \
  -run TestInsertFind \
  -target-backend=mongodb \
  -target-url=mongodb://127.0.0.1:27017/ .
```

### Compat suite: DumboDB vs MongoDB

```bash
# Start MongoDB with auth on port 47017:
cd ferretdb && docker compose up -d mongodb-secure

make ferretdb-compat
# Results: .runtime/ferretdb-compat.txt
```

Runs the `*_compat_test.go` files, which send each operation to both DumboDB
(`-target-url`) and MongoDB (`-compat-url`) and assert that the results match.
Differences indicate dumbodb behaviour that diverges from MongoDB.

### MongoDB reference baseline

```bash
# Start MongoDB without auth on port 37017:
cd ferretdb && docker compose up -d mongodb

make mongodb-reference
# Results: .runtime/mongodb-reference.txt
```

Runs the full integration suite against real MongoDB.  Use this to find the
"gold standard" pass rate.  Any test that fails here is a FerretDB test-suite
issue or a MongoDB limitation, **not** a dumbodb bug.

Override defaults with environment variables:

```bash
MONGO_HOST=127.0.0.1 MONGO_PORT=37017 make mongodb-reference
```

### FerretDB reference baseline

Runs the suite against FerretDB itself to establish a FerretDB baseline.

**External server mode** (recommended):

```bash
# 1. Start PostgreSQL (FerretDB's backend):
cd ferretdb && docker compose up -d postgres

# 2. Build and start FerretDB on port 27018:
cd ferretdb && go build -o /tmp/ferretdb ./cmd/ferretdb/
/tmp/ferretdb --listen-addr=127.0.0.1:27018 \
  --postgresql-url='postgres://pg-user:pg-pass@127.0.0.1:5432/postgres' &

# 3. Run the reference suite:
make ferretdb-reference
# Results: .runtime/ferretdb-reference.txt
```

**In-process mode** (FerretDB starts inside the test binary):

```bash
cd ferretdb && docker compose up -d postgres

POSTGRESQL_URL='postgres://pg-user:pg-pass@127.0.0.1:5432/postgres' \
  make ferretdb-reference
```

Override the external server address with:

```bash
FERRETDB_HOST=127.0.0.1 FERRETDB_PORT=27018 make ferretdb-reference
```
