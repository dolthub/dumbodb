# MongoDB Secondary Replication Protocol

Status: research basis for implementation  
Reference implementation: MongoDB Community Server 8.0.28  
Author: BSONnet  
Date: 2026-09-14

## Decision

DumboDB should join a MongoDB replica set as one data-bearing member configured
with `hidden: true`, `priority: 0`, and `votes: 0`. It will never be eligible to
become primary and will not affect election or majority-write quorums. It will use
the same logical initial-sync and ongoing oplog protocols as a stock secondary,
but translate the resulting database states into DumboDB commit history.

This avoids the consistency hole in a snapshot plus change-stream design. A
stock initial sync chooses an oplog position before cloning, fetches oplog entries
while the clone is running, and applies the buffered entries through a stop
position selected after the clone. The snapshot and the concurrent changes are
therefore joined by the same ordered oplog history. There is no attempt to assign
a change-stream resume token to an independently produced snapshot.

MongoDB does not publish a stable specification for its internal replication
commands. The implementation must therefore be compatibility-tested by server
release. Version 8.0.28 is the reference oracle, not an assertion that every 8.0
patch has an identical private protocol.

## Evidence and confidence

This document labels claims by their evidence:

- **DOC**: documented by the versioned MongoDB manual or a MongoDB specification.
- **SRC**: derived from source or tests at the exact `r8.0.28` tag.
- **WIRE**: observed in a live 8.0.28 three-member replica set through a
  bidirectional TCP recording proxy.
- **DESIGN**: a DumboDB requirement or deliberate simplification.

The wire lab used one primary, one ordinary electable secondary, and one hidden,
priority-zero, non-voting secondary. Every advertised replica-set address passed
through the proxy, so the capture included commands initiated by the subject
secondary and responses from its peers. Initial sync, steady writes, a
multi-document transaction, catalog changes, authentication, primary step-down,
election, and sync-source replacement were exercised.

## What is and is not negotiated

The first connection-level `hello` is an ordinary MongoDB command in an `OP_MSG`.
The subject secondary sent the following shape (volatile values omitted):

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
`compression`, `setName`, configuration version, role, primary and member
addresses, election identity, topology version, and last-write information.
In this all-8.0.28 lab the server response advertised wire version 25 as both its
minimum and maximum. Snappy was selected and subsequent traffic was predominantly
`OP_COMPRESSED` containing `OP_MSG`. **WIRE**

This is a real version and capability handshake, but it is not a versioned
replication-protocol handshake. It negotiates the general MongoDB wire range and
compression. The private fields and replication commands still have to be
interpreted according to the participating server versions and feature
compatibility version (FCV). MongoDB supports rolling 7.0-to-8.0 upgrades, which
demonstrates intentional adjacent-version interoperability; the upgrade procedure
also holds incompatible 8.0 behavior behind FCV until all binaries have been
upgraded. **DOC** [Upgrade a Replica Set to 8.0][upgrade]

**DESIGN:** DumboDB must reject a peer whose wire range does not overlap its tested
range, record peer binary/version information for diagnostics, and gate every
source-derived compatibility path by an explicit profile. The first profile is
`mongodb-8.0`, verified against 8.0.28. Negotiated wire version alone must not be
treated as proof that every internal field is compatible.

## Internal authentication

With keyfile authentication enabled, the subject advertised
`saslSupportedMechs: "local.__system"` in `hello`, then performed
`saslStart` and `saslContinue` against the `local` database. The observed start
shape was:

```javascript
{
  saslStart: 1,
  mechanism: "SCRAM-SHA-256",
  options: {skipEmptyExchange: true},
  payload: BinData(...),
  $db: "local"
}
```

The payload is deliberately excluded from this document and from repository
fixtures. **WIRE** Source establishes that the internal principal is
`__system@local` and that the configured internal authentication provider selects
keyfile SCRAM or X.509 behavior. **SRC**

**DESIGN:** internal authentication is part of replica membership, not optional
driver authentication. The initial implementation needs keyfile SCRAM-SHA-256.
X.509 cluster authentication is a separate compatibility increment. Credentials,
SCRAM conversations, and raw authenticated captures must never enter DumboDB
commits or diagnostic logs.

Inbound member authentication uses the same `__system@local` identity. That
principal is replication infrastructure, not a normal DumboDB user: its verifier
belongs in the replication control store, and `saslStart` resolves it through a
dedicated internal-auth path before consulting `admin.system.users`. It grants only
the internal cluster actions required by the member protocol. **DESIGN**

MongoDB user and role changes are not copied byte-for-byte into DumboDB's own auth
collections. The oplog applier recognizes changes to `admin.system.users` and
`admin.system.roles` and translates their meaning into DumboDB's authentication and
RBAC document format. Supported SCRAM credentials, role inheritance, authentication
restrictions, and privileges are preserved. An unsupported authentication mechanism,
resource pattern, action, or role expression aborts replication with the identity
and unsupported feature named; privileges must never be silently broadened or
dropped. DumboDB-local administrative identities occupy a reserved ownership domain
and cannot be overwritten by a same-named source identity. **DESIGN**

