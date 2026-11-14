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

package common

import (
	"context"
	"errors"
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// The expectations are mongod 8.0.28's, read from the bounds of a hinted
// scan of a partial index.
func TestQueryImpliesPartialFilter(t *testing.T) {
	doc := func(pairs ...any) *types.Document { return must.NotFail(types.NewDocument(pairs...)) }
	arr := func(vs ...any) *types.Array { return must.NotFail(types.NewArray(vs...)) }
	b := func(op string, v any) *types.Document { return doc("b", doc(op, v)) }
	a := func(op string, v any) *types.Document { return doc("a", doc(op, v)) }

	bExists := doc("b", doc("$exists", true))
	for name, tc := range map[string]struct {
		query *types.Document
		want  bool
	}{
		"eq":                 {doc("b", int32(1)), true},
		"eq null":            {doc("b", types.Null), false},
		"ne null":            {b("$ne", types.Null), true},
		"ne value":           {b("$ne", int32(1)), false},
		"gt MinKey":          {b("$gt", types.MinKey), true},
		"gte MinKey":         {b("$gte", types.MinKey), true},
		"lt MaxKey":          {b("$lt", types.MaxKey), true},
		"lte null":           {b("$lte", types.Null), false},
		"gt null":            {b("$gt", types.Null), false},
		"in":                 {b("$in", arr(int32(1), int32(2))), true},
		"in with null":       {b("$in", arr(int32(1), types.Null)), false},
		"nin null":           {b("$nin", arr(types.Null)), true},
		"nin value":          {b("$nin", arr(int32(1))), false},
		"type null":          {b("$type", "null"), true},
		"regex":              {b("$regex", "^a"), true},
		"exists":             {b("$exists", true), true},
		"exists false":       {b("$exists", false), false},
		"elemMatch":          {b("$elemMatch", doc("$gt", int32(1))), true},
		"size":               {b("$size", int32(1)), true},
		"mod":                {b("$mod", arr(int32(2), int32(0))), true},
		"not eq null":        {b("$not", doc("$eq", types.Null)), true},
		"not in null":        {b("$not", doc("$in", arr(types.Null))), true},
		"not gt":             {b("$not", doc("$gt", int32(1))), false},
		"not type null":      {b("$not", doc("$type", "null")), false},
		"array value":        {doc("b", arr(int32(1))), true},
		"NaN":                {doc("b", math.NaN()), true},
		"dotted subfield":    {doc("b.c", int32(1)), false},
		"other field":        {doc("a", int32(1)), false},
		"one-branch $and":    {doc("$and", arr(doc("b", int32(1)))), true},
		"$or all imply":      {doc("$or", arr(doc("b", int32(1)), doc("b", int32(2)))), true},
		"$or one does not":   {doc("$or", arr(doc("b", int32(1)), doc("a", int32(2)))), false},
		"$or with null":      {doc("$or", arr(doc("b", int32(1)), doc("b", types.Null))), false},
		"one-branch $or":     {doc("$or", arr(doc("b", int32(1)))), true},
		"nor null":           {doc("$nor", arr(doc("b", types.Null))), true},
		"nor exists false":   {doc("$nor", arr(b("$exists", false))), false},
		"beside other field": {doc("a", int32(1), "b", doc("$gt", int32(1))), true},
		"range with null":    {doc("b", doc("$gte", int32(-1), "$lte", types.Null)), true},
	} {
		got, known, err := QueryImpliesPartialFilter(context.Background(), tc.query, bExists)
		if err != nil || !known || got != tc.want {
			t.Errorf("$exists: %s: got %v (known %v), want %v", name, got, known, tc.want)
		}
	}

	aNonNegative := a("$gte", int32(0))
	for name, tc := range map[string]struct {
		query *types.Document
		want  bool
	}{
		"eq 0":               {doc("a", int32(0)), true},
		"eq -1":              {doc("a", int32(-1)), false},
		"gte 0":              {a("$gte", int32(0)), true},
		"gt 0":               {a("$gt", int32(0)), true},
		"gt -1":              {a("$gt", int32(-1)), false},
		"gte -1":             {a("$gte", int32(-1)), false},
		"lt":                 {a("$lt", int32(5)), false},
		"two-sided":          {doc("a", doc("$gte", int32(0), "$lt", int32(5))), true},
		"two-sided below":    {doc("a", doc("$gte", int32(-1), "$gt", int32(0))), true},
		"in":                 {a("$in", arr(int32(1), int32(2))), true},
		"in negative":        {a("$in", arr(int32(0), int32(-1))), false},
		"in NaN":             {a("$in", arr(int32(1), math.NaN())), false},
		"gte -0.0":           {a("$gte", math.Copysign(0, -1)), true},
		"gte decimal 0":      {a("$gte", types.Decimal128{H: 0x3040000000000000}), true},
		"gte int64 0":        {a("$gte", int64(0)), true},
		"gte Infinity":       {a("$gte", math.Inf(1)), true},
		"NaN":                {doc("a", math.NaN()), false},
		"gte NaN":            {a("$gte", math.NaN()), false},
		"string":             {doc("a", "x"), false},
		"gt string":          {a("$gt", ""), false},
		"bool":               {doc("a", true), false},
		"array":              {a("$eq", arr(int32(1))), false},
		"elemMatch":          {a("$elemMatch", doc("$gte", int32(0))), false},
		"type number":        {a("$type", "number"), false},
		"gt MinKey":          {a("$gt", types.MinKey), false},
		"not lt 0":           {a("$not", doc("$lt", int32(0))), false},
		"all":                {a("$all", arr(int32(1))), true},
		"$and any child":     {doc("a", doc("$gte", int32(-1)), "$and", arr(a("$gte", int32(0)))), true},
		"$or all within":     {doc("$or", arr(doc("a", int32(1)), a("$in", arr(int32(2), int32(3))))), true},
		"$or one outside":    {doc("$or", arr(doc("a", int32(1)), doc("a", int32(-2)))), false},
		"beside other field": {doc("a", doc("$gte", int32(0)), "b", int32(1)), true},
	} {
		got, known, err := QueryImpliesPartialFilter(context.Background(), tc.query, aNonNegative)
		if err != nil || !known || got != tc.want {
			t.Errorf("$gte 0: %s: got %v (known %v), want %v", name, got, known, tc.want)
		}
	}

	for name, pfe := range map[string]*types.Document{
		"$or":          doc("$or", arr(doc("a", int32(1)), doc("b", int32(1)))),
		"$type":        a("$type", "number"),
		"$in":          a("$in", arr(int32(1))),
		"exists false": doc("a", doc("$exists", false)),
	} {
		if _, known, _ := QueryImpliesPartialFilter(context.Background(), doc("a", int32(1)), pfe); known {
			t.Errorf("%s: a filter expression it does not model must be unknown", name)
		}
	}
}

// wideConjunction is {k0: {op: v}, ..., k<n-1>: {op: v}}.
func wideConjunction(n int, op string, v any) *types.Document {
	d := types.MakeDocument(n)
	for i := 0; i < n; i++ {
		d.AppendDecoded("k"+strconv.Itoa(i), must.NotFail(types.NewDocument(op, v)))
	}
	return d
}

// A partial filter is read on every hinted query, so deciding implication
// must take time linear in its size.
func TestQueryImpliesPartialFilterWideFilter(t *testing.T) {
	pfe := wideConjunction(100_000, "$gte", int32(0))
	query := must.NotFail(types.NewDocument("a", must.NotFail(types.NewDocument("$gt", int32(0)))))
	start := time.Now()
	implied, known, err := QueryImpliesPartialFilter(context.Background(), query, pfe)
	elapsed := time.Since(start)
	if err != nil || !known || implied {
		t.Fatalf("got implied %v, known %v, err %v; want not implied", implied, known, err)
	}
	if elapsed > time.Second {
		t.Errorf("took %v for a 100,000-conjunct filter", elapsed)
	}
	t.Logf("took %v", elapsed)
}

// A wide query against a wide filter stops when its context is done: each of
// the query's $or branches implies every conjunct of the filter.
func TestQueryImpliesPartialFilterHonorsContext(t *testing.T) {
	branch := wideConjunction(500, "$eq", int32(1))
	or := types.MakeArray(500)
	for i := 0; i < 500; i++ {
		or.Append(branch)
	}
	query := must.NotFail(types.NewDocument("$or", or))
	pfe := wideConjunction(500, "$exists", true)

	if implied, _, err := QueryImpliesPartialFilter(context.Background(), query, pfe); err != nil || !implied {
		t.Fatalf("got implied %v, err %v; want implied", implied, err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, _, err := QueryImpliesPartialFilter(ctx, query, pfe); !errors.Is(err, context.Canceled) {
		t.Fatalf("err = %v, want context.Canceled", err)
	}
}
