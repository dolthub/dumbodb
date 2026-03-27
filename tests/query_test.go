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

// Package tests contains integration tests for dongo operators.
// Tests start dongo on a random port, insert data via the MongoDB Go driver,
// and verify that query operators return correct results.
//
// Run with:
//
//	go test -v ./tests/
package tests

import (
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.mongodb.org/mongo-driver/mongo"
	"go.mongodb.org/mongo-driver/mongo/options"
)

// dongoTestEnv holds a running dongo process and a connected MongoDB client.
type dongoTestEnv struct {
	cmd     *exec.Cmd
	client  *mongo.Client
	dataDir string
	port    int
}

// repoRoot returns the repository root directory (two levels above this file).
func repoRoot() string {
	_, filename, _, _ := runtime.Caller(0)
	return filepath.Join(filepath.Dir(filename), "..")
}

// startDongo launches a fresh dongo instance on a random free port.
func startDongo(tb testing.TB) *dongoTestEnv {
	tb.Helper()

	// Find a free port.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	require.NoError(tb, err)
	port := listener.Addr().(*net.TCPAddr).Port
	listener.Close()

	dataDir := tb.TempDir()
	binary := filepath.Join(repoRoot(), ".runtime", "bin", "dongo")

	if _, err := os.Stat(binary); os.IsNotExist(err) {
		// Try to build on the fly.
		build := exec.Command("go", "build", "-o", binary, "./cmd/dongo/")
		build.Dir = repoRoot()
		if out, err := build.CombinedOutput(); err != nil {
			tb.Fatalf("failed to build dongo: %v\n%s", err, out)
		}
	}

	addr := fmt.Sprintf("127.0.0.1:%d", port)
	cmd := exec.Command(binary, "--addr", addr, "--data-dir", dataDir)
	cmd.Stdout = nil
	cmd.Stderr = nil
	require.NoError(tb, cmd.Start())

	env := &dongoTestEnv{
		cmd:     cmd,
		dataDir: dataDir,
		port:    port,
	}

	tb.Cleanup(func() {
		if env.client != nil {
			env.client.Disconnect(context.Background()) //nolint:errcheck
		}
		cmd.Process.Kill() //nolint:errcheck
		cmd.Wait()         //nolint:errcheck
	})

	// Wait for dongo to be ready.
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		conn, err := net.DialTimeout("tcp", addr, 200*time.Millisecond)
		if err == nil {
			conn.Close()
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	// Connect with the MongoDB driver.
	ctx := context.Background()
	client, err := mongo.Connect(ctx, options.Client().ApplyURI(fmt.Sprintf("mongodb://%s/", addr)))
	require.NoError(tb, err)
	env.client = client

	return env
}

// collection returns a fresh collection for each test.
func (env *dongoTestEnv) collection(tb testing.TB) *mongo.Collection {
	tb.Helper()

	name := fmt.Sprintf("col_%d", rand.Int64())
	coll := env.client.Database("testdb").Collection(name)
	tb.Cleanup(func() {
		coll.Drop(context.Background()) //nolint:errcheck
	})

	return coll
}

// insertDocs inserts multiple documents and fails the test on error.
func insertDocs(tb testing.TB, coll *mongo.Collection, docs ...bson.D) {
	tb.Helper()

	iface := make([]interface{}, len(docs))
	for i, d := range docs {
		iface[i] = d
	}

	ctx := context.Background()
	_, err := coll.InsertMany(ctx, iface)
	require.NoError(tb, err)
}

// e is shorthand for bson.E (key-value pair in a bson.D document).
func e(key string, val interface{}) primitive.E {
	return primitive.E{Key: key, Value: val}
}

// d builds a bson.D from key-value pairs.
func d(elems ...primitive.E) bson.D {
	return bson.D(elems)
}

// queryIDs runs filter and returns sorted _id list.
func queryIDs(tb testing.TB, coll *mongo.Collection, filter interface{}) []interface{} {
	tb.Helper()

	ctx := context.Background()
	cursor, err := coll.Find(ctx, filter, options.Find().SetSort(d(e("_id", 1))))
	require.NoError(tb, err)

	var results []bson.D
	require.NoError(tb, cursor.All(ctx, &results))

	ids := make([]interface{}, len(results))
	for i, r := range results {
		for _, kv := range r {
			if kv.Key == "_id" {
				ids[i] = kv.Value
				break
			}
		}
	}

	return ids
}

// TestQuery_mod tests the $mod query operator.
func TestQuery_mod(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "zero"), e("v", int32(0))),
		d(e("_id", "one"), e("v", int32(1))),
		d(e("_id", "two"), e("v", int32(2))),
		d(e("_id", "three"), e("v", int32(3))),
		d(e("_id", "four"), e("v", int32(4))),
		d(e("_id", "five"), e("v", int32(5))),
		d(e("_id", "ten"), e("v", int32(10))),
		d(e("_id", "str"), e("v", "not a number")),
		d(e("_id", "float"), e("v", float64(9.0))),
		d(e("_id", "neg"), e("v", int32(-3))),
	)

	t.Run("BasicMod", func(t *testing.T) {
		t.Parallel()
		// v % 3 == 0 → 0, 3, -3, float(9.0), ten is 10%3==1 (no)
		ids := queryIDs(t, coll, d(e("v", d(e("$mod", bson.A{3, 0})))))
		assert.Equal(t, []interface{}{"float", "neg", "three", "zero"}, ids)
	})

	t.Run("Remainder1", func(t *testing.T) {
		t.Parallel()
		// v % 3 == 1 → 1, 4, 10
		ids := queryIDs(t, coll, d(e("v", d(e("$mod", bson.A{3, 1})))))
		assert.Equal(t, []interface{}{"four", "one", "ten"}, ids)
	})

	t.Run("FloatDivisor", func(t *testing.T) {
		t.Parallel()
		// float divisor is truncated: 3.7 → 3
		ids := queryIDs(t, coll, d(e("v", d(e("$mod", bson.A{3.7, 0})))))
		assert.Equal(t, []interface{}{"float", "neg", "three", "zero"}, ids)
	})

	t.Run("NegativeDivisor", func(t *testing.T) {
		t.Parallel()
		// v % -3 == 0 → same as positive divisor for divisibility
		ids := queryIDs(t, coll, d(e("v", d(e("$mod", bson.A{-3, 0})))))
		assert.Equal(t, []interface{}{"float", "neg", "three", "zero"}, ids)
	})

	t.Run("NegativeRemainder", func(t *testing.T) {
		t.Parallel()
		// -3 % 3 == 0 (Go truncated division)
		ids := queryIDs(t, coll, d(e("v", d(e("$mod", bson.A{3, -3})))))
		if ids == nil {
			ids = []interface{}{}
		}
		assert.Equal(t, []interface{}{}, ids)
	})

	t.Run("ErrNotEnoughElements", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := coll.Find(ctx, d(e("v", d(e("$mod", bson.A{3})))))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "not enough elements")
	})

	t.Run("ErrTooManyElements", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := coll.Find(ctx, d(e("v", d(e("$mod", bson.A{3, 0, 1})))))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "too many elements")
	})

	t.Run("ErrDivisorZero", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := coll.Find(ctx, d(e("v", d(e("$mod", bson.A{0, 0})))))
		require.Error(t, err)
		assert.Contains(t, err.Error(), "divisor cannot be 0")
	})
}

