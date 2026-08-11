# Commit identity from the authenticated user

**Status:** Design, not yet implemented.

## First principle (scoped to `--auth`)

**When `--auth` is enabled, a client can never assert its own commit identity.**
The name and email recorded on any Dolt commit, merge, revert, rebase,
cherry-pick, or tag are set by the server from the authenticated session -- not by
anything on the wire. Under `--auth` this is a hard invariant, not a default: the
version-control commands reject an `author` or `committer` parameter.

The whole feature is gated on `--auth`:

- **`--auth` off** -- unchanged from today. There is no authenticated principal to
  trust, so the wire remains the only identity source: the existing `author`
  parameter is honored (and, as today, stamps both author and committer), and an
  omitted author falls back to the `dumbodb <dumbodb@dumbodb>` constant. Nothing
  in this design alters the no-auth developer experience.
- **`--auth` on** -- the model below applies: identity comes from the
  authenticated session, the `author`/`committer` params are rejected, and the
  git-style committer/author split is enforced.

## Committer vs. author (under `--auth`)

Following git's model:

- **Committer** = the authenticated identity. Always, on every command. This is
  the audit property: it records who actually ran the operation.
- **Author** = the committer for freshly authored commits; for commands that
  *replay existing commits*, the author is propagated from the commit being
  replayed.

| Command | Committer | Author |
|---------|-----------|--------|
| `dumboCommit` | authenticated user | authenticated user |
| `dumboMerge` | authenticated user | authenticated user |
| `dumboRevert` | authenticated user | authenticated user |
| `dumboRebase` | authenticated user | preserved from replayed commit |
| `dumboCherryPick` | authenticated user | preserved from picked commit |
| `dumboTag` (tagger) | authenticated user | -- |

The **preserved author** on rebase/cherry-pick is not a violation of the first
principle: that value is carried from an existing commit object in the repo
(itself server-stamped when created), not supplied by the acting client. The
identity the server hands the client is the **committer** line, and that is fixed
everywhere.

## Identity storage

Identity is a dumbo-specific field on the user document in `admin.system.users`:

```
commitIdentity: { name: "<string>", email: "<string>" }
```

It is **admin-controlled**: set only through `createUser` / `updateUser` (which
already require an administrative action). There is deliberately **no
self-service path** -- it is not part of `customData`, and `changeOwnCustomData`
cannot touch it. A user cannot edit the identity the server will stamp for them.

### Interface extension

`createUser` and `updateUser` gain an optional `commitIdentity` document
(added to their `RejectUnknownFields` allowlists):

- `createUser`: `commitIdentity` optional; omitted -> resolved by fallback below.
- `updateUser`: `commitIdentity` follows updateUser's replace semantics -- present
  replaces, absent leaves unchanged; an explicit empty/null clears it back to the
  fallback.
- `usersInfo`: returns `commitIdentity` when present (a dumbo extension field;
  parity comparison treats it as a permitted extra).

### Validation

Enough to guarantee the value round-trips into Dolt's `Name <email>` form and
cannot smuggle a second identity. On failure, `BadValue` (2) at create/update:

- **name**: non-empty; no `<` or `>`.
- **email**: non-empty; exactly one `@`; non-empty local and domain parts; no
  whitespace; no `<` or `>`.

Not full RFC 5322 -- just non-corrupting.

## Resolution and fallback

Any missing piece is derived from the auth identity (`username`, auth `db`). One
rule, applied field-by-field:

- `name  = commitIdentity.name  ?? username`
- `email = commitIdentity.email ?? username + "@" + authDb`

So a totally unset identity resolves to `username <username@authDb>`, and a
name-only identity gets `email = username@authDb`. There is no "missing identity"
error; absence is just the degenerate case of the same rule.

The resolved identity is computed once at authentication success and cached on
`ConnInfo` (alongside the privilege cache), so commits do not re-read
`system.users`. It is invalidated by the same auth generation counter that
invalidates privileges.

**`--auth` off:** none of the above runs. There is no authenticated principal, so
the legacy path is used verbatim -- an `author` param if supplied, else the
`dumbodb <dumbodb@dumbodb>` constant. `commitIdentity` is meaningless without auth
and is never resolved.

## Plumbing

The resolved identity flows from `ConnInfo` into the commit boundary:

