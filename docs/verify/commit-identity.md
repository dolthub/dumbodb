# Commit Identity Verification

Manual verification guide for server-set commit identity under `--auth`.

> **Automated equivalent:** `tests/verify/commit_identity_test.go`. Each section
> below mirrors one `TestCommitIdentity*` function exactly -- same servers, users,
> commands, and expected values. Run them with:
> ```
> go test ./tests/verify/ -run TestCommitIdentity -v
> ```
> Every test starts its own server; the `mongosh` steps below assume the same
> (start a fresh server per section). Under `--auth`, create the first admin via
> the localhost exception, then reconnect as that admin:
> ```
> // localhost, fresh --auth server, zero users:
> db.getSiblingDB("admin").runCommand({ createUser: "admin", pwd: "admin-pw",
>   roles: [ { role: "root", db: "admin" } ] })
> // then: mongosh "mongodb://admin:admin-pw@localhost:27017/?authSource=admin"
> ```

---

## Stamping (`TestCommitIdentityStamping`)

Server: `--auth`. As admin, create `alice` (with an identity) and `bob` (without),
both `readWrite` on `shop`:

```js
db.getSiblingDB("shop").runCommand({ createUser: "alice", pwd: "pw",
  roles: [ { role: "readWrite", db: "shop" } ],
  commitIdentity: { name: "Alice Dev", email: "alice@corp.io" } })
db.getSiblingDB("shop").runCommand({ createUser: "bob", pwd: "pw",
  roles: [ { role: "readWrite", db: "shop" } ] })
```

**commit stamps the stored identity** -- as `alice` (`authSource=shop`):

```js
const shop = db.getSiblingDB("shop")
shop.items.insertOne({ _id: 1 })
shop.runCommand({ dumboCommit: 1, message: "change" })
// author == committer == "Alice Dev <alice@corp.io>"
```

**commit falls back to username@authDb** -- as `bob`:

```js
const shop = db.getSiblingDB("shop")
shop.items.insertOne({ _id: 2 })
shop.runCommand({ dumboCommit: 1, message: "change" })
// author == committer == "bob <bob@shop>"
```

**revert stamps the acting identity** -- as `alice`, commit `_id: 3` (becomes HEAD),
then as `bob` revert that commit:

```js
// as alice:
const shop = db.getSiblingDB("shop")
shop.items.insertOne({ _id: 3 })
const target = shop.runCommand({ dumboCommit: 1, message: "change" })

// as bob (copy target.commitId from alice's response above):
db.getSiblingDB("shop").runCommand({ dumboRevert: 1, commit: "<target.commitId>" })
// author == committer == "bob <bob@shop>"
```

**tag stamps the acting identity as tagger** -- as `alice`:

```js
db.getSiblingDB("shop@main").runCommand({ dumboTag: 1, name: "v1", message: "release" })
// author == "Alice Dev <alice@corp.io>"
```

---

## Cherry-pick (`TestCommitIdentityReplayStamping`)

Server: `--auth`. Branch operations need a grant covering `repo@feature`; a
`readWrite`-on-`repo` grant does not (authorization matches the raw `db@branch`
string), so this uses two `root` users. As admin:

```js
const admin = db.getSiblingDB("admin")
admin.runCommand({ createUser: "aa", pwd: "pw", roles: [ { role: "root", db: "admin" } ],
  commitIdentity: { name: "Alice Dev", email: "alice@corp.io" } })
admin.runCommand({ createUser: "bb", pwd: "pw", roles: [ { role: "root", db: "admin" } ],
  commitIdentity: { name: "Bob Ops", email: "bob@corp.io" } })
```

As `aa` (`authSource=admin`), author a commit on a feature branch:

```js
const repo = db.getSiblingDB("repo")
repo.items.insertOne({ _id: 1 }); repo.runCommand({ dumboCommit: 1, message: "base" })
db.getSiblingDB("repo@main").runCommand({ dumboBranch: 1, branch: "feature" })
db.getSiblingDB("repo@feature").items.insertOne({ _id: 2 })
const c2 = db.getSiblingDB("repo@feature").runCommand({ dumboCommit: 1, message: "add-two" })
// c2.author == "Alice Dev <alice@corp.io>"
```

