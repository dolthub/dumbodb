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

package operators

import (
	"fmt"
	"math"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// coerceToString is Value::coerceToString: numbers print as C++ streams
// print them, dates in ISO format, timestamps as "Mon DD HH:MM:SS:inc",
// nullish values as "", and other types fail with 16007.
func coerceToString(v any) (string, error) {
	switch v := v.(type) {
	case string:
		return v, nil
	case int32:
		return strconv.FormatInt(int64(v), 10), nil
	case int64:
		return strconv.FormatInt(v, 10), nil
	case float64:
		return streamDouble(v), nil
	case types.Decimal128:
		return bson.NewDecimal128(v.H, v.L).String(), nil
	case time.Time:
		return v.UTC().Format("2006-01-02T15:04:05.000Z"), nil
	case types.Timestamp:
		secs, inc := int64(uint64(v)>>32), uint32(v)
		return time.Unix(secs, 0).UTC().Format("Jan _2 15:04:05") + ":" + strconv.FormatUint(uint64(inc), 10), nil
	case nil, types.NullType, types.UndefinedType:
		return "", nil
	default:
		return "", handlererrors.NewCommandErrorMsg(handlererrors.ErrCannotConvertToString,
			fmt.Sprintf("can't convert from BSON type %s to String", handlerparams.AliasFromType(v)))
	}
}

// streamDouble formats f as a C++ ostream does by default, like printf %g.
func streamDouble(f float64) string {
	switch {
	case math.IsNaN(f):
		return "nan"
	case math.IsInf(f, 1):
		return "inf"
	case math.IsInf(f, -1):
		return "-inf"
	default:
		return strconv.FormatFloat(f, 'g', 6, 64)
	}
}

// asciiLower and asciiUpper change case as the C locale does: ASCII only.
func asciiLower(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'A' <= c && c <= 'Z' {
			b[i] = c + 'a' - 'A'
		}
	}
	return string(b)
}

func asciiUpper(s string) string {
	b := []byte(s)
	for i, c := range b {
		if 'a' <= c && c <= 'z' {
			b[i] = c - ('a' - 'A')
		}
	}
	return string(b)
}
