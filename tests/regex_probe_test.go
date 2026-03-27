package tests

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestProbeRegexCI(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("name", "Hello World")),
		d(e("_id", 2), e("name", "goodbye world")),
	)
	filter := bson.D{{"name", primitive.Regex{Pattern: "hello", Options: "i"}}}
	ids := queryIDs(t, coll, filter)
	t.Logf("case-insensitive regex result: %v", ids)
	assert.Equal(t, []interface{}{int32(1)}, ids)
}

func TestProbeRegexMultiline(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("text", "line one\nline two")),
		d(e("_id", 2), e("text", "single line")),
	)
	filter := bson.D{{"text", primitive.Regex{Pattern: "^line two", Options: "m"}}}
	ids := queryIDs(t, coll, filter)
	t.Logf("multiline regex result: %v", ids)
	assert.Equal(t, []interface{}{int32(1)}, ids)
}

func TestProbeTextPhrase(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	createTextIndex(t, coll, bson.D{{"content", "text"}}, "content_text")
	insertDocs(t, coll,
		d(e("_id", 1), e("content", "quick brown fox")),
		d(e("_id", 2), e("content", "brown fox quick")),
	)
	filter := bson.D{{"$text", bson.D{{"$search", `"quick brown"`}}}}
	ctx := context.Background()
	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	t.Logf("phrase text search result count: %d", len(results))
	assert.Equal(t, 1, len(results))
}

func TestProbeTextDiacriticSensitive(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	createTextIndex(t, coll, bson.D{{"word", "text"}}, "word_text")
	insertDocs(t, coll,
		d(e("_id", 1), e("word", "café")),
		d(e("_id", 2), e("word", "cafe")),
	)
	filter := bson.D{{"$text", bson.D{{"$search", "cafe"}, {"$diacriticSensitive", false}}}}
	ctx := context.Background()
	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	t.Logf("diacritic-insensitive count: %d", len(results))
	// In MongoDB, diacriticSensitive:false should match both "café" and "cafe"
	assert.Equal(t, 2, len(results))
}

func TestProbeTextLanguage(t *testing.T) {
	t.Skip("stemming not implemented: $language/$search word stemming requires NLP support")

	env := startDongo(t)
	coll := env.collection(t)
	createTextIndex(t, coll, bson.D{{"body", "text"}}, "body_text")
	insertDocs(t, coll,
		d(e("_id", 1), e("body", "running quickly")),
		d(e("_id", 2), e("body", "run fast")),
	)
	// $language enables stemming: searching "run" should match "running" too
	filter := bson.D{{"$text", bson.D{{"$search", "run"}, {"$language", "en"}}}}
	ctx := context.Background()
	cursor, err := coll.Find(ctx, filter)
	require.NoError(t, err)
	var results []bson.D
	require.NoError(t, cursor.All(ctx, &results))
	t.Logf("stemming result count: %d", len(results))
	// Both should match due to stemming (run -> running)
	assert.Equal(t, 2, len(results))
}

func TestProbeModInt32(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("v", int32(9))),
		d(e("_id", 2), e("v", int32(10))),
	)
	filter := bson.D{{"v", bson.D{{"$mod", bson.A{3, 0}}}}}
	ids := queryIDs(t, coll, filter)
	t.Logf("mod int32: %v", ids)
	assert.Equal(t, []interface{}{int32(1)}, ids)
}

func TestProbeModInt64(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("v", int64(9))),
		d(e("_id", 2), e("v", int64(10))),
	)
	filter := bson.D{{"v", bson.D{{"$mod", bson.A{3, 0}}}}}
	ids := queryIDs(t, coll, filter)
	t.Logf("mod int64: %v", ids)
	assert.Equal(t, []interface{}{int32(1)}, ids)
}

func TestProbeModNestedField(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("a", d(e("b", 9)))),
		d(e("_id", 2), e("a", d(e("b", 10)))),
	)
	filter := bson.D{{"a.b", bson.D{{"$mod", bson.A{3, 0}}}}}
	ids := queryIDs(t, coll, filter)
	t.Logf("mod nested: %v", ids)
	assert.Equal(t, []interface{}{int32(1)}, ids)
}

func TestProbeModNonNumericField(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("v", "hello")),
		d(e("_id", 2), e("v", nil)),
		d(e("_id", 3), e("v", true)),
	)
	filter := bson.D{{"v", bson.D{{"$mod", bson.A{3, 0}}}}}
	ids := queryIDs(t, coll, filter)
	t.Logf("mod non-numeric: %v", ids)
	assert.Equal(t, []interface{}{}, ids)
}

func TestProbeModNaNDivisor(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)
	insertDocs(t, coll,
		d(e("_id", 1), e("v", 9)),
	)
	// NaN divisor should fail
	ctx := context.Background()
	_, err := coll.Find(ctx, bson.D{{"v", bson.D{{"$mod", bson.A{math.NaN(), 0}}}}})
	t.Logf("NaN divisor error: %v", err)
	assert.Error(t, err)
}
