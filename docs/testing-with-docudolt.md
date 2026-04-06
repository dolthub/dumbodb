# Testing DocuDolt with the FerretDB Compatibility Suite

The FerretDB integration suite supports a *compat mode* that runs identical operations
against two servers and compares results field-by-field.  By pointing it at DocuDolt (target)
and a real MongoDB instance (compat reference), you can produce a precise diff of every
behavioral difference.

## Quick-start (Makefile)

```bash
# 1. Start MongoDB on port 47017 (once)
cd ferretdb
docker compose up -d mongodb-secure

# 2. Run the compat suite (builds docudolt, starts it, runs tests, stops it)
cd ..
make ferretdb-compat
```

Results land in `.runtime/ferretdb-compat.txt`.

## How it works

```
FerretDB integration suite
        │
        ├──► DocuDolt   (target)  mongodb://127.0.0.1:27017/
        │      -target-backend=ferretdb
        │
        └──► MongoDB (compat)  mongodb://username:password@127.0.0.1:47017/?replicaSet=rs0
               -compat-url
```

For each compat test the suite:

1. Inserts the same documents into both servers.
2. Runs the same query / command on both.
3. Normalises and compares every response field.
4. Fails if the responses differ.

Compat test files live in `ferretdb/integration/*_compat_test.go`.

## Starting both servers manually

### MongoDB (compat reference)

The `mongodb-secure` service in `ferretdb/docker-compose.yml` provides a replica-set
MongoDB instance on port 47017 with credentials `username` / `password`:

```bash
cd ferretdb
docker compose up -d mongodb-secure
```

### DocuDolt (target under test)

```bash
make build
mkdir -p .runtime/docudolt-data
.runtime/bin/docudolt --addr 127.0.0.1:27017 --data-dir .runtime/docudolt-data
```

Or let `make ferretdb-compat` start and stop it automatically.

## Running the compat suite

### Via Make (recommended)

```bash
make ferretdb-compat          # full suite
DOCUDOLT_PORT=27017 make ferretdb-compat   # override port
```

### Manually

```bash
cd ferretdb/integration
go test -count=1 -timeout=0 \
  -tags=ferretdb_dev \
  -race=false \
  -target-backend=ferretdb \
  -target-url='mongodb://127.0.0.1:27017/' \
  -compat-url='mongodb://username:password@127.0.0.1:47017/?replicaSet=rs0' \
  ./...
```

Run a single compat test by name:

```bash
go test -count=1 -timeout=60s -tags=ferretdb_dev -v \
  -run TestInsertCompatSimple \
  -target-backend=ferretdb \
  -target-url='mongodb://127.0.0.1:27017/' \
  -compat-url='mongodb://username:password@127.0.0.1:47017/?replicaSet=rs0' \
  .
```

## Interpreting failures

A compat test failure looks like:

```
--- FAIL: TestQueryCompatSort/SortAscending (0.12s)
    compat_test.go:123: response mismatch
        target (docudolt):  {n: 3, ok: 1}
        compat (mongodb): {n: 3, ok: 1.0}
```

Common patterns:

| Pattern | Likely cause |
|---------|-------------|
| Type mismatch (`int` vs `float`) | DocuDolt returns a different BSON type |
| Missing field in target | Command not yet implemented in DocuDolt |
| Extra field in compat | MongoDB-specific metadata DocuDolt omits |
| Value mismatch on cursor | Sort / collation difference |
| `SKIP` in output | Test requires `-compat-url`; skipped when not set |

Run with `-v` and `-debug-setup` for verbose logs:

```bash
go test … -v -debug-setup -run TestQueryCompatSort .
```

## FerretDB Taskfile integration (upstream)

To add a first-class `task test-integration-docudolt` target in the FerretDB repo,
apply `patches/ferretdb-taskfile-docudolt.patch` to a FerretDB checkout:

```bash
cd ferretdb
git apply ../patches/ferretdb-taskfile-docudolt.patch
```

See the patch for the full task definition.  The environment variable
`DOCUDOLT_URL` (default `mongodb://127.0.0.1:27017/`) controls where docudolt
is expected to listen.

## Docker Compose service for DocuDolt

To start DocuDolt alongside the FerretDB test stack, add the following snippet to
`ferretdb/docker-compose.yml`:

```yaml
  docudolt:
    image: ghcr.io/dolthub/docudolt:latest   # or build locally
    container_name: ferretdb_docudolt
    ports:
      - "27017:27017"
    command: --addr 0.0.0.0:27017 --data-dir /var/docudolt-data
    extra_hosts:
      - "host.docker.internal:host-gateway"
    volumes:
      - docudolt-data:/var/docudolt-data

volumes:
  docudolt-data:
```

Then run `docker compose up -d docudolt mongodb-secure` before invoking the compat suite.
