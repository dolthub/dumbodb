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
	"bytes"
	"context"
	"io"
	"reflect"
	"sort"
	"testing"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/val"
)

// collMapAt returns the prolly.Map for collName at the given commit hash.
func collMapAt(t *testing.T, b *Backend, dbName, commitHash, collName string) prolly.Map {
	t.Helper()
	ctx := context.Background()
	db, err := b.getOrOpenDB(ctx, dbName, false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	am, err := amFromCommitHash(ctx, db, commitHash)
	if err != nil {
		t.Fatalf("amFromCommitHash(%s): %v", commitHash, err)
	}
	m, err := collectionMapFromAM(ctx, db, am, collName)
	if err != nil {
		t.Fatalf("collectionMapFromAM(%s): %v", collName, err)
	}
	return m
}

// changeLabel renders a change as "<kind>:<_id>" by decoding the relevant image.
func changeLabel(t *testing.T, b *Backend, dbName string, c collChange) string {
	t.Helper()
	db, _ := b.getOrOpenDB(context.Background(), dbName, false)
	v := c.to
	kind := "added"
	switch c.kind {
	case collRemoved:
		v, kind = c.from, "removed"
	case collModified:
		v, kind = c.to, "modified"
	}
	doc, err := readDocFromValue(context.Background(), db.ns, v)
	if err != nil {
		t.Fatalf("decode: %v", err)
	}
	id, _ := doc.Get("_id")
	return kind + ":" + sprintID(id)
}

func sprintID(id any) string {
	switch v := id.(type) {
	case int64:
		return string(rune('0' + v)) // single-digit ids in these tests
	default:
		return "?"
	}
}

// bruteForceChanges is the reference implementation: a dual-IterAll merge walk
// (the pattern being replaced). Returns sorted "<kind>:<_id>" labels.
func bruteForceChanges(t *testing.T, b *Backend, dbName string, fromMap, toMap prolly.Map) []string {
	t.Helper()
	ctx := context.Background()
	db, _ := b.getOrOpenDB(ctx, dbName, false)

	label := func(v val.Tuple, kind string) string {
		doc, err := readDocFromValue(ctx, db.ns, v)
		if err != nil {
			t.Fatalf("decode: %v", err)
		}
		id, _ := doc.Get("_id")
		return kind + ":" + sprintID(id)
	}

	ia, _ := fromMap.IterAll(ctx)
	ib, _ := toMap.IterAll(ctx)
	kA, vA, eA := ia.Next(ctx)
	kB, vB, eB := ib.Next(ctx)
	var out []string
	for {
		da, dbb := eA == io.EOF, eB == io.EOF
		if da && dbb {
			break
		}
		switch {
		case da:
			out = append(out, label(vB, "added"))
			kB, vB, eB = ib.Next(ctx)
		case dbb:
			out = append(out, label(vA, "removed"))
			kA, vA, eA = ia.Next(ctx)
		default:
			switch bytes.Compare(kA, kB) {
			case -1:
				out = append(out, label(vA, "removed"))
				kA, vA, eA = ia.Next(ctx)
			case 1:
				out = append(out, label(vB, "added"))
				kB, vB, eB = ib.Next(ctx)
			default:
				if !bytes.Equal(vA, vB) {
					out = append(out, label(vB, "modified"))
				}
				kA, vA, eA = ia.Next(ctx)
				kB, vB, eB = ib.Next(ctx)
			}
		}
	}
	sort.Strings(out)
	return out
}

func diffMapsChanges(t *testing.T, b *Backend, dbName string, fromMap, toMap prolly.Map) []string {
	t.Helper()
	var out []string
	err := forEachCollectionChange(context.Background(), fromMap, toMap, func(c collChange) (bool, error) {
		out = append(out, changeLabel(t, b, dbName, c))
		return false, nil
	})
	if err != nil {
		t.Fatalf("forEachCollectionChange: %v", err)
	}
	sort.Strings(out)
	return out
}

func TestForEachCollectionChange_MatchesBruteForce(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	const db = "cd"

	// commit A: docs 1,2,3.
	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(1), "v", int64(1)))
	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(2), "v", int64(1)))
	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(3), "v", int64(1)))
	hA := commitTS(t, b, db, "main", "A", 10_000)

	// commit B: modify 2, delete 3, add 4.
	updateDoc(t, b, db, "main", "c", mustDoc(t, "_id", int64(2), "v", int64(2)))
	deleteID(t, b, db, "main", "c", int64(3))
	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(4), "v", int64(1)))
	hB := commitTS(t, b, db, "main", "B", 20_000)

	mapA := collMapAt(t, b, db, hA, "c")
	mapB := collMapAt(t, b, db, hB, "c")

	got := diffMapsChanges(t, b, db, mapA, mapB)
	want := bruteForceChanges(t, b, db, mapA, mapB)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("DiffMaps changes %v != brute force %v", got, want)
	}
	if !reflect.DeepEqual(got, []string{"added:4", "modified:2", "removed:3"}) {
		t.Fatalf("unexpected changes: %v", got)
	}

	// Reverse direction: removed/added swap, modified stays.
	gotRev := diffMapsChanges(t, b, db, mapB, mapA)
	wantRev := bruteForceChanges(t, b, db, mapB, mapA)
	if !reflect.DeepEqual(gotRev, wantRev) {
		t.Fatalf("reverse: %v != %v", gotRev, wantRev)
	}
}

