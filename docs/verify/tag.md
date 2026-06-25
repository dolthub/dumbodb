# dumboTag Verification

> **Automated equivalent:** `tests/verify/tag_test.go`
> Run with `go test ./tests/ -run TestTagVerify -count=1 -timeout=5m`

Manual verification guide for `dumboTag` end-to-end behavior. Work through each
scenario top to bottom. Each section builds on the previous setup.

Tags share Dolt's tag refspec (`refs/tags/<name>`) and use the Dolt tag
flatbuffer, so tags created via `dumboTag` are visible to `dolt tag` on the
underlying repository, and vice versa.

## Parameters

| Parameter | Type   | Required | Default                | Description                                                                  |
|-----------|--------|----------|------------------------|------------------------------------------------------------------------------|
| `name`    | string | no\*     |  --                      | Tag name. Required for create and delete. Omit to list. Must not contain `@` or whitespace. |
| `hash`    | string | no       | connection branch HEAD | Rootish (commit hash, branch, ancestor expression, or another tag) to tag.   |
| `delete`  | bool   | no       | `false`                | Delete the named tag. Mutually requires `name`.                              |
| `message` | string | no       | `""`                   | Tag description.                                                             |
| `author`  | string | no       | `"dumbodb <dumbodb@dumbodb>"`| Tagger identity "Name <email>".                                                                 |

\* `name` is required for create and delete; omit it to list all tags.

## Prerequisites

A running DumboDB instance and `mongosh` installed. Connect to your instance:

```js
mongosh mongodb://localhost:27017
```

Replace `localhost:27017` with your DumboDB address if different.

A working `dolt` CLI is required for Scenario 6 (cross-tool visibility). It
must point at the same data directory the DumboDB server is using.

---

## Setup: Create a database with two commits

Run this once before the scenarios below.

```js
var db = db.getSiblingDB("tagvdb")
db.dropDatabase()

// Commit 1: one document
db.items.insertOne({ _id: 1, label: "alpha" })
const r1 = db.runCommand({ doltCommit: 1, message: "commit one", author: "alice <alice@acme.com>" })
printjson(r1)
const hash1 = r1.commitId

// Commit 2: second document added
db.items.insertOne({ _id: 2, label: "beta" })
const r2 = db.runCommand({ doltCommit: 1, message: "commit two", author: "bob <bob@widgets.io>" })
printjson(r2)
const hash2 = r2.commitId

print("hash1 =", hash1)
print("hash2 =", hash2)
```

After setup, `tagvdb` has two commits on `main`:
- **hash1**  -- one document (`_id:1`)
- **hash2**  -- two documents (current `main` HEAD)

---

## Scenario 1: Create a tag at the current branch HEAD

When `hash` is omitted, `dumboTag` tags the connection's current branch HEAD.

```js
db.getSiblingDB("tagvdb").runCommand({
  dumboTag: 1,
  name:    "v-head",
  message: "tag at current head",
  author:  "alice <alice@example.com>",
})
```

Expected:

```json
{
  "name":      "v-head",
  "commitId":  "<hash2>",
  "author":    "alice <alice@example.com>",
  "message":   "tag at current head",
  "timestamp": ISODate("..."),
  "ok": 1
}
```

Key checks:
- `tags[0].name` is `"v-head"`
- `tags[0].commitId` equals `hash2` (current main HEAD)
- `tags[0].author`, `message` echo what you provided
- `ok` is `1`

---

## Scenario 2: Create a tag at a specific commit hash

`hash` accepts any rootish  -- a commit hash, a branch name, an ancestor
expression (e.g. `main~1`), or another tag name.

```js
db.getSiblingDB("tagvdb").runCommand({
  dumboTag: 1,
  name:    "v1.0",
  hash:    hash1,
  message: "first release"
})
```

Expected:

```json
{
  "name":     "v1.0",
  "commitId": "<hash1>",
  "author":   "dumbodb <dumbodb@dumbodb>",
  "message":  "first release",
  "timestamp": ISODate("..."),
  "ok": 1
}
```

Key checks:
- `tags[0].commitId` equals `hash1` (not the current main HEAD)
- `author` defaulted to `dumbodb <dumbodb@dumbodb>` (no `author` provided)

---

## Scenario 3: List all tags

Calling `dumboTag` with no `name` and no `delete` lists every tag in the database.

```js
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1 })
```

Expected:

