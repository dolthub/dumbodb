# DumboDB Remote Sync (Client Push/Pull)

**Issue:** workspace-he3
**Date:** 2026-08-21
**Status:** Draft (planning)

---

## 1. Goal and Scope

DumboDB needs Dolt-style remotes: the ability to push and pull a database
to/from external locations using the same protocols Dolt supports (`file://`,
`s3://`, `gs://`, `http(s)://` for DoltHub, `ssh://`).

This document covers the **client side**: DumboDB pushing to and fetching from a
remote. The **server side** -- receiving pushes into a running DumboDB server --
is out of scope here and will be specified later.

Delivery order:

1. `file://` push/pull -- simplest transport, no network.
2. Push/pull to DoltHub (`http(s)://`, gRPC client transport).
3. Remaining schemes (`s3://`, `gs://`, `ssh://`, ...) as far as they prove feasible. Not every Dolt scheme is guaranteed portable to DumboDB; we will support as many as we can.

## 2. Guiding Principles

1. **Lean on Dolt.** Dolt already implements efficient chunk transfer (diff,
   dedup, table-file streaming via the puller). This work is a thin wrapper; we
   do not reimplement chunk movement.
2. **A DumboDB database is a Dolt database.** Same NBS chunk format; collections
   are BSON-in-prolly-trees instead of SQL tables. Every Dolt remote -- DoltHub
   included -- stores and transfers it as opaque chunks with zero DumboDB
   awareness. To DoltHub it is "a Dolt database filled with binary nonsense,"
   which is fine and requires no receiving-side support.
3. **Config is MongoDB-native, in the admin database.** Remote definitions are
   documents in `admin.system.*` collections, mutated only by `dumbo*` commands,
   following the existing `system.users` / `system.roles` pattern. We do **not**
   use Dolt's `env.Remote` / `repo_state` layer for config storage.
4. **Push does not remap refs.** Client push behaves like git/Dolt: you push a
   branch to the remote's ref space directly. (How a running DumboDB server
   handles an *inbound* push is a separate, later concern.)

## 3. Transports

Remotes are distinguished by URL scheme, and each scheme maps to a Dolt client
transport:

| Scheme | Transport | Carries gRPC? |
|---|---|---|
| `file://` | local filesystem NBS store | no |
| `s3://`, `gs://`, `aws` | blob store | no |
| `http(s)://` (DoltHub) | remotestorage gRPC client | yes |
| `ssh://` | gRPC over ssh | yes |

Where gRPC appears here it is only a *client* transport (to DoltHub, or over
ssh). Running an in-process gRPC *server* to receive pushes is part of the
deferred server-side work.

## 4. Configuration Storage

All config lives in the `admin` database as server-managed `system.*`
collections, keyed by a db-qualified `_id` (`<db>.<name>`), mirroring
`system.users` (`_id = "<db>.<user>"`, `internal/handler/users/createuser.go:63`).

### 4.1 `admin.system.remotes`

One document per remote:

```
{ _id: "<db>.<remote>", name: "<remote>", db: "<db>", url: "<url>" }
```

Example:

```
{ _id: "mydb.origin", name: "origin", db: "mydb",
  url: "file:///srv/backups/mydb" }
```

### 4.2 `admin.system.branches`

One document per branch. v0 carries only upstream tracking:

```
{ _id: "<db>.<branch>", branch: "<branch>", db: "<db>",
  upstream: { remote: "<remote>", ref: "<ref>" } }
```

Two separate collections (not one) so a remote and a branch of the same name do
not collide on `_id`, mirroring `system.users` vs `system.roles`.

Only `dumbo*` commands write these collections; clients never insert directly.
Query all config for a database with `{ db: "<name>" }`.

## 5. Commands

Each command has a `dumbo<Verb>` name plus a `dolt<Verb>` alias, matching the
existing convention (`internal/handler/unknown_field_policy.go`). The target
database is implicit from the connection.

### 5.1 `dumboRemote`

```
db.runCommand({ dumboRemote: 1, action: "add",    name: "origin", url: "file:///..." })
db.runCommand({ dumboRemote: 1, action: "list" })
db.runCommand({ dumboRemote: 1, action: "remove", name: "origin" })
```

