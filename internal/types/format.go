// Copyright 2021 FerretDB Inc.
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
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/bson/primitive"
)

// FormatAnyValue formats BSON value for error message output.
func FormatAnyValue(value any) string {
	assertType(value)

	switch value := value.(type) {
	case *Document:
		return formatDocument(value)
	case *Array:
		return formatArray(value)

	case float64:
		return formatDouble(value)

	case string:
		return fmt.Sprintf(`"%v"`, value)
	case Binary:
		return fmt.Sprintf("BinData(%d, %X)", value.Subtype, value.B)
	case ObjectID:
		return fmt.Sprintf("ObjectId('%x')", value)
	case bool:
		return fmt.Sprintf("%v", value)
	case time.Time:
		return fmt.Sprintf("new Date(%d)", value.UnixMilli())
	case NullType:
		return "null"
	case UndefinedType:
		return "undefined"
	case Regex:
		return fmt.Sprintf("/%s/%s", value.Pattern, value.Options)
	case int32:
		return fmt.Sprintf("%d", value)
	case Timestamp:
		return fmt.Sprintf("Timestamp(%v, %v)", int64(value)>>32, int32(value))
	case int64:
		return fmt.Sprintf("%d", value)
	case Decimal128:
		return primitive.NewDecimal128(value.H, value.L).String()
	case MinKeyType:
		return "MinKey"
	case MaxKeyType:
		return "MaxKey"
	default:
		panic(fmt.Sprintf("unknown type %T", value))
	}
}

func formatDocument(doc *Document) string {
	doc.load()
	if len(doc.fields) == 0 {
		return "{}"
	}

	result := "{ "

	for i, f := range doc.fields {
		if i > 0 {
			result += ", "
		}

		result += fmt.Sprintf("%s: %s", f.key, FormatAnyValue(f.value))
	}

	return result + " }"
}

func formatArray(array *Array) string {
	if len(array.s) == 0 {
		return "[]"
	}

	result := "[ "

	for _, elem := range array.s {
		result += fmt.Sprintf("%s, ", FormatAnyValue(elem))
	}

	result = strings.TrimSuffix(result, ", ")

	return result + " ]"
}

// formatDouble formats f as MongoDB's StringBuilder::appendDoubleNice does:
// %.16g, then ".0" unless the text holds '.', 'E' or 'N' (so "1e+20.0",
// "nan.0" and "inf.0").
func formatDouble(f float64) string {
	s := strconv.FormatFloat(f, 'g', 16, 64)
	switch s {
	case "NaN":
		s = "nan"
	case "+Inf":
		s = "inf"
	case "-Inf":
		s = "-inf"
	}
	if !strings.ContainsAny(s, ".EN") {
		s += ".0"
	}
	return s
}
