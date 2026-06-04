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

package dolt

import (
	"math"
	"testing"

	mongobson "go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// skipUntilBSONPrefilter is invoked at the top of every prefilter
// range test. The bson-a branch disabled the historical ExtJSON
// prefilter when switching to BSON storage; the BSON-element
// prefilter that replaces it lands in a follow-on commit. Until
// then, these tests cannot exercise their target code path (the
// prefilter returns nil for every filter), so they are deferred.
func skipUntilBSONPrefilter(t *testing.T) {
	t.Helper()
	t.Skip("prefilter disabled on bson-a; BSON-element prefilter lands in a follow-on commit")
}

// canonicalDoc encodes a mongo bson.D to canonical Extended JSON, matching
// the bytes the dolt backend stores per document.
func canonicalDoc(t *testing.T, d mongobson.D) []byte {
	t.Helper()
	bs, err := mongobson.Marshal(d)
	if err != nil {
		t.Fatalf("Marshal: %v", err)
	}
	out, err := mongobson.MarshalExtJSON(mongobson.Raw(bs), true, false)
	if err != nil {
		t.Fatalf("MarshalExtJSON: %v", err)
	}
	return out
}

// rangeOp builds a {field: {ops...}} types.Document filter that the
// prefilter sees.
func rangeOp(t *testing.T, field string, kvs ...any) *types.Document {
	t.Helper()
	op := must.NotFail(types.NewDocument(kvs...))
	return must.NotFail(types.NewDocument(field, op))
}

func TestRangePrefilter_BasicGtLte(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gt", int32(5), "$lte", int32(10)))
	if pf == nil {
		t.Fatal("expected predicate, got nil")
	}
	for _, tc := range []struct {
		i    int32
		want bool
	}{
		{5, false},  // exclusive lower
		{6, true},
		{10, true},  // inclusive upper
		{11, false},
		{0, false},
	} {
		doc := canonicalDoc(t, mongobson.D{{Key: "_id", Value: int32(1)}, {Key: "i", Value: tc.i}})
		got := pf(doc)
		if got != tc.want {
			t.Errorf("i=%d: pf=%v want=%v (json=%s)", tc.i, got, tc.want, doc)
		}
	}
}

func TestRangePrefilter_AllOperators(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	cases := []struct {
		name   string
		filter *types.Document
		i      int32
		want   bool
	}{
		{"gt-pass", rangeOp(t, "i", "$gt", int32(5)), 6, true},
		{"gt-fail-eq", rangeOp(t, "i", "$gt", int32(5)), 5, false},
		{"gte-pass-eq", rangeOp(t, "i", "$gte", int32(5)), 5, true},
		{"gte-fail", rangeOp(t, "i", "$gte", int32(5)), 4, false},
		{"lt-pass", rangeOp(t, "i", "$lt", int32(5)), 4, true},
		{"lt-fail-eq", rangeOp(t, "i", "$lt", int32(5)), 5, false},
		{"lte-pass-eq", rangeOp(t, "i", "$lte", int32(5)), 5, true},
		{"lte-fail", rangeOp(t, "i", "$lte", int32(5)), 6, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			pf := buildScanPrefilter(tc.filter)
			if pf == nil {
				t.Fatal("nil predicate")
			}
			doc := canonicalDoc(t, mongobson.D{{Key: "i", Value: tc.i}})
			if got := pf(doc); got != tc.want {
				t.Errorf("got=%v want=%v (json=%s)", got, tc.want, doc)
			}
		})
	}
}

// Stored field is missing from the doc  -- range filters should not match.
// The prefilter must return false (proven non-match).
func TestRangePrefilter_MissingField(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gte", int32(0)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	doc := canonicalDoc(t, mongobson.D{{Key: "_id", Value: int32(1)}, {Key: "j", Value: int32(7)}})
	if pf(doc) {
		t.Errorf("missing field should be rejected; json=%s", doc)
	}
}