- The VC command handlers pass the resolved identity (not a wire param) into
  `CommitParams` / `MergeParams` / etc.
- The **auto-commit path**, which today hardcodes `"dumbodb", "dumbodb@dumbodb"`
  (`internal/backends/dolt/branch_ws.go:266`, `backend.go:1044`), reads the
  identity recorded on `ConnInfo` at the `AutoCommitBoundary`.

## Rejection matrix -- client-supplied identity params

The rejection applies **only under `--auth`**. Removing `author`/`committer` from
a command's allowlist is done conditionally: with `--auth` on, the field is
absent from the allowlist and hits `RejectUnknownFields`, yielding
`ErrIDLUnknownField` (**40415**, "BSON field '<cmd>.<field>' is an unknown
field"); with `--auth` off, the field stays allowed and behaves as today.

All rows below are `--auth` **on** unless noted.

| # | Command | Field sent | Expected |
|---|---------|-----------|----------|
| CI-R-01 | `dumboCommit` | `author` | reject 40415 |
| CI-R-02 | `dumboCommit` | `committer` | reject 40415 |
| CI-R-03 | `dumboMerge` | `author` | reject 40415 |
| CI-R-04 | `dumboMerge` | `committer` | reject 40415 |
| CI-R-05 | `dumboRevert` | `author` | reject 40415 |
| CI-R-06 | `dumboRevert` | `committer` | reject 40415 |
| CI-R-07 | `dumboRebase` | `author` | reject 40415 |
| CI-R-08 | `dumboRebase` | `committer` | reject 40415 |
| CI-R-09 | `dumboCherryPick` | `author` | reject 40415 |
| CI-R-10 | `dumboCherryPick` | `committer` | reject 40415 |
| CI-R-11 | `dumboTag` | `author` | reject 40415 |
| CI-R-12 | `dumboTag` | `committer` | reject 40415 |
| CI-R-13 | any of the above | both `author` and `committer` | reject 40415 (first unknown field) |
| CI-R-14 | `dumboCommit`, `--auth` **off** | `author` | accepted; stamps author=committer per the param (today's behavior) |
| CI-R-15 | `dumboMerge`/`dumboRevert`, `--auth` **off** | `author` | accepted (today's behavior) |

Note CI-R-09/10 are a behavior change from today even before auth: they document
the `--auth`-on path. With `--auth` off, `dumboCherryPick` retains its current
handling (`author` rejected with a custom `BadValue`, `committer` accepted).

## Identity-stamping matrix -- who is recorded

Asserted DumboDB-only (git has no MongoDB counterpart). `U` = the resolved
authenticated identity; `H(x)` = author preserved from replayed commit `x`.

All rows are `--auth` **on** unless the setup says otherwise.

| # | Setup | Command | Committer | Author |
|---|-------|---------|-----------|--------|
| CI-S-01 | user has `commitIdentity {name,email}` | `dumboCommit` | U | U |
| CI-S-02 | user has name only | `dumboCommit` | U (email=`user@db`) | same |
| CI-S-03 | user has no `commitIdentity` | `dumboCommit` | `user <user@db>` | same |
| CI-S-04 | `--auth` **off**, no `author` param | `dumboCommit` | `dumbodb <dumbodb@dumbodb>` | same |
| CI-S-05 | `--auth` **off**, `author: "B <b@x>"` | `dumboCommit` | `B <b@x>` | same (today's behavior) |
| CI-S-06 | authenticated user | `dumboMerge` (non-FF) | U | U |
| CI-S-07 | authenticated user | `dumboRevert` | U | U |
| CI-S-08 | rebase commits authored by B | `dumboRebase` as A | A | H(B) |
| CI-S-09 | cherry-pick a commit authored by B | `dumboCherryPick` as A | A | H(B) |
| CI-S-10 | authenticated user | `dumboTag` | tagger = U | -- |
| CI-S-11 | auto-commit (`--auto-commit` / admin boundary) as A | implicit commit | A | A |
| CI-S-12 | auto-commit, `--auth` **off** | implicit commit | `dumbodb <dumbodb@dumbodb>` | same |
| CI-S-13 | `updateUser` clears `commitIdentity`, re-auth | `dumboCommit` | `user <user@db>` | same |
