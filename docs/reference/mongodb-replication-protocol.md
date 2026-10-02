# MongoDB Replica-Set Member Protocol (8.0.28)

Reference implementation: MongoDB Community Server 8.0.28 (recorded 2026-09-14).

MongoDB does not publish a stable specification for its internal replication
commands. Everything here is specific to 8.0.28 and is not a promise that
every 8.0 patch release uses the same private protocol.

## Evidence labels

- **DOC**: documented in the versioned MongoDB manual or a MongoDB
  specification.
- **SRC**: derived from source or tests at the `r8.0.28` tag.
- **WIRE**: observed in a live 8.0.28 three-member replica set through a
  bidirectional TCP recording proxy.

The wire lab had one primary, one ordinary electable secondary, and one
secondary configured hidden, priority 0, and non-voting (the "subject"). Every
advertised replica-set address went through the proxy, so the capture includes
commands the subject sent and the responses from its peers. The runs covered
initial sync, steady writes, a multi-document transaction, catalog changes,
authentication, primary step-down, election, and sync-source replacement.

## What is and is not negotiated

The first `hello` on a connection is an ordinary MongoDB command in an
`OP_MSG`. The subject sent this shape (volatile values omitted):

```javascript
{
  hello: 1,
  client: {
    driver: {name: "NetworkInterfaceTL-ReplNetwork", version: "8.0.28"},
    os: {...}
  },
  hostInfo: "subject-host:subject-port",
  compression: ["snappy", "zstd", "zlib"],
  internalClient: {minWireVersion: 6, maxWireVersion: 25},
  hangUpOnStepDown: false,
  saslSupportedMechs: "local.__system",
  $db: "admin"
}
```

The peer's response included `minWireVersion`, `maxWireVersion`,
`compression`, `setName`, the configuration version, its role, the primary and
member addresses, election identity, topology version, and last-write
information. In this all-8.0.28 lab the server advertised wire version 25 as
both minimum and maximum. Snappy was selected, and most later traffic was
`OP_COMPRESSED` wrapping `OP_MSG`. **WIRE**

This handshake negotiates the general MongoDB wire range and compression. It
is not a versioned replication-protocol handshake: private fields and
replication commands must still be read according to the servers' versions and
feature compatibility version (FCV). MongoDB supports rolling upgrades from 7.0
to 8.0, and the upgrade procedure keeps incompatible 8.0 behavior behind FCV
until every binary is upgraded. **DOC** [Upgrade a Replica Set to 8.0][upgrade]

## Internal authentication

With keyfile authentication on, the subject advertised
`saslSupportedMechs: "local.__system"` in `hello`, then ran `saslStart` and
`saslContinue` against the `local` database. The start message looked like
this:

```javascript
{
  saslStart: 1,
  mechanism: "SCRAM-SHA-256",
  options: {skipEmptyExchange: true},
  payload: BinData(...),
  $db: "local"
}
```

**WIRE** The internal principal is `__system@local`. The configured internal
authentication provider chooses keyfile SCRAM or X.509. **SRC**

### Logical cluster time

`$clusterTime` is vector-clock metadata. A long keyfile-authenticated run
captured 1,108 messages carrying `$clusterTime` in both directions. Every
internal member message left out `signature` after `__system` authentication.
The same cluster returned a real signed cluster time to an unauthenticated
external client. A server with authentication disabled sent the legacy dummy
signature: a zero hash and `keyId: 0`. **WIRE**

This is intended in 8.0 at the latest FCV. `VectorClock::SignedComponentFormat`
treats internal clients as authorized, omits the dummy signature as an
optimization, and accepts a missing inbound signature as the dummy proof. A
principal with `advanceClusterTime` skips cryptographic validation. Unauthorized
external clients must present a signature that the logical-time key manager can
verify. **SRC**

## Configuration and identity

A member needs the replica-set name, its own configured host and member `_id`,
and the current configuration. Configuration freshness is ordered by
configuration `term`, then by `version` when the terms are equal or absent.
MongoDB 8.0 supports only replica-set protocol version 1.
**DOC** [Replica-set configuration][config]

