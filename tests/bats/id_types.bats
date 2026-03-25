#!/usr/bin/env bats
# _id type tests.
# Verifies that documents can be inserted and retrieved using each supported
# BSON type as the _id field.  One test per type, using mongosh exclusively.

load helpers

DONGO_PORT=37028

setup() {
    DONGO_DATA_DIR="$(mktemp -d)"
    start_dongo "$DONGO_DATA_DIR" "$DONGO_PORT"
}

teardown() {
    stop_dongo
    rm -rf "$DONGO_DATA_DIR"
}

# Helper: insert a doc then find it, asserting both succeed and the findOne
# output satisfies a jq expression.
#   $1 - mongosh URI
#   $2 - collection name
#   $3 - JS _id expression
#   $4 - jq expression to evaluate against the findOne result
assert_id_roundtrip() {
    local uri="$1" col="$2" id_expr="$3" jq_expr="$4"

    run mongosh "$uri" --quiet --eval \
        "JSON.stringify(db.${col}.insertOne({_id: ${id_expr}, k: 1, v: 42}))"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    run mongosh "$uri" --quiet --eval \
        "JSON.stringify(db.${col}.findOne({_id: ${id_expr}}))"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e "$jq_expr"
}

@test '_id as ObjectId' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_objectid" \
        "ObjectId('aabbccddeeff001122334455')" \
        '._id == "aabbccddeeff001122334455" and .k == 1 and .v == 42'
}

@test '_id as string' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_string" \
        "'hello-world'" \
        '._id == "hello-world" and .k == 1 and .v == 42'
}

@test '_id as int32' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_int32" \
        "NumberInt(42)" \
        '._id == 42 and .k == 1 and .v == 42'
}

@test '_id as int64' {
    # JSON.stringify encodes NumberLong as {low, high, unsigned} since JS
    # cannot represent int64 natively.
    # 9007199254740993 = 0x0020000000000001 → low=1, high=2097152
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_int64" \
        "NumberLong('9007199254740993')" \
        '._id.low == 1 and ._id.high == 2097152 and .k == 1 and .v == 42'
}

@test '_id as double' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_double" \
        "3.14" \
        '._id == 3.14 and .k == 1 and .v == 42'
}

@test '_id as bool' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_bool" \
        "true" \
        '._id == true and .k == 1 and .v == 42'
}

@test '_id as binary (BinData)' {
    # BinData(0, ...) round-trips as the base64 string in JSON.stringify output.
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_binary" \
        "BinData(0, 'aGVsbG8=')" \
        '._id == "aGVsbG8=" and .k == 1 and .v == 42'
}

@test '_id as UUID' {
    # UUID is BinData subtype 4.  mongosh decodes it back to UUID string form.
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_uuid" \
        "UUID('6ba7b810-9dad-11d1-80b4-00c04fd430c8')" \
        '._id == "6ba7b810-9dad-11d1-80b4-00c04fd430c8" and .k == 1 and .v == 42'
}

@test '_id as embedded document' {
    local uri="mongodb://127.0.0.1:${DONGO_PORT}/test"

    # Insert doc1: _id with key order {a, b}
    run mongosh "$uri" --quiet --eval \
        "JSON.stringify(db.col_subdoc.insertOne({_id: {a: 1, b: 'x'}, v: 42}))"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    # Insert doc2: _id with reversed key order {b, a} — this is a distinct _id
    run mongosh "$uri" --quiet --eval \
        "JSON.stringify(db.col_subdoc.insertOne({_id: {b: 'x', a: 1}, v: 99}))"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '.acknowledged == true'

    # findOne with key order {a, b} — must return doc1 (v == 42), not doc2
    run mongosh "$uri" --quiet --eval \
        "JSON.stringify(db.col_subdoc.findOne({_id: {a: 1, b: 'x'}}))"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '._id.a == 1 and ._id.b == "x" and .v == 42'

    # findOne with key order {b, a} — must return doc2 (v == 99), not doc1
    run mongosh "$uri" --quiet --eval \
        "JSON.stringify(db.col_subdoc.findOne({_id: {b: 'x', a: 1}}))"
    [ "$status" -eq 0 ]
    echo "$output" | jq -e '._id.b == "x" and ._id.a == 1 and .v == 99'

    # Collection must contain exactly 2 documents
    run mongosh "$uri" --quiet --eval \
        "db.col_subdoc.find({}).count()"
    [ "$status" -eq 0 ]
    [ "$output" -eq 2 ]
}

@test '_id as Date' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_date" \
        "new Date('2024-01-01T00:00:00.000Z')" \
        '.k == 1 and .v == 42'
}

@test '_id as Decimal128' {
    assert_id_roundtrip \
        "mongodb://127.0.0.1:${DONGO_PORT}/test" \
        "col_decimal" \
        "NumberDecimal('123.456')" \
        '.k == 1 and .v == 42'
}