func TestForEachCollectionChange_EdgeCases(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	const db = "cde"

	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(1)))
	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(2)))
	hA := commitTS(t, b, db, "main", "A", 10_000)
	mapA := collMapAt(t, b, db, hA, "c")
	empty, err := newEmptyMap(ctx, mustDB(t, b, db).ns)
	if err != nil {
		t.Fatal(err)
	}

	// Identical maps: no changes.
	if got := diffMapsChanges(t, b, db, mapA, mapA); len(got) != 0 {
		t.Fatalf("identical maps should have no changes, got %v", got)
	}
	// empty -> mapA: everything added.
	if got := diffMapsChanges(t, b, db, empty, mapA); !reflect.DeepEqual(got, []string{"added:1", "added:2"}) {
		t.Fatalf("empty->A added: %v", got)
	}
	// mapA -> empty: everything removed.
	if got := diffMapsChanges(t, b, db, mapA, empty); !reflect.DeepEqual(got, []string{"removed:1", "removed:2"}) {
		t.Fatalf("A->empty removed: %v", got)
	}
}

func TestForEachCollectionChange_EarlyStop(t *testing.T) {
	b := newTestBackend(t)
	ctx := context.Background()
	const db = "cds"

	insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(1)))
	hA := commitTS(t, b, db, "main", "one", 5_000)
	for i := 2; i <= 5; i++ {
		insertOne(t, ctx, collAt(t, b, db, "main", "c"), mustDoc(t, "_id", int64(i)))
	}
	hB := commitTS(t, b, db, "main", "five", 10_000) // four docs added vs hA

	mapA := collMapAt(t, b, db, hA, "c")
	mapB := collMapAt(t, b, db, hB, "c")

	visited := 0
	err := forEachCollectionChange(ctx, mapA, mapB, func(c collChange) (bool, error) {
		visited++
		return true, nil // stop after the first change
	})
	if err != nil {
		t.Fatalf("forEachCollectionChange: %v", err)
	}
	if visited != 1 {
		t.Fatalf("early stop should visit exactly 1 change, visited %d", visited)
	}
}

func mustDB(t *testing.T, b *Backend, name string) *dbState {
	t.Helper()
	db, err := b.getOrOpenDB(context.Background(), name, false)
	if err != nil {
		t.Fatalf("getOrOpenDB: %v", err)
	}
	return db
}