Replication ownership is persisted outside `admin.system.users` and
`admin.system.roles`, keyed by namespace and identity. The supported MongoDB
document remains usable by DumboDB authentication and RBAC without a private field
that could leak through `usersInfo` or `rolesInfo`. Initial sync and oplog
application reject a source identity that collides with an unowned local identity,
and every successful replicated identity mutation invalidates cached authorization.
**DESIGN**

### Logical cluster time

`$clusterTime` is vector-clock metadata, not incidental framing. A sustained
keyfile-authenticated lab run captured 1,108 messages containing `$clusterTime`
across both directions. All internal member messages omitted `signature` after
`__system` authentication. The same cluster returned real signed cluster time to
an unauthenticated external client, while an authentication-disabled server emitted
the legacy dummy signature with a zero hash and `keyId: 0`. **WIRE**

This is intentional in 8.0 at the latest FCV. `VectorClock::SignedComponentFormat`
treats internal clients as authorized, omits the dummy signature as an optimization,
and accepts an absent inbound signature as the dummy proof. A principal authorized
for `advanceClusterTime` bypasses cryptographic validation; unauthorized external
clients require a verifiable signature from the logical-time key manager. **SRC**

**DESIGN:** after successful internal authentication, DumboDB accepts unsigned
member `$clusterTime`, rejects it before authentication, enforces MongoDB's
maximum-future-time bound, and advances its logical clock monotonically. It emits
unsigned `$clusterTime` to authenticated internal peers under the MongoDB 8.0/latest
FCV profile. Mixed-FCV profiles may require a dummy signature and need a separate
probe. Replication does not need to clone `admin.system.keys` merely to sign internal
member traffic. External-client logical-time signing remains a general MongoDB
server compatibility concern, outside this replication increment.

## Configuration and identity

A member needs the replica-set name, its own configured host and member `_id`, and
the current configuration. Configuration freshness is ordered by configuration
`term`, then `version` when terms are equal or absent. MongoDB 8.0 only supports
replica-set protocol version 1. **DOC** [Replica-set configuration][config]

Configurations propagate in heartbeat responses. A newly started, unconfigured
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

The peer returned its full `config` because the requester was stale. Once
configured, heartbeats contained the subject's address and member ID. **WIRE**
The heartbeat parser retains compatibility for older senders that omit some newer
fields, including `primaryId`. **SRC**

The configured member owns one replica data set. DumboDB stores that data set on
each database repository's `main` branch; operators do not configure a replication
branch. Initial sync starts from the repository initial commit, and ongoing
replicated commits continue on `main`. Database partitioning is an apply/storage
concern, not a stock MongoDB replication boundary. **DESIGN**

Stock logical initial sync enumerates every non-`local` database. A member does not
ask MongoDB to initial-sync only selected databases. DumboDB may later filter or
route namespaces, but doing so makes its data set intentionally unlike a MongoDB
secondary and must be treated as a product mode rather than protocol behavior.
**DOC/SRC**

The reads of `config.transactions`, `config.image_collection`, and
`config.system.preimages` described in this document are source-side protocol
operations. DumboDB's reserved `config` database is not materialized as an ordinary
user database. Required transaction fragments and image records are translated into
typed records in the replication control store. **DESIGN**

The initial profile validates and stores `config.transactions` and
`config.image_collection` records by logical-session identity and source optime. It
ignores the source session cache in `config.system.sessions`, which is not replicated
application state. `config.system.preimages` is rejected while change-stream
pre-images remain unsupported. None of these paths creates a user-visible DumboDB
`config` database. **DESIGN**

## Heartbeats and topology

All members heartbeat all other members, including hidden and non-voting members.
Heartbeats establish liveness, exchange configuration, communicate member state
and term, identify the believed primary and sync source, and carry replication
positions. The default heartbeat interval is normally two seconds. **DOC/SRC**

An 8.0.28 heartbeat response may include:

- `state`, `term`, config `v`, `configTerm`, and set name;
- `primaryId`, `electable`, and the member's time;
- applied (`opTime`/`wallTime`), written (`writtenOpTime`/`writtenWallTime`),
  and durable (`durableOpTime`/`durableWallTime`) positions;
- a newer full `config` when the requester is behind;
- `$replData` response metadata.

Not every field is present in every state. Parsers must enforce required invariants
while tolerating the optional combinations accepted by 8.0.28. **SRC/WIRE**

The subject configuration `hidden: true, priority: 0, votes: 0` has three distinct
effects. Hidden keeps it out of normal driver read selection. Priority zero makes
it ineligible to become primary. Zero votes prevents it from voting and removes it
from election and majority-write quorums. None of those settings removes the need
to track configurations, terms, primary changes, member liveness, or sync sources.
**DOC** [Replica-set members][members]

**DESIGN:** DumboDB will not implement candidacy, vote requests, primary catch-up,
or primary service in its first member profile. It must nevertheless update its
term when peers report a newer term and accurately identify itself as a secondary
only after initial sync has completed. The operator-facing setup must validate all
three configuration properties; relying on only `hidden` or only `priority: 0` is
unsafe.

## Inbound member command surface

A replica-set member is both a client and a server. The first implementation serves
these inbound commands:

| Command | Required behavior |
|---|---|
| `hello` | Return set identity, addresses, config version, primary belief, wire range, compression, and state-dependent `secondary`/`isWritablePrimary`. Never report either role during initial sync. |
| `replSetHeartbeat` | Authenticate the member, validate set/member/config identity, process newer terms, and return current state, optimes, primary belief, sync source, and a newer config when needed. |
| `_isSelf` | Confirm whether the probed advertised address identifies this process. |
| `replSetGetConfig` | Return the installed configuration for compatible administrative inspection. |
| `replSetGetStatus` | Return honest member state, initial-sync status, source, and replication positions. |
| `replSetGetRBID` | Return a persistent rollback ID. It is required if this member is ever considered as a source, although outbound replication is not supported initially. |

`topologyVersion` is enabled in the replication profile. Its process ID is stable
for one process lifetime and its counter advances whenever an externally observable
topology fact changes. Consequently, awaitable `hello` must also be implemented;
DumboDB cannot advertise `topologyVersion` and immediately answer every awaitable
request without waiting for a change or deadline. **DESIGN**

Serving downstream replication is outside the first implementation. Therefore
`find`/`getMore` on `local.oplog.rs` and downstream `replSetUpdatePosition` are
rejected with a stable, explicit unsupported-source error rather than partially
implemented. A live probe refined the consequence: a voting secondary explicitly
rejects `replSetSyncFrom` targeting a non-voter, but automatic selection relaxes the
hidden/non-voter preference on its second pass. In the lab, after the primary became
unavailable, a voting secondary briefly selected the hidden, non-voting member,
fetched through it, caught up, and then cleared it because it was no longer ahead.
Thus other members can attempt to chain from DumboDB as a fallback. They receive an
immediate refusal and select another source; if none exists, they cannot progress
until a supported source returns. **SRC/WIRE/DESIGN**

## Logical initial sync

MongoDB's public description says that logical initial sync clones all non-local
databases while concurrently buffering new oplog records, then applies the
buffered records and enters `SECONDARY`. It warns that the source oplog window must
cover the whole operation. **DOC** [Replica Set Data Synchronization][sync]

The exact 8.0.28 sequence, reconstructed from source and confirmed on the wire, is:

1. Select one eligible sync source and connect/authenticate.
2. Drop existing replicated user data on the destination.
3. Run `replSetGetRBID` and save the source rollback ID.
4. Read the newest source oplog entry. This is the default begin-fetch position.
5. Inspect `config.transactions` for the oldest prepared or in-progress transaction.
   If necessary, move begin-fetch earlier so the transaction history is available.
6. Read the newest source oplog entry again and save the begin-apply position.
7. Read `admin.system.version` for the source FCV after that position.
8. Start two concurrent tracks: fetch oplog entries into a destination-side buffer,
   and clone every non-local database.
9. After cloning, read the newest source oplog entry as the stop position.
10. Apply buffered entries from the begin-apply boundary through the stop position.
11. If the source oplog did not advance, seed the destination oplog appropriately.
12. Run `replSetGetRBID` again. A changed rollback ID invalidates the attempt.
13. Persist consistent completion state and enter `SECONDARY`.

This is the protocol answer to the snapshot-boundary problem. The clone is not
assumed to represent one instant. The oplog covers changes concurrent with the
clone, and the before/after positions plus rollback-ID comparison prove that the
history used to reconcile it remained valid. **SRC/WIRE**

The observed catalog and clone commands were ordinary MongoDB commands:

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

Collection cloning uses the collection UUID rather than only its name. It preserves
options, validators, indexes, and unfinished index-build information. The natural
order document cursor requests a server cursor resume token; transient retries can
use `resumeAfter`. This token belongs to the collection-clone cursor and must not be
confused with a change-stream resume token. **SRC/WIRE**

Collection drop or rename and temporary network errors can be retried within the
initial-sync retry period. Persistent failure, an oplog gap, source rollback, or an
unrecoverable catalog race restarts the attempt. The attempt must be idempotent
because partially cloned state is disposable. **DOC/SRC**

**DESIGN:** the destination-side oplog buffer and initial-sync control records must
live outside the replicated DumboDB user state. The old emulation of MongoDB's
`local` database inside normal DumboDB transactions is explicitly not a dependency;
that implementation caused a global failed lock and is being removed. A dedicated
replication control store must atomically record attempt identity, source, RBID,
fetch/apply boundaries, and completion state.

Initial sync materializes the clone on `main` while fetched operations accumulate.
Only after buffered operations through the selected stop optime have been applied
may the member report a valid secondary image. Failed attempts reset `main` to the
repository initial commit before retrying. Incomplete data is hidden by MongoDB
member state, as it is on a stock secondary, rather than by a DumboDB branch.
**DESIGN**

Like a stock secondary, DumboDB selects and validates a sync source before clearing
existing replicated data for initial sync. It resets each database's `main` to the
repository initial commit rather than requiring an empty data directory. Starting
with `--replSet` therefore authorizes initial sync to replace existing `main` data.
Other DumboDB branches are outside the MongoDB replica data set and are not used by
replication. **SRC/DESIGN**

## Ongoing oplog fetch

Secondaries pull rather than receive pushed mutations. The source collection is
`local.oplog.rs`, a capped collection whose entries are ordered by `OpTime`: a BSON
timestamp plus election term. This is not wall-clock ordering and is unrelated to
a change-stream resume token. **DOC/SRC** [Replica Set Oplog][oplog]

