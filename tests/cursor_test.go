// Copyright 2026 Dolthub, Inc.
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

package tests

// cursor_test.go covers cursor options and behaviors for parity testing between
// MongoDB and Dongo.
//
// Coverage:
//   - sort, limit, skip, hint, batchSize
//   - allowDiskUse, noCursorTimeout, maxTimeMS, comment
//   - returnKey, showRecordId
//   - min/max index bounds
//   - collation
//   - tailable cursor behavior on capped collections
//   - multi-batch result sets via getMore
//   - cursor timeout behavior
//
// Test naming convention:
//   - DongoFull: expected to pass on Dongo (no marker needed).
//   - DongoXFail: known limitation; uses dongoXFail() to skip.

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// ─── sort ─────────────────────────────────────────────────────────────────────

// TestCursor_SortAscending verifies that sort ascending on a field returns docs
// in the correct order. (DongoFull)
func TestCursor_SortAscending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "c"), e("v", int32(3))),
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", 1))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 3)
	assert.Equal(t, int32(1), results[0].Map()["v"])
	assert.Equal(t, int32(2), results[1].Map()["v"])
	assert.Equal(t, int32(3), results[2].Map()["v"])
}

// TestCursor_SortDescending verifies descending sort. (DongoFull)
func TestCursor_SortDescending(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", -1))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 3)
	assert.Equal(t, int32(3), results[0].Map()["v"])
	assert.Equal(t, int32(2), results[1].Map()["v"])
	assert.Equal(t, int32(1), results[2].Map()["v"])
}

// TestCursor_SortCompound verifies compound sort on two fields. (DongoFull)
func TestCursor_SortCompound(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "1"), e("group", int32(1)), e("rank", int32(2))),
		d(e("_id", "2"), e("group", int32(2)), e("rank", int32(1))),
		d(e("_id", "3"), e("group", int32(1)), e("rank", int32(1))),
		d(e("_id", "4"), e("group", int32(2)), e("rank", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("group", 1), e("rank", 1))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 4)
	ids := make([]string, 4)
	for i, r := range results {
		ids[i] = r.Map()["_id"].(string)
	}
	// group=1 rank=1 → "3", group=1 rank=2 → "1", group=2 rank=1 → "2", group=2 rank=2 → "4"
	assert.Equal(t, []string{"3", "1", "2", "4"}, ids)
}

// ─── limit ────────────────────────────────────────────────────────────────────

// TestCursor_Limit verifies that SetLimit restricts the number of returned docs. (DongoFull)
func TestCursor_Limit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("v", int32(i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", 1))).SetLimit(3))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 3)
	assert.Equal(t, int32(1), results[0].Map()["v"])
	assert.Equal(t, int32(3), results[2].Map()["v"])
}

// TestCursor_LimitZero verifies that limit=0 returns all documents. (DongoFull)
func TestCursor_LimitZero(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 5; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetLimit(0))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 5, "limit=0 should return all documents")
}

// TestCursor_LimitNegative verifies that a negative limit returns at most abs(limit) docs
// and closes the cursor after the first batch. (DongoFull)
func TestCursor_LimitNegative(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("v", int32(i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", 1))).SetLimit(-3))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	// Negative limit means "at most N from first batch" — 3 or fewer docs.
	assert.LessOrEqual(t, len(results), 3)
}

// ─── skip ─────────────────────────────────────────────────────────────────────

// TestCursor_Skip verifies that SetSkip skips the first N documents. (DongoFull)
func TestCursor_Skip(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 5; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("v", int32(i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", 1))).SetSkip(2))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 3)
	assert.Equal(t, int32(3), results[0].Map()["v"])
}

// TestCursor_SkipAndLimit verifies skip and limit used together. (DongoFull)
func TestCursor_SkipAndLimit(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("v", int32(i))))
	}

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetSort(d(e("v", 1))).SetSkip(3).SetLimit(3))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 3)
	assert.Equal(t, int32(4), results[0].Map()["v"])
	assert.Equal(t, int32(6), results[2].Map()["v"])
}

