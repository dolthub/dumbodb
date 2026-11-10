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

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// clusteredIndexSpec is the clusteredIndex option listCollections reports for
// every collection: DumboDB clusters each collection by _id.
func clusteredIndexSpec() *types.Document {
	return must.NotFail(types.NewDocument(
		"v", int32(2),
		"key", must.NotFail(types.NewDocument("_id", int32(1))),
		"name", "_id_",
		"unique", true,
	))
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
	if v, _ := spec.Get("v"); v != nil {
		if !isNumber(v, 2) {
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
	version := must.NotFail(spec.Get("v"))
	if whole, err := handlerparams.GetWholeNumberParam(version); err == nil {
		version = int32(whole)
	}
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

// clusteredIndexHasCustomName reports whether document's clusteredIndex spec
// names its index something other than the default _id_.
func clusteredIndexHasCustomName(document *types.Document) bool {
	v, _ := document.Get("clusteredIndex")
	spec, ok := v.(*types.Document)
	if !ok {
		return false
	}
	name, _ := spec.Get("name")
	return name != nil && name != backends.DefaultIndexName
}