The initial tailable request observed from the secondary was equivalent to:

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

The observed continuations were equivalent to:

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

The numeric batch size is an implementation tuning value, not a semantic constant.
The first `find` had the `OP_MSG` checksum-present flag in this capture. Implementors
must follow framing and negotiated compression rules rather than reproducing only
the decoded BSON. **WIRE** [MongoDB Wire Protocol][wire]

The inclusive `ts >= lastFetchedTimestamp` predicate is intentional. The first
returned entry must be the exact last fetched entry. It is checked and skipped,
proving continuity before any new entries are accepted. No first entry means the
source is not ahead in the necessary way; a nonmatching first entry indicates a
divergent or truncated history and drives rollback or too-stale handling. **SRC**

The fetch stream may use exhaust semantics, in which the server sends additional
`OP_MSG` responses marked `moreToCome`. DumboDB's network layer must support both
normal `getMore` and exhaust response flow because the choice is connection and
server dependent. **SRC** [OP_MSG specification][opmsg]

## Replication metadata

Replication commands request two private metadata documents by placing marker
fields in the command. The source attaches the corresponding documents to the
response.

`$replData` contains the source's view of:

- current election `term`;
- last committed optime and wall time;
- last visible optime;
- configuration version and term;
- replica-set ID;
- sync-source index;
- whether the sender is primary.

`$oplogQueryData` contains:

- last committed optime and wall time;
- last applied and last written optimes;
- rollback ID (`rbid`);
- believed primary index;
- sync-source index and host.

The capture contained both documents beside the oplog cursor. **SRC/WIRE** A member
uses them to learn commit progress, detect newer terms/configurations, detect source
rollback, and decide whether its source is stalled or part of a chain that cannot
advance. Indexes are meaningful only relative to the applicable configuration;
source explicitly warns that `primaryIndex` alone is unsafe across config versions.
**SRC**

## Oplog entries and application

The exact schema is defined in `oplog_entry.idl`, not fully in the manual. Important
fields include:

- `ts` and `t`: timestamp and election term forming the operation's `OpTime`;
- `op`: operation kind (`i`, `u`, `d`, `c`, `n`, and internal variants);
- `ns`: namespace;
- `ui`: collection UUID;
- `o`: inserted document, update delta/replacement, command, or no-op payload;
- `o2`: document key or secondary operation data;
- `wall`: diagnostic wall-clock time;
- `v`: oplog version;
- `lsid`, `txnNumber`, `stmtId`/`stmtIds`, `prevOpTime`, and
  `partialTxn`: retryable-write and transaction linkage;
- `fromMigrate`, pre/post-image links, tenant, and optional record metadata.

**SRC** [Oplog entry IDL][oplog-idl]

An oplog entry records an idempotent replication effect, not necessarily the
client's original command. Multi-document updates become independently applicable
effects. Transactions can be represented by `applyOps` command entries split over
multiple oplog records and linked by `prevOpTime`; prepared transactions add
prepare and commit/abort behavior. Catalog commands such as create, drop, rename,
and index operations also appear and must be applied in oplog order. **DOC/SRC**

**DESIGN:** an accepted source optime is the durable identity for ingestion. A
DumboDB commit may contain one oplog entry or a deterministic batch, but the control
record must preserve the inclusive interval of source optimes and the exact final
optime. A transaction must become visible atomically in DumboDB even when its source
representation spans multiple entries. Wall time is metadata only and must never
drive ordering or deduplication.

### Document field-order deviation

MongoDB preserves BSON document field order. DumboDB deliberately canonicalizes
stored documents by sorting object keys lexicographically at every level, except
within the top-level `_id` value where order participates in identity. Canonical
ordering is required for deterministic Prolly diffs and merges, but it means the
replica does not preserve the source document's BSON bytes or presentation order.

This deviation can affect behavior, not only presentation. MongoDB compares whole
embedded documents in field order, so equality, range, sort, grouping, distinct,
and index behavior involving embedded-document values may differ after
canonicalization. Differential tests must measure those cases explicitly. State
comparison may ignore object field order, but must remain exact for field names,
values, BSON types, arrays, and `_id`. **DESIGN**

### Version 2 update diffs

MongoDB 8.0 normally records modifier updates as a version 2 diff rather than a
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

Within an object diff, `d`, `i`, and `u` delete, insert, and update fields;
`s<field>` descends into that field. An array diff has `a: true`, optional `l` to
truncate or extend its logical length, `u<index>` to replace an element, and
`s<index>` for a nested element diff. Array indices are decimal suffixes, not BSON
field names. Operations are interpreted as one delta against the pre-update
document, with malformed or conflicting paths rejected. **SRC**

The pinned-server corpus produced nested deletes/inserts/updates, nested array
element changes, sparse extension through `u4`, whole-array replacements chosen by
the encoder for `$pop`, and retry-image linkage. Because the normal encoder chose a
whole-array replacement for the tested shrink operations, the lab also submitted a
legal version 2 delta through `applyOps`; `{sarr: {a: true, l: 2}}` was accepted,
replicated unchanged, and truncated the array. **WIRE** The applier requires golden
tests for every one of these shapes and must compare the final document against the
pinned server, not merely compare decoded diffs. **DESIGN**

