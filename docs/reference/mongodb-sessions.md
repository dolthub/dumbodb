# MongoDB Logical Sessions

## Session identity

Source: the MongoDB manual and the Driver Sessions specification.

The `lsid` (logical session ID) is a UUID that the client generates and sends
with every command:

```js
// { insert: "col", documents: [...], lsid: { id: UUID("abc-123-...") } }
```

A session belongs to its `lsid`, not to a TCP connection. Drivers keep one
session (one `lsid`) over a pool of connections, so a socket reset does not
end the session.

The MongoDB Driver Specification forbids drivers from accepting an `lsid`
supplied by the caller. `StartSession()` always generates a new UUID. So two
unrelated clients can share an `lsid` only by building raw wire-protocol
messages; no official driver can do it.

Server-side sessions expire after `logicalSessionTimeoutMinutes` of inactivity.
The default is 30.

Retryable writes deduplicate retries by `(lsid, txnNumber)`. Drivers retry
under the same `lsid`.

## Probe results (MongoDB 8.0, recorded 2026-05-20)

These were probed against a replica set and a standalone server. Raw OP_MSG
frames were used so that no driver could override the `lsid`.

**Concurrent non-transactional ops on one lsid from two connections:**
MongoDB accepts all of them. Plain reads and writes get no
session-is-locked check. Drivers never do this, but the server allows it.

**Transactions on one lsid:** `txnNumber` must increase. If connection B sends
`startTransaction: true` with txnNumber=2 on an lsid where connection A is in
txnNumber=1, A's next operation on txn 1 fails:

```
code: 225
codeName: TransactionTooOld
errmsg: "Cannot start transaction with { txnNumber: 1 } on session ...
         because a newer transaction with txnNumberAndRetryCounter
         { txnNumber: 2, ... } has already started on this session."
```

**`endSessions` is advisory, not an immediate kill:**

- On an lsid with a transaction in flight, it returns `ok: 1`. The next insert
  in the transaction succeeds, and so does `commitTransaction`. The transaction
  commits as if `endSessions` had never been sent.
- On an idle lsid (no transaction), it returns `ok: 1`. A later insert on the
  same lsid succeeds.
- On an lsid the server has never seen, it returns `ok: 1`.

The session keeps working until the idle timeout removes it.

## Session listing

Source: the MongoDB manual (not probed).

The `$listSessions` and `$listLocalSessions` aggregation stages enumerate
sessions. They return only the caller's sessions unless `allUsers: true` is
given, which requires extra privileges.
