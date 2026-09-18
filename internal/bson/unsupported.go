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
	"fmt"

	mongobson "go.mongodb.org/mongo-driver/bson"
	"go.mongodb.org/mongo-driver/bson/bsontype"
)

func UnsupportedTypeName(raw []byte) (string, bool, error) {
	return unsupportedTypeName(mongobson.Raw(raw))
}

func unsupportedTypeName(raw mongobson.Raw) (string, bool, error) {
	elements, err := raw.Elements()
	if err != nil {
		return "", false, fmt.Errorf("reading raw BSON elements: %w", err)
	}
	for _, element := range elements {
		value := element.Value()
		if name, ok := unsupportedScalarTypeName(value.Type); ok {
			return name, true, nil
		}
		switch value.Type {
		case bsontype.EmbeddedDocument:
			if name, ok, err := unsupportedTypeName(value.Document()); err != nil || ok {
				return name, ok, err
			}
		case bsontype.Array:
			if name, ok, err := unsupportedTypeName(value.Array()); err != nil || ok {
				return name, ok, err
			}
		}
	}
	return "", false, nil
}

func unsupportedScalarTypeName(valueType bsontype.Type) (string, bool) {
	switch valueType {
	case bsontype.JavaScript:
		return "JavaScript", true
	case bsontype.CodeWithScope:
		return "JavaScriptWithScope", true
	case bsontype.Symbol:
		return "Symbol", true
	case bsontype.DBPointer:
		return "DBPointer", true
	case bsontype.Undefined:
		return "Undefined", true
	default:
		return "", false
	}
}
