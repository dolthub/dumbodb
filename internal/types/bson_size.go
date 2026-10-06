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

package types

import (
	"fmt"
	"strconv"
	"time"
)

// BSONSize returns the length of the document's BSON encoding without encoding it.
func (d *Document) BSONSize() int {
	size := 5
	for _, f := range d.fields {
		size += 1 + len(f.key) + 1 + bsonValueSize(f.value)
	}
	return size
}

func (a *Array) bsonSize() int {
	size := 5
	for i, v := range a.s {
		size += 1 + len(strconv.Itoa(i)) + 1 + bsonValueSize(v)
	}
	return size
}

func bsonValueSize(v any) int {
	switch v := v.(type) {
	case *Document:
		return v.BSONSize()
	case *Array:
		return v.bsonSize()
	case float64, time.Time, Timestamp, int64:
		return 8
	case string:
		return 4 + len(v) + 1
	case Binary:
		return 4 + 1 + len(v.B)
	case ObjectID:
		return ObjectIDLen
	case bool:
		return 1
	case NullType, MinKeyType, MaxKeyType:
		return 0
	case Regex:
		return len(v.Pattern) + 1 + len(v.Options) + 1
	case int32:
		return 4
	case Decimal128:
		return 16
	default:
		panic(fmt.Sprintf("invalid BSON value type %T", v))
	}
}
