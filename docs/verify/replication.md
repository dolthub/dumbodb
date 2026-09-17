# Verify: MongoDB secondary replication

DumboDB can join a MongoDB replica set as a hidden, non-voting member and
replicate the primary's data into DumboDB commit history. Every change that
arrives becomes a commit you can read with `dumboLog` and inspect with
`dumboDiff`.

Unlike the other verify documents, this one needs more than a running
dumbodb: it needs a real MongoDB replica set for DumboDB to follow. The
setup below builds one from scratch.

Everything here was walked through in `mongosh 2.3.1` against `mongod 8.0.28`
and dumbodb `v0.6.3-61-g193c6f7`. The stamp is part of the content: if you
change a command or an expected output, re-walk the document and update it in
the same commit. The automated analog is the replication suite in
`dolthub/dumbodb-parity-testing` (`go test -tags replication ./tests/`).

## Setup

You need a `mongod` binary (8.0.x) and a built `dumbodb`. Two servers:
MongoDB as the primary on 27017, DumboDB as the replica on 27018.

```bash
mkdir -p /tmp/verify/{rs0,dumbo}

mongod --replSet rs0 --port 27017 --dbpath /tmp/verify/rs0 \
       --bind_ip 127.0.0.1 --nounixsocket &

dumbodb --replSet rs0 --addr 127.0.0.1:27018 --data-dir /tmp/verify/dumbo &
```

Initiate the set with MongoDB alone in it. DumboDB joins later.

```js
// mongosh --port 27017
rs.initiate({ _id: "rs0", members: [ { _id: 0, host: "127.0.0.1:27017" } ] })
rs.status().myState   // Expected: 1 (PRIMARY), after a few seconds
```

Key check: `--replSet rs0` on the dumbodb command line is what puts it in
replica mode. Without it dumbodb is an ordinary standalone server.

## Scenario 1: Replicate a primary that already has data

Put some documents in MongoDB before DumboDB joins, so they have to arrive
through initial sync rather than the oplog.

```js
// mongosh --port 27017
db = db.getSiblingDB("shop")
db.items.insertMany([
  { _id: 1, name: "item-1", qty: 10 },
  { _id: 2, name: "item-2", qty: 20 },
  { _id: 3, name: "item-3", qty: 30 }
])
```

Now add DumboDB to the set. It must be `hidden`, `priority: 0` and
`votes: 0`: it is never eligible to be primary and never affects elections
or majority writes.

```js
// mongosh --port 27017
cfg = rs.conf()
cfg.version = cfg.version + 1
cfg.members.push({ _id: 1, host: "127.0.0.1:27018", hidden: true, priority: 0, votes: 0 })
rs.reconfig(cfg)
```

Wait a few seconds, then check both members from the primary:

```js
rs.status().members.map(m => [m.name, m.stateStr, m.health])
// Expected:
//   [ [ "127.0.0.1:27017", "PRIMARY",   1 ],
//     [ "127.0.0.1:27018", "SECONDARY", 1 ] ]
```

Key checks:
- DumboDB reaches `SECONDARY`, not `STARTUP2`. `STARTUP2` means it is still
  cloning, or stuck; `rs.status()` carries an `initialSyncStatus` document
  with the reason if it failed.
- `health: 1` means the primary is accepting DumboDB's heartbeats.

Read the data straight from DumboDB:

```js
// mongosh --port 27018
db = db.getSiblingDB("shop")
db.items.find().toArray()
// Expected: the three documents inserted above, byte for byte
```

Key check: DumboDB is a secondary, so it is read-only. Writing to it
directly is not how data gets in; it arrives from the primary.

## Scenario 2: Replicate into an empty DumboDB, then make changes

Everything after the join arrives through the oplog rather than the clone.
Change one document and add another, on the primary:

```js
// mongosh --port 27017
db = db.getSiblingDB("shop")
db.items.updateOne({ _id: 1 }, { $set: { qty: 99 } })
db.items.insertOne({ _id: 4, name: "item-4", qty: 40 })
```

Give it a few seconds, then read DumboDB again:

```js
// mongosh --port 27018
db.getSiblingDB("shop").items.find().sort({ _id: 1 }).toArray()
// Expected:
//   { _id: 1, name: "item-1", qty: 99 }   <- updated
//   { _id: 2, name: "item-2", qty: 20 }
//   { _id: 3, name: "item-3", qty: 30 }
//   { _id: 4, name: "item-4", qty: 40 }   <- new
```