CRUD over `admin.system.remotes`.

### 5.2 `dumboPush`

```
db.runCommand({ dumboPush: 1, to: "origin", branch: "main" })
```

Resolve the remote doc, open the remote as a `DoltDB`, invoke `actions.Push`
(section 6); the remote's `refs/heads/<branch>` is updated directly (not
remapped).

### 5.3 `dumboFetch`

```
db.runCommand({ dumboFetch: 1, from: "origin" })
```

Pulls **every** remote branch into local tracking refs
(`refs/remotes/<remote>/*`), git-fetch style; does not move any local branch
head. There is no per-branch argument.

Exact required/optional arguments (default branch set, refspecs, force, tags)
are TBD; see section 7.

## 6. Push/Pull Mechanics

**Resolved:** the client path reuses Dolt's `actions.Push` /
`actions.FetchCommit` directly. Those functions need **no `DoltEnv` and no
`RepoState`** -- only two `*doltdb.DoltDB` handles, a `*doltdb.Commit`, a temp
dir, and refs (`dolt/go/libraries/doltcore/env/actions/remotes.go:54`). DumboDB
already holds the local handle (`dbState.doltDB` / `datasDB`); the remote is
opened standalone from its URL. The `env`-coupled wrappers (`DoPush`,
`PushToRemoteBranch`, `FetchRefSpecs`) are the only consumers of `RepoState`, and
we do not use them -- so the stubbed `repo_state_adapter` / `GetRemoteDB` hooks
are irrelevant to this path.

### 6.1 Push sequence

1. Read the remote doc from `admin.system.remotes`; get its `url`.
2. First push to a *new* `file://` remote only: `dbfactory.PrepareDB(ctx, nbf,
   url, nil)` to create the target directory (`dbfactory/factory.go:124`).
3. Open the remote: `remoteDB, _ := doltdb.LoadDoltDBWithParams(ctx,
   srcDB.Format(), url, filesys.LocalFS, params)` (`doltdb.go:189`) -- no gRPC
   dialer needed for `file://`.
4. Resolve the local commit to push: `localDB.Resolve(ctx, cs, headRef)` ->
   `.ToCommit()`.
5. `actions.Push(ctx, tempDir, mode, destBranchRef, remoteTrackingRef, localDB,
   remoteDB, commit, statsCh)`. Transfers novel chunks (internally via
   `pull.NewPuller`), updates the remote's `refs/heads/<branch>`, and sets the
   local remote-tracking ref.

### 6.2 Fetch sequence

Mirror: open the remote `DoltDB`, `actions.FetchCommit(ctx, tempDir, remoteDB,
localDB, remoteCommit, statsCh)` (`remotes.go:309`), then update the local
tracking ref with `localDB.SetHead(...)`.

### 6.3 Fallback

If we ever need to shed the `actions` layer: drive `pull.NewPuller`
(`store/datas/pull/puller.go:66`) directly on
`datas.ChunkStoreFromDatabase(datasDB)` + the remote store's chunk store, and
`SetHead` manually. `actions.Push` already does exactly this with correct
fast-forward semantics, so it is the default.

## 7. Open Questions

The leverage fork -- formerly the main open question -- is **resolved**: use
`actions.Push` / `actions.FetchCommit` directly (section 6); no `DoltEnv` /
`RepoState`. Remaining:

1. **`dumboPush` / `dumboFetch` argument surface.** `actions.Push` takes a
   `mode` (fast-forward-only vs force), the target branch ref, and a resolved
   commit. Map: `branch` -> resolve local head + `destBranchRef`; a `force` flag
   -> `mode`. Multi-branch / refspec fan-out and tags are later.
2. **`admin.system.branches` upstream semantics.** `actions.Push` already writes
   a local remote-tracking ref (`refs/remotes/<remote>/<branch>`); decide when
   `upstream` is recorded in `system.branches` (first push with an explicit
   target?) and how `dumboFetch` consumes it as a default.