Configurations travel in heartbeat responses. A newly started, unconfigured
subject sent:

```javascript
{
  replSetHeartbeat: "rs0",
  configVersion: -2,
  configTerm: -1,
  hbv: 1,
  from: "",
  fromId: -1,
  term: NumberLong(0),
  primaryId: -1,
  maxTimeMSOpOnly: 10000,
  $replData: 1,
  $clusterTime: {...},
  $db: "admin"
}
```

The peer returned its full `config` because the requester was behind. Once
configured, heartbeats carried the subject's address and member ID. **WIRE**
The heartbeat parser still accepts older senders that leave out some newer
fields, including `primaryId`. **SRC**

Logical initial sync enumerates every non-`local` database. A member cannot ask
for initial sync of selected databases only. **DOC/SRC**

## Heartbeats and topology

Every member heartbeats every other member, including hidden and non-voting
members. Heartbeats establish liveness, exchange configuration, report member
state and term, name the believed primary and sync source, and carry
replication positions. The default heartbeat interval is two seconds.
**DOC/SRC**

An 8.0.28 heartbeat response may include:

- `state`, `term`, config `v`, `configTerm`, and the set name;
- `primaryId`, `electable`, and the member's time;
- applied (`opTime`/`wallTime`), written (`writtenOpTime`/`writtenWallTime`),
  and durable (`durableOpTime`/`durableWallTime`) positions;
- a newer full `config` when the requester is behind;
- `$replData` response metadata.

Not every field appears in every state. **SRC/WIRE**

The member configuration `hidden: true, priority: 0, votes: 0` has three
separate effects:
- Hidden keeps the member out of normal driver read selection.
- Priority 0 makes it ineligible to become primary.
- Zero votes stops it from voting and removes it from election and
  majority-write quorums.

None of these settings relieves the member of tracking configurations, terms,
primary changes, member liveness, or sync sources.
**DOC** [Replica-set members][members]

A voting secondary rejects an explicit `replSetSyncFrom` that names a
non-voter. Automatic selection, however, relaxes the hidden and non-voter
preference on its second pass. In the lab, after the primary became
unavailable, a voting secondary briefly picked the hidden non-voting member,
fetched through it, caught up, and then dropped it once it was no longer
ahead. **SRC/WIRE**

## Logical initial sync

The MongoDB manual describes logical initial sync this way:
- It clones all non-local databases while buffering new oplog records at the
  same time.
- It then applies the buffered records and enters `SECONDARY`.
- The source's oplog window must cover the whole operation.

**DOC** [Replica Set Data Synchronization][sync]

The exact 8.0.28 sequence, rebuilt from source and confirmed on the wire:

1. Select one eligible sync source; connect and authenticate.
2. Drop existing replicated user data on the destination.
3. Run `replSetGetRBID` and save the source rollback ID.
4. Read the newest source oplog entry. This is the default begin-fetch position.
5. Look in `config.transactions` for the oldest prepared or in-progress
   transaction. If needed, move begin-fetch earlier so that transaction's
   history is available.
6. Read the newest source oplog entry again and save it as the begin-apply
   position.
7. Read the source FCV from `admin.system.version` after that position.
8. Start two tracks at once: fetch oplog entries into a buffer on the
   destination, and clone every non-local database.
9. After cloning, read the newest source oplog entry as the stop position.
10. Apply buffered entries from the begin-apply position through the stop
    position.
11. If the source oplog did not advance, seed the destination oplog.
12. Run `replSetGetRBID` again. A changed rollback ID invalidates the attempt.
13. Persist a consistent completion state and enter `SECONDARY`.

The clone is not assumed to represent one instant in time. The oplog covers
changes made while the clone ran, and the before and after positions, together
with the rollback-ID check, prove that the history used to reconcile it stayed
valid. **SRC/WIRE**

The catalog and clone commands seen on the wire were ordinary MongoDB commands:

| Stage | Command and important fields |
|---|---|
| Source stability | `replSetGetRBID: 1` on `admin` |
| Oplog boundary | `find: "oplog.rs"`, natural descending sort, limit 1, local read concern |
| Transaction floor | `find: "transactions"` in `config`, prepared/in-progress filter, sorted by start optime |
| FCV | `find: "system.version"` in `admin`, filter `_id: "featureCompatibilityVersion"` |
| Source sync identity | `find: "replset.initialSyncId"` in `local`, limit 1 |
| Database catalog | `listDatabases` with `nameOnly: true`; then `dbStats` |
| Collection catalog | `listCollections`; then `collStats` and `count` |
| Index catalog | `listIndexes` by collection UUID with `includeBuildUUIDs: true` |
| Documents | `find` by collection UUID, natural hint, `noCursorTimeout: true`, and `$_requestResumeToken: true` |

Collection cloning works by collection UUID, not just by name. It keeps
options, validators, indexes, and information about unfinished index builds.
The natural-order document cursor asks for a server cursor resume token, and
transient retries can use `resumeAfter`. That token belongs to the clone
cursor; it is not a change-stream resume token. **SRC/WIRE**

A collection drop or rename, or a temporary network error, can be retried
within the initial-sync retry period. Any of the following restarts the
attempt:
- persistent failure;
- a gap in the oplog;
- a rollback on the source;
- a catalog race that cannot be recovered.

Partially cloned state is thrown away. **DOC/SRC**

## Ongoing oplog fetch

Secondaries pull mutations; nothing pushes them. The source is
`local.oplog.rs`, a capped collection ordered by `OpTime`, which is a BSON
timestamp plus the election term. This order is not wall-clock order and has
nothing to do with change-stream resume tokens.
**DOC/SRC** [Replica Set Oplog][oplog]

The first tailable request from the secondary was equivalent to:

```javascript
{
  find: "oplog.rs",
  filter: {ts: {$gte: lastFetchedTimestamp}},
  batchSize: 13981010,
  tailable: true,
  awaitData: true,
  term: NumberLong(1),
  maxTimeMS: NumberLong(60000),
  readConcern: {level: "local", afterClusterTime: Timestamp(0, 1)},
  $replData: 1,
  $oplogQueryData: 1,
  $readPreference: {mode: "secondaryPreferred"},
  $clusterTime: {...},
  $db: "local"
}
```

The continuations were equivalent to:

```javascript
{
  getMore: NumberLong(cursorId),
  collection: "oplog.rs",
  batchSize: 13981010,
  maxTimeMS: NumberLong(5000),
  term: NumberLong(1),
  lastKnownCommittedOpTime: {ts: Timestamp(0, 0), t: NumberLong(-1)},
  $replData: 1,
  $oplogQueryData: 1,
  $readPreference: {mode: "secondaryPreferred"},
  $clusterTime: {...},
  $db: "local"
}
```

The batch size is a tuning value, not a meaningful constant. In this capture
the first `find` had the `OP_MSG` checksum-present flag set.
**WIRE** [MongoDB Wire Protocol][wire]

The `ts >= lastFetchedTimestamp` predicate is inclusive on purpose:
- The first returned entry must be exactly the last entry already fetched. It
  is checked and skipped, which proves continuity before any new entry is
  accepted.
- No first entry means the source is not ahead in the way required.
- A first entry that doesn't match means the history diverged or was
  truncated, which leads to rollback or too-stale handling.

**SRC**

The fetch stream may use exhaust semantics, where the server sends extra
`OP_MSG` responses marked `moreToCome`. Whether it does depends on the
connection and the server. **SRC** [OP_MSG specification][opmsg]

## Replication metadata

Replication commands request two private metadata documents by including marker
fields in the command. The source attaches the matching documents to its
response.

`$replData` holds the source's view of:

- the current election `term`;
- the last committed optime and wall time;
- the last visible optime;
- the configuration version and term;
- the replica-set ID;
- the sync-source index;
- whether the sender is primary.

`$oplogQueryData` holds:

- the last committed optime and wall time;
- the last applied and last written optimes;
- the rollback ID (`rbid`);
- the index of the believed primary;
- the sync-source index and host.

The capture had both documents next to the oplog cursor. **SRC/WIRE** A member
uses them to:
- learn commit progress;
- notice newer terms and configurations;
- detect a rollback on the source;
- decide whether its source has stalled or sits in a chain that cannot advance.

