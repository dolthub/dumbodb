# Authentication & RBAC Verification

> **Automated equivalent:** `tests/verify/auth_rbac_test.go`
> Run with `go test ./tests/verify/ -run TestAuthRBACVerify -count=1 -timeout=5m`
> The same behaviour is additionally pinned against real MongoDB by the parity
> auth suite (`auth_*.go` in dumbodb-parity-testing).

Manual verification of authentication (`--auth`) and role-based access control.
Work through the scenarios top to bottom; later scenarios reuse users created
earlier.

## Prerequisites

Start a server with access control on and a **fresh, empty** data directory (so
the localhost exception applies and no users pre-exist):

```bash
dumbodb --auth --addr 127.0.0.1:27017 --data-dir /tmp/authtest
```

`mongosh` installed. Two things to keep straight throughout:

- **authSource.** A user authenticates against the database it was created in.
  A user created in `appdb` connects with `.../appdb` (authSource `appdb`); the
  bootstrap admin created in `admin` connects with `.../admin`.
- **One identity per connection.** Each `mongosh` connection authenticates as a
  single user. To act as a different user, open a new `mongosh` with that user's
  credentials.

---

## Scenario 1: Auth gate and first-user bootstrap

Connect unauthenticated and confirm the gate rejects real work but the localhost
exception lets you create the first user.

```js
// mongosh mongodb://127.0.0.1:27017
db.getSiblingDB("appdb").things.find().toArray()
```

**Expect:** rejected, `Unauthorized` (13) -- authentication required.

```js
db.getSiblingDB("admin").createUser({
  user: "admin", pwd: "admin-pw", roles: [{ role: "root", db: "admin" }]
})
```

**Expect:** `{ ok: 1 }`. The first `createUser` from localhost with no users
present is permitted; the exception is now spent.

```js
db.getSiblingDB("appdb").things.find().toArray()   // still unauthenticated
```

**Expect:** still `Unauthorized` (13).

---

## Scenario 2: SCRAM authentication

```bash
mongosh "mongodb://admin:admin-pw@127.0.0.1:27017/admin"
```

**Expect:** connects. Confirm identity:

```js
db.runCommand({ connectionStatus: 1 }).authInfo.authenticatedUsers
// -> [ { user: "admin", db: "admin" } ]
```

Wrong password:

```bash
mongosh "mongodb://admin:wrong@127.0.0.1:27017/admin"
```

**Expect:** `AuthenticationFailed` (18), "Authentication failed."

---

## Scenario 3: Built-in role enforcement

As `admin`, create two users in `appdb`:

```js
// connected as admin
var app = db.getSiblingDB("appdb")
app.createUser({ user: "reader", pwd: "pw", roles: [{ role: "read",      db: "appdb" }] })
app.createUser({ user: "writer", pwd: "pw", roles: [{ role: "readWrite", db: "appdb" }] })
app.things.insertOne({ _id: 1, v: "seed" })   // admin is root; seed a doc
```

New connection as `reader`:

```bash
mongosh "mongodb://reader:pw@127.0.0.1:27017/appdb"
```
```js
db.things.find().toArray()          // Expect: [ { _id: 1, v: "seed" } ]
db.things.insertOne({ _id: 2 })     // Expect: Unauthorized (13)
```

New connection as `writer`:

```bash
mongosh "mongodb://writer:pw@127.0.0.1:27017/appdb"
```
```js
db.things.insertOne({ _id: 2, v: "bywriter" })   // Expect: ok
```

---

## Scenario 4: Custom role, single privilege

As `admin`:

```js
var app = db.getSiblingDB("appdb")
app.runCommand({
  createRole: "inserter",
  privileges: [{ resource: { db: "appdb", collection: "" }, actions: ["insert"] }],
  roles: []
})
app.createUser({ user: "ins", pwd: "pw", roles: [{ role: "inserter", db: "appdb" }] })
```

New connection as `ins`:

```js
db.things.insertOne({ _id: 3 })     // Expect: ok
db.things.find().toArray()          // Expect: Unauthorized (13) -- only insert granted
```

---

## Scenario 5: Grant / revoke a privilege (live)

As `admin`, widen then narrow the `inserter` role while `ins` stays connected:

```js
db.getSiblingDB("appdb").runCommand({
  grantPrivilegesToRole: "inserter",
  privileges: [{ resource: { db: "appdb", collection: "" }, actions: ["find"] }]
})
```

On the `ins` connection (no reconnect):

```js
db.things.find().toArray()          // Expect: now succeeds
```

Revoke it as `admin`:

```js
db.getSiblingDB("appdb").runCommand({
  revokePrivilegesFromRole: "inserter",
  privileges: [{ resource: { db: "appdb", collection: "" }, actions: ["find"] }]
})
```

On the `ins` connection:

```js
db.things.find().toArray()          // Expect: Unauthorized (13) again
```

---

## Scenario 6: Role inheritance

As `admin`:

```js
var app = db.getSiblingDB("appdb")
app.runCommand({ createRole: "finder",
  privileges: [{ resource: { db: "appdb", collection: "" }, actions: ["find"] }], roles: [] })
app.runCommand({ createRole: "wrapper", privileges: [], roles: [{ role: "finder", db: "appdb" }] })
app.createUser({ user: "wrap", pwd: "pw", roles: [{ role: "wrapper", db: "appdb" }] })
```

New connection as `wrap`:

```js
db.things.find().toArray()          // Expect: succeeds (find inherited via finder)
```

Inspect the closure as `admin`:

```js
db.getSiblingDB("appdb").runCommand({ rolesInfo: "wrapper", showPrivileges: true })
// -> roles[0].inheritedPrivileges includes the find privilege
```

---

## Scenario 7: Dynamic user-role changes (live)

As `admin`, create a user with **no** roles:

```js
db.getSiblingDB("appdb").createUser({ user: "dyn", pwd: "pw", roles: [] })
```

New connection as `dyn`:

```js
db.things.find().toArray()          // Expect: Unauthorized (13)
```

As `admin`, grant `read`:

```js
db.getSiblingDB("appdb").runCommand({
  grantRolesToUser: "dyn", roles: [{ role: "read", db: "appdb" }]
})
```

On the `dyn` connection (no reconnect):

```js
db.things.find().toArray()          // Expect: now succeeds
```

As `admin`, revoke it:

```js
db.getSiblingDB("appdb").runCommand({
  revokeRolesFromUser: "dyn", roles: [{ role: "read", db: "appdb" }]
})
```

On the `dyn` connection:

```js
db.things.find().toArray()          // Expect: Unauthorized (13) again
```

---

## Scenario 8: Self-service

A user may view and change-password on **itself** without privileges over other
users.

On the `reader` connection:

```js
db.runCommand({ usersInfo: "reader" })     // Expect: ok, returns reader
db.runCommand({ usersInfo: "writer" })     // Expect: Unauthorized (13)
```

Changing your own password needs the `changeOwnPassword` action. As `admin`:

```js
var app = db.getSiblingDB("appdb")
app.runCommand({ createRole: "selfmgr",
  privileges: [{ resource: { db: "appdb", collection: "" }, actions: ["changeOwnPassword"] }],
  roles: [] })
app.createUser({ user: "self", pwd: "pw", roles: [{ role: "selfmgr", db: "appdb" }] })
```

New connection as `self`:

```js
db.runCommand({ updateUser: "self",   pwd: "newpw" })   // Expect: ok
db.runCommand({ updateUser: "reader", pwd: "newpw" })   // Expect: Unauthorized (13)
```

---

## Scenario 9: authenticationRestrictions

As `admin`, create a user pinned to a client source you are **not** connecting
from:

```js
db.getSiblingDB("appdb").createUser({
  user: "restricted", pwd: "pw", roles: [{ role: "read", db: "appdb" }],
  authenticationRestrictions: [{ clientSource: ["10.0.0.1"] }]
})
```

```bash
mongosh "mongodb://restricted:pw@127.0.0.1:27017/appdb"
```

**Expect:** authentication fails (the restriction is unmet; the client sees a
generic auth failure).

Recreate pinned to loopback:

```js
db.getSiblingDB("appdb").dropUser("restricted")
db.getSiblingDB("appdb").createUser({
  user: "restricted", pwd: "pw", roles: [{ role: "read", db: "appdb" }],
  authenticationRestrictions: [{ clientSource: ["127.0.0.1"] }]
})
```

```bash
mongosh "mongodb://restricted:pw@127.0.0.1:27017/appdb"
```

**Expect:** connects.

---

## Scenario 10: The admin database is reserved

On the `admin` connection (root):

```js
db.getSiblingDB("admin").probe.insertOne({ _id: 1 })
```

**Expect:** `Unauthorized` (13) -- the admin database is reserved; its contents
are managed only through the user/role commands. Even root cannot write it
directly.

---

## Scenario 11: connectionStatus with privileges

On the `reader` connection:

```js
var ai = db.runCommand({ connectionStatus: 1, showPrivileges: true }).authInfo
ai.authenticatedUsers            // -> [ { user: "reader", db: "appdb" } ]
ai.authenticatedUserRoles        // -> [ { role: "read", db: "appdb" } ]
ai.authenticatedUserPrivileges   // -> the read role's {resource, actions} list
```

---

## Scenario 12: One authentication per connection

A connection carries a single authenticated identity; MongoDB never stacks two
users on one connection. On the `admin` connection (already authenticated):

```js
db.getSiblingDB("appdb").auth("reader", "pw")
```

**Expect:** `{ ok: 1 }`. This does *not* prove a second identity was added --
mongosh re-authenticates through the driver, so the call succeeds and the shell
now acts as `reader`, not as `admin` + `reader`. `connectionStatus` always lists
exactly one user, never two.

The underlying wire-level guarantee -- that a raw second SCRAM handshake on the
same connection is accepted at `saslStart` but does not authenticate the second
user, leaving the original identity in place -- is what `TestAuthRBACVerify`
Scenario12 asserts, since mongosh hides the SASL exchange.

---

## Scenario 13: Role-management error fidelity

As `admin`, confirm the management commands return MongoDB's codes:

```js
var app = db.getSiblingDB("appdb")
app.runCommand({ createRole: "dup", privileges: [], roles: [] })
app.runCommand({ createRole: "dup", privileges: [], roles: [] })
// Expect: error 51002 (role already exists)

app.runCommand({ createRole: "orphan", privileges: [],
  roles: [{ role: "ghost", db: "appdb" }] })
// Expect: error 31, RoleNotFound ("Could not find role: ghost@appdb")

app.runCommand({ dropRole: "read" })
// Expect: error 2, BadValue ("read@appdb is a built-in role and cannot be modified")
```
