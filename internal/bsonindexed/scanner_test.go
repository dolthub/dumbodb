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

package bsonindexed

import (
	"io"
	"testing"

	"github.com/FerretDB/wire/wirebson"
)

func encodeBSON(t *testing.T, fields ...any) []byte {
	t.Helper()
	if len(fields)%2 != 0 {
		t.Fatalf("encodeBSON: odd number of args")
	}
	doc := wirebson.MakeDocument(len(fields) / 2)
	for i := 0; i < len(fields); i += 2 {
		key, ok := fields[i].(string)
		if !ok {
			t.Fatalf("encodeBSON: field[%d] key is not a string", i)
		}
		if err := doc.Add(key, fields[i+1]); err != nil {
			t.Fatalf("encodeBSON: doc.Add(%q): %v", key, err)
		}
	}
	raw, err := doc.Encode()
	if err != nil {
		t.Fatalf("encodeBSON: doc.Encode: %v", err)
	}
	return []byte(raw)
}

func scanAll(t *testing.T, buf []byte) []scanEvent {
	t.Helper()
	s := NewScanner(buf)
	var events []scanEvent
	for {
		err := s.AdvanceToNextLocation()
		if err == io.EOF {
			return events
		}
		if err != nil {
			t.Fatalf("scanner: %v", err)
		}
		events = append(events, scanEvent{
			state: s.Path().State(),
			path:  s.Path().ToMongoPath(),
		})
	}
}

type scanEvent struct {
	state PathType
	path  string
}

func TestScannerEmptyDoc(t *testing.T) {
	buf := encodeBSON(t)
	events := scanAll(t, buf)
	if len(events) != 2 {
		t.Fatalf("got %d events, want 2 (start+end). events: %+v", len(events), events)
	}
	// Empty doc may emit ObjectInitialElement->EndOfValue or skip to
	// EndOfValue directly via openRoot's fast path.
	if events[0].state != ObjectInitialElement && events[0].state != EndOfValue {
		t.Errorf("event[0].state = %v; want ObjectInitialElement or EndOfValue", events[0].state)
	}
}

func TestScannerSingleScalarField(t *testing.T) {
	buf := encodeBSON(t, "a", int32(7))
	events := scanAll(t, buf)
	want := []scanEvent{
		{ObjectInitialElement, ""},
		{StartOfValue, "a"},
		{EndOfValue, "a"},
		{EndOfValue, ""},
	}
	if !eventsEqual(events, want) {
		t.Errorf("events:\n  got  %+v\n  want %+v", events, want)
	}
}

func TestScannerNestedObject(t *testing.T) {
	inner, err := wirebson.NewDocument("x", int32(1), "y", int32(2))
	if err != nil {
		t.Fatalf("inner: %v", err)
	}
	buf := encodeBSON(t, "a", inner, "b", "hello")
	events := scanAll(t, buf)
	want := []scanEvent{
		{ObjectInitialElement, ""},
		{StartOfValue, "a"},
		{ObjectInitialElement, "a"},
		{StartOfValue, "a.x"},
		{EndOfValue, "a.x"},
		{StartOfValue, "a.y"},
		{EndOfValue, "a.y"},
		{EndOfValue, "a"},
		{StartOfValue, "b"},
		{EndOfValue, "b"},
		{EndOfValue, ""},
	}
	if !eventsEqual(events, want) {
		t.Errorf("events:\n  got  %+v\n  want %+v", events, want)
	}
}

func TestScannerArray(t *testing.T) {
	arr, err := wirebson.NewArray(int32(10), int32(20), int32(30))
	if err != nil {
		t.Fatalf("arr: %v", err)
	}
	buf := encodeBSON(t, "items", arr)
	events := scanAll(t, buf)
	want := []scanEvent{
		{ObjectInitialElement, ""},
		{StartOfValue, "items"},
		{ArrayInitialElement, "items"},
		{StartOfValue, "items.0"},
		{EndOfValue, "items.0"},
		{StartOfValue, "items.1"},
		{EndOfValue, "items.1"},
		{StartOfValue, "items.2"},
		{EndOfValue, "items.2"},
		{EndOfValue, "items"},
		{EndOfValue, ""},
	}
	if !eventsEqual(events, want) {
		t.Errorf("events:\n  got  %+v\n  want %+v", events, want)
	}
}

func TestScannerStringValue(t *testing.T) {
	buf := encodeBSON(t, "msg", "hello, world")
	events := scanAll(t, buf)
	want := []scanEvent{
		{ObjectInitialElement, ""},
		{StartOfValue, "msg"},
		{EndOfValue, "msg"},
		{EndOfValue, ""},
	}
	if !eventsEqual(events, want) {
		t.Errorf("events:\n  got  %+v\n  want %+v", events, want)
	}
}

func TestScannerMixedTypes(t *testing.T) {
	buf := encodeBSON(t,
		"i32", int32(1),
		"i64", int64(2),
		"f64", float64(3.14),
		"str", "x",
		"b", true,
		"n", wirebson.Null,
	)
	events := scanAll(t, buf)
	if len(events) != 14 {
		t.Errorf("event count = %d; want 14. events: %+v", len(events), events)
	}
	seen := []string{}
	in := map[string]bool{}
	for _, e := range events {
		if !in[e.path] {
			in[e.path] = true
			seen = append(seen, e.path)
		}
	}
	want := []string{"", "i32", "i64", "f64", "str", "b", "n"}
	if !stringSlicesEqual(seen, want) {
		t.Errorf("path order:\n  got  %v\n  want %v", seen, want)
	}
}

func eventsEqual(a, b []scanEvent) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func stringSlicesEqual(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