Pre/post images have two storage paths. Retryable `findAndModify` marks the oplog
entry with `needsRetryImage: "preImage"` or `"postImage"` and stores the image in
source `config.image_collection`, keyed by session, transaction number, and optime.
A collection configured with `changeStreamPreAndPostImages.enabled` stores its
pre-images in `config.system.preimages`, keyed by collection UUID, operation optime,
and applyOps index. The lab observed both paths. **SRC/WIRE** DumboDB stores these as
typed control-store side records linked to the source optime and collection mapping;
it does not create a user-visible `config` database. Missing required retry images
abort application. Change-stream pre-images are retained only if DumboDB exposes
the corresponding change-stream behavior; until then, the collection option causes
an explicit initial-sync refusal rather than silent degradation. **DESIGN**

MongoDB distinguishes written, durable, applied, and majority-committed progress.
DumboDB must not report an optime as written until its oplog representation and
control checkpoint survive restart, or as applied/durable until the corresponding
DumboDB commit is durably reachable. Conflating fetched bytes with durable or
applied data could let an upstream node overestimate safety. Because the DumboDB
member is non-voting, it does not contribute to majority write concern, but false
progress still misleads diagnostics and chained replication. **DESIGN**

## Progress reporting

The downstream member sends `replSetUpdatePosition` to its current sync source.
The observed request carried an `optimes` array with entries for known live members:

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

**WIRE** The reporter is triggered by position changes and also sends keepalives
derived from the configured heartbeat interval. It serializes outstanding reports;
if progress changes during a request, another report is sent immediately after a
successful response. Source retains a downgrade path for peers that reject the
newer written-optime format, falling back to applied fields. The receiver similarly
accepts older reports without explicit written fields by treating applied as
written. **SRC**

Progress is forwarded along sync chains, not only directly to the primary. This is
why an `optimes` array can describe more than the sending member. **SRC/WIRE**

The DumboDB reporter serializes requests to the current sync source, sends its own
durable checkpoint plus healthy member positions learned through heartbeats, and
uses the installed replica configuration version. A durable publication wakes the
reporter immediately. If another publication finishes while a request is in flight,
the reporter sends the newer position immediately after the response; otherwise a
heartbeat-period keepalive refreshes the source. Failed connections are replaced on
the next report without advancing acknowledged progress. **DESIGN**

## Sync-source selection and primary changes

A secondary normally chooses a reachable data-bearing member that is ahead of its
last fetched optime and compatible with its index-building mode. Selection considers
read preference, member state, blacklist/denylist status, ping time, visibility,
voting status, configured delay, staleness, and whether chaining is allowed. It
makes a strict first pass and a less restrictive second pass if no candidate is
available. **DOC/SRC** [Replica Set Data Synchronization][sync]

During steady state it can replace the current source when:

- the source leaves the configuration or a forced source was requested;
- chaining is disabled and a different primary appears;
- a non-primary source is not ahead and has no source of its own;
- a sync cycle would prevent progress;
- another eligible member is more than `maxSyncSourceLagSecs` ahead;
- the current source fails strict eligibility and a better source exists;
- a materially lower-latency eligible source exists, subject to anti-thrashing rules;
- transport or oplog-fetch errors make another source preferable.

**SRC** In the lab, stepping down node 1 caused node 3 to win election. The hidden,
non-voting subject remained secondary, learned the new primary through heartbeats,
closed/replaced its replication connection, and resumed fetching from node 3.
**WIRE**

**DESIGN:** source replacement must not create a new DumboDB history. The candidate
must return the exact last fetched entry at the inclusive continuity check. Only
then may the existing `main` history advance. A source change without continuity
enters rollback/recovery handling.

## Rollback, stale history, and recovery

`replSetGetRBID` returns the source's rollback ID. Initial sync checks it before and
after the clone. Ongoing oplog metadata carries it on every response. A changed RBID
means the source rolled back while it was being used; previously inferred boundaries
from that source cannot be trusted without revalidation. **SRC/WIRE**

Steady-state divergence is detected by comparing local and remote oplog histories.
MongoDB rollback-to-stable finds a common point, rolls local data back to that point,
and then resumes forward replication. If the required common history has fallen out
of the source oplog, the member is too stale and requires initial sync. Startup
recovery also replays durable oplog state so applied data catches up consistently.
**SRC** [Replication internals][repl-readme]

DumboDB commit history can identify the abandoned source history during recovery,
but it must expose only one MongoDB state. **DESIGN:** on divergence, find the last
common source `OpTime`, reset `main` to that commit, and apply the winning history.
Never silently continue a linear history after mismatched continuity. If no retained
common optime exists, reset `main` to the repository initial commit and perform a
fresh initial sync. Preserving abandoned commits under an optional DumboDB ref is an
internal audit policy, not part of replication configuration or visible replica
state.

Rollback is a restartable control-store operation. Before changing any `main` head,
the member persists the source, source RBID, common source `OpTime`, target commit for
every database, and a unique audit-branch name. It first creates that audit branch
from every current `main` head, then hard-resets each `main` to its mapped common
commit. A restart with this manifest still pending reports `RECOVERING` and repeats
both operations idempotently. Only after all resets succeed does the member move the
active provenance head to the common interval, publish the rolled-back checkpoint,
and return to `SECONDARY`.