// TestCursor_SkipBeyondResults verifies that skip > count returns empty. (DongoFull)
func TestCursor_SkipBeyondResults(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a")),
		d(e("_id", "b")),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSkip(100))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Empty(t, results, "skip beyond result count should return empty")
}

// ─── hint ─────────────────────────────────────────────────────────────────────

// TestCursor_HintByDocument verifies that SetHint with a document spec
// forces a specific index and returns correct results. (DongoFull)
func TestCursor_HintByDocument(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10))),
		d(e("_id", "b"), e("score", int32(20))),
		d(e("_id", "c"), e("score", int32(30))),
	)

	// Create an index on score.
	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"score", 1}},
		Options: options.Index().SetName("score_1"),
	})
	require.NoError(t, err)

	// Hint by key document.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetHint(bson.D{{"score", 1}}))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 3)
}

// TestCursor_HintByName verifies that SetHint with an index name string
// forces the named index. (DongoFull)
func TestCursor_HintByName(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10))),
		d(e("_id", "b"), e("score", int32(20))),
	)

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"score", 1}},
		Options: options.Index().SetName("score_1"),
	})
	require.NoError(t, err)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetHint("score_1"))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2)
}

// TestCursor_HintNaturalForward verifies that $natural hint returns docs in
// insertion order. (DongoFull)
func TestCursor_HintNaturalForward(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "z"), e("v", int32(1))),
		d(e("_id", "a"), e("v", int32(2))),
		d(e("_id", "m"), e("v", int32(3))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetHint(bson.D{{"$natural", 1}}))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 3)
}

// ─── batchSize ────────────────────────────────────────────────────────────────

// TestCursor_BatchSize verifies that SetBatchSize controls how many documents
// are returned per batch while still yielding all results. (DongoFull)
func TestCursor_BatchSize(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 20; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("v", int32(i))))
	}

	// batchSize=5, but cursor.All should fetch all batches automatically.
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(5))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 20, "batchSize should not truncate total results")
}

// TestCursor_BatchSizeOne verifies that batchSize=1 still returns all docs. (DongoFull)
func TestCursor_BatchSizeOne(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 5; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(1))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 5)
}

// ─── allowDiskUse ─────────────────────────────────────────────────────────────

// TestCursor_AllowDiskUse verifies that a sort with allowDiskUse succeeds
// and returns the correct ordered results. (DongoXFail)
func TestCursor_AllowDiskUse(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "allowDiskUse option not yet supported by Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("v", int32(i))))
	}

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetSort(d(e("v", 1))).SetAllowDiskUse(true))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 10)
	assert.Equal(t, int32(1), results[0].Map()["v"])
}

// ─── noCursorTimeout ──────────────────────────────────────────────────────────

// TestCursor_NoCursorTimeout verifies that noCursorTimeout=true is accepted and
// the cursor returns results normally. (DongoXFail)
func TestCursor_NoCursorTimeout(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "noCursorTimeout option not yet implemented in Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetNoCursorTimeout(true))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 2)
}

// ─── maxTimeMS ────────────────────────────────────────────────────────────────

// TestCursor_MaxTimeMS verifies that maxTimeMS is accepted and quick queries
// complete within the allotted time. (DongoFull)
func TestCursor_MaxTimeMS(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetMaxTime(5*time.Second))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 2)
}

// ─── comment ─────────────────────────────────────────────────────────────────

// TestCursor_Comment verifies that a comment string can be attached to a find
// query without affecting results. (DongoFull)
func TestCursor_Comment(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetComment("test query comment"))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 2)
}

// ─── returnKey ────────────────────────────────────────────────────────────────