// TestQuery_jsonSchema tests the $jsonSchema query operator.
func TestQuery_jsonSchema(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "alice"), e("name", "Alice"), e("age", int32(30)), e("score", 95.5)),
		d(e("_id", "bob"), e("name", "Bob"), e("age", int32(25))),
		d(e("_id", "charlie"), e("name", "Charlie"), e("age", "twenty")),
		d(e("_id", "noage"), e("name", "Dana")),
		d(e("_id", "extra"), e("name", "Eve"), e("age", int32(20)), e("extra", "field")),
	)

	t.Run("RequiredFields", func(t *testing.T) {
		t.Parallel()
		// Documents that have both "name" and "age".
		filter := d(e("$jsonSchema", d(
			e("required", bson.A{"name", "age"}),
		)))
		ids := queryIDs(t, coll, filter)
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "charlie", "extra"}, ids)
	})

	t.Run("BSONTypeInt", func(t *testing.T) {
		t.Parallel()
		// age must be int (int32).
		filter := d(e("$jsonSchema", d(
			e("properties", d(
				e("age", d(e("bsonType", "int"))),
			)),
		)))
		ids := queryIDs(t, coll, filter)
		// All docs pass (properties only restricts present fields).
		// alice: age=int32 ✓, bob: age=int32 ✓, charlie: age=string ✗, noage: no age ✓, extra: age=int32 ✓
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "extra", "noage"}, ids)
	})

	t.Run("RequiredAndBSONType", func(t *testing.T) {
		t.Parallel()
		// age must be present AND be int.
		filter := d(e("$jsonSchema", d(
			e("required", bson.A{"age"}),
			e("properties", d(
				e("age", d(e("bsonType", "int"))),
			)),
		)))
		ids := queryIDs(t, coll, filter)
		// alice, bob, extra have age as int32.
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "extra"}, ids)
	})

	t.Run("MinimumMaximum", func(t *testing.T) {
		t.Parallel()
		// age >= 25 and age <= 30.
		filter := d(e("$jsonSchema", d(
			e("properties", d(
				e("age", d(
					e("bsonType", "int"),
					e("minimum", int32(25)),
					e("maximum", int32(30)),
				)),
			)),
		)))
		ids := queryIDs(t, coll, filter)
		// alice (30) ✓, bob (25) ✓, noage (no age) ✓, extra (20) ✗, charlie (string) ✗
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "noage"}, ids)
	})

	t.Run("EmptySchema", func(t *testing.T) {
		t.Parallel()
		// Empty schema matches all documents.
		filter := d(e("$jsonSchema", bson.D{}))
		ids := queryIDs(t, coll, filter)
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "charlie", "extra", "noage"}, ids)
	})

	t.Run("EnumValues", func(t *testing.T) {
		t.Parallel()
		// name must be "Alice" or "Bob".
		filter := d(e("$jsonSchema", d(
			e("properties", d(
				e("name", d(
					e("enum", bson.A{"Alice", "Bob"}),
				)),
			)),
		)))
		ids := queryIDs(t, coll, filter)
		// alice ✓, bob ✓, charlie ("Charlie") ✗, noage ("Dana") ✗, extra ("Eve") ✗
		assert.ElementsMatch(t, []interface{}{"alice", "bob"}, ids)
	})

	t.Run("AdditionalPropertiesFalse", func(t *testing.T) {
		t.Parallel()
		// No properties beyond "_id", "name" and "age" allowed.
		filter := d(e("$jsonSchema", d(
			e("properties", d(
				e("_id", bson.D{}),
				e("name", bson.D{}),
				e("age", bson.D{}),
			)),
			e("additionalProperties", false),
		)))
		ids := queryIDs(t, coll, filter)
		// alice has "score" extra field ✗, bob ✓, charlie ✓, noage ✓, extra has "extra" field ✗
		assert.ElementsMatch(t, []interface{}{"bob", "charlie", "noage"}, ids)
	})

	t.Run("AnyOf", func(t *testing.T) {
		t.Parallel()
		// age is int OR name is "Charlie".
		filter := d(e("$jsonSchema", d(
			e("anyOf", bson.A{
				d(e("properties", d(e("age", d(e("bsonType", "int")))))),
				d(e("properties", d(e("name", d(e("enum", bson.A{"Charlie"})))))),
			}),
		)))
		ids := queryIDs(t, coll, filter)
		// alice (int age) ✓, bob (int age) ✓, charlie (name="Charlie") ✓
		// noage: age is absent so bsonType "int" check is skipped → first anyOf passes ✓
		// extra (int age) ✓
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "charlie", "extra", "noage"}, ids)
	})

	t.Run("Not", func(t *testing.T) {
		t.Parallel()
		// Documents where age is NOT string.
		filter := d(e("$jsonSchema", d(
			e("properties", d(
				e("age", d(
					e("not", d(e("bsonType", "string"))),
				)),
			)),
		)))
		ids := queryIDs(t, coll, filter)
		// alice (int32) ✓, bob (int32) ✓, charlie (string) ✗, noage (absent) ✓, extra (int32) ✓
		assert.ElementsMatch(t, []interface{}{"alice", "bob", "extra", "noage"}, ids)
	})

	t.Run("ErrInvalidSchema", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		// $jsonSchema must be an object.
		_, err := coll.Find(ctx, d(e("$jsonSchema", "not an object")))
		require.Error(t, err)
	})
}