Common-point search walks retained commit intervals newest first and accepts only an
interval whose final `OpTime` is still present in the source oplog. A point is unsafe
when the control store contains transaction fragments or replicated metadata that
cannot be reconstructed at that point, or when catalog/authentication mappings were
updated after it. Such a point is skipped. If no safe point remains, the current
heads are preserved on an RBID-qualified audit branch, control provenance is moved to
a fresh generation, and normal initial sync resets `main` to each repository's
initial commit before cloning.

The replication control store must survive process restart and independently
record:

- replica-set ID, set name, member ID, and accepted configuration term/version;
- current term and last known primary;
- current source and RBID;
- last fetched, durably buffered, written, applied, and durable optimes;
- mapping from source optime intervals to DumboDB commits;
- initial-sync attempt ID, boundaries, and phase;
- transaction fragments not yet atomically applied.

These records must not be rolled back merely because an application transaction or
DumboDB work session fails. **DESIGN**

Replication control state and commit-interval provenance are stored in the reserved
`admin.system.dumbodb.replication` collection. The control document contains the
bounded mutable state as structured BSON. Each published commit interval is a
separate immutable document tagged with the active provenance generation. The
active generation retains every interval needed for source-optime lookup and
rollback; storage therefore grows by one document per DumboDB replication commit,
while the common append path writes a constant-size document independent of retained
history. A fresh initial sync advances the generation and removes the old generation.
A rollback copies its retained prefix into a new generation before atomically making
that generation active, then removes the old generation. Interrupted cleanup may
leave inactive documents, but they are ignored and can be removed safely later.

Publication inserts the immutable interval before advancing the checkpoint in the
control document. A crash between those mutations leaves the interval visible on
restart but the old checkpoint remains reportable; retry recognizes the identical
interval and completes the control update without reapplying data. Both mutations
use normal collection operations, so the Prolly storage layer owns atomicity,
durability, and crash recovery. DumboDB does not create replication state files or
implement journal writes, torn-record recovery, or filesystem durability. In-memory
lookup by source optime is binary. The replication benchmark must measure
publication and lookup/startup cost at the selected batch size before production
batching defaults are fixed. The collection is visible through `listCollections`
and normal queries so operators and tests can inspect it. Ordinary clients cannot
insert, update, delete, rename, drop, modify, compact, or change indexes on it; only
the replication-control backend can mutate it. **DESIGN**

Publishing a source interval uses a durable manifest identified by a hash of its
source boundaries and sorted database set. The manifest is written before any
mutation as `applying`. A crash in that phase hard-resets the named database
working roots to their current `main` heads, clears fetched progress after the last
published interval, and refetches the operation. Once every mutation succeeds, the
manifest durably changes to `ready`; recovery can then finish publication without
reapplying it. The manifest records each resulting `main` commit ID. Every database commit
uses the manifest ID in its commit message. After a crash or ambiguous commit
result, recovery compares each database HEAD with that message, records an already
completed commit, and commits only the remaining working roots. The final control
document update contains the complete sorted database-to-commit mapping and the
written, durable, and applied checkpoint. Only that collection update makes the
positions reportable. **DESIGN**

## Collection identity

MongoDB's source collection UUID (`ui`) is the authoritative replication identity.
It is stable across rename and is the key carried by oplog operations. DumboDB's
existing collection UUID is deterministically derived from database and collection
name, so it cannot substitute for `ui` and changes when a collection is renamed.
**SRC/DESIGN**

The control store maintains this mapping:

```text
(replicaSetId, sourceCollectionUUID)
    -> (database, collection, localCollectionUUID, creationOpTime, state)
```

Initial sync creates it with the local catalog entry. Every apply resolves `ui`
through it, never by recomputing from the current name. A rename commit atomically
renames the local catalog object and updates the mapped name/local UUID before its
source optime can be reported as applied. Drop tombstones the mapping at the drop
optime so a later reuse of the same name cannot receive operations for the old
source UUID. **DESIGN**

While the member is attached, replication owns `main`. Concurrent ordinary writes
and history-mutating `dumboReset`, `dumboRevert`, `dumboMerge`, or `dumboCherryPick`
operations against `main` are unsupported during this milestone. General branch
permissions and replication out of DumboDB are not prerequisites; operators may
fork the replicated history for independent work. Enforcement beyond this operating
contract is deferred to operational hardening. On a user fork, the
source UUID is provenance only. If a user-created collection collides by name with
a later replicated collection during a merge, the catalog merge must surface a
conflict rather than choose either identity silently. **DESIGN**

## Catalog compatibility gate

Initial sync performs a complete catalog preflight before publishing a secondary
state. The replication applier has only `SUPPORT` and `REJECT` outcomes. It never
uses an ordinary command path that accepts an option but fails to preserve or
enforce it. The initial MongoDB 8.0 profile is:

| Source feature | Replication result | Reason or condition |
|---|---|---|
| Ordinary collections and `_id` indexes | SUPPORT | Core representation |
| Validators and validation modes | SUPPORT | Stored and enforced by DumboDB |
| Views | SUPPORT | Preserve `viewOn`, pipeline, and collation |
| Time-series collections | SUPPORT | Without any TTL option |
| Capped collections | REJECT | Capped storage semantics are absent |
| Collection or index TTL | REJECT | Wall-clock deletion conflicts with historical state |
| Sparse indexes | SUPPORT | Sparse property is enforced |
| Partial indexes | SUPPORT | Preserve and enforce the filter expression |
| Unique indexes | SUPPORT | Preserve and enforce uniqueness |
| Non-simple index collation | REJECT | Currently stored but not enforced by index behavior |
| Text indexes | REJECT | Language and weight options are currently discarded |
| `2d` and `2dsphere` indexes | REJECT | Geometry/version options are currently discarded |
| Wildcard indexes | REJECT | Projection semantics are currently discarded |
| Hidden indexes | REJECT | Planner-level hiding is not enforced |
| Storage-engine-specific index options | REJECT | No equivalent DumboDB behavior |
| Clustered collections | REJECT | Clustered identity/layout is not implemented |
| Unfinished index builds | REJECT | Initial-sync continuation semantics are not implemented |
| `changeStreamPreAndPostImages` | REJECT initially | May become SUPPORT with change-stream image retention |

**DESIGN** This table describes the replication gate, not a blanket claim about all
DumboDB command handlers. Any feature promoted to `SUPPORT` requires differential
catalog, write-enforcement, query, and restart tests. A rejection names the database,
collection, index, and unsupported option. Preflight races are still caught during
oplog application; encountering an unsupported create or `collMod` stops progress
at the preceding applied optime.

## Operator surface and observability

Replication is an explicit server mode, configured at startup with the compatible
MongoDB `--replSet <name>` and `--keyFile <path>` settings. The installed
member/config identity is persisted in the control store. The process waits for an
authenticated peer heartbeat carrying a
configuration that contains its advertised address; no separate seed list is needed
for normal `rs.add` setup. It refuses to begin unless its member is `hidden: true`,
`priority: 0`, and `votes: 0`. A restart resumes from persisted state and revalidates
the current configuration before fetching. **DESIGN**

Removing the member from the replica-set configuration stops fetching and enters a
detached state without deleting `main` history. An administrative
`dumboReplicationDetach` operation may then clear active replication ownership while
preserving the history and its source-optime provenance. Reattaching to a different
replica-set ID requires a new initial sync; identities cannot be spliced. **DESIGN**

`replSetGetStatus` provides MongoDB-compatible member status and durable optimes.
`serverStatus.repl` reports replica-set identity and role,
`serverStatus.metrics.repl` reports apply, buffer, initial-sync, network, and
sync-source counters, and top-level `serverStatus.opcountersRepl` reports replicated
operation counts. These are the existing MongoDB monitoring surfaces; replication
does not add a DumboDB-specific status command. The internal source-optime-to-commit
mapping remains recovery metadata and is not exposed publicly. Durable positions are
read from the same control records used for recovery, while process-lifetime counters
are identified by their standard `serverStatus` placement. Secrets and authentication
payloads are never included. **DESIGN**

## Commit granularity and back-pressure boundary

Commit granularity is deliberately not fixed by this protocol design. The first
implementation uses deterministic batching with configurable limits on source
operation count, encoded bytes, and elapsed collection time; transaction visibility
remains atomic regardless of a limit. Exact defaults require a benchmark measuring
source rate, apply rate, commit latency, storage per million operations, lock wait,
and garbage-collection cost. That benchmark is a prerequisite for production
defaults, not for beginning transport and oplog parsing. **DESIGN**

The fetch buffer is bounded. When apply falls behind, DumboDB continues to report
only its truthful written/durable/applied positions, exposes the declining source
oplog-window budget, and alerts before the remaining window crosses the configured
minimum. It never advances progress to reduce upstream pressure. If the bounded
buffer fills, fetching pauses; if the source truncates the required continuity
point, the member enters `TooStale` and requires initial sync. The current global
database write lock makes sustained apply a likely chokepoint; throughput claims
depend on the working-set session-ownership work or measurements showing adequate
headroom. **DESIGN**

## Required state machine

```text
UNCONFIGURED
    | heartbeat supplies config containing self
    v
STARTUP -> STARTUP2 / INITIAL_SYNC
                  | clone + buffered oplog + final RBID succeeds
                  v
              SECONDARY <--------------------------+
                  |                                 |
                  | source unavailable/new primary |
                  +--> SELECT_SOURCE --> continuity+
                  |
                  | history mismatch
                  v
              ROLLBACK/RECOVERING --> common point -+
                  |
                  | no common retained history / too stale
                  v
              INITIAL_SYNC
```

The external `hello` role must follow this state. A process with an incomplete clone
must not report `secondary: true`. A permanently non-electable implementation may
omit candidate/primary transitions but cannot collapse startup, initial sync,
secondary, rollback, and recovering into one state. **DESIGN**

## Implementation boundaries

The protocol-facing implementation divides into these components:

1. **Member transport:** MongoDB message framing, checksums, compression,
   request-response and exhaust, connection lifecycle, deadlines, and internal auth.
2. **Topology agent:** `hello`, heartbeat mesh, config installation, term tracking,
   primary belief, liveness, and source selection.