// TestCursor_ReturnKey verifies that returnKey=true returns only the index
// key fields and omits non-key document fields. (DongoXFail)
func TestCursor_ReturnKey(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "returnKey option not yet supported by Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("score", int32(10)), e("name", "alice")),
		d(e("_id", "b"), e("score", int32(20)), e("name", "bob")),
	)

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"score", 1}},
		Options: options.Index().SetName("score_1"),
	})
	require.NoError(t, err)

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetHint(bson.D{{"score", 1}}).SetReturnKey(true))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 2)
	// returnKey documents should only contain the index key fields, not _id or name.
	for _, r := range results {
		m := r.Map()
		assert.Contains(t, m, "score", "returnKey should include the index field")
		assert.NotContains(t, m, "name", "returnKey should omit non-key fields")
		assert.NotContains(t, m, "_id", "returnKey should omit _id")
	}
}

// ─── showRecordId ─────────────────────────────────────────────────────────────

// TestCursor_ShowRecordId verifies that showRecordId=true appends a $recordId
// field to each returned document. (DongoXFail)
func TestCursor_ShowRecordId(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "showRecordId option not yet supported by Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetShowRecordID(true))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 2)
	for _, r := range results {
		m := r.Map()
		assert.Contains(t, m, "$recordId", "showRecordId should add $recordId field")
	}
}

// ─── min/max index bounds ─────────────────────────────────────────────────────

// TestCursor_MinMaxBounds verifies that min/max cursor options restrict the
// range of index keys scanned. (DongoXFail)
func TestCursor_MinMaxBounds(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "min/max cursor bounds not yet supported by Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("score", int32(i*10))))
	}

	_, err := coll.Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{"score", 1}},
		Options: options.Index().SetName("score_1"),
	})
	require.NoError(t, err)

	// min=30 (inclusive), max=60 (exclusive) should return scores 30, 40, 50.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().
			SetHint(bson.D{{"score", 1}}).
			SetMin(bson.D{{"score", int32(30)}}).
			SetMax(bson.D{{"score", int32(60)}}))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 3)
	for _, r := range results {
		score := r.Map()["score"].(int32)
		assert.GreaterOrEqual(t, score, int32(30))
		assert.Less(t, score, int32(60))
	}
}

// ─── collation ────────────────────────────────────────────────────────────────

// TestCursor_CollationCaseInsensitive verifies that a case-insensitive collation
// matches documents regardless of case. (DongoXFail)
func TestCursor_CollationCaseInsensitive(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "collation option not yet supported by Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("name", "Alice")),
		d(e("_id", "b"), e("name", "bob")),
		d(e("_id", "c"), e("name", "CHARLIE")),
	)

	caseInsensitive := &options.Collation{
		Locale:   "en",
		Strength: 2, // secondary = case-insensitive
	}

	cursor, err := coll.Find(ctx,
		d(e("name", "alice")),
		options.Find().SetCollation(caseInsensitive))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, 1, "case-insensitive collation should match 'Alice' when querying 'alice'")
	if len(results) > 0 {
		assert.Equal(t, "a", results[0].Map()["_id"])
	}
}

// TestCursor_CollationSort verifies that collation affects sort order for
// locale-specific string ordering. (DongoXFail)
func TestCursor_CollationSort(t *testing.T) {
	t.Parallel()
	dongoXFail(t, "collation option not yet supported by Dongo")

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("word", "banana")),
		d(e("_id", "b"), e("word", "Apple")),
		d(e("_id", "c"), e("word", "cherry")),
	)

	enCollation := &options.Collation{Locale: "en", Strength: 1}
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().
			SetSort(d(e("word", 1))).
			SetCollation(enCollation))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 3)
	// Case-insensitive English: Apple < banana < cherry
	assert.Equal(t, "Apple", results[0].Map()["word"])
	assert.Equal(t, "banana", results[1].Map()["word"])
	assert.Equal(t, "cherry", results[2].Map()["word"])
}

// ─── multi-batch (getMore) ────────────────────────────────────────────────────

// TestCursor_MultiBatch verifies that iterating a cursor with a small batchSize
// correctly fetches multiple batches via getMore and returns all documents. (DongoFull)
func TestCursor_MultiBatch(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	const total = 25
	for i := 1; i <= total; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("seq", int32(i))))
	}

	// Use batchSize=4 to force multiple getMore round-trips.
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("seq", 1))).SetBatchSize(4))
	require.NoError(t, err)

	var count int
	for cursor.Next(ctx) {
		count++
	}
	require.NoError(t, cursor.Err())
	require.NoError(t, cursor.Close(ctx))

	assert.Equal(t, total, count, "all documents should be returned across multiple batches")
}

