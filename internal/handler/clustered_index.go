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
	"fmt"
	"math"

	"github.com/cockroachdb/apd/v3"
	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// clusteredIndexSpec is the clusteredIndex option listCollections reports for
// every collection: DumboDB clusters each collection by _id, in an index named
// name, or _id_ when name is empty.
func clusteredIndexSpec(name string) *types.Document {
	return must.NotFail(types.NewDocument(
		"v", int32(2),
		"key", must.NotFail(types.NewDocument("_id", int32(1))),
		"name", clusteredIndexName(name),
		"unique", true,
	))
}

// clusteredIndexName is a recorded clustered index name, or the default.
func clusteredIndexName(recorded string) string {
	if recorded == "" {
		return backends.DefaultIndexName
	}
	return recorded
}

// validateClusteredIndex checks a create command's clusteredIndex option as
// MongoDB does. A valid option changes nothing: every collection is clustered.
func validateClusteredIndex(document *types.Document) error {
	v, _ := document.Get("clusteredIndex")
	if v == nil {
		return nil
	}
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "create")
	}
	spec, isDoc := v.(*types.Document)
	switch v := v.(type) {
	case bool:
		if v {
			return fail(handlererrors.ErrClusteredIndexLegacyFormat,
				"The 'clusteredIndex' legacy format {clusteredIndex: <bool>} is only supported for specific internal collections and vice versa")
		}
	case *types.Document:
	default:
		return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
			"BSON field 'create.clusteredIndex' is the wrong type '%s', expected types '[bool, object]'", handlerparams.AliasFromType(v)))
	}
	if isDoc {
		if err := validateClusteredIndexSpec(spec, fail); err != nil {
			return err
		}
	}
	if viewOn, _ := document.Get("viewOn"); viewOn != nil {
		return fail(handlererrors.ErrClusteredIndexOnView, "The 'clusteredIndex' option is not supported with views")
	}
	if idIndex, _ := document.Get("idIndex"); idIndex != nil {
		return fail(handlererrors.ErrInvalidOptions, "The 'clusteredIndex' option is not supported with the 'idIndex' option")
	}
	if size, _ := document.Get("size"); size != nil {
		return fail(handlererrors.ErrClusteredIndexCappedSize,
			"'size' field for capped collections is not allowed on clustered collections. Did you mean 'capped: true' with 'expireAfterSeconds'?")
	}
	return nil
}

func validateClusteredIndexSpec(spec *types.Document, fail func(handlererrors.ErrorCode, string) error) error {
	for _, field := range spec.Keys() {
		value := must.NotFail(spec.Get(field))
		switch field {
		case "key":
			if _, ok := value.(*types.Document); !ok {
				return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field 'create.clusteredIndex.key' is the wrong type '%s', expected type 'object'", handlerparams.AliasFromType(value)))
			}
		case "unique":
			if _, ok := value.(bool); !ok {
				return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field 'create.clusteredIndex.unique' is the wrong type '%s', expected type 'bool'", handlerparams.AliasFromType(value)))
			}
		case "name":
			if _, ok := value.(string); !ok {
				return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field 'create.clusteredIndex.name' is the wrong type '%s', expected type 'string'", handlerparams.AliasFromType(value)))
			}
		case "v":
			switch value.(type) {
			case types.NullType, int32, int64, float64, types.Decimal128:
			default:
				return fail(handlererrors.ErrTypeMismatch, fmt.Sprintf(
					"BSON field 'create.clusteredIndex.v' is the wrong type '%s', expected types '[long, int, double, decimal']",
					handlerparams.AliasFromType(value)))
			}
		default:
			return fail(handlererrors.ErrIDLUnknownField, fmt.Sprintf("BSON field 'create.clusteredIndex.%s' is an unknown field.", field))
		}
	}
	for _, required := range []string{"key", "unique"} {
		if !spec.Has(required) {
			return fail(handlererrors.ErrMissingField, fmt.Sprintf(
				"BSON field 'create.clusteredIndex.%s' is missing but a required field", required))
		}
	}
	key := must.NotFail(spec.Get("key")).(*types.Document)
	if key.Len() != 1 || key.Keys()[0] != "_id" || !isNumber(must.NotFail(key.Get("_id")), 1) {
		return fail(handlererrors.ErrInvalidIndexSpecificationOption, "The clusteredIndex option is only supported for key: {_id: 1}")
	}
	if !must.NotFail(spec.Get("unique")).(bool) {
		return fail(handlererrors.ErrClusteredIndexNotUnique, "The clusteredIndex option requires unique: true to be specified")
	}
	if v, ok := clusteredIndexVersion(spec); ok {
		if v != 2 {
			return fail(handlererrors.ErrClusteredIndexVersion, fmt.Sprintf(
				"Invalid clusteredIndex specification %s; cannot create a clusteredIndex with v=%v", formatClusteredIndexSpec(spec), v))
		}
	}
	return nil
}