3. **Initial syncer:** source/RBID validation, boundary selection, catalog clone,
   resumable document cursors, oplog buffering, catch-up, and atomic publication.
4. **Oplog fetcher:** inclusive continuity query, cursor/exhaust handling, metadata,
   buffering, and source-change classification.
5. **Oplog translator/applier:** complete entry schema, catalog operations,
   retryable writes, transaction assembly, deterministic batching, and DumboDB
   commits.
6. **Progress reporter:** honest written/durable/applied positions and forwarded
   member positions.
7. **Recovery controller:** restart recovery, source rollback, common-point search,
   `main` history repair, too-stale reinitialization, and audit preservation.
8. **Control store:** replication metadata outside user database transactions.

Each component needs golden message fixtures from sanitized live captures and
differential tests against the pinned `mongod`. Unit tests derived only from source
structures are insufficient because framing, optional fields, compression,
authentication ordering, and connection replacement are observable only as an
integrated system.

## Compatibility policy

- Support begins with MongoDB Community Server 8.0 and an 8.0 FCV profile.
- The protocol oracle is 8.0.28 source and executable behavior.
- Accept only overlapping wire-version ranges and supported compression/auth modes.
- Treat unknown required oplog operations or transaction forms as fatal to forward
  progress, never as skippable events.
- Treat document field-order canonicalization as the explicit storage deviation
  described above. It does not permit silent loss, coercion, or omission of any
  other supported document or catalog state.
- Preserve unknown optional fields in diagnostic capture where safe, but do not
  infer semantics.
- Run the full lab against every MongoDB patch claimed as supported and against
  mixed-version rolling-upgrade topologies before broadening the support statement.
- Keep parser compatibility localized by profile; do not scatter version tests
  through storage or apply logic.

## Reproducible verification lab

The investigation used the official
`mongodb-linux-x86_64-ubuntu2204-8.0.28` Community archive and source tag
`r8.0.28`. A TCP proxy listened on three addresses advertised in the replica-set
configuration and forwarded each to a private `mongod` port. The proxy preserved
bytes while recording direction, connection, message header, and payload. A
separate decoder expanded Snappy/zlib `OP_COMPRESSED`, parsed `OP_MSG` sections,
and rendered BSON as canonical Extended JSON.

The test sequence was:

1. Initiate node 1 as primary through its proxy address.
2. Create validated collections, a unique index, and 2,000 seed documents.
3. Add the subject as `{hidden: true, priority: 0, votes: 0}` and record its complete
   `STARTUP` -> `STARTUP2` -> `SECONDARY` initial sync.
4. Add an electable node 3.
5. Run insert, update, delete, a cross-collection transaction, create with validator,
   index create/drop, rename collection, and drop collection.
6. Step down node 1, allow node 3 to win, and observe subject source replacement.
7. Restart all nodes with a keyfile and record the internal SCRAM handshake.
8. Leave authenticated replication running and classify signatures on every
   `$clusterTime` in both directions.
9. Pause one voting secondary, advance the hidden non-voter, stop the primary, and
   observe the voting secondary's two-pass source selection and oplog fetch.
10. Generate nested, array, replacement, retry-image, and pre-image updates; inspect
    their oplog and both image collections. Submit a legal `l` array-resize diff
    through `applyOps` because the normal encoder chose replacement arrays for the
    tested `$pop` operations.

During the initial run, commands initiated by the subject toward its source included
the following counts. Counts are evidence of coverage, not protocol constants:

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

The capture is intentionally not checked into the repository: the authenticated
run contains SASL payloads and captures are environment-specific. Reproduction
must use newly generated test-only credentials. A publishable fixture generator
should decode, structurally redact all authentication payloads and cluster keys,
replace IDs/addresses/times with typed placeholders, then assert that re-encoding
retains the same BSON types and message flags.

## Known unknowns and required probes

This investigation establishes the main 8.0.28 member protocol but is not yet proof
of complete MongoDB compatibility. Implementation must add focused differential
tests for:

- every supported oplog command and transaction representation, including prepared
  and multi-entry transactions, retryable writes, time-series buckets, views, and
  index builds interrupted by failover;
- rollback-to-stable traffic and recovery after process death at every durability
  boundary;
- an oplog rolling past the requested continuity point (`TooStale`);
- reconfiguration while cloning and during steady fetch;
- mixed 7.0/8.0 rolling-upgrade topology and FCV transitions;
- X.509 cluster authentication and TLS;
- exhaust cursors under compression, checksum errors, oversized messages, malformed
  BSON, and cursor invalidation;
- delayed members, disabled chaining, and forced source changes;
- sharded-cluster/config-server and tenant-specific fields, which are outside the
  first standalone replica-set scope.

These are explicit compatibility tests, not reasons to return to change streams.
The stock secondary algorithm supplies the required snapshot/history relationship;
the remaining work is faithful protocol and apply implementation.

## Sources

Primary sources were preferred throughout:

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

The MongoDB wire-protocol manual page carries a CC BY-NC-SA notice. This document
summarizes observed and source-derived behavior and does not reproduce that page.
MongoDB server source is SSPL-licensed; implementation should use clean-room-style
behavioral tests and independently written code rather than copying server code.

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
