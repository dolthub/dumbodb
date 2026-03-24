# Dongo

Dongo is a MongoDB-compatible server backed by [Dolt](https://github.com/dolthub/dolt).

## Build

```bash
git clone --recurse-submodules https://github.com/dolthub/dongo
cd dongo
make build
# Binary: .runtime/bin/dongo
```

Or manually:

```bash
go build -o /tmp/dongo ./cmd/dongo/
```

## Run

```bash
mkdir -p /tmp/dongo-data
.runtime/bin/dongo --data-dir /tmp/dongo-data
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

## Run FerretDB integration tests

The `ferretdb/` directory is a Git submodule containing the [FerretDB](https://github.com/FerretDB/FerretDB)
integration test suite.  Initialize it once:

```bash
git submodule update --init --recursive
```

### Test targets overview

| Make target | Target under test | Purpose |
|---|---|---|
| `ferretdb-scorecard` | Dongo | Dongo's current pass rate |
| `ferretdb-compat` | Dongo vs MongoDB | Diff Dongo against MongoDB behavior |
| `mongodb-reference` | Real MongoDB | Gold-standard baseline |
| `ferretdb-reference` | FerretDB itself | FerretDB baseline |

Run the reference targets to determine whether a failure is a **dongo-specific
regression** or a **known FerretDB/MongoDB limitation**.  If `mongodb-reference`
also fails a test, it is not a dongo bug.

### Dongo scorecard

```bash
make ferretdb-scorecard
# Results: .runtime/ferretdb-scorecard.txt
```

Builds Dongo, starts it on `127.0.0.1:27017`, runs the full FerretDB integration
suite with `-target-backend=mongodb` (Dongo speaks the MongoDB wire protocol), and
writes results to `.runtime/ferretdb-scorecard.txt`.

**Understanding failures:**

- **Real dongo bugs** — features not yet implemented (expected at this stage).
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

### Compat suite: Dongo vs MongoDB

```bash
# Start MongoDB with auth on port 47017:
cd ferretdb && docker compose up -d mongodb-secure

make ferretdb-compat
# Results: .runtime/ferretdb-compat.txt
```

Runs the `*_compat_test.go` files, which send each operation to both Dongo
(`-target-url`) and MongoDB (`-compat-url`) and assert that the results match.
Differences indicate dongo behaviour that diverges from MongoDB.

### MongoDB reference baseline

```bash
# Start MongoDB without auth on port 37017:
cd ferretdb && docker compose up -d mongodb

make mongodb-reference
# Results: .runtime/mongodb-reference.txt
```

Runs the full integration suite against real MongoDB.  Use this to find the
"gold standard" pass rate.  Any test that fails here is a FerretDB test-suite
issue or a MongoDB limitation, **not** a dongo bug.

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