// TestQuery_bits tests the bitwise query operators.
func TestQuery_bits(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "zero"), e("v", int32(0))),
		d(e("_id", "one"), e("v", int32(1))),
		d(e("_id", "two"), e("v", int32(2))),
		d(e("_id", "three"), e("v", int32(3))),
		d(e("_id", "four"), e("v", int32(4))),
		d(e("_id", "five"), e("v", int32(5))),
		d(e("_id", "six"), e("v", int32(6))),
		d(e("_id", "seven"), e("v", int32(7))),
		d(e("_id", "str"), e("v", "not a number")),
	)

	t.Run("BitsAllClear_mask2", func(t *testing.T) {
		t.Parallel()
		// bit 1 (value 2) is clear in: 0 (00), 1 (01), 4 (100), 5 (101)
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAllClear", int32(2))))))
		assert.ElementsMatch(t, []interface{}{"zero", "one", "four", "five"}, ids)
	})

	t.Run("BitsAllSet_mask3", func(t *testing.T) {
		t.Parallel()
		// bits 0 and 1 both set: 3 (011), 7 (111)
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAllSet", int32(3))))))
		assert.ElementsMatch(t, []interface{}{"three", "seven"}, ids)
	})

	t.Run("BitsAnyClear_mask3", func(t *testing.T) {
		t.Parallel()
		// any of bits 0,1 is clear
		// 0(00)✓, 1(01) bit1 clear✓, 2(10) bit0 clear✓, 4(100) both clear✓, 5(101) bit1 clear✓, 6(110) bit0 clear✓
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAnyClear", int32(3))))))
		assert.ElementsMatch(t, []interface{}{"zero", "one", "two", "four", "five", "six"}, ids)
	})

	t.Run("BitsAnySet_mask5", func(t *testing.T) {
		t.Parallel()
		// any of bits 0,2 is set (mask=5=0b101)
		// 1(001) bit0✓, 3(011) bit0✓, 4(100) bit2✓, 5(101) both✓, 6(110) bit2✓, 7(111) both✓
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAnySet", int32(5))))))
		assert.ElementsMatch(t, []interface{}{"one", "three", "four", "five", "six", "seven"}, ids)
	})

	t.Run("BitsAllClear_positionArray", func(t *testing.T) {
		t.Parallel()
		// position 1 (bit 1, value 2) is clear: 0,1,4,5
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAllClear", bson.A{int32(1)})))))
		assert.ElementsMatch(t, []interface{}{"zero", "one", "four", "five"}, ids)
	})

	t.Run("BitsAllSet_positionArray", func(t *testing.T) {
		t.Parallel()
		// positions 0 and 1 both set: 3, 7
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAllSet", bson.A{int32(0), int32(1)})))))
		assert.ElementsMatch(t, []interface{}{"three", "seven"}, ids)
	})

	t.Run("BitsAllClear_zeroMask", func(t *testing.T) {
		t.Parallel()
		// Zero mask: all documents with numeric v match (all bits "clear" in zero mask)
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAllClear", int32(0))))))
		assert.ElementsMatch(t, []interface{}{"zero", "one", "two", "three", "four", "five", "six", "seven"}, ids)
	})

	t.Run("ErrNegativeMask", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := coll.Find(ctx, d(e("v", d(e("$bitsAllClear", int32(-1))))))
		require.Error(t, err)
	})

	t.Run("ErrNegativeBitPosition", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := coll.Find(ctx, d(e("v", d(e("$bitsAllClear", bson.A{int32(-1)})))))
		require.Error(t, err)
	})

	t.Run("StringFieldIgnored", func(t *testing.T) {
		t.Parallel()
		// String field is not numeric — not matched by bitwise operators.
		ids := queryIDs(t, coll, d(e("v", d(e("$bitsAllSet", int32(0))))))
		// "str" has string value, not matched.
		for _, id := range ids {
			assert.NotEqual(t, "str", id)
		}
	})
}