Indexes mean something only against the configuration they came from, and the
source code explicitly warns that `primaryIndex` alone is unsafe across
configuration versions. **SRC**

## Oplog entries

The exact schema is defined in `oplog_entry.idl`; the manual covers only part
of it. Important fields:

- `ts` and `t`: timestamp and election term, together the operation's `OpTime`;
- `op`: operation kind (`i`, `u`, `d`, `c`, `n`, and internal variants);
- `ns`: namespace;
- `ui`: collection UUID, which stays the same across renames;
- `o`: the inserted document, the update delta or replacement, the command,
  or the no-op payload;
- `o2`: document key or secondary operation data;
- `wall`: wall-clock time, for diagnostics only;
- `v`: oplog version;
- `lsid`, `txnNumber`, `stmtId`/`stmtIds`, `prevOpTime`, and `partialTxn`:
  links for retryable writes and transactions;
- `fromMigrate`, links to pre- and post-images, tenant, and optional record
  metadata.

**SRC** [Oplog entry IDL][oplog-idl]

An oplog entry records an idempotent replication effect, which is not
necessarily the client's original command:
- A multi-document update becomes a set of independently applicable effects.
- A transaction can appear as `applyOps` command entries split across several
  oplog records and linked by `prevOpTime`. Prepared transactions add prepare,
  commit and abort entries.
- Catalog commands such as create, drop, rename and index operations appear
  too, and must be applied in oplog order.

**DOC/SRC**

A direct write to a secondary is refused with code `10107`, codeName
`NotWritablePrimary`, and the legacy message `not master`. **WIRE**

### Version 2 update diffs

MongoDB 8.0 normally records modifier updates as a version 2 diff, not as a
replacement or a client-style `$set` document:

```javascript
{
  $v: 2,
  diff: {
    d: {removedField: false},
    i: {insertedField: value},
    u: {updatedField: value},
    snested: {u: {child: value}},
    sarray: {a: true, l: 2, u0: value, s1: {u: {child: value}}}
  }
}
```

The grammar:
- In an object diff, `d`, `i` and `u` delete, insert and update fields, and
  `s<field>` descends into that field.
- An array diff has `a: true`. An optional `l` truncates or extends its
  logical length, `u<index>` replaces an element, and `s<index>` holds a
  nested diff for an element. The indexes are decimal suffixes, not BSON field
  names.
- The whole diff is read as one delta against the document before the update.
  Malformed or conflicting paths are rejected.

**SRC**

The pinned-server corpus produced:
- nested deletes, inserts and updates;
- changes to nested array elements;
- sparse extension through `u4`;
- whole-array replacements, which the encoder chose for `$pop`;
- retry-image links.

The normal encoder used whole-array replacement for the shrink operations
tested. To exercise `l`, the lab submitted a legal version 2 delta through
`applyOps`: `{sarr: {a: true, l: 2}}` was accepted, replicated unchanged, and
truncated the array. **WIRE**

### Pre- and post-images

Pre- and post-images are stored in two different places:

- **Retryable `findAndModify`:** the oplog entry is marked
  `needsRetryImage: "preImage"` or `"postImage"`. The image goes to
  `config.image_collection`, keyed by session, transaction number and optime.
- **Change-stream images:** a collection with
  `changeStreamPreAndPostImages.enabled` stores its pre-images in
  `config.system.preimages`, keyed by collection UUID, operation optime and
  applyOps index.

The lab observed both paths. **SRC/WIRE**

MongoDB tracks written, durable, applied and majority-committed progress
separately.

## Progress reporting

A downstream member sends `replSetUpdatePosition` to its current sync source.
The request seen on the wire carried an `optimes` array with an entry for each
known live member:

```javascript
{
  replSetUpdatePosition: 1,
  optimes: [{
    writtenOpTime: {ts: Timestamp(...), t: NumberLong(...)},
    writtenWallTime: ISODate(...),
    appliedOpTime: {ts: Timestamp(...), t: NumberLong(...)},
    appliedWallTime: ISODate(...),
    durableOpTime: {ts: Timestamp(...), t: NumberLong(...)},
    durableWallTime: ISODate(...),
    memberId: 1,
    cfgver: NumberLong(...)
  }],
  $replData: {...},
  maxTimeMSOpOnly: 15000,
  $clusterTime: {...},
  $db: "admin"
}
```