3. **Scheme rollout.** `file` and the cloud-blob schemes
   (`aws`/`oss`/`gs`/`az`/`oci`) open standalone via `dbfactory`; `http(s)`
   (DoltHub) and `ssh` additionally need Dolt's gRPC dial provider. `file://`
   first needs none. (`mem://`, an in-memory Dolt store, is useful as a throwaway remote in tests but is not a supported scheme in its own right.)

## 8. Development Plan

Tracked in beads under epic `workspace-1np`: nine dependency-ordered child
tasks, each with a test plan emphasizing hermetic, CI-durable coverage. The
design-doc task (`workspace-he3`) is this epic's reference.

| ID | Task | Depends on | Test approach |
|----|------|-----------|---------------|
| `.1` | `dumboRemote` + `admin.system.remotes` | -- | Hermetic in-process (`newTestBackend`): CRUD, `<db>.<name>` `_id`, db-scoped list, duplicate/nonexistent, alias parity. No credentials. |
| `.2` | Remote URL parsing + transport resolution | -- | Pure table-driven parse tests per scheme; reject malformed/unknown. No credentials. |
| `.3` | `dumboPush` to `file://` | `.1`, `.2` | Hermetic temp-dir / `mem://` remotes: round-trip, `PrepareDB` on new remote, idempotent re-push, fast-forward vs force, tracking ref. No credentials. |
| `.4` | `dumboFetch` from `file://` | `.3` | Hermetic A->file->B round-trip equality, no-op fetch, tracking ref, bad-ref error. No credentials. |
| `.5` | `admin.system.branches` upstream | `.3`, `.4` | Hermetic: upstream recorded on push, used as default on fetch/push. No credentials. |
| `.6` | Test & CI strategy for credentialed / live remotes | -- | Builds the in-process remotesrv fixture (loopback gRPC) + gated-test helper; documents secret names. |
| `.7` | DoltHub push/pull (http(s) gRPC) | `.3`, `.4`, `.6` | Hermetic against the local remotesrv fixture (CI-durable); gated live DoltHub test (CI token, skips without). Credentials required. |
| `.8` | Cloud-blob schemes (s3/gs/az/oci/oss) | `.2`, `.6` | Local S3 mock where possible; else gated live with CI secrets. Credentials required. |
| `.9` | `ssh://` scheme (gRPC over ssh) | `.6`, `.7` | Wiring unit coverage + gated live ssh test. Credentials required. |

### 8.1 Testing and CI principles

- **Hermetic by default.** Every task's default tests are network-free and
  deterministic: temp-dir `file://`, `mem://`, and -- for the gRPC client path --
  an in-process Dolt remotesrv served over loopback (never real DoltHub).
- **Credentialed / live tests are gated.** The schemes that need credentials
  (`.7` DoltHub, `.8` cloud-blob, `.9` ssh) keep their live tests behind build
  tags plus env-var CI secrets and `t.Skip` when the secret is unset, so a
  missing secret never breaks CI. Task `.6` builds the hermetic remotesrv fixture
  and the gating convention that `.7`-`.9` reuse, and is a prerequisite for all
  of them.

### 8.2 Ready to start

`.1` (`dumboRemote`), `.2` (URL parsing), and `.6` (CI/credential strategy) have
no blockers. The `file://` path (`.3` -> `.4` -> `.5`) unblocks from `.1` + `.2`;
the credentialed schemes all wait on `.6`.

## 9. References

- Push / fetch core (no env): `dolt/go/libraries/doltcore/env/actions/remotes.go:54` (`Push`), `:309` (`FetchCommit`)
- Chunk mover: `dolt/go/store/datas/pull/puller.go:66` (`NewPuller`)
- Open a remote standalone: `dolt/go/libraries/doltcore/doltdb/doltdb.go:189` (`LoadDoltDBWithParams`), `dolt/go/libraries/doltcore/dbfactory/factory.go:102` (`CreateDB`) / `:124` (`PrepareDB`)
- Config identity pattern: `internal/handler/users/createuser.go:63` (system.users `_id`)
- Stubbed remote hooks (NOT used by this path): `internal/backends/dolt/repo_state_adapter.go:50`, `internal/backends/dolt/dsess_provider.go:108`
- Command naming convention: `internal/handler/unknown_field_policy.go`
