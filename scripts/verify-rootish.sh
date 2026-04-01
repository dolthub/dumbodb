#!/usr/bin/env bash
# verify-rootish.sh — end-to-end smoke test for rootish connection string behavior.
#
# Usage:
#   bash scripts/verify-rootish.sh [host:port]
#
# Default host:port is localhost:27017. Run with a live Dongo instance.
#
# Tests only what is currently implemented:
#   - dongoCommit
#   - commit-hash rootish (reads + write-blocking)
#   - ancestor-expression rootish (reads + write-blocking)
#   - dongoCurrentBranch on branch vs read-only connections
#   - parse-time rejection of HEAD, HEAD~N, reflog, and range syntax
#
# Does NOT test dongoBranch/dongoMerge/dongoLog/dongoStatus/dongoReset —
# those are not yet implemented.

set -euo pipefail

HOST="${1:-localhost:27017}"
URI="mongodb://${HOST}"
DBNAME="vrootish_$$"
COLL="items"
PASS=0
FAIL=0

_js() {
    # Run a JS snippet and return its stdout. Exits non-zero on mongosh error.
    mongosh "$URI" --quiet --eval "$1" 2>&1
}

ok() {
    echo "  PASS: $1"
    PASS=$((PASS + 1))
}

fail() {
    echo "  FAIL: $1"
    echo "        $2"
    FAIL=$((FAIL + 1))
}

expect_no_error() {
    local label="$1"
    local js="$2"
    local out
    out=$(_js "$js" 2>&1) && ok "$label" || fail "$label" "$out"
}