**WIRE** How the reporter behaves:
- It sends a report when a position changes, plus keepalives based on the
  configured heartbeat interval.
- It sends one report at a time. If progress changes while a report is in
  flight, it sends another as soon as that one succeeds.
- If a peer rejects the newer written-optime format, it falls back to applied
  fields only.
- On the receiving side, an older report with no written fields is accepted
  and its applied values are treated as written.

**SRC**

Progress is passed along sync chains, not only straight to the primary. That
is why the `optimes` array can describe members other than the sender.
**SRC/WIRE**

## Sync-source selection and primary changes

A secondary normally chooses a reachable data-bearing member that is ahead of
its last fetched optime and compatible with its index-building mode. Selection
weighs:
- read preference and member state;
- blacklist or denylist status;
- ping time and visibility;
- voting status and configured delay;
- staleness, and whether chaining is allowed.

It makes a strict first pass, then a less restrictive second pass if it found
no candidate. **DOC/SRC** [Replica Set Data Synchronization][sync]

In steady state it can switch sources when:

- the source leaves the configuration, or a forced source was requested;
- chaining is disabled and a different primary appears;
- a non-primary source is not ahead and has no source of its own;
- a sync cycle would block progress;
- another eligible member is more than `maxSyncSourceLagSecs` ahead;
- the current source fails strict eligibility and a better source exists;
- an eligible source with much lower latency exists (with anti-thrashing rules);
- transport or oplog-fetch errors make another source preferable.

**SRC** In the lab, stepping down node 1 let node 3 win the election. The
hidden non-voting subject stayed secondary, learned of the new primary from
heartbeats, closed and replaced its replication connection, and resumed
fetching from node 3. **WIRE**

## Rollback and stale history

`replSetGetRBID` returns the source's rollback ID:
- Initial sync checks it before and after the clone.
- Ongoing oplog metadata carries it on every response.
- A changed RBID means the source rolled back while it was in use. Any
  boundaries already worked out from that source can't be trusted until they
  are checked again.

**SRC/WIRE**

Steady-state divergence is found by comparing the local and remote oplog
histories. MongoDB's rollback-to-stable:
1. finds a common point;
2. rolls local data back to it;
3. resumes forward replication.

If the common history it needs has dropped out of the source oplog, the member
is too stale and needs a fresh initial sync. Startup recovery also replays
durable oplog state so applied data catches up consistently.
**SRC** [Replication internals][repl-readme]

## Reproducing the lab

The investigation used the official
`mongodb-linux-x86_64-ubuntu2204-8.0.28` Community archive and the source tag
`r8.0.28`. A TCP proxy listened on the three addresses advertised in the
replica-set configuration and forwarded each one to a private `mongod` port.
The proxy kept the bytes unchanged and recorded the direction, connection,
message header and payload of each message. A separate decoder expanded
Snappy and zlib `OP_COMPRESSED`, parsed the `OP_MSG` sections, and printed the
BSON as canonical Extended JSON.

Sequence:

1. Initiate node 1 as primary through its proxy address.
2. Create validated collections, a unique index, and 2,000 seed documents.
3. Add the subject as `{hidden: true, priority: 0, votes: 0}` and record its
   full `STARTUP` -> `STARTUP2` -> `SECONDARY` initial sync.
4. Add an electable node 3.
5. Run insert, update, delete, a cross-collection transaction, create with a
   validator, index create and drop, rename collection, and drop collection.
6. Step down node 1, let node 3 win, and watch the subject replace its source.
7. Restart all nodes with a keyfile and record the internal SCRAM handshake.
8. Leave authenticated replication running and classify the signature on every
   `$clusterTime` in both directions.
9. Pause one voting secondary, advance the hidden non-voter, stop the primary,
   and watch the voting secondary's two-pass source selection and oplog fetch.