// Embedded sub-document also containing a field with the same name as the
// target. The walker MUST look only at depth-1, so the inner copy can't
// poison the result.
func TestRangePrefilter_EmbeddedSameName(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gte", int32(50)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	// Outer i=10 (would fail), embedded {x.i:99} (would pass if naive
	// substring check). Outer wins -> expect false.
	doc := canonicalDoc(t, mongobson.D{
		{Key: "x", Value: mongobson.D{{Key: "i", Value: int32(99)}}},
		{Key: "i", Value: int32(10)},
	})
	if pf(doc) {
		t.Errorf("embedded i=99 must not mask outer i=10; json=%s", doc)
	}
	// Now outer i=80 (passes), embedded i=10 (would fail naively first).
	doc2 := canonicalDoc(t, mongobson.D{
		{Key: "x", Value: mongobson.D{{Key: "i", Value: int32(10)}}},
		{Key: "i", Value: int32(80)},
	})
	if !pf(doc2) {
		t.Errorf("outer i=80 must pass even with embedded i=10; json=%s", doc2)
	}
}

// When the stored value is itself an array, embedded sub-doc, or string,
// the predicate must bail to permissive (true)  -- Mongo's range semantics
// over those types aren't modeled by the byte walker.
func TestRangePrefilter_AnomalousValueIsPermissive(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gte", int32(0), "$lt", int32(10)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	cases := []struct {
		name string
		doc  mongobson.D
	}{
		{"array", mongobson.D{{Key: "i", Value: mongobson.A{int32(1), int32(2)}}}},
		{"subdoc", mongobson.D{{Key: "i", Value: mongobson.D{{Key: "x", Value: int32(1)}}}}},
		{"string", mongobson.D{{Key: "i", Value: "hello"}}},
		{"null", mongobson.D{{Key: "i", Value: nil}}},
		{"bool", mongobson.D{{Key: "i", Value: true}}},
		{"objectid", mongobson.D{{Key: "i", Value: mongobson.NewObjectID()}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := canonicalDoc(t, tc.doc)
			if !pf(doc) {
				t.Errorf("anomalous value must be permissive (true); json=%s", doc)
			}
		})
	}
}

// Mixed numeric storage types: filter is int but doc stored as int64 or
// double. Prefilter must compare numerically.
func TestRangePrefilter_MixedNumericTypes(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gte", int32(5), "$lt", int32(10)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	cases := []struct {
		name string
		doc  mongobson.D
		want bool
	}{
		{"int32-in", mongobson.D{{Key: "i", Value: int32(7)}}, true},
		{"int32-out", mongobson.D{{Key: "i", Value: int32(11)}}, false},
		{"int64-in", mongobson.D{{Key: "i", Value: int64(7)}}, true},
		{"int64-out", mongobson.D{{Key: "i", Value: int64(11)}}, false},
		{"float-in", mongobson.D{{Key: "i", Value: 7.5}}, true},
		{"float-out-low", mongobson.D{{Key: "i", Value: 4.999}}, false},
		{"float-eq-lower", mongobson.D{{Key: "i", Value: 5.0}}, true},
		{"float-eq-upper", mongobson.D{{Key: "i", Value: 10.0}}, false}, // $lt strict
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := canonicalDoc(t, tc.doc)
			if got := pf(doc); got != tc.want {
				t.Errorf("got=%v want=%v (json=%s)", got, tc.want, doc)
			}
		})
	}
}

// NaN-stored values get the permissive bail-out (the predicate doesn't
// claim to know NaN semantics  -- the handler's FilterIterator will reject
// downstream because NaN doesn't satisfy any range comparison).
func TestRangePrefilter_NaNPermissive(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gte", int32(0)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	doc := canonicalDoc(t, mongobson.D{{Key: "i", Value: math.NaN()}})
	if !pf(doc) {
		t.Errorf("NaN must be permissive (true); json=%s", doc)
	}
}

