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

package bson

import (
	"testing"

	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

func TestUnsupportedTypeName(t *testing.T) {
	tests := []struct {
		name  string
		value any
		want  string
	}{
		{name: "javascript", value: primitive.JavaScript("return 1"), want: "JavaScript"},
		{name: "javascript with scope", value: primitive.CodeWithScope{Code: "return x", Scope: mongobson.D{{Key: "x", Value: int32(1)}}}, want: "JavaScriptWithScope"},
		{name: "symbol", value: primitive.Symbol("symbol"), want: "Symbol"},
		{name: "db pointer", value: primitive.DBPointer{DB: "archive.items", Pointer: primitive.NewObjectID()}, want: "DBPointer"},
		{name: "undefined", value: primitive.Undefined{}, want: "Undefined"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			raw, err := mongobson.Marshal(mongobson.D{{Key: "nested", Value: mongobson.A{mongobson.D{{Key: "value", Value: test.value}}}}})
			if err != nil {
				t.Fatal(err)
			}
			name, ok, err := UnsupportedTypeName(raw)
			if err != nil {
				t.Fatal(err)
			}
			if !ok || name != test.want {
				t.Fatalf("unsupported type = %q, %v, want %q", name, ok, test.want)
			}
		})
	}
}

func TestUnsupportedTypeNameAcceptsRepresentedTypes(t *testing.T) {
	raw, err := mongobson.Marshal(mongobson.D{
		{Key: "minimum", Value: primitive.MinKey{}},
		{Key: "maximum", Value: primitive.MaxKey{}},
		{Key: "nested", Value: mongobson.D{{Key: "value", Value: int32(1)}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	name, ok, err := UnsupportedTypeName(raw)
	if err != nil {
		t.Fatal(err)
	}
	if ok || name != "" {
		t.Fatalf("represented BSON reported as unsupported: %q", name)
	}
}
