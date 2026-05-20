# SessionRegistry Design

## Goals and Non-Goals

**Goals**

- Map a MongoDB `lsid` to a long-lived `*dsess.DoltSession` so a logical
  session can span multiple TCP connections and survive short idle windows.
- Enforce single use: at any moment at most one connection holds an active
  reference to the session. Stale references from prior connections must
  refuse to read or write.
- Keep the read/write hot path lock-free with respect to the registry. Many
  in-memory mutations may accumulate between commits; none of them may take
  a registry-level lock.
- Update `lastUsed` on every read and every write. Sweep is keyed only on
  this timestamp.
- Preserve dolt's GC safepoint integration: a session's chunks must not be
  reclaimed while the session is in flight.

**Non-Goals**

- Refcounting. Not used anywhere in this design.
- Blocking handoff. Reconnect does not wait for stale users; it cancels them.
- Serialising commands on a single lsid. The wire dispatch is assumed to
  serialise frames per lsid by convention (MongoDB drivers do this).

## Single-Use Mechanism

The registry hands out a **shadow session** -- a small struct that fronts the
real `*dsess.DoltSession` and carries a `lastUsed` atomic and an `active`
latch (also atomic).

When a new connection arrives with an lsid that is already known:

1. The registry reads the current shadow from the entry.
2. It atomically flips the old shadow's `active` latch to `false`.
3. It creates a new shadow over the same `*dsess.DoltSession`, copying
   `lastUsed` forward so the timeout window does not reset.
4. It atomically swaps the entry's shadow pointer to the new one.

The stale connection still has a pointer to the old shadow. On its next
operation it reads `active == false` and returns `ErrShadowInvalidated`.
The dispatch layer translates that into a wire-protocol error and the
stale connection is effectively done.

The new connection holds the new shadow and continues uninterrupted.

This is what gives us single use without refcounting and without blocking.
There is exactly one valid shadow per lsid at any moment, by construction.

## Data Structures