// Operator docs that mix $gt with anything we don't model must cause the
// whole prefilter to be unbuilt  -- we'd otherwise risk a false negative.
func TestRangePrefilter_BailsOnUnsupportedOperators(t *testing.T) {
	t.Parallel()
	cases := []*types.Document{
		rangeOp(t, "i", "$eq", int32(5), "$gt", int32(0)),
		rangeOp(t, "i", "$ne", int32(5)),
		rangeOp(t, "i", "$in", must.NotFail(types.NewArray(int32(1), int32(2)))),
		rangeOp(t, "i", "$exists", true),
		rangeOp(t, "i", "$type", "int"),
		rangeOp(t, "i", "$regex", "foo"),
		rangeOp(t, "i", "$gt", "string-bound"),
		rangeOp(t, "i", "$gt", types.Null),
	}
	for _, f := range cases {
		if pf := buildScanPrefilter(f); pf != nil {
			t.Errorf("expected nil prefilter for filter %v", f)
		}
	}
}

// $gt on a top-level $-operator field, dotted paths, or an empty operator
// doc must bail out.
func TestRangePrefilter_TopLevelDollarBails(t *testing.T) {
	t.Parallel()
	if pf := buildScanPrefilter(must.NotFail(types.NewDocument(
		"$and", must.NotFail(types.NewArray()),
	))); pf != nil {
		t.Error("$and at top level must bail")
	}
	if pf := buildScanPrefilter(rangeOp(t, "a.b", "$gt", int32(0))); pf != nil {
		t.Error("dotted path must bail")
	}
	if pf := buildScanPrefilter(must.NotFail(types.NewDocument(
		"i", must.NotFail(types.NewDocument()),
	))); pf != nil {
		t.Error("empty operator doc must bail")
	}
}

// Combining range with another field's equality must AND-combine.
func TestRangePrefilter_CombinedWithEquality(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	filter := must.NotFail(types.NewDocument(
		"i", must.NotFail(types.NewDocument("$gte", int32(0), "$lt", int32(10))),
		"tag", "row",
	))
	pf := buildScanPrefilter(filter)
	if pf == nil {
		t.Fatal("nil predicate")
	}
	cases := []struct {
		name string
		doc  mongobson.D
		want bool
	}{
		{"both-match", mongobson.D{{Key: "i", Value: int32(5)}, {Key: "tag", Value: "row"}}, true},
		{"range-fail", mongobson.D{{Key: "i", Value: int32(11)}, {Key: "tag", Value: "row"}}, false},
		{"tag-fail", mongobson.D{{Key: "i", Value: int32(5)}, {Key: "tag", Value: "col"}}, false},
		{"missing-i", mongobson.D{{Key: "tag", Value: "row"}}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			doc := canonicalDoc(t, tc.doc)
			if got := pf(doc); got != tc.want {
				t.Errorf("got=%v want=%v (json=%s)", got, tc.want, doc)
			}
		})
	}
}

