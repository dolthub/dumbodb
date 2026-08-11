# Commit Identity Verification

Manual verification guide for server-set commit identity under `--auth`. Work
through each scenario top to bottom.

> **Automated equivalent:** `tests/verify/commit_identity_test.go` (the
> `TestCommitIdentity*` functions) cover every scenario here. Run them with:
> ```
> go test ./tests/verify/ -run TestCommitIdentity -v
> ```

## Concept

Under `--auth`, the server stamps the author and committer of every commit,
merge, revert, rebase, cherry-pick, and tag from the **authenticated user** -- a
client can never assert its own identity. The identity is admin-configured per
user (`commitIdentity {name,email}`), with any missing piece filled from the
login (`name <- username`, `email <- username@authDb`). The committer is always
the acting user; the author equals the committer except on replay
(rebase/cherry-pick), where the original commit's author is preserved. See
`docs/design/commit-identity.md`.

With `--auth` off, none of this applies: the legacy `author` parameter is honored
and the default identity is `dumbodb <dumbodb@dumbodb>`.

## Prerequisites

A DumboDB instance started with `--auth`, and `mongosh`. Because `--auth` forces
login, bootstrap the first admin via the localhost exception:

```js
// On a localhost connection to a fresh --auth server with zero users:
db.getSiblingDB("admin").runCommand({
  createUser: "admin", pwd: "admin-pw",
  roles: [ { role: "root", db: "admin" } ]
})
```

Reconnect authenticated as `admin` for the scenarios below:

```
mongosh "mongodb://admin:admin-pw@localhost:27017/?authSource=admin"
```

---

## Setup: users with and without a commit identity

```js
const admin = db.getSiblingDB("admin")

// alice has an explicit commit identity; bob has none.
admin.runCommand({
  createUser: "alice", pwd: "pw",
  roles: [ { role: "readWrite", db: "shop" } ],
  commitIdentity: { name: "Alice Dev", email: "alice@corp.io" }
})
admin.runCommand({
  createUser: "bob", pwd: "pw",
  roles: [ { role: "readWrite", db: "shop" } ]
})
```

`usersInfo` echoes the stored identity (a dumbo extension field):

```js
admin.getSiblingDB("shop").runCommand({ usersInfo: "alice" })
// users[0].commitIdentity == { name: "Alice Dev", email: "alice@corp.io" }
```

Validation: a malformed email is rejected with `BadValue` (2):

```js
admin.runCommand({
  createUser: "bad", pwd: "pw", roles: [],
  commitIdentity: { name: "Bad", email: "not-an-email" }
})
// { ok: 0, code: 2, ... }
```

---

## Scenario 1: commit stamps the stored identity

Connect as `alice` (`authSource=shop`), write, and commit:

```js
const shop = db.getSiblingDB("shop")
shop.items.insertOne({ _id: 1 })
shop.runCommand({ dumboCommit: 1, message: "change" })
// author == "Alice Dev <alice@corp.io>", committer == "Alice Dev <alice@corp.io>"
```

## Scenario 2: fallback to username@authDb

Connect as `bob` (no stored identity):

```js
const shop = db.getSiblingDB("shop")
shop.items.insertOne({ _id: 2 })
shop.runCommand({ dumboCommit: 1, message: "change" })
// author == committer == "bob <bob@shop>"
```

## Scenario 3: revert stamps the acting user (a new commit)

As `bob`, revert one of alice's commits:

```js
shop.runCommand({ dumboRevert: 1, commit: "<hashOfAlicesCommit>" })
// author == committer == "bob <bob@shop>"  (not alice's)
```

## Scenario 4: rebase / cherry-pick preserve the original author

Cherry-pick a commit **authored by alice** while connected as **bob**:

```js
db.getSiblingDB("shop@main").runCommand({ dumboCherryPick: 1, commit: "<aliceCommitHash>" })
// author    == "Alice Dev <alice@corp.io>"   (preserved from the picked commit)
// committer == "bob <bob@shop>"               (the acting user)
```

`dumboRebase` behaves the same: replayed commits keep their author; the committer
becomes the acting user.

## Scenario 5: tag tagger

```js
db.getSiblingDB("shop@main").runCommand({ dumboTag: 1, name: "v1", message: "release" })
// author (tagger) == the acting user's identity
```

## Scenario 6: auto-commit (server started with `--auth --auto-commit`)

Each write auto-commits, stamped with the acting identity:

```js
shop.items.insertOne({ _id: 3 })     // as alice, auto-commits
shop.runCommand({ dumboLog: 1, limit: 1 })
// commits[0].author == commits[0].committer == "Alice Dev <alice@corp.io>"
```

## Scenario 7: a client cannot supply an identity

Under `--auth`, any `author`/`committer` field is rejected with
`IDLUnknownField` (40415):

```js
shop.runCommand({ dumboCommit: 1, message: "m", author: "x <x@y.z>" })
// { ok: 0, code: 40415, errmsg: "BSON field 'dumboCommit.author' is an unknown field" }
```

The same holds for `dumboMerge`, `dumboRevert`, `dumboRebase`, `dumboCherryPick`,
and `dumboTag`, and for a `committer` field on any of them.

## Scenario 8: `--auth` off honors the author parameter

On a server started **without** `--auth`, the legacy behavior is unchanged:

```js
shop.items.insertOne({ _id: 9 })
shop.runCommand({ dumboCommit: 1, message: "m", author: "Ext Author <ext@x.io>" })
// author == committer == "Ext Author <ext@x.io>"
```

## Scenario 9: updateUser sets, replaces, and clears the identity

```js
const admin = db.getSiblingDB("admin")
admin.getSiblingDB("shop").runCommand({ updateUser: "bob",
  commitIdentity: { name: "Bob B", email: "bob@corp.io" } })   // set
admin.getSiblingDB("shop").runCommand({ updateUser: "bob",
  commitIdentity: { name: "Bob C", email: "bobc@corp.io" } })  // replace (wholesale)
admin.getSiblingDB("shop").runCommand({ updateUser: "bob", commitIdentity: null }) // clear -> fallback
```

After the clear, bob's commits fall back to `bob <bob@shop>` again.