```go
package sqlctx

import (
    "errors"
    "sync"
    "sync/atomic"
    "time"

    "github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"
)

// ErrShadowInvalidated is returned by Shadow.Use when the shadow has been
// superseded by a reconnect or removed by Sweep/End. Callers treat it as a
// terminal error for the stale connection.
var ErrShadowInvalidated = errors.New("session shadow invalidated by reconnect or sweep")

// Shadow is the data-path reference to a session. The latch (active) is
// flipped by the registry when this shadow is superseded; data-path
// operations check it on every call so a stale connection cannot keep
// using the underlying *dsess.DoltSession.
//
// writeMu fences the latch flip against in-progress commits. The
// supersede / sweep / end paths acquire writeMu before flipping active to
// false, so a commit currently in flight runs to completion before any
// cancellation takes effect. Reads and non-commit writes do not acquire
// writeMu -- they live entirely on the active latch.
type Shadow struct {
    sess     *dsess.DoltSession
    lastUsed atomic.Int64  // unix nanoseconds
    active   atomic.Bool   // false once superseded

    writeMu  sync.Mutex    // held by Commit, by registry latch-flip paths
}

// Use checks the latch, records activity, brackets fn with
// sess.CommandBegin / CommandEnd so the GC safepoint controller sees
// the session as in-flight for the duration, and invokes fn against
// the underlying session. Returns ErrShadowInvalidated if the shadow
// is no longer the active one for its lsid. Suitable for reads and
// non-commit writes that mutate only in-memory working set state.
func (s *Shadow) Use(now time.Time, fn func(*dsess.DoltSession) error) error

// Commit runs fn while holding writeMu, fencing the shadow's latch against
// supersede / sweep / end for the duration of the commit. fn is expected
// to be the dsess transaction-commit path (three-way merge + fsync). A
// reconnect or sweep arriving during the commit will block on writeMu and
// only flip the latch after the commit returns; the caller's commit
// completes against the on-disk state it started against, and the next
// operation on this shadow then observes the invalidated latch and errors.
//
// On return the latch may have been flipped while writeMu was held by the
// commit; callers that need to chain further work after the commit must
// check Active() afterward.
func (s *Shadow) Commit(now time.Time, fn func(*dsess.DoltSession) error) error

// LastUsed returns the most recent activity timestamp. Atomic load only.
func (s *Shadow) LastUsed() time.Time

// Active reports whether the shadow is still the active one for its lsid.
func (s *Shadow) Active() bool

// sessionEntry is the per-lsid registry record. The shadow pointer is the
// only thing that changes after creation; the underlying *dsess.DoltSession
// is permanent for the entry's lifetime.
type sessionEntry struct {
    sess   *dsess.DoltSession
    shadow atomic.Pointer[Shadow]
}

// SessionFactory mints a fresh *dsess.DoltSession for a given lsid. Called
// only on first Connect for a previously unknown id.
type SessionFactory func(lsid string) (*dsess.DoltSession, error)

// SessionRegistry maps lsid -> sessionEntry. All registry-level
// synchronisation (creation, supersession, sweep, end) is serialised
// through a single mutex. Data-path operations never touch this mutex --
// they operate on a cached *Shadow obtained from Connect.
type SessionRegistry struct {
    mu       sync.Mutex
    sessions map[string]*sessionEntry
    timeout  time.Duration
    factory  SessionFactory
    nowFn    func() time.Time
}

func NewSessionRegistry(timeout time.Duration, factory SessionFactory) *SessionRegistry
func (r *SessionRegistry) WithClock(now func() time.Time) *SessionRegistry

// Connect returns the active Shadow for lsid. If lsid is new, mints a
// session via factory. If lsid is known, supersedes the existing shadow
// (flipping its latch to false) and returns a new shadow that inherits
// lastUsed.
//
// This is the only place the caller obtains a Shadow. The connection is
// expected to cache the returned pointer on its ConnInfo and use it for
// every subsequent read or write until the connection drops or another
// connection supersedes it.
func (r *SessionRegistry) Connect(lsid string) (*Shadow, error)

// End is the advisory implementation of Mongo's endSessions. It marks
// the entry as stale (lastUsed = 0) so the next Sweep tick reaps it,
// but does NOT flip the latch and does NOT remove from the map. Active
// connections continue to function until Sweep runs. Returns whether
// lsid was known; either way, no immediate teardown occurs.
func (r *SessionRegistry) End(lsid string) bool

// PurgeNow forcibly invalidates and removes the entry for lsid. The
// shadow's latch is flipped, fenced against any in-flight commit, and
// the entry is deleted. Used by Sweep internally and by tests; not
// exposed to wire-protocol handlers.
func (r *SessionRegistry) PurgeNow(lsid string) bool

// Sweep removes entries whose shadow.lastUsed is older than asOf - timeout.
// The shadow is invalidated before removal so any in-flight operations on
// the stale connection error out cleanly.
func (r *SessionRegistry) Sweep(asOf time.Time) int

// Get returns the current shadow for lsid without recording activity.
// Intended for observation/tests.
func (r *SessionRegistry) Get(lsid string) (*Shadow, bool)

// Len reports the number of live entries.
func (r *SessionRegistry) Len() int
```

## Lifecycle

