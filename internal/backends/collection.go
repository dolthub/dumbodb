// Copyright 2021 FerretDB Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package backends

import (
	"cmp"
	"context"
	"slices"
	"time"

	"go.opentelemetry.io/otel"
	otelcodes "go.opentelemetry.io/otel/codes"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// DefaultIndexName names the index created with every collection; it defines
// the document primary key.
const DefaultIndexName = "_id_"

// ReservedMetadataIndexName is a reserved index name: user index creation with
// it is rejected and it is filtered from listIndexes.
const ReservedMetadataIndexName = "__dumbo_metadata__"

// ReservedCatalogName is the internal collection that durably stores
// per-collection metadata. It is hidden from listCollections/dbStats and the
// version-control walks, and rejected as a user collection name.
const ReservedCatalogName = "__dumbo_catalog__"

// Collection is a generic interface for all backends for accessing collection.
//
// Collection object should be stateless and temporary;
// all state should be in the Backend that created Database instance that created this Collection instance.
// Handler can create and destroy Collection objects on the fly.
// Creating a Collection object does not imply the creation of the database or collection.
//
// Collection methods should be thread-safe.
//
// See collectionContract and its methods for additional details.
type Collection interface {
	Query(context.Context, *QueryParams) (*QueryResult, error)
	Count(context.Context, *CountParams) (*CountResult, error)
	Explain(context.Context, *ExplainParams) (*ExplainResult, error)
	InsertAll(context.Context, *InsertAllParams) (*InsertAllResult, error)
	UpdateAll(context.Context, *UpdateAllParams) (*UpdateAllResult, error)
	DeleteAll(context.Context, *DeleteAllParams) (*DeleteAllResult, error)

	Stats(context.Context, *CollectionStatsParams) (*CollectionStatsResult, error)
	Compact(context.Context, *CompactParams) (*CompactResult, error)

	ListIndexes(context.Context, *ListIndexesParams) (*ListIndexesResult, error)
	CreateIndexes(context.Context, *CreateIndexesParams) (*CreateIndexesResult, error)
	DropIndexes(context.Context, *DropIndexesParams) (*DropIndexesResult, error)
}

type collectionContract struct {
	c Collection
}

// CollectionContract wraps Collection and enforces its contract.
//
// All backend implementations should use that function when they create new Collection instances.
// The handler should not use that function.
//
// See collectionContract and its methods for additional details.
func CollectionContract(c Collection) Collection {
	return &collectionContract{
		c: c,
	}
}

type QueryParams struct {
	Filter *types.Document
	Sort   *types.Document
	Limit  int64

	// Hint forces index selection: a name string or key-pattern document
	// selects that index; {$natural: <int>} forces a collection scan.
	Hint any

	OnlyRecordIDs bool
	Comment       string
	// Collated is set when the query runs under a non-simple collation.
	Collated bool

	// Collation is the effective operation collation, nil when simple/binary.
	Collation *types.Document
}

type QueryResult struct {
	Iter types.DocumentsIterator
}

// Query executes a query against the collection.
//
// If database or collection does not exist it returns empty iterator.
//
// The passed context should be used for canceling the initial query.
// It also can be used to close the returned iterator and free underlying resources,
// but doing so is not necessary - the handler will do that anyway.
//
// Filter may be ignored, or safely applied partially or entirely.
// Extra documents will be filtered out by the handler.
//
// Sort should have one of the following forms: nil, {}, {"$natural": int64(1)} or {"$natural": int64(-1)}.
// Other field names are not supported.
// If non-empty, it should be applied.
//
// Limit, if non-zero, should be applied.
func (cc *collectionContract) Query(ctx context.Context, params *QueryParams) (*QueryResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Query")
	defer span.End()

	if params == nil {
		params = new(QueryParams)
	}

	if params.Sort.Len() != 0 {
		must.BeTrue(params.Sort.Len() == 1)
		sortValue := params.Sort.Map()["$natural"].(int64)

		if sortValue != -1 && sortValue != 1 {
			panic("sort value must be 1 (for ascending) or -1 (for descending)")
		}
	}

	res, err := cc.c.Query(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

type CountParams struct {
	// Filter, when non-nil and non-empty, asks the backend to return the
	// count of matching documents instead of the full collection size.
	// Backends may decline (signaled via CountResult.Filtered=false) when
	// the filter cannot be answered cheaply (no covering index, complex
	// operators, etc.); the handler then falls back to a scan.
	Filter *types.Document

	// Collation is the effective operation collation, nil when simple/binary.
	Collation *types.Document
}

type CountResult struct {
	// Count is the document count. When Filtered is true, this is the
	// number of documents matching CountParams.Filter; otherwise it is
	// the unfiltered collection size (or 0 if the backend declined to
	// answer a filtered count).
	Count int64

	// Filtered is true when the backend honored a non-empty
	// CountParams.Filter. False means the handler must apply the filter
	// itself via Query+FilterIterator. Always true (or moot) for
	// unfiltered calls.
	Filtered bool
}

// Count returns the total number of documents in the collection.
//
// Backends are expected to implement this in O(1) when possible (e.g. via
// tree metadata) for unfiltered calls. When CountParams.Filter is set, the
// backend may attempt an index-only count and signal success via
// CountResult.Filtered=true; otherwise it returns the unfiltered count with
// Filtered=false and the handler falls back to a scan.
//
// If database or collection does not exist, returns Count=0 and no error.
func (cc *collectionContract) Count(ctx context.Context, params *CountParams) (*CountResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Count")
	defer span.End()

	if params == nil {
		params = new(CountParams)
	}

	res, err := cc.c.Count(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

type ExplainParams struct {
	Filter     *types.Document
	Sort       *types.Document
	Projection *types.Document
	Limit      int64
	Skip       int64

	// Command is the original wire command being explained ("find", "count",
	// "distinct", "aggregate"). The backend picks a top-level explain shape
	// based on this (e.g. count emits a COUNT stage, distinct emits a
	// DISTINCT_SCAN under PROJECTION_COVERED). Defaults to "find".
	Command string

	// DistinctKey is the field name passed to the distinct command. Ignored
	// for other commands.
	DistinctKey string

	// Collated is set when the query runs under a non-simple collation.
	Collated bool

	// Collation is the effective operation collation, nil when simple/binary.
	Collation *types.Document

	// Hint, when non-nil, requests that the backend plan the query using the
	// specified index. It may be a document like {field: 1} naming a key
	// pattern, or a single-string value naming an index by name (the latter
	// is left to the backend to interpret).
	Hint any
}

type ExplainResult struct {
	QueryPlanner   *types.Document
	FilterPushdown bool
	SortPushdown   bool
	LimitPushdown  bool
}

// Explain return a backend-specific execution plan for the given query.
//
// Database or collection may not exist; that's not an error, it still
// returns the ExplainResult with QueryPlanner.
//
// The ExplainResult's FilterPushdown field is set to true if the backend could have applied the requested filtering
// partially or completely (but safely in any case).
// If it wasn't possible to apply it safely at least partially, that field should be set to false.
//
// The ExplainResult's SortPushdown field is set to true if the backend could have applied the whole requested sorting.
// If it was possible to apply it only partially or not at all, that field should be set to false.
func (cc *collectionContract) Explain(ctx context.Context, params *ExplainParams) (*ExplainResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Explain")
	defer span.End()

	if params == nil {
		params = new(ExplainParams)
	}

	// Explain receives arbitrary sort docs so the backend can render a
	// SORT stage in the plan tree. The historical "$natural-only"
	// precondition was tied to the Query path's pushdown gate; it does
	// not apply to explain.

	res, err := cc.c.Explain(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

type InsertAllParams struct {
	Docs []*types.Document

	// SkipDurableSync, when true, tells the backend the client opted out of a
	// synchronous journal fsync via MongoDB writeConcern (j:false or w:0).
	// Backends that support it may acknowledge before the write is durable and
	// rely on a periodic background flush.
	SkipDurableSync bool

	// ReturnDocHashes asks the backend to report the content hash of every
	// stored document in InsertAllResult.DocHashes.
	ReturnDocHashes bool
}

type InsertAllResult struct {
	// DocHashes holds the content hash of each stored document, in Docs order,
	// when ReturnDocHashes was set; nil otherwise. A backend that does not
	// address documents by content leaves it nil.
	DocHashes []string
}

// InsertAll inserts documents into the collection.
//
// The operation should be atomic.
// If some documents cannot be inserted, the operation should be rolled back,
// and the first encountered error should be returned.
//
// All documents are expected to be valid and include _id fields.
// They will be frozen.
//
// Both database and collection may or may not exist; they should be created automatically if needed.
func (cc *collectionContract) InsertAll(ctx context.Context, params *InsertAllParams) (*InsertAllResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "InsertAll")
	defer span.End()

	now := time.Now()
	for _, doc := range params.Docs {
		doc.SetRecordID(types.NextTimestamp(now).Signed())
		doc.Freeze()
	}

	res, err := cc.c.InsertAll(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err, ErrorCodeInsertDuplicateID)

	return res, err
}

type UpdateAllParams struct {
	Docs []*types.Document

	// FieldMutations, when non-nil, must have the same length as Docs. Entry i,
	// if non-empty, describes the field-level changes that transform the prior
	// stored version of Docs[i] into Docs[i]. Backends may use it to apply a
	// partial update that preserves structural sharing instead of rewriting
	// the document wholesale. A nil or empty entry, or a nil FieldMutations
	// slice altogether, means the backend must write Docs[i] in full.
	FieldMutations [][]FieldMutation

	// SkipDurableSync, when true, tells the backend the client opted out of a
	// synchronous journal fsync via MongoDB writeConcern. See InsertAllParams.
	SkipDurableSync bool

	// ReturnDocHashes asks the backend to report the content hash of every
	// stored document in UpdateAllResult.DocHashes.
	ReturnDocHashes bool
}

// FieldMutation describes a single field-level change applied to a document.
//
// Only flat, top-level field assignments and deletions are representable:
// Key must be a bare field name (no dot-notation, no array indices, no MongoDB
// path operators). Callers that cannot express a mutation cleanly should omit
// the mutation list for that document so the backend falls back to a full
// rewrite.
type FieldMutation struct {
	// Key is the top-level field name to modify.
	Key string

	// Unset, when true, removes Key from the document. Otherwise Key is set
	// to Value.
	Unset bool

	// Value is the new value to assign when Unset is false. It must be a
	// BSON-encodable value of the same Go type that appears in a
	// types.Document (string, int32, int64, float64, bool, *types.Document,
	// *types.Array, types.ObjectID, time.Time, etc.).
	Value any
}

type UpdateAllResult struct {
	Updated int32

	// DocHashes has one entry per Docs entry when ReturnDocHashes was set,
	// holding the content hash of the document as stored; nil otherwise.
	// Entry i is empty when Docs[i] matched nothing and was not written.
	DocHashes []string
}

// UpdateAll updates documents in collection.
//
// The operation should be atomic.
// If some documents cannot be updated, the operation should be rolled back,
// and the first encountered error should be returned.
//
// All documents are expected to be valid and include _id fields.
// They will be frozen.
//
// Database or collection may not exist; that's not an error.
func (cc *collectionContract) UpdateAll(ctx context.Context, params *UpdateAllParams) (*UpdateAllResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "UpdateAll")
	defer span.End()

	for _, doc := range params.Docs {
		doc.Freeze()
	}

	res, err := cc.c.UpdateAll(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

type DeleteAllParams struct {
	IDs       []any
	RecordIDs []int64

	// SkipDurableSync, when true, tells the backend the client opted out of a
	// synchronous journal fsync via MongoDB writeConcern. See InsertAllParams.
	SkipDurableSync bool
}

type DeleteAllResult struct {
	Deleted int32
}

// DeleteAll deletes documents in collection.
//
// Passed IDs may contain duplicates or point to non-existing documents.
//
// The operation should be atomic.
// If some documents cannot be deleted, the operation should be rolled back,
// and the first encountered error should be returned.
//
// Database or collection may not exist; that's not an error.
func (cc *collectionContract) DeleteAll(ctx context.Context, params *DeleteAllParams) (*DeleteAllResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "DeleteAll")
	defer span.End()

	must.BeTrue((params.IDs == nil) != (params.RecordIDs == nil))

	res, err := cc.c.DeleteAll(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

type CollectionStatsParams struct {
	Refresh bool
}

type CollectionStatsResult struct {
	CountDocuments  int64
	SizeTotal       int64
	SizeIndexes     int64
	SizeCollection  int64
	SizeFreeStorage int64
	IndexSizes      []IndexSize
}

type IndexSize struct {
	Name string
	Size int64
}

// Stats returns statistic estimations about the collection.
// All returned values are not exact, but might be more accurate when Stats is called with `Refresh: true`.
//
// Possible errors: ErrorCodeDatabaseDoesNotExist, ErrorCodeCollectionDoesNotExist.
func (cc *collectionContract) Stats(ctx context.Context, params *CollectionStatsParams) (*CollectionStatsResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "CollectionStats")
	defer span.End()

	res, err := cc.c.Stats(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err, ErrorCodeDatabaseDoesNotExist, ErrorCodeCollectionDoesNotExist)

	return res, err
}

type CompactParams struct {
	Full bool
}

type CompactResult struct{}

// Compact reduces the disk space collection takes (by defragmenting, removing dead rows, etc)
// and refreshes its statistics.
//
// If full is true, the operation should try to reduce the disk space as much as possible,
// even if collection or the whole database will be locked for some time.
func (cc *collectionContract) Compact(ctx context.Context, params *CompactParams) (*CompactResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "Compact")
	defer span.End()

	res, err := cc.c.Compact(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err, ErrorCodeDatabaseDoesNotExist, ErrorCodeCollectionDoesNotExist)

	return res, err
}

type ListIndexesParams struct{}

type ListIndexesResult struct {
	Indexes []IndexInfo
}

type IndexInfo struct {
	Name                    string
	Key                     []IndexKeyPair
	Unique                  bool
	Sparse                  bool            // true if the index only covers documents with the indexed field(s)
	PartialFilterExpression *types.Document // non-nil for partial indexes; only matching docs are indexed

	// Collation is the index's collation spec, nil for the binary default.
	// MongoDB distinguishes indexes on the same key by collation, so two
	// indexes may share a key pattern when their collations differ.
	Collation *types.Document

	// Hidden marks the index invisible to the query planner while still
	// maintained. Tracked and echoed by listIndexes; planner-level hiding
	// is not yet applied.
	Hidden bool

	// Lossy: the index stored a value the KeyString encoding cannot
	// represent faithfully (Decimal128); the planner never consults a
	// lossy index. Sticky until rebuild.
	Lossy bool

	// Multikey: the index expanded an array value into per-element
	// entries; range-entry counts would be per element, not per doc,
	// so count fast paths skip range filters on it. Sticky until
	// rebuild.
	Multikey bool

	// MatchesPartialFilter, when non-nil, reports whether doc satisfies the partial
	// filter expression. If nil, all documents are considered as matching (no partial
	// filter). Set by the handler layer at index creation time, and reattached by
	// backends after restart via MatchPartialFilter using the persisted
	// PartialFilterExpression  -- both paths route through the predicate registered
	// by RegisterPartialFilterMatcher to avoid a circular import on handler/common.
	MatchesPartialFilter func(doc *types.Document) (bool, error)
}

type IndexKeyPair struct {
	Field       string
	Descending  bool
	Text        bool // true if this is a text index field
	Geo2DSphere bool // true if this is a 2dsphere geospatial index field
	Geo2D       bool // true if this is a 2d (legacy planar) index field
	Hashed      bool // true if this is a hashed index field
}

// ListIndexes returns a list of collection indexes.
//
// The errors for non-existing database and non-existing collection are the same.
func (cc *collectionContract) ListIndexes(ctx context.Context, params *ListIndexesParams) (*ListIndexesResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "ListIndexes")
	defer span.End()

	res, err := cc.c.ListIndexes(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err, ErrorCodeCollectionDoesNotExist)

	if res != nil && len(res.Indexes) > 0 {
		must.BeTrue(slices.IsSortedFunc(res.Indexes, func(a, b IndexInfo) int {
			return cmp.Compare(a.Name, b.Name)
		}))
	}

	return res, err
}

type CreateIndexesParams struct {
	Indexes []IndexInfo
}

type CreateIndexesResult struct{}

// CreateIndexes creates indexes for the collection.
//
// The operation should be atomic.
// If some indexes cannot be created, the operation should be rolled back,
// and the first encountered error should be returned.
//
// Database or collection may not exist; that's not an error.
func (cc *collectionContract) CreateIndexes(ctx context.Context, params *CreateIndexesParams) (*CreateIndexesResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "CreateIndexes")
	defer span.End()

	res, err := cc.c.CreateIndexes(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

type DropIndexesParams struct {
	Indexes []string
}

type DropIndexesResult struct{}

// DropIndexes drops indexes for the collection.
//
// The operation should be atomic.
// If some indexes cannot be dropped, the operation should be rolled back,
// and the first encountered error should be returned.
//
// Database or collection may not exist; that's not an error.
func (cc *collectionContract) DropIndexes(ctx context.Context, params *DropIndexesParams) (*DropIndexesResult, error) {
	ctx, span := otel.Tracer("").Start(ctx, "DropIndexes")
	defer span.End()

	res, err := cc.c.DropIndexes(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

// DistinctScan forwards to the wrapped collection if it implements
// DistinctScanner, otherwise returns (nil, nil) so the handler falls back to
// the Query path. Without this forwarder the optional interface is hidden by
// the contract wrapper that every backend uses.
func (cc *collectionContract) DistinctScan(ctx context.Context, params *DistinctParams) (*DistinctResult, error) {
	ds, ok := cc.c.(DistinctScanner)
	if !ok {
		return nil, nil
	}

	ctx, span := otel.Tracer("").Start(ctx, "DistinctScan")
	defer span.End()

	res, err := ds.DistinctScan(ctx, params)
	if err != nil {
		span.SetStatus(otelcodes.Error, "")
	}

	checkError(err)

	return res, err
}

var (
	_ Collection      = (*collectionContract)(nil)
	_ DistinctScanner = (*collectionContract)(nil)
)
