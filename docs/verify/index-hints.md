# Verify: index hints

Index hints let a client override the query planner and force a specific
index (or a full collection scan). DumboDB honors hints on both the
explain path and at runtime, matching MongoDB.

These steps assume a running dumbodb on `mongodb://localhost:27017`. Each
scenario builds on the setup block below. The automated analog is
`tests/index_hints_verify_test.go`.

## Setup

```js
db.items.insertMany([
  { _id: 1, name: "alpha",   city: "NYC" },
  { _id: 2, name: "bravo",   city: "LA"  },
  { _id: 3, name: "charlie", city: "NYC" }
])
db.items.createIndex({ name: 1 }, { name: "by_name" })
db.items.createIndex({ city: 1 }, { name: "by_city" })
```

## Scenario 1: A hint forces a specific index (by name)

The filter constrains both `name` and `city`. Left alone the planner
picks `by_name` (the first filtered field with an index); a hint
overrides that choice.

```js
db.items.find({ name: "alpha", city: "NYC" }).explain().queryPlanner.winningPlan
// Expected (no hint): IXSCAN(by_name) under FETCH

db.items.find({ name: "alpha", city: "NYC" }).hint("by_city").explain().queryPlanner.winningPlan
// Expected: IXSCAN(by_city) under FETCH

db.items.find({ name: "alpha", city: "NYC" }).hint("by_city").toArray()
// Expected: [ { _id: 1, name: "alpha", city: "NYC" } ]
```

Key checks:
- The unhinted plan uses `by_name`; the hinted plan uses `by_city`. The
  hint changed the chosen index.
- The result set is identical and correct regardless of which index
  served the query.

## Scenario 2: A hint by key pattern selects the same index

```js
db.items.find({ name: "alpha", city: "NYC" }).hint({ city: 1 }).explain().queryPlanner.winningPlan
// Expected: IXSCAN(by_city) under FETCH
```

Key check: a key-pattern hint `{city: 1}` resolves to `by_city`, the
same index a name hint `"by_city"` selects.

## Scenario 3: A $natural hint forces a collection scan

```js
db.items.find({ city: "NYC" }).hint({ $natural: 1 }).explain().queryPlanner.winningPlan
// Expected: { stage: "COLLSCAN" }

db.items.find({ city: "NYC" }).hint({ $natural: 1 }).toArray()
// Expected: the two NYC documents (_id 1 and 3)
```

Key checks:
- `$natural` forces `COLLSCAN` even though `by_city` covers the filter.
- The scan still returns the correct documents.

## Scenario 4: A hint naming a non-existent index errors

```js
db.items.find({ city: "NYC" }).hint("no_such_index")
// Expected: error, code BadValue (2):
//   "hint provided does not correspond to an existing index"
```

Key check: an unresolvable hint is an error, not silently ignored. (A
`$natural` hint and any valid index hint are accepted; only a hint that
names no existing index fails.)

## Scenario 5: Hinting a non-covering index still returns correct results

A hint can name an index that does not cover the filter. The query still
returns the right documents -- the index choice only affects how they
are found, never which match.

```js
db.items.find({ city: "NYC" }).hint("by_name").toArray()
// Expected: the two NYC documents (_id 1 and 3)
```

Key check: forcing an index unrelated to the filter does not drop or add
documents.