10. Generate nested, array, replacement, retry-image and pre-image updates, and
    inspect their oplog entries and both image collections. Submit a legal `l`
    array-resize diff through `applyOps`.

Commands the subject sent to its source during the initial run (these counts
show coverage; they are not protocol constants):

| Command | Count |
|---|---:|
| `_isSelf` | 2 |
| `hello` | 7 |
| `replSetHeartbeat` | 67 |
| `replSetGetRBID` | 2 |
| `replSetUpdatePosition` | 21 |
| `listDatabases` | 1 |
| `dbStats` | 3 |
| `listCollections` | 3 |
| `collStats` | 11 |
| `count` | 10 |
| `listIndexes` | 11 |
| `find` | 19 |
| `getMore` | 4 |
| `_refreshQueryAnalyzerConfiguration` | 7 |

The capture is not checked in: the authenticated run contains SASL payloads,
and captures depend on the environment. To reproduce it, generate new
credentials for testing only.

## Not covered by these observations

- prepared and multi-entry transactions, time-series buckets, and index builds
  interrupted by failover, as they appear in the oplog;
- rollback-to-stable traffic;
- an oplog rolling past the requested continuity point (`TooStale`);
- reconfiguration during cloning and during steady fetch;
- mixed 7.0/8.0 rolling-upgrade topologies and FCV transitions;
- X.509 cluster authentication and TLS;
- exhaust cursors combined with compression, checksum errors, oversized
  messages, malformed BSON, and cursor invalidation;
- delayed members, disabled chaining, and forced source changes;
- sharded-cluster, config-server and tenant-specific fields.

## Sources

- [MongoDB 8.0 replication manual][replication]
- [Replica Set Data Synchronization][sync]
- [Replica Set Oplog][oplog]
- [Replica-set members][members]
- [Replica-set configuration][config]
- [Upgrade a Replica Set to 8.0][upgrade]
- [MongoDB Wire Protocol][wire]
- [OP_MSG specification][opmsg]
- [MongoDB 8.0.28 replication internals][repl-readme]
- [MongoDB 8.0.28 oplog entry IDL][oplog-idl]
- [MongoDB 8.0.28 initial syncer][initial-syncer]
- [MongoDB 8.0.28 oplog fetcher][oplog-fetcher]
- [MongoDB 8.0.28 heartbeat arguments][heartbeat]
- [MongoDB 8.0.28 progress reporter][reporter]
- [MongoDB 8.0.28 sync-source selection][topology]
- [MongoDB 8.0.28 replication metadata][metadata]
- [MongoDB 8.0.28 vector-clock implementation][vector-clock]

The MongoDB wire-protocol manual page carries a CC BY-NC-SA notice. This
document summarizes behavior that was observed or derived from source; it does
not reproduce that page. MongoDB server source is SSPL-licensed: write
independent code and verify it with behavioral tests rather than copying server
code.

[replication]: https://www.mongodb.com/docs/v8.0/replication/
[sync]: https://www.mongodb.com/docs/v8.0/core/replica-set-sync/
[oplog]: https://www.mongodb.com/docs/v8.0/core/replica-set-oplog/
[members]: https://www.mongodb.com/docs/v8.0/core/replica-set-members/
[config]: https://www.mongodb.com/docs/v8.0/reference/replica-configuration/
[upgrade]: https://www.mongodb.com/docs/v8.0/release-notes/8.0-upgrade-replica-set/
[wire]: https://www.mongodb.com/docs/v8.0/reference/mongodb-wire-protocol/
[opmsg]: https://specifications.readthedocs.io/en/latest/message/OP_MSG/
[repl-readme]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/README.md
[oplog-idl]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/oplog_entry.idl
[initial-syncer]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/initial_syncer.cpp
[oplog-fetcher]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/oplog_fetcher.cpp
[heartbeat]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/repl_set_heartbeat_args_v1.cpp
[reporter]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/reporter.cpp
[topology]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/repl/topology_coordinator.cpp
[metadata]: https://github.com/mongodb/mongo/tree/r8.0.28/src/mongo/rpc/metadata
[vector-clock]: https://github.com/mongodb/mongo/blob/r8.0.28/src/mongo/db/vector_clock.cpp