```
Connection 1 arrives with lsid X:
  Registry.Connect(X):
    mu.Lock
      no entry for X -> factory(X) creates *dsess.DoltSession S
      shadow_A := &Shadow{sess: S, lastUsed: now, active: true}
      entry  := &sessionEntry{sess: S}
      entry.shadow.Store(shadow_A)
      sessions[X] = entry
    mu.Unlock
  return shadow_A
Connection 1 caches shadow_A on its ConnInfo.

Connection 1 does many reads/writes:
  shadow_A.Use(now, fn) on every frame:
    if !shadow_A.active.Load(): return ErrShadowInvalidated
    shadow_A.lastUsed.Store(now)
    fn(shadow_A.sess)
  Registry mutex never touched.

Connection 1 disconnects (no close event needed):
  ConnInfo goroutine exits.
  shadow_A pointer dropped by ConnInfo, but the registry still holds it
  via entry.shadow. lastUsed reflects the last activity.

Connection 2 arrives with lsid X (within timeout):
  Registry.Connect(X):
    mu.Lock
      entry exists -> shadow_A := entry.shadow.Load()
      shadow_A.writeMu.Lock                // fences against in-flight Commit
        shadow_A.active.Store(false)       // latch off
        carriedLastUsed := shadow_A.lastUsed.Load()
      shadow_A.writeMu.Unlock
      shadow_B := &Shadow{sess: entry.sess,
                          lastUsed: carriedLastUsed,
                          active: true}
      entry.shadow.Store(shadow_B)
    mu.Unlock
  return shadow_B
Connection 2 caches shadow_B and proceeds.

Connection 1 (stale, if it ever returns):
  shadow_A.Use(now, fn):
    shadow_A.active.Load() -> false -> ErrShadowInvalidated
  Dispatch layer surfaces this as a wire-level "session closed" error.

Sweep (periodic, e.g. every minute):
  Registry.Sweep(now):
    collect eligible lsids:
      mu.Lock
        for lsid, entry := range sessions:
          if entry.shadow.Load().lastUsed.Load() < now - timeout:
            add lsid to eligible
      mu.Unlock
    for each eligible lsid:
      Registry.PurgeNow(lsid)              // re-checks under mu, fences commit

End (Mongo endSessions; advisory, matches Mongo's behavior):
  Registry.End(lsid):
    mu.Lock
      entry, ok := sessions[lsid]
      if !ok: return false           // matches Mongo's ok:1 on unknown
      entry.shadow.Load().lastUsed.Store(0)
    mu.Unlock
    return true
  (The next Sweep tick reaps the entry via PurgeNow.)

PurgeNow (internal; called by Sweep and tests):
  Registry.PurgeNow(lsid):
    mu.Lock
      entry, ok := sessions[lsid]
      if !ok: return false
      shadow := entry.shadow.Load()
      shadow.writeMu.Lock              // wait for in-flight Commit
        shadow.active.Store(false)
      shadow.writeMu.Unlock
      entry.sess.SessionEnd()          // GC controller: stop tracking
      delete(sessions, lsid)
    mu.Unlock
    return true
```

## Locking and Concurrency

| Path                      | Locks held                                              |
|---------------------------|---------------------------------------------------------|
| `Shadow.Use` (hot path)   | None. `active.Load`, `lastUsed.Store`, then `fn`.       |
| `Shadow.Commit`           | `shadow.writeMu` for the duration of `fn` (the commit). |
| `Shadow.LastUsed`         | None. `lastUsed.Load`.                                  |
| `Registry.Connect`        | `r.mu`, then briefly `oldShadow.writeMu` to fence.      |
| `Registry.Sweep`          | `r.mu`, then briefly each-shadow `writeMu` for reaped.  |
| `Registry.End`            | `r.mu`, then briefly `shadow.writeMu` to fence.         |
| `Registry.Get`            | `r.mu`. Observational only.                             |

Lock order: registry `r.mu` is always taken first, shadow `writeMu`
second. Commit takes only `shadow.writeMu` (never the registry mutex),
so there is no inversion. The shadow's `writeMu` is held briefly by the
registry paths (just the latch flip), and only the commit can hold it
for a long time -- by design, the registry paths wait for the commit to
land.

The registry mutex covers map mutation and shadow supersession in one
critical section so that "old shadow flipped off, new shadow stored"
happens atomically from the perspective of other Connect / End / Sweep
callers. The data path (`Use`) is entirely outside this mutex; the
commit path takes only the per-shadow writeMu, so concurrent commits on
different lsids run fully in parallel.

`sync.Map` is intentionally not used. The registry has at most a small
number of mutations per second (one per Mongo client connect, plus
Sweep / End), and the simpler `sync.Mutex` design avoids `sync.Map`'s
amortised-cost surprises and read-write fence subtleties. If profiling
ever shows the registry mutex as hot, we can migrate to `sync.Map` -- the
data-path contract does not change.

