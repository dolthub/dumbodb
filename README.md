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

```bash
make ferretdb-scorecard
```

This builds Dongo, starts it, runs the FerretDB integration suite against it, and writes results to `.runtime/ferretdb-scorecard.txt`.

To run a single test manually:

```bash
cd ferretdb/integration
go test -count=1 -timeout=60s -tags=ferretdb_dev -v \
  -run TestInsertFind \
  -target-backend=mongodb \
  -target-url=mongodb://127.0.0.1:27017/ .
```