Key check: the update and the insert both arrived, and `qty` on `_id: 1` is
99 rather than 10.

## Scenario 3: See the changes as commits

This is the part MongoDB cannot do. Each batch DumboDB applies becomes a
commit on `main`.

```js
// mongosh --port 27018
db.getSiblingDB("shop").runCommand({ dumboLog: 1 }).commits
  .map(c => [c.commitId.substring(0, 8), c.author, c.message.substring(0, 40)])
```

Expected shape:

```
[ "ug2s87ph", "MongoDB Replication <replication@dumbodb>", "MongoDB replication publication 0055854a" ]
[ "br9em4ro", "MongoDB Replication <replication@dumbodb>", "MongoDB replication publication 12ce8ef5" ]
...
[ "6b0fsnmg", "dumbodb <dumbodb@dumbodb>",                 "Initialize database" ]
```

Key checks:
- Replicated commits are authored by `MongoDB Replication`, so they are
  distinguishable from commits a user made.
- The oldest commit is `Initialize database`; everything above it arrived
  through replication.

The commit messages are opaque hashes, so to see what actually changed,
diff two adjacent commits. Take two neighbouring `commitId` values from the
log above, older first:

```js
db.getSiblingDB("shop").runCommand({
  dumboDiff: 1, from: "e76tgsc5kgq7v0ljjq620lgb1bj20cjk", to: "br9em4ro23940v1q88duhtj6ccsm50f5"
}).changes
```

Expected, for the pair that carried the update:

```json
[ { "name": "items", "type": "collection", "status": "modified",
    "documents": {
      "added": [], "removed": [],
      "modified": [ { "_id": 1, "diff": [
        { "path": "$.qty", "from": 10, "to": 99, "type": "modified" }
      ] } ]
    } } ]
```

Key check: the diff is field-level. It names the document, the path, and
both values, so a replicated update is fully attributable after the fact.

The pair that carried the insert shows it as an addition:

```json
[ { "name": "items", "status": "modified",
    "documents": { "added": [ { "_id": 4, "name": "item-4", "qty": 40 } ] } } ]
```

### Idle history check

Leave the primary idle for at least 20 seconds, then run `dumboLog` again.
The commit count must be unchanged. MongoDB periodically writes no-op oplog
entries; DumboDB advances its durable replication checkpoint for those entries
without adding empty commits to the versioned history.

## Scenario 4: Inspect replication health

```js
// mongosh --port 27018
status = db.adminCommand({ replSetGetStatus: 1 })
server = db.adminCommand({ serverStatus: 1 })
```

Key checks:
- `status.myState` is `2` (`SECONDARY`) after initial sync completes.
- `status.optimes.appliedOpTime`, `durableOpTime`, and `writtenOpTime` are
  non-zero and advance as the primary accepts writes.
- `server.repl.secondary` agrees with `status.myState`.
- `server.metrics.repl.buffer`, `apply`, `network`, and `syncSource` contain
  numeric counters, and `server.opcountersRepl` counts applied operation types.

## Scenario 5: Stop replicating, keep the history

Remove the member from the set the way you would in MongoDB:

```js
// mongosh --port 27017
cfg = rs.conf()
cfg.version = cfg.version + 1
cfg.members = cfg.members.filter(m => m.host !== "127.0.0.1:27018")
rs.reconfig(cfg)
```

```js
// mongosh --port 27017, after the reconfig
db.getSiblingDB("shop").items.insertOne({ _id: 5, name: "item-5", qty: 50 })
```

```js
// mongosh --port 27018
db.getSiblingDB("shop").items.find().sort({ _id: 1 }).toArray()
// Expected: four documents. _id 5 does NOT appear.
db.getSiblingDB("shop").runCommand({ dumboLog: 1 }).commits.length
// Expected: unchanged, and the replicated history is still readable
```

Key checks:
- The removed member stops taking new writes.
- It keeps serving, and it keeps everything it replicated. A removed
  DumboDB member is not a discarded stale node; it holds versioned history
  you can read, diff and branch from.

## Teardown

```bash
pkill -f "mongod --replSet rs0"
pkill -f "dumbodb --replSet rs0"
rm -rf /tmp/verify
```