## GC Safepoint Integration

The factory passed to `NewSessionRegistry` is responsible for creating
sessions wired to the process's `gcctx.GCSafepointController`. Concretely,
the factory will call `dsess.NewDetachedSession(..., gcSafepointController,
...)` so the resulting session's `CommandBegin / CommandEnd / SessionEnd`
methods delegate to the controller.

Two integration points:

1. **Session creation**: after the factory returns, the registry calls
   `sess.CommandBegin()` immediately followed by `sess.CommandEnd()`. This
   registers the session with the controller as "known but quiesced." From
   that moment on, GC events visit the session's roots via
   `VisitGCRoots`, keeping its working-set chunks pinned.

2. **Session removal**: `End` and `Sweep` call `sess.SessionEnd()` on the
   underlying session, deregistering it. The controller stops visiting it;
   its chunks become eligible for collection on the next GC cycle.

**Open question on per-command bracketing.** In dolt's SQL path, every
statement is bracketed by `SessionCommandBegin / SessionCommandEnd` so GC
visits never overlap a statement's mutations. For DumboDB we have a
choice:

- **(A) No per-operation bracketing.** Session stays quiesced for GC. GC
  visits may overlap data-path mutations. Requires that `VisitGCRoots`
  tolerates the session's internal state changing under it. This is the
  "no locks on the hot path" model.

- **(B) Per-frame bracketing.** Wrap each wire frame in
  `SessionCommandBegin / CommandEnd`. Adds one mutex acquisition pair on
  the controller's global mutex per frame. Matches dolt's existing
  pattern.

Recommend (A) initially, with a property test that drives many concurrent
mutations while GC is firing to validate that `VisitGCRoots` is consistent.
Fall back to (B) only if we discover a real race. Worth flagging this
question for review before code.

## Wire-Dispatch Integration

`ConnInfo` gains a single field:

```go
type ConnInfo struct {
    ...
    cachedShadow *sqlctx.Shadow  // most recent active shadow for the conn
}
```

In `clientconn/conn.go`, after lsid extraction:

```go
ci := conninfo.Get(connCtx)
lsid := ci.LSID()
if lsid == "" {
    // Driver did not attach an implicit session (handshake, ping, legacy
    // wire client). Mint a server-private synthetic id and cache it on
    // ConnInfo so every subsequent frame from this connection lands on
    // the same shadow. Reconnect from a different TCP connection is not
    // possible against a synthetic id, by design.
    lsid = "synthetic:" + uuid.NewString()
    ci.SetLSID(lsid)
}

shadow := ci.CachedShadow()
if shadow == nil || !shadow.Active() {
    var err error
    shadow, err = c.h.SessionRegistry().Connect(lsid)
    if err != nil { ... }
    ci.SetCachedShadow(shadow)
}

err := shadow.Use(time.Now(), func(sess *dsess.DoltSession) error {
    return c.runHandler(connCtx, msg, command, sess)
})
if errors.Is(err, sqlctx.ErrShadowInvalidated) {
    // Surface to client as a session-closed error.
}
```

Synthetic ids are tagged with a `synthetic:` prefix so log lines, GC
debugging, and Sweep stats can distinguish driver-supplied from
server-generated sessions. The prefix has no semantic meaning to the
registry -- it treats the id as opaque.

If the frame does not carry an lsid (see below), the dispatch generates
a synthetic one and caches it on `ConnInfo` for the rest of the
connection. The connection still gets a session shadow; the only thing
it loses is the ability to reconnect on a fresh TCP connection -- the
synthetic id is server-private and won't be re-presented by any future
client. When the connection closes, Sweep eventually reaps it.

### Where lsids actually come from

Drivers attach an implicit session id to commands starting with MongoDB
wire protocol 3.6 (2017). The fields are:

```
{ <command>: ..., lsid: { id: <UUID Binary> }, $db: "...", ... }
```