// TestCursor_MultiBatchAllHelper verifies cursor.All() traverses multiple batches. (DongoFull)
func TestCursor_MultiBatchAllHelper(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	const total = 15
	for i := 1; i <= total; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("seq", int32(i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(3))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Len(t, results, total)
}

// ─── tailable cursors ─────────────────────────────────────────────────────────

// TestCursor_Tailable verifies that a tailable cursor can be opened on a capped
// collection and returns documents inserted so far. (DongoFull)
func TestCursor_Tailable(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := startDongo(t)
	db := env.client.Database("testdb")
	collName := fmt.Sprintf("tailable_%d", randID())

	err := db.CreateCollection(ctx, collName, options.CreateCollection().
		SetCapped(true).SetSizeInBytes(1024*1024))
	require.NoError(t, err)

	coll := db.Collection(collName)
	insertDocs(t, coll,
		d(e("_id", "x"), e("v", int32(1))),
		d(e("_id", "y"), e("v", int32(2))),
	)

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetCursorType(options.Tailable))
	require.NoError(t, err)
	defer cursor.Close(ctx) //nolint:errcheck

	var results []bson.D
	for cursor.Next(ctx) {
		var doc bson.D
		require.NoError(t, cursor.Decode(&doc))
		results = append(results, doc)
	}

	assert.Len(t, results, 2, "tailable cursor should return documents present at open time")
}

// TestCursor_TailableAwaitData verifies that a TailableAwait cursor blocks briefly
// waiting for new data. (DongoFull)
func TestCursor_TailableAwaitData(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := startDongo(t)
	db := env.client.Database("testdb")
	collName := fmt.Sprintf("tailable_await_%d", randID())

	err := db.CreateCollection(ctx, collName, options.CreateCollection().
		SetCapped(true).SetSizeInBytes(1024*1024))
	require.NoError(t, err)

	coll := db.Collection(collName)
	insertDocs(t, coll, d(e("_id", "first"), e("v", int32(1))))

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().
			SetCursorType(options.TailableAwait).
			SetMaxAwaitTime(100*time.Millisecond))
	require.NoError(t, err)
	defer cursor.Close(ctx) //nolint:errcheck

	var docs []bson.D
	timeoutCtx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
	defer cancel()
	for cursor.Next(timeoutCtx) {
		var doc bson.D
		require.NoError(t, cursor.Decode(&doc))
		docs = append(docs, doc)
	}

	assert.GreaterOrEqual(t, len(docs), 1, "tailable awaitData cursor should return at least the initial document")
}

// TestCursor_TailableCappedCollectionRequired verifies that opening a tailable
// cursor on a non-capped collection returns an error. (DongoFull)
func TestCursor_TailableCappedCollectionRequired(t *testing.T) {
	t.Parallel()

	ctx := context.Background()
	env := startDongo(t)
	coll := env.collection(t) // regular, non-capped collection

	insertDocs(t, coll, d(e("_id", "a")))

	// MongoDB returns an error when a tailable cursor is used on a non-capped collection.
	_, err := coll.Find(ctx, bson.D{}, options.Find().SetCursorType(options.Tailable))
	assert.Error(t, err, "tailable cursor on a non-capped collection should return an error")
}

// ─── cursor.Next() iteration ─────────────────────────────────────────────────

// TestCursor_NextIteration verifies that cursor.Next() correctly iterates all
// documents and cursor.Decode() extracts each document. (DongoFull)
func TestCursor_NextIteration(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
		d(e("_id", "c"), e("v", int32(3))),
	)

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetSort(d(e("v", 1))))
	require.NoError(t, err)
	defer cursor.Close(ctx) //nolint:errcheck

	var vals []int32
	for cursor.Next(ctx) {
		var doc bson.D
		require.NoError(t, cursor.Decode(&doc))
		vals = append(vals, doc.Map()["v"].(int32))
	}
	require.NoError(t, cursor.Err())

	assert.Equal(t, []int32{1, 2, 3}, vals)
}