As `bb`, cherry-pick it onto main (copy `c2.commitId` from aa's response above):

```js
db.getSiblingDB("repo@main").runCommand({ dumboCherryPick: 1, commit: "<c2.commitId>" })
// author    == "Alice Dev <alice@corp.io>"   (preserved from the picked commit)
// committer == "Bob Ops <bob@corp.io>"        (the acting user)
```

---

## Merge and rebase (`TestCommitIdentityMergeAndRebase`)

Server: `--auth`. Same two `root` users `aa`/`bb` as the cherry-pick section.

**merge commit is authored and committed by the actor** -- as `aa`, diverge main
and feature; as `bb`, merge (no fast-forward):

```js
// as aa:
const mrg = db.getSiblingDB("mrg")
mrg.items.insertOne({ _id: 1 }); mrg.runCommand({ dumboCommit: 1, message: "base" })
db.getSiblingDB("mrg@main").runCommand({ dumboBranch: 1, branch: "feature" })
mrg.items.insertOne({ _id: 2 }); mrg.runCommand({ dumboCommit: 1, message: "main-2" })
db.getSiblingDB("mrg@feature").items.insertOne({ _id: 3 })
db.getSiblingDB("mrg@feature").runCommand({ dumboCommit: 1, message: "feat-3" })

// as bb:
db.getSiblingDB("mrg@main").runCommand({ dumboMerge: 1, mergeIn: "feature", noFF: true })
// author == committer == "Bob Ops <bob@corp.io>"
```

**rebase preserves the replayed author, actor is committer** -- as `aa`, diverge;
as `bb`, rebase feature onto main and read HEAD:

```js
// as aa:
const rbs = db.getSiblingDB("rbs")
rbs.items.insertOne({ _id: 1 }); rbs.runCommand({ dumboCommit: 1, message: "base" })
db.getSiblingDB("rbs@main").runCommand({ dumboBranch: 1, branch: "feature" })
rbs.items.insertOne({ _id: 2 }); rbs.runCommand({ dumboCommit: 1, message: "main-2" })
db.getSiblingDB("rbs@feature").items.insertOne({ _id: 3 })
db.getSiblingDB("rbs@feature").runCommand({ dumboCommit: 1, message: "feat-3" })

// as bb:
db.getSiblingDB("rbs@feature").runCommand({ dumboRebase: 1, onto: "main" })
db.getSiblingDB("rbs@feature").runCommand({ dumboLog: 1, limit: 1 })
// commits[0].author    == "Alice Dev <alice@corp.io>"   (preserved)
// commits[0].committer == "Bob Ops <bob@corp.io>"        (the actor)
```

---

## Auto-commit (`TestCommitIdentityAutoCommit`)

Server: `--auth --auto-commit`. As admin, create `alice` (with identity) and `bob`
(without), both `readWrite` on `auto`:

```js
db.getSiblingDB("auto").runCommand({ createUser: "alice", pwd: "pw",
  roles: [ { role: "readWrite", db: "auto" } ],
  commitIdentity: { name: "Alice Dev", email: "alice@corp.io" } })
db.getSiblingDB("auto").runCommand({ createUser: "bob", pwd: "pw",
  roles: [ { role: "readWrite", db: "auto" } ] })
```

Each write auto-commits. As `alice`:

```js
const auto = db.getSiblingDB("auto")
auto.items.insertOne({ _id: 1 })
auto.runCommand({ dumboLog: 1, limit: 1 })
// commits[0].author == commits[0].committer == "Alice Dev <alice@corp.io>"
```

As `bob`:

```js
const auto = db.getSiblingDB("auto")
auto.items.insertOne({ _id: 2 })
auto.runCommand({ dumboLog: 1, limit: 1 })
// commits[0].author == commits[0].committer == "bob <bob@auto>"
```

---

## Client identity is rejected (`TestCommitIdentityRejectsClientIdentity`)

Server: `--auth`. As admin, create `dev` (root, with identity); connect as `dev`.
Under `--auth` every `author`/`committer` field is rejected with `IDLUnknownField`
(40415) -- validated before the command runs, so the other (dummy) fields do not
matter:

```js
const d = db.getSiblingDB("repo@main")
d.runCommand({ dumboCommit: 1, author: "x <x@y.z>" })                       // 40415
d.runCommand({ dumboCommit: 1, committer: "x <x@y.z>" })                    // 40415
d.runCommand({ dumboMerge: 1, mergeIn: "x", author: "x <x@y.z>" })          // 40415
d.runCommand({ dumboMerge: 1, mergeIn: "x", committer: "x <x@y.z>" })       // 40415
d.runCommand({ dumboRevert: 1, commit: "abc", author: "x <x@y.z>" })        // 40415
d.runCommand({ dumboRebase: 1, onto: "main", author: "x <x@y.z>" })         // 40415
d.runCommand({ dumboRebase: 1, onto: "main", committer: "x <x@y.z>" })      // 40415
d.runCommand({ dumboCherryPick: 1, commit: "abc", author: "x <x@y.z>" })    // 40415
d.runCommand({ dumboCherryPick: 1, commit: "abc", committer: "x <x@y.z>" }) // 40415
d.runCommand({ dumboTag: 1, name: "v1", author: "x <x@y.z>" })             // 40415
```

---

## `--auth` off honors the author parameter (`TestCommitIdentityAuthOffHonorsAuthor`)

Server: **no** `--auth`. No login; the legacy `author` parameter is stamped verbatim:

```js
const repo = db.getSiblingDB("repo")
repo.items.insertOne({ _id: 1 })
repo.runCommand({ dumboCommit: 1, message: "m", author: "Ext Author <ext@x.io>" })
// author == committer == "Ext Author <ext@x.io>"
```

---

## usersInfo and updateUser (`TestCommitIdentityUsersInfo`)

Server: `--auth`. As admin, run each against the `appid` database. `usersInfo`
echoes `commitIdentity` (a dumbo extension field):

```js
const appid = db.getSiblingDB("appid")
const rw = [ { role: "readWrite", db: "appid" } ]

// full identity round-trips
appid.runCommand({ createUser: "full", pwd: "pw", roles: rw,
  commitIdentity: { name: "Alice Example", email: "alice@acme.com" } })
appid.runCommand({ usersInfo: "full" })   // users[0].commitIdentity == { name: "Alice Example", email: "alice@acme.com" }

// name-only identity
appid.runCommand({ createUser: "nameonly", pwd: "pw", roles: rw,
  commitIdentity: { name: "Bob" } })
appid.runCommand({ usersInfo: "nameonly" })  // commitIdentity.name == "Bob", no email

// no identity
appid.runCommand({ createUser: "plain", pwd: "pw", roles: rw })
appid.runCommand({ usersInfo: "plain" })     // no commitIdentity

// invalid email rejected -> BadValue (2)
appid.runCommand({ createUser: "bad", pwd: "pw", roles: rw,
  commitIdentity: { name: "Bad", email: "not-an-email" } })   // { ok: 0, code: 2 }

// updateUser: set -> replace -> clear
appid.runCommand({ createUser: "mut", pwd: "pw", roles: rw })
appid.runCommand({ updateUser: "mut", commitIdentity: { name: "Carol", email: "carol@acme.com" } })
appid.runCommand({ updateUser: "mut", commitIdentity: { name: "Dave",  email: "dave@acme.com" } })
appid.runCommand({ updateUser: "mut", commitIdentity: null })   // cleared -> usersInfo shows no commitIdentity

// updateUser rejects a malformed identity -> BadValue (2)
appid.runCommand({ updateUser: "full", commitIdentity: { name: "X<y", email: "x@y.z" } })  // { ok: 0, code: 2 }

// commitIdentity is independent of customData
appid.runCommand({ usersInfo: "full", showCustomData: false })  // still returns commitIdentity
```
