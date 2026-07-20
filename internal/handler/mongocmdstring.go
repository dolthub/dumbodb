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

package handler

import (
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// mongoCommandString renders a command document in MongoDB's shell-style debug
// format, matching the text MongoDB embeds when it reports a command in an
// "not authorized on <db> to execute command <doc>" error.
func mongoCommandString(doc *types.Document) string {
	if doc == nil {
		return "{}"
	}
	return mongoDocString(doc)
}

func mongoDocString(doc *types.Document) string {
	if doc.Len() == 0 {
		return "{}"
	}

	var b strings.Builder
	b.WriteString("{ ")
	for i, k := range doc.Keys() {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(k)
		b.WriteString(": ")
		b.WriteString(mongoValueString(must.NotFail(doc.Get(k))))
	}
	b.WriteString(" }")
	return b.String()
}

func mongoArrayString(arr *types.Array) string {
	if arr.Len() == 0 {
		return "[]"
	}

	var b strings.Builder
	b.WriteString("[ ")
	for i := 0; i < arr.Len(); i++ {
		if i > 0 {
			b.WriteString(", ")
		}
		b.WriteString(mongoValueString(must.NotFail(arr.Get(i))))
	}
	b.WriteString(" ]")
	return b.String()
}

func mongoValueString(v any) string {
	switch x := v.(type) {
	case *types.Document:
		return mongoDocString(x)
	case *types.Array:
		return mongoArrayString(x)
	case string:
		return strconv.Quote(x)
	case bool:
		return strconv.FormatBool(x)
	case int32:
		return strconv.FormatInt(int64(x), 10)
	case int64:
		return strconv.FormatInt(x, 10)
	case float64:
		return mongoDoubleString(x)
	case types.NullType, nil:
		return "null"
	case types.Binary:
		return mongoBinaryString(x)
	case types.ObjectID:
		return fmt.Sprintf("ObjectId('%x')", x[:])
	default:
		return fmt.Sprintf("%v", v)
	}
}

// mongoDoubleString renders a double the way MongoDB does, keeping a decimal
// point on whole values (1.0, not 1) while leaving NaN/Inf and exponent forms
// untouched.
func mongoDoubleString(f float64) string {
	s := strconv.FormatFloat(f, 'g', -1, 64)
	for _, r := range s {
		if r == '.' || r == 'e' || r == 'E' || r == 'n' || r == 'N' || r == 'i' || r == 'I' {
			return s
		}
	}
	return s + ".0"
}

func mongoBinaryString(bin types.Binary) string {
	if bin.Subtype == types.BinaryUUID && len(bin.B) == 16 {
		b := bin.B
		return fmt.Sprintf("UUID(%q)", fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16]))
	}

	return fmt.Sprintf("BinData(%d, %q)", int(bin.Subtype), base64.StdEncoding.EncodeToString(bin.B))
}