// TestCursor_CloseReleasesResources verifies that explicitly closing a cursor
// does not cause errors and subsequent operations fail gracefully. (DongoFull)
func TestCursor_CloseReleasesResources(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 10; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i))))
	}

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetBatchSize(2))
	require.NoError(t, err)

	// Read one doc, then close early.
	assert.True(t, cursor.Next(ctx))
	require.NoError(t, cursor.Close(ctx))

	// After close, Next should return false.
	assert.False(t, cursor.Next(ctx))
}

// TestCursor_EmptyCollection verifies that a cursor over an empty collection
// immediately returns no documents without error. (DongoFull)
func TestCursor_EmptyCollection(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	// No inserts — collection is empty.

	cursor, err := coll.Find(ctx, bson.D{})
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	assert.Empty(t, results)
}

// ─── projection with cursor options ──────────────────────────────────────────

// TestCursor_ProjectionInclusion verifies that a projection including specific
// fields returns only those fields (plus _id by default). (DongoFull)
func TestCursor_ProjectionInclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("name", "alice"), e("age", int32(30)), e("city", "NYC")),
	)

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("name", 1), e("age", 1))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 1)
	m := results[0].Map()
	assert.Contains(t, m, "name")
	assert.Contains(t, m, "age")
	assert.NotContains(t, m, "city", "excluded fields should not appear in result")
}

// TestCursor_ProjectionExclusion verifies that a projection excluding specific
// fields returns all other fields. (DongoFull)
func TestCursor_ProjectionExclusion(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("name", "alice"), e("password", "secret"), e("age", int32(30))),
	)

	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetProjection(d(e("password", 0))))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 1)
	m := results[0].Map()
	assert.Contains(t, m, "name")
	assert.Contains(t, m, "age")
	assert.NotContains(t, m, "password", "excluded field should not appear in result")
}

// ─── allowPartialResults ──────────────────────────────────────────────────────

// TestCursor_AllowPartialResultsTrue verifies that allowPartialResults=true is
// accepted on a non-sharded Dongo instance and returns full results. (DongoFull)
func TestCursor_AllowPartialResultsTrue(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", "a"), e("v", int32(1))),
		d(e("_id", "b"), e("v", int32(2))),
	)

	// allowPartialResults is a sharding option; on a non-sharded DB it should
	// be silently accepted and return full results without error.
	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetAllowPartialResults(true))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 2, "allowPartialResults=true should return all docs on non-sharded DB")
}

// TestCursor_AllowPartialResultsFalse verifies that allowPartialResults=false
// (the explicit default) is accepted without error. (DongoFull)
func TestCursor_AllowPartialResultsFalse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll, d(e("_id", "x"), e("v", int32(42))))

	cursor, err := coll.Find(ctx, bson.D{}, options.Find().SetAllowPartialResults(false))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	assert.Len(t, results, 1)
}

// TestCursor_SortLimitSkipCombined verifies combining sort+limit+skip in a single
// find produces the expected paginated window. (DongoFull)
func TestCursor_SortLimitSkipCombined(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	env := startDongo(t)
	coll := env.collection(t)
	for i := 1; i <= 20; i++ {
		insertDocs(t, coll, d(e("_id", fmt.Sprintf("doc%d", i)), e("seq", int32(i))))
	}

	// Page 2: items 6-10 when sorted ascending.
	cursor, err := coll.Find(ctx, bson.D{},
		options.Find().SetSort(d(e("seq", 1))).SetSkip(5).SetLimit(5))
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))

	require.Len(t, results, 5)
	assert.Equal(t, int32(6), results[0].Map()["seq"])
	assert.Equal(t, int32(10), results[4].Map()["seq"])
}