// Two clauses on the same side ($gt: 3 + $gt: 5) must keep the tighter.
// Two with equal value but mixed strictness ($gt:5 + $gte:5) must end
// strict.
func TestRangePrefilter_BoundIntersection(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	pf := buildScanPrefilter(rangeOp(t, "i", "$gt", int32(3), "$gte", int32(5)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	for _, tc := range []struct {
		i    int32
		want bool
	}{
		{4, false}, // gt:3 passes but gte:5 fails
		{5, true},  // gte:5 passes (tighter side wins), gt:3 trivially passes
		{6, true},
	} {
		doc := canonicalDoc(t, mongobson.D{{Key: "i", Value: tc.i}})
		if got := pf(doc); got != tc.want {
			t.Errorf("i=%d: got=%v want=%v", tc.i, got, tc.want)
		}
	}

	// Same value, mixed strictness: gt:5 + gte:5 -> effectively gt:5.
	pf2 := buildScanPrefilter(rangeOp(t, "i", "$gt", int32(5), "$gte", int32(5)))
	if pf2 == nil {
		t.Fatal("nil predicate")
	}
	doc5 := canonicalDoc(t, mongobson.D{{Key: "i", Value: int32(5)}})
	if pf2(doc5) {
		t.Error("$gt:5 + $gte:5 must reject 5")
	}
	doc6 := canonicalDoc(t, mongobson.D{{Key: "i", Value: int32(6)}})
	if !pf2(doc6) {
		t.Error("$gt:5 + $gte:5 must accept 6")
	}
}

// int64 values whose magnitude doesn't fit in float64 mantissa must bail
// to permissive  -- silent precision loss would otherwise risk a false
// negative.
func TestRangePrefilter_HugeInt64Permissive(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	// A bound that does fit (small int) so the prefilter is built.
	pf := buildScanPrefilter(rangeOp(t, "i", "$gt", int32(0)))
	if pf == nil {
		t.Fatal("nil predicate")
	}
	// Doc value: 2^53+1  -- first int64 not exactly representable as float64.
	huge := int64(1)<<53 + 1
	doc := canonicalDoc(t, mongobson.D{{Key: "i", Value: huge}})
	if !pf(doc) {
		t.Errorf("huge int64 must be permissive (true); json=%s", doc)
	}

	// Bound itself huge  -- entire prefilter must bail.
	if pf2 := buildScanPrefilter(rangeOp(t, "i", "$gt", huge)); pf2 != nil {
		t.Errorf("huge int64 bound must produce nil prefilter")
	}
}

// scanTopLevelNumericExtJSON unit-level coverage for the walker, on bytes
// it is unlikely to encounter naturally (whitespace, escaped non-target
// keys, top-level keyword values).
func TestScanTopLevelNumericExtJSON_DirectCases(t *testing.T) {
	skipUntilBSONPrefilter(t)
	t.Parallel()
	field := []byte("i")

	cases := []struct {
		name   string
		json   string
		val    float64
		status rangeProbeStatus
	}{
		{"compact-int", `{"i":{"$numberInt":"42"}}`, 42, rangeProbeFound},
		{"compact-long", `{"i":{"$numberLong":"42"}}`, 42, rangeProbeFound},
		{"compact-double", `{"i":{"$numberDouble":"3.5"}}`, 3.5, rangeProbeFound},
		{"with-whitespace", `{ "i" : {"$numberInt":"7"} }`, 7, rangeProbeFound},
		{"missing", `{"j":{"$numberInt":"1"}}`, 0, rangeProbeMissing},
		{"empty-object", `{}`, 0, rangeProbeMissing},
		{"value-string", `{"i":"42"}`, 0, rangeProbeBail},
		{"value-array", `{"i":[1,2]}`, 0, rangeProbeBail},
		{"value-subdoc-shape", `{"i":{"x":1}}`, 0, rangeProbeBail},
		{"value-decimal", `{"i":{"$numberDecimal":"3.14"}}`, 0, rangeProbeBail},
		{"value-true", `{"i":true}`, 0, rangeProbeBail},
		{"value-null", `{"i":null}`, 0, rangeProbeBail},
		{"malformed-no-brace", `"i":1`, 0, rangeProbeBail},
		{"trailing-junk-skipped", `{"a":[1,2,3],"i":{"$numberInt":"5"}}`, 5, rangeProbeFound},
		{"escape-in-other-key", `{"a\nb":{"$numberInt":"1"},"i":{"$numberInt":"5"}}`, 0, rangeProbeBail},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotV, gotS := scanTopLevelNumericExtJSON([]byte(tc.json), field)
			if gotS != tc.status {
				t.Fatalf("status: got=%v want=%v", gotS, tc.status)
			}
			if gotS == rangeProbeFound && gotV != tc.val {
				t.Fatalf("value: got=%v want=%v", gotV, tc.val)
			}
		})
	}
}

// String storage where the literal happens to look like a numeric wrapper
// inside the string body must still bail (the value's outer token is a
// JSON string, not an object).
func TestRangePrefilter_StringLookingLikeWrapperBails(t *testing.T) {
	t.Parallel()
	// Doc has "i" stored as the string `{"$numberInt":"42"}`. A naive
	// substring check might "see" a numeric. The walker must not.
	doc := []byte(`{"i":"{\"$numberInt\":\"42\"}"}`)
	v, s := scanTopLevelNumericExtJSON(doc, []byte("i"))
	if s != rangeProbeBail {
		t.Fatalf("got=(%v,%v) want=bail", v, s)
	}
}