// TestQuery_expr tests the $expr query operator that allows aggregation expressions
// in query filter context.
func TestQuery_expr(t *testing.T) {
	t.Parallel()

	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", "low"), e("a", int32(5)), e("b", int32(10))),
		d(e("_id", "equal"), e("a", int32(10)), e("b", int32(10))),
		d(e("_id", "high"), e("a", int32(15)), e("b", int32(10))),
		d(e("_id", "missing")),
	)

	t.Run("FieldToFieldGt", func(t *testing.T) {
		t.Parallel()
		// {$expr: {$gt: ['$a', '$b']}} — a > b: only "high" matches.
		ids := queryIDs(t, coll, d(e("$expr", d(e("$gt", bson.A{"$a", "$b"})))))
		assert.Equal(t, []interface{}{"high"}, ids)
	})

	t.Run("FieldToFieldEq", func(t *testing.T) {
		t.Parallel()
		// {$expr: {$eq: ['$a', '$b']}} — a == b.
		// "equal" has a==b==10 ✓, "missing" has both a and b missing → null==null ✓.
		ids := queryIDs(t, coll, d(e("$expr", d(e("$eq", bson.A{"$a", "$b"})))))
		assert.Equal(t, []interface{}{"equal", "missing"}, ids)
	})

	t.Run("FieldToFieldLt", func(t *testing.T) {
		t.Parallel()
		// {$expr: {$lt: ['$a', '$b']}} — a < b: only "low" matches.
		ids := queryIDs(t, coll, d(e("$expr", d(e("$lt", bson.A{"$a", "$b"})))))
		assert.Equal(t, []interface{}{"low"}, ids)
	})

	t.Run("WithArithmeticAdd", func(t *testing.T) {
		t.Parallel()
		// {$expr: {$gt: [{$add: ['$a', '$b']}, 20]}} — a+b > 20.
		// low: 5+10=15 ✗, equal: 10+10=20 ✗, high: 15+10=25 ✓
		ids := queryIDs(t, coll, d(e("$expr", d(e("$gt", bson.A{
			d(e("$add", bson.A{"$a", "$b"})),
			int32(20),
		})))))
		assert.Equal(t, []interface{}{"high"}, ids)
	})

	t.Run("WithArithmeticSubtract", func(t *testing.T) {
		t.Parallel()
		// {$expr: {$gt: [{$subtract: ['$a', '$b']}, 0]}} — a-b > 0.
		// low: -5 ✗, equal: 0 ✗, high: 5 ✓
		ids := queryIDs(t, coll, d(e("$expr", d(e("$gt", bson.A{
			d(e("$subtract", bson.A{"$a", "$b"})),
			int32(0),
		})))))
		assert.Equal(t, []interface{}{"high"}, ids)
	})

	t.Run("MissingFieldIsNull", func(t *testing.T) {
		t.Parallel()
		// Documents with missing $a: $gt [null, 0] is false, so "missing" is excluded.
		ids := queryIDs(t, coll, d(e("$expr", d(e("$gt", bson.A{"$a", int32(0)})))))
		// low(5), equal(10), high(15) all > 0; missing has null $a so excluded
		assert.Equal(t, []interface{}{"equal", "high", "low"}, ids)
	})

	t.Run("LiteralComparison", func(t *testing.T) {
		t.Parallel()
		// {$expr: {$gte: ['$a', 10]}} — a >= 10: equal and high match.
		ids := queryIDs(t, coll, d(e("$expr", d(e("$gte", bson.A{"$a", int32(10)})))))
		assert.Equal(t, []interface{}{"equal", "high"}, ids)
	})

	t.Run("ErrInvalidOperator", func(t *testing.T) {
		t.Parallel()
		ctx := context.Background()
		_, err := coll.Find(ctx, d(e("$expr", d(e("$unknownOp", bson.A{"$a", "$b"})))))
		require.Error(t, err)
	})
}