expect_code_96() {
    local label="$1"
    local js="$2"
    local out
    out=$(_js "
try {
  $js
  throw new Error('expected error, got none');
} catch (e) {
  if (e.code === 96) { print('OK'); } else { throw new Error('expected code 96, got ' + e.code + ': ' + e.message); }
}" 2>&1)
    if echo "$out" | grep -q '^OK$'; then
        ok "$label"
    else
        fail "$label" "$out"
    fi
}

echo "Dongo rootish verification — ${URI}"
echo "Database: ${DBNAME}"
echo

# ---------------------------------------------------------------------------
echo "=== Setup: two commits on main ==="

SETUP_OUT=$(_js "
const db2 = db.getSiblingDB('${DBNAME}');
db2.getCollection('${COLL}').insertOne({ _id: 1, v: 'first' });
const r1 = db2.runCommand({ dongoCommit: 1, message: 'first commit' });
if (r1.ok !== 1) throw new Error('dongoCommit 1 failed: ' + JSON.stringify(r1));
db2.getCollection('${COLL}').insertOne({ _id: 2, v: 'second' });
const r2 = db2.runCommand({ dongoCommit: 1, message: 'second commit' });
if (r2.ok !== 1) throw new Error('dongoCommit 2 failed: ' + JSON.stringify(r2));
print(r1.hash);
")

HASH1=$(echo "$SETUP_OUT" | tail -1)
if [[ -z "$HASH1" || ${#HASH1} -ne 32 ]]; then
    echo "FATAL: setup failed — could not get 32-char commit hash"
    echo "Output was: $SETUP_OUT"
    exit 1
fi
echo "hash1 = ${HASH1}"
echo

# ---------------------------------------------------------------------------
echo "=== Scenario 1: commit-hash rootish reads correct snapshot ==="

expect_no_error "find at hash1 returns 1 doc" "
const snap = db.getSiblingDB('${DBNAME}__${HASH1}');
const docs = snap.getCollection('${COLL}').find({}).toArray();
if (docs.length !== 1) throw new Error('expected 1 doc, got ' + docs.length);
if (docs[0]._id !== 1) throw new Error('expected _id:1, got ' + docs[0]._id);
"

expect_no_error "countDocuments at hash1 = 1" "
const n = db.getSiblingDB('${DBNAME}__${HASH1}').getCollection('${COLL}').countDocuments({});
if (n !== 1) throw new Error('expected 1, got ' + n);
"

expect_no_error "main still has 2 docs after hash1 read" "
const n = db.getSiblingDB('${DBNAME}').getCollection('${COLL}').countDocuments({});
if (n !== 2) throw new Error('expected 2, got ' + n);
"

echo

# ---------------------------------------------------------------------------
echo "=== Scenario 2: commit-hash rootish blocks writes (code 96) ==="

expect_code_96 "insert on hash rootish" \
    "db.getSiblingDB('${DBNAME}__${HASH1}').getCollection('${COLL}').insertOne({ _id: 99 });"

expect_code_96 "updateOne on hash rootish" \
    "db.getSiblingDB('${DBNAME}__${HASH1}').getCollection('${COLL}').updateOne({ _id: 1 }, { \$set: { v: 'x' } });"

expect_code_96 "deleteOne on hash rootish" \
    "db.getSiblingDB('${DBNAME}__${HASH1}').getCollection('${COLL}').deleteOne({ _id: 1 });"

echo

# ---------------------------------------------------------------------------
echo "=== Scenario 3: ancestor-expression rootish (main~1) ==="

expect_no_error "main~1 returns 1 doc" "
const n = db.getSiblingDB('${DBNAME}__main~1').getCollection('${COLL}').countDocuments({});
if (n !== 1) throw new Error('expected 1, got ' + n);
"

expect_no_error "main~0 returns 2 docs (same as HEAD)" "
const n = db.getSiblingDB('${DBNAME}__main~0').getCollection('${COLL}').countDocuments({});
if (n !== 2) throw new Error('expected 2, got ' + n);
"

expect_code_96 "insert on main~1 rootish" \
    "db.getSiblingDB('${DBNAME}__main~1').getCollection('${COLL}').insertOne({ _id: 99 });"

echo

# ---------------------------------------------------------------------------
echo "=== Scenario 4: dongoCurrentBranch ==="

expect_no_error "dongoCurrentBranch on plain db returns 'main'" "
const r = db.getSiblingDB('${DBNAME}').runCommand({ dongoCurrentBranch: 1 });
if (r.ok !== 1) throw new Error('command failed: ' + JSON.stringify(r));
if (r.branch !== 'main') throw new Error('expected main, got ' + r.branch);
"

expect_no_error "dongoCurrentBranch on __main returns 'main'" "
const r = db.getSiblingDB('${DBNAME}__main').runCommand({ dongoCurrentBranch: 1 });
if (r.ok !== 1) throw new Error('command failed: ' + JSON.stringify(r));
if (r.branch !== 'main') throw new Error('expected main, got ' + r.branch);
"

expect_code_96 "dongoCurrentBranch on hash rootish returns code 96" \
    "db.getSiblingDB('${DBNAME}__${HASH1}').runCommand({ dongoCurrentBranch: 1 });"

expect_code_96 "dongoCurrentBranch on ancestor rootish returns code 96" \
    "db.getSiblingDB('${DBNAME}__main~1').runCommand({ dongoCurrentBranch: 1 });"

echo

# ---------------------------------------------------------------------------
echo "=== Scenario 5: parse-time rejections (code 96) ==="

expect_code_96 "HEAD rootish rejected at parse time" \
    "db.getSiblingDB('${DBNAME}__HEAD').getCollection('${COLL}').find({}).toArray();"

expect_code_96 "HEAD~1 rootish rejected at parse time" \
    "db.getSiblingDB('${DBNAME}__HEAD~1').getCollection('${COLL}').find({}).toArray();"

expect_code_96 "reflog syntax rejected" \
    "db.getSiblingDB('${DBNAME}__main@{yesterday}').getCollection('${COLL}').find({}).toArray();"

expect_code_96 "range syntax (..) rejected" \
    "db.getSiblingDB('${DBNAME}__main..feature').getCollection('${COLL}').find({}).toArray();"

expect_code_96 "range syntax (...) rejected" \
    "db.getSiblingDB('${DBNAME}__main...feature').getCollection('${COLL}').find({}).toArray();"

echo

# ---------------------------------------------------------------------------
echo "=== Cleanup ==="
_js "db.getSiblingDB('${DBNAME}').dropDatabase();" >/dev/null 2>&1 && echo "  dropped ${DBNAME}" || true

echo
echo "Results: ${PASS} passed, ${FAIL} failed"
[[ $FAIL -eq 0 ]]
