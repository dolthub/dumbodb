# DumboDB —Document DB with Version Control

[DumboDB](https://github.com/dolthub/dumbodb) is a document database that speaks the
MongoDB 8.0 wire protocol and supports Git-style version control (commit, branch,
merge, diff) over your collections. It's built on the [Dolt](https://github.com/dolthub/dolt)
storage engine.

Connect to DumboDB with any MongoDB driver or `mongosh`:

```bash
mongosh mongodb://localhost:27017
```

> **DumboDB is pre-1.0 software and not yet ready for production use.**
>
> As in MongoDB, access control is off unless you start the server with `--auth`.
> Without it, every connection has full access, so run unauthenticated instances
> only in trusted environments and do not expose them to untrusted networks.

## How to use this image

Running this image with no arguments starts the DumboDB server bound to
`0.0.0.0:27017`, storing data in `/var/lib/dumbodb`:

```bash
docker run -p 27017:27017 dolthub/dumbodb:latest
```

To persist data on the host, mount a volume to `/var/lib/dumbodb`:

```bash
docker run -p 27017:27017 \
  -v /path/on/host:/var/lib/dumbodb \
  dolthub/dumbodb:latest
```

To override flags (e.g. enable auto-commit, change log level), pass them after
the image name:

```bash
docker run -p 27017:27017 dolthub/dumbodb:latest \
  --data-dir /var/lib/dumbodb \
  --addr 0.0.0.0:27017 \
  --auto-commit \
  --log-level debug
```

Run `--help` to see all supported flags:

```bash
docker run --rm dolthub/dumbodb:latest --help
```

## Building the image

The Dockerfile supports three modes via the `DUMBODB_VERSION` build arg:

### Build a specific released version

```bash
docker build -f docker/Dockerfile --build-arg DUMBODB_VERSION=0.1.0 \
  -t dumbodb:0.1.0 .
```

### Build the latest released version

```bash
docker build -f docker/Dockerfile --build-arg DUMBODB_VERSION=latest \
  -t dumbodb:latest .
```

### Build from local source

The build context must contain a `dumbodb/` directory with the source tree. From
a parent directory containing `dumbodb/`:

```bash
docker build -f dumbodb/docker/Dockerfile --build-arg DUMBODB_VERSION=source \
  -t dumbodb:source .
```

> **Warning** When building from source, no other directories in the build
> context may have names starting with `dumbodb` — the Dockerfile uses a
> wildcard (`dumbodb*/`) to conditionally copy the source tree, and additional
> matches will break the build.

## Persisting data

DumboDB stores all data under the directory passed to `--data-dir`. In this
image that's `/var/lib/dumbodb`, declared as a `VOLUME`. Mount a host directory
or a named volume to keep data across container restarts:

```bash
docker volume create dumbodb-data
docker run -p 27017:27017 -v dumbodb-data:/var/lib/dumbodb dolthub/dumbodb:latest
```

## Version control commands

DumboDB exposes Git-style operations as MongoDB `runCommand` calls
(`dumboCommit`, `dumboBranch`, `dumboMerge`, `dumboLog`, etc.). See the
[Command Reference](https://github.com/dolthub/dumbodb/wiki/Commands) for the
full list and the [project README](https://github.com/dolthub/dumbodb#example-usage)
for examples.