// formatClusteredIndexSpec renders the spec as mongod echoes it: v, key, name (default "_id_"), unique.
func formatClusteredIndexSpec(spec *types.Document) string {
	name := backends.DefaultIndexName
	if n, _ := spec.Get("name"); n != nil {
		name = n.(string)
	}
	version, _ := clusteredIndexVersion(spec)
	normalized := must.NotFail(types.NewDocument(
		"v", version,
		"key", must.NotFail(spec.Get("key")),
		"name", name,
		"unique", must.NotFail(spec.Get("unique")),
	))
	return formatDupKey(normalized)
}

func isNumber(v any, want int32) bool {
	switch n := v.(type) {
	case int32:
		return n == want
	case int64:
		return n == int64(want)
	case float64:
		return n == float64(want)
	}
	return false
}

// requestedClusteredIndexName returns the name document's clusteredIndex
// spec gives its index, _id_ by default, or "" for no clusteredIndex.
func requestedClusteredIndexName(document *types.Document) string {
	v, _ := document.Get("clusteredIndex")
	spec, ok := v.(*types.Document)
	if !ok {
		return ""
	}
	if name, _ := spec.Get("name"); name != nil {
		if s, ok := name.(string); ok {
			return s
		}
	}
	return backends.DefaultIndexName
}

// clusteredIndexVersion returns spec's v as mongod reads it into an int32,
// or false when v is unset or null: a double truncates toward zero, a
// Decimal128 rounds half to even, both saturate at the int32 bounds, and NaN
// is 0.
func clusteredIndexVersion(spec *types.Document) (int32, bool) {
	value, _ := spec.Get("v")
	switch v := value.(type) {
	case int32:
		return v, true
	case int64:
		return int32(max(min(v, math.MaxInt32), math.MinInt32)), true
	case float64:
		return saturatedInt32(math.Trunc(v)), true
	case types.Decimal128:
		d, _, err := apd.NewFromString(bson.NewDecimal128(v.H, v.L).String())
		if err != nil || d.Form == apd.NaN || d.Form == apd.NaNSignaling {
			return 0, true
		}
		if d.Form == apd.Infinite {
			if d.Negative {
				return math.MinInt32, true
			}
			return math.MaxInt32, true
		}
		var rounded apd.Decimal
		ctx := apd.BaseContext.WithPrecision(40)
		ctx.Rounding = apd.RoundHalfEven
		if _, err := ctx.RoundToIntegralValue(&rounded, d); err != nil {
			return 0, true
		}
		f, err := rounded.Float64()
		if err != nil {
			return 0, true
		}
		return saturatedInt32(f), true
	}
	return 0, false
}

// saturatedInt32 converts an integral f to int32, clamping it to the int32
// range, with NaN as 0.
func saturatedInt32(f float64) int32 {
	switch {
	case math.IsNaN(f):
		return 0
	case f >= math.MaxInt32:
		return math.MaxInt32
	case f <= math.MinInt32:
		return math.MinInt32
	}
	return int32(f)
}