```json
{
  "tags": [
    { "name": "v-head", "commitId": "<hash2>", "author": "alice <alice@example.com>", "message": "tag at current head", "timestamp": ISODate("...") },
    { "name": "v1.0",   "commitId": "<hash1>", "author": "dumbodb <dumbodb@dumbodb>", "message": "first release",       "timestamp": ISODate("...") }
  ],
  "ok": 1
}
```

Key checks:
- Both `v-head` and `v1.0` appear in the array (order is not guaranteed).
- Each entry has the metadata from the original create.

---

## Scenario 4: Delete a tag

Pass `delete: true` along with `name` to remove a tag. The response echoes the
deleted tag's name and the commit it pointed at.

```js
db.getSiblingDB("tagvdb").runCommand({
  dumboTag: 1,
  name:    "v-head",
  delete:  true
})
```

Expected:

```json
{ "name": "v-head", "commitId": "<hash2>", "ok": 1 }
```

Verify it's gone by listing again:

```js
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1 })
// Expected: tags contains v1.0 only; v-head is absent.
```

---

## Scenario 5: Tag persists across server restart

Tags are durable  -- they survive a server restart because they live in Dolt's
`refs/tags/` namespace on disk.

```js
// 1. Confirm v1.0 -> hash1 before restart
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1 })
// Expected: tags includes { name: "v1.0", commitId: <hash1>, ... }
```

Now stop and restart the DumboDB server (e.g. `Ctrl-C`, then re-run the same
launch command pointing at the same data directory). Reconnect with `mongosh`
and re-list:

```js
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1 })
// Expected: tags still includes v1.0 -> hash1 with the original metadata.
```

Key check: `v1.0` appears with the same `commitId`, `author`, and
`message` as before the restart.

---

## Scenario 6: Duplicate tag name is rejected

Creating a tag whose name already exists must fail rather than silently
overwriting the existing tag.

```js
db.getSiblingDB("tagvdb").runCommand({
  dumboTag: 1,
  name:    "v1.0",
  hash:    hash2
})
// Expected: command fails. ok: 0; errmsg contains "already exists".
```

Verify the original tag is unchanged:

```js
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1 })
// Expected: v1.0 still points at hash1 (NOT hash2).
```

Key check: the second create errors and `v1.0` still points at `hash1`.

---

## Scenario 7: Deleting a nonexistent tag is rejected

```js
db.getSiblingDB("tagvdb").runCommand({
  dumboTag: 1,
  name:    "does-not-exist",
  delete:  true
})
// Expected: command fails. ok: 0; errmsg indicates the tag does not exist.
```

Key check: the response is an error; no existing tags are affected.

---

## Scenario 8: Invalid tag names

Tag names must not contain `@` (reserved as the database/branch delimiter) or
whitespace. Names must not end with a path segment that looks like a commit
hash (32 lowercase base32 characters).

```js
// "@" is rejected
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1, name: "bad@name" })
// Expected: error  -- name must not contain '@'

// Whitespace is rejected
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1, name: "bad name" })
// Expected: error  -- name must not contain whitespace

// 32 lowercase base32 chars -- looks like a commit hash
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1, name: "na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// Expected: error  -- name looks like a commit hash

// Path ending with a hash-like segment is also rejected
db.getSiblingDB("tagvdb").runCommand({ dumboTag: 1, name: "releases/na7kfra98h45fr2u5qtr30o2ggm7vh61" })
// Expected: error  -- name ends with a segment that looks like a commit hash
```

Key check: all calls return errors; no tag is created.

---

## Quick Reference

| Command                                                          | Effect                                            |
|------------------------------------------------------------------|---------------------------------------------------|
| `{ dumboTag: 1 }`                                                | List all tags                                     |
| `{ dumboTag: 1, name: "v" }`                                     | Create tag `v` at current branch HEAD             |
| `{ dumboTag: 1, name: "v", hash: "<rootish>" }`                  | Create tag `v` at the resolved rootish            |
| `{ dumboTag: 1, name: "v", message: "...", author: "Name <email>" }` | Create tag with custom metadata          |
| `{ dumboTag: 1, name: "v", delete: true }`                       | Delete tag `v`                                    |

- Tag entries have `name`, `commitId`, `author`, `message`, `timestamp`.
- `hash` accepts any rootish: commit hash, branch name, ancestor expression, or another tag.
- Omitting `hash` on create uses the connection's branch HEAD.
- Tag names must not contain `@` or whitespace.
- Creating a tag with an existing name returns an error.
- Deleting a tag that does not exist returns an error.
- Tags persist across server restart and are visible to/from the `dolt tag` CLI.
