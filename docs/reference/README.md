# Reference

The documents in this directory describe how systems **outside this codebase**
behave: MongoDB servers and drivers, ICU, and Dolt. They cover only the parts
of those systems that DumboDB depends on. They are not complete documentation
of any of them.

- For how DumboDB itself behaves, read the code. The code is always the
  authority.
- The parts covered were chosen by what DumboDB depends on. A fact here
  describes the outside system, not DumboDB's implementation, current or
  planned.
- Each fact names its source: the MongoDB version probed, the source tag read,
  or the specification cited. A fact is only as good as that source. If a
  fresh probe of the stated version disagrees, the probe wins and the document
  should be corrected.

The MongoDB and ICU facts were recorded during earlier DumboDB development
(2026-05 to 2026-09) and moved here without being probed again. The Dolt
facts were read from the pinned Dolt source; check them again when the
pinned version changes.

| Document | Covers |
|---|---|
| [mongodb-replication-protocol.md](mongodb-replication-protocol.md) | MongoDB 8.0.28 replica-set member protocol: handshake, heartbeats, initial sync, oplog fetch, metadata, oplog entries, progress, sync source selection, rollback |
| [mongodb-collation.md](mongodb-collation.md) | MongoDB 8.0 collation scopes and precedence; ICU and Go collation library facts |
| [mongodb-sessions.md](mongodb-sessions.md) | MongoDB 8.0 logical session (`lsid`) behavior and driver constraints |
| [mongodb-views.md](mongodb-views.md) | MongoDB 8.0 standard (computed) view behavior |
| [dolt-storage.md](dolt-storage.md) | Dolt serialized objects, chunk graph, chunk stores, and optimistic locking, verified against the Dolt version pinned in `go.mod` |
