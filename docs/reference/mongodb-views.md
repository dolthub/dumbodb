# MongoDB Standard Views

Probed against MongoDB 8.0.4 (recorded 2026-07-28).

A standard (computed) view is `{name, viewOn, pipeline, collation}`. It stores
no data. A read on a view runs `pipeline` over the source `viewOn`, then applies
the caller's own filter, sort, skip, limit, and projection on top. In effect, a
read on a view is an aggregation over its source. Writes are rejected.

- **Nested views**: a view may be defined on another view. Reads resolve through
  the chain (v2 on v1 on base returned the correctly double-filtered row).
- **Cycles**: detected and rejected ("View cycle detected").
- **Max nesting depth = 20**: the 20th level fails with `ViewDepthLimitExceeded`
  ("View depth too deep or view cycle detected").
- **Redefine**: `collMod {viewOn, pipeline}` redefines a view in place.
- **distinct**: works on a view.
- **Name collision**: creating a view with the name of an existing collection
  returns `NamespaceExists`.
- **Writes**: insert, update, and delete on a view return
  `CommandNotSupportedOnView`.
- **listCollections**: reports `type: "view"` and
  `options: {viewOn, pipeline, collation}`, with the collation fully resolved.
- **Collation**: a view's collation is fixed at creation.

An "on-demand materialized view" is not a separate object. It is the pattern of
an aggregation that ends in `$merge` or `$out` and writes into an ordinary
collection.