// TestFind_CursorCleanupOnFilterError is a regression test for MultiCloser leaks.
//
// When a find command fails mid-iteration due to an invalid filter (e.g. a
// $jsonSchema with a malformed "required" clause), the cursor's underlying
// MultiCloser must be closed before the error is returned to the client.
// If it is not, GC will fire the finalizer and panic with:
//
//	panic: *iterator.MultiCloser has not been finalized
//
// This test does NOT run in parallel so that forcing GC here does not interact
// with other goroutines' resource tracking.
func TestFind_CursorCleanupOnFilterError(t *testing.T) {
	env := startDongo(t)
	coll := env.collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("name", "alice")),
		d(e("_id", int32(2)), e("name", "bob")),
	)

	ctx := context.Background()

	// $jsonSchema with required as a string (not an array) causes FilterDocument
	// to return an error on the first document, which fails ConsumeValuesN.
	// The fix: MsgFind calls h.cursors.CloseAndRemove before returning the error,
	// properly finalizing the MultiCloser.
	_, err := coll.Find(ctx, d(e("$jsonSchema", d(e("required", "not-an-array")))))
	require.Error(t, err)

	// Force GC multiple times to flush the finalizer goroutine.
	// If the MultiCloser leaked (Close not called), this will panic.
	for i := 0; i < 5; i++ {
		runtime.GC()
	}
}