Modern drivers stamp this on every command they issue. Frames that
arrive at DumboDB without an lsid fall into a small number of buckets:

| Source | Why no lsid |
|---|---|
| Handshake (`hello`, `isMaster`, `buildInfo`) | First messages on a connection, sent before the driver knows the server's capabilities. Drivers omit lsid here by spec. |
| Health-check `ping` and monitoring connections | The monitoring sub-connection in Mongo drivers is "session-less" by design -- it must not interfere with a primary's session-affinity decisions. |
| Pre-3.6 driver releases | Old Java / Node / C# / Python drivers from before implicit sessions existed. Rare in practice but still around in long-lived deployments. |
| Wire-protocol clients written by hand | CTF tooling, ad-hoc test harnesses, vanity drivers. They tend to omit lsid because the spec doesn't *require* it for commands; it's the driver's job to attach one. |
| Mongo shell with `--norc` or specific contexts | Earlier shell versions; current `mongosh` always attaches lsid. |
| Some replication / sharding internal commands | Not relevant to DumboDB (we don't speak those) but worth knowing for completeness. |

For our purposes the practical buckets are handshake and ping. Synthetic
lsids cost a UUID generation per affected connection -- negligible.

## Implementation Plan

1. **Add `Shadow` and `SessionRegistry` in `internal/sqlctx/`** (new
   `session_registry.go`). Pure-data-structure work, no integration. Tests
   below.

2. **Add `ErrShadowInvalidated` and the `Use` / `Active` / `LastUsed`
   surface.** Verified by unit tests.

3. **Wire factory + GC controller via `dsess.NewDetachedSession`.** Factory
   is supplied by `Backend` when constructing the registry.

4. **`Backend` owns the registry.** New field `Backend.sessions
   *sqlctx.SessionRegistry`. Initialised in `NewBackend`. Surfaced via a
   getter `Backend.SessionRegistry() *sqlctx.SessionRegistry`.

5. **Wire `Handler.SessionRegistry()` getter.** Same as the existing
   `SessionIsolation()` getter pattern.

6. **`ConnInfo.CachedShadow` field + accessors.**

7. **Wire dispatch in `clientconn/conn.go`** (the loop in `route` after
   lsid extraction). On every frame: ensure cached shadow is active, else
   Connect; then `shadow.Use(now, ...)` wraps the handler invocation.

8. **Sweep ticker.** `Backend.Run` (or a similar lifecycle hook) starts a
   goroutine that calls `registry.Sweep(time.Now())` on a fixed period
   (e.g. every minute). Sweep goroutine exits on `Backend.Close`.

9. **Remove the `maxPoolSize=1` workaround** in
   `tests/session_isolation_test.go TestSessionIsolation_MultiBranchIsolationOneSession`.
   Verify the test still passes (now via real lsid-keyed sessions instead
   of the conn:%p hack).

10. **Update `Backend.OnSessionEnd / OnTransactionCommit / OnTransactionAbort`**
    to either accept a `*Shadow` (preferred) or be looked up by lsid from
    the registry. This is where the pending-WS overlay machinery from
    `.3.8` plugs in cleanly.

Steps 1-7 are the registry MVP. 8-10 are the integration that makes it
do something useful.

## Test Plan

`internal/sqlctx/session_registry_test.go`:

| Test | What it proves |
|------|----------------|
| `Connect_CreatesAndCaches` | First Connect mints a session; subsequent Connect with same lsid returns a new shadow over the same underlying session |
| `Connect_InvalidatesPriorShadow` | After a reconnect, the prior shadow's Active() is false |
| `Connect_NewShadowInheritsLastUsed` | The reconnected shadow's LastUsed equals the old shadow's LastUsed at the moment of supersession |
| `Use_BumpsLastUsedAtomic` | Shadow.Use updates LastUsed to "now" |
| `Use_OnInvalidatedShadowErrors` | After Connect supersedes a shadow, Use returns ErrShadowInvalidated |
| `FactoryError_NoEntryStored` | Factory failure leaves the registry empty |
| `End_InvalidatesAndRemoves` | End flips active to false and removes from the map |
| `End_OnUnknownLsidReturnsFalse` | |
| `Sweep_RemovesIdle` | Shadow whose LastUsed is past cutoff is invalidated and removed |
| `Sweep_KeepsActive` | Shadow within timeout survives sweep |
| `Sweep_InvalidatesBeforeRemoval` | The swept shadow's Active() is false (verifies the latch flip happens before deletion, so any in-flight Use sees the error rather than operating on a removed session) |
| `Race_UseVsConnect` | Many goroutines doing Use against shadow_A while another goroutine calls Connect (superseding to shadow_B). After supersession, all subsequent Use calls on shadow_A error. Run under -race. |
| `Race_UseVsSweep` | Use loop against an active shadow while Sweep fires repeatedly with a not-yet-elapsed timeout. Use must continue to succeed; shadow.LastUsed must keep advancing. Run under -race. |
| `Commit_FencesAgainstReconnect` | (Q6 confirmed requirement.) Goroutine A calls Shadow.Commit with fn that sleeps 50ms. Goroutine B calls Registry.Connect for the same lsid 5ms after A starts. Assertions: (a) B's Connect does NOT return before A's commit fn returns (measured via timestamps; B's return time must be >= A's commit-fn return time minus a small skew tolerance); (b) after both return, the original shadow_A.Active() is false; (c) the new shadow_B is the entry's current shadow; (d) shadow_A.Use returns ErrShadowInvalidated. |
| `Commit_FencesAgainstSweep` | Same fence semantics with Sweep firing during the commit. Sweep must observe the commit-fn return time before it can flip the latch. |
| `Commit_OnInvalidatedShadowErrors` | Shadow whose active=false (already superseded) -- Shadow.Commit returns ErrShadowInvalidated immediately, fn does not run. |
| `Commit_ConcurrentDifferentLsidsParallel` | Two concurrent Commits on shadows for different lsids run in parallel (writeMu is per-shadow). Verify via timing: 2 commits each sleeping 50ms complete in ~50ms wall-clock, not ~100ms. |
| `Commit_DurabilityUnaffectedByConcurrentSupersede` | Commit fn writes a durable side-effect (counter increment to a test sink) and returns. Concurrent supersede during the commit must not corrupt the side-effect: every started commit must complete its side-effect exactly once before the supersede takes effect. |
| `End_IsAdvisoryNotImmediate` | After End(lsid), the existing shadow.Active() must still be true (End does not flip the latch). Use on the shadow still succeeds. Sweep on the next tick reaps the entry because End set lastUsed to 0. |
| `End_DoesNotFenceAgainstCommit` | Commit fn sleeping 50ms; End called 5ms in. End returns immediately (before commit returns). Commit completes normally. Subsequent Sweep reaps. |
| `Concurrent_Connect_DifferentLsids` | Many goroutines Connect to different lsids in parallel. Registry length matches the number of distinct lsids. Run under -race. |
| `Concurrent_Connect_SameLsid` | Many goroutines Connect to the same lsid in parallel. Exactly one shadow is "active" at the end; all others were invalidated. (Tests the supersession path under contention.) |
| `LastUsed_AcrossSupersession` | Idle pattern: T=0 Connect, T=4 reconnect (carry forward lastUsed), T=8 Sweep with 10-minute timeout -- the reconnected shadow survives because lastUsed was inherited |

`internal/sqlctx/session_registry_integration_test.go` (later, after
wire-dispatch integration):

| Test | What it proves |
|------|----------------|
| `LSIDReconnectResumesState` | Two TCP connections with the same lsid in sequence; the second sees pending state written by the first |
| `LSIDReconnectInvalidatesFirstConn` | Two TCP connections with the same lsid; after the second connects, the first's reads/writes return "session closed" |

`tests/session_isolation_test.go` regression:

- Drop the `maxPoolSize=1` workaround in
  `TestSessionIsolation_MultiBranchIsolationOneSession`. The test should
  pass against the new lsid-keyed registry without the pool-size hack.

## Resolved Decisions

### GC bracketing: per-frame Begin/End (model B)

There are two models for integrating with `gcctx.GCSafepointController`.

**Model A (no per-operation bracketing).** Register the session with the
controller once at creation (`SessionCommandBegin` immediately paired with
`SessionCommandEnd`). The session is then always "quiesced" from GC's
point of view. Whenever GC fires, the controller calls `VisitGCRoots` on
the session, which walks `dbStates`, branch heads, write-session working
sets, and pins those chunks. Operations on the data path do not bracket;
they just mutate. Zero locks on the hot path.

**Model B (per-frame bracketing).** Every wire frame is wrapped in
`sess.CommandBegin()` / `defer sess.CommandEnd()` around the handler
invocation. Between frames the session is quiesced; during a frame it has
`OutstandingCommand = true` and GC will not visit its roots. This matches
dolt's existing SQL path, where every statement is bracketed.

**Choosing B.** Model A is risky because `VisitGCRoots` reads the
session's `dbStates` map (briefly under `d.mu`) and then iterates
branch-state internals outside that lock. If a data-path mutation is
concurrently rewriting a branch state, GC could observe an inconsistent
walk -- a chunk might be missed, allowing the collector to reclaim it
while the session is still using it.

Cost of B: one mutex pair on the `GCSafepointController.mu` per wire
frame (`SessionCommandBegin` takes the controller's lock briefly to flip
`OutstandingCommand`; `SessionCommandEnd` does the same). This is **not**
the registry's mutex -- it's the GC controller's, and dolt itself takes
this lock per statement in normal SQL serving. At ~50ns per Lock+Unlock,
even 100k QPS amounts to ~10ms/sec of controller-lock time spread across
cores -- negligible.

Decision: model B. The shadow's `Use` and `Commit` paths both call
`sess.CommandBegin()` / `defer sess.CommandEnd()` around the user's `fn`.

### Connection-close: no registry action

No hook needed on TCP disconnect. The connection drops its cached shadow
pointer; the registry entry stays. Sweep handles the eventual reap based
on `lastUsed`. (Confirmed.)

### Sweep period: 1 minute (frequency, not timeout)

The two values are independent. `timeout` (default 30 min, configurable)
is how long a session can be idle before becoming eligible for reap.
Sweep period (default 1 min, fixed) is how often the registry walks
itself looking for eligible entries.

Implementation: a goroutine in `Backend.Run` calls
`registry.Sweep(time.Now())` on a 1-minute ticker and exits on
`Backend.Close`.

### Reconnect during commit: blocks until commit completes (Q6 confirmed)

The race: connection A is mid-commit (`shadow_A.Commit` is running,
holding `writeMu`). Connection B arrives with the same lsid.
`Registry.Connect` for lsid X reads `shadow_A`, then calls
`shadow_A.writeMu.Lock()` to fence the latch flip. This blocks B's
`Connect` call until A's commit returns. Once A's commit finishes:

1. B's `Connect` acquires `writeMu`, flips `shadow_A.active` to false,
   releases `writeMu`.
2. B creates `shadow_B` carrying forward `lastUsed`, swaps into the
   entry.
3. A's next operation on `shadow_A` (a read, write, or another commit)
   observes `active = false` and returns `ErrShadowInvalidated`.

The durable write completed against the on-disk state A had been merging
against. No partial commit, no torn fsync. A's connection is then
effectively dead from the next operation onward.

**Test requirements** (added below): explicit timing-based assertion that
B's Connect does not return before A's commit returns; explicit
assertion that after the commit + supersede, A's next operation returns
`ErrShadowInvalidated`.

### Wire-protocol behavior (from live-Mongo probes)

Probed against MongoDB 8.0 (replica set on `127.0.0.1:27019`,
standalone on `127.0.0.1:27017`) using raw OP_MSG frames so the driver
could not override our forged `lsid` field.

**Concurrent non-transactional ops on the same lsid from two
connections:** Mongo accepts all of them. There is no
"session-is-locked" enforcement for plain reads/writes. Drivers
generate unique lsids per logical session, so this case is
out-of-spec but server-accepted.

**Transactions on the same lsid:** monotonic `txnNumber`. When
connection B sends `startTransaction: true` with txnNumber=2 on an
lsid that's mid-txn=1 on connection A, A's next operation on txn 1
returns:

```
code: 225
codeName: TransactionTooOld
errmsg: "Cannot start transaction with { txnNumber: 1 } on session ...
         because a newer transaction with txnNumberAndRetryCounter
         { txnNumber: 2, ... } has already started on this session."
```

**`endSessions` is a best-effort hint, not an immediate kill.** Tested
three scenarios:

- `endSessions` on an lsid with an in-flight transaction: returns
  `ok: 1`. The next operation on the txn (insert) succeeds. The
  subsequent `commitTransaction` succeeds. The txn lands as if
  `endSessions` had not been called.
- `endSessions` on an idle (no-txn) lsid: returns `ok: 1`. A
  subsequent insert on the same lsid succeeds.
- `endSessions` on a never-seen lsid: returns `ok: 1`.

So `endSessions` is purely advisory; the session continues working until
the idle timeout reaps it normally.

These findings drive two changes to the design:

**Decision on stale-connection error code: code 225 TransactionTooOld.**
That's the closest existing Mongo error for "this session was
superseded." For transactional contexts the message can mirror Mongo's
format; for plain-op contexts we use a tailored message ("session was
taken over by a newer connection"). Driver-side error handling for code
225 already treats the session as terminal, which is exactly what we
want.

**Decision on End semantics: best-effort, not immediate.** Mongo's
`endSessions` does not invalidate or remove. Match that by changing
`Registry.End` to mark the entry as "stale" (set `lastUsed` to a value
that guarantees the next Sweep tick reaps it) rather than flipping the
latch and deleting in place. The MsgEndSessions handler just calls this
hint and returns `ok: 1`, matching Mongo. Explicit deletion is still
available internally as `Registry.PurgeNow(lsid)` for tests and for the
sweep path itself, but is no longer the public End surface.

```go
// End is the implementation of Mongo's endSessions: an advisory hint.
// Returns true if the lsid was known; either way, no immediate
// invalidation occurs. Sweep will reap on its next tick.
func (r *SessionRegistry) End(lsid string) bool {
    r.mu.Lock()
    defer r.mu.Unlock()
    entry, ok := r.sessions[lsid]
    if !ok {
        return false
    }
    // Mark lastUsed as stale: zero means "older than any cutoff."
    entry.shadow.Load().lastUsed.Store(0)
    return true
}
```

This also removes the cross-stream interaction with Q6: End no longer
needs to fence against an in-flight commit. The commit completes
normally; the next Sweep tick reaps the entry after the commit lands.

## Wire-Protocol Differences from MongoDB

The probes above reveal one area where this design is *stricter* than
MongoDB:

- **Mongo allows concurrent non-transactional ops on the same lsid from
  two connections.** Our design supersedes shadow_A the moment shadow_B
  arrives, so the older connection's next op errors out.
- **Mongo allows concurrent transactions with different `txnNumber`s on
  the same lsid only via monotonic supersession.** Our design has no
  notion of `txnNumber` at the registry layer; the latch flip is on
  Connect, not on txn-start.

This is deliberate -- DumboDB enforces single-writer-per-lsid as a
correctness property of its session model (per the requirements at the
top of this doc). The cost is a slightly stricter contract than Mongo's:
two connections that share an lsid for non-txn ops would coexist on
Mongo but would race on DumboDB, with the loser's next op getting
`code 225`. In practice Mongo drivers generate unique lsids per
StartSession, so this stricter contract has no visible effect on
real-world clients; it only affects forged-lsid traffic, which is the
"adversarial reconnect" case the design is meant to handle.
