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

package common

import (
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// GeoNearNotAllowedMsg is MongoDB's verbatim Location5626500 message
// (including its "operationfor" typo) so the error compares equal.
const GeoNearNotAllowedMsg = "$geoNear, $near, and $nearSphere are not allowed in this context, " +
	"as these operators require sorting geospatial data. If you do not need sort, consider using " +
	"$geoWithin instead. Check out https://dochub.mongodb.org/core/near-sort-operation and " +
	"https://dochub.mongodb.org/core/nearSphere-sort-operationfor more details."

// ValidateValidatorExpressions returns the error MongoDB raises when it
// parses validator as a query for create or collMod: non-deterministic
// expressions and $where, $text and geo sorts are not allowed, and the
// query and its $expr must parse.
func ValidateValidatorExpressions(validator *types.Document) error {
	for _, name := range []string{"$rand", "$sampleRate"} {
		if containsKey(validator, name) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRandInValidator,
				name+" not allowed inside collection validators", "validator")
		}
	}
	for _, name := range []string{"$where", "$text"} {
		if containsKey(validator, name) {
			return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBadValue,
				name+" is not allowed in collection validators", "validator")
		}
	}
	if FindGeoSortKey(validator) != nil {
		return handlererrors.NewCommandErrorMsg(handlererrors.ErrGeoNearNotAllowedInContext, GeoNearNotAllowedMsg)
	}
	if err := validateFilter(validator, nil, false); err != nil {
		return err
	}
	return validateExprs(validator)
}

// validateExprs parses each $expr in filter, including under $and, $or and
// $nor.
func validateExprs(filter *types.Document) error {
	for _, key := range filter.Keys() {
		value := must.NotFail(filter.Get(key))
		switch key {
		case "$expr":
			if _, err := operators.ParseExpr(must.NotFail(types.NewDocument("$expr", value)), "validator"); err != nil {
				return err
			}
		case "$and", "$or", "$nor":
			branches, ok := value.(*types.Array)
			if !ok {
				continue
			}
			for i := range branches.Len() {
				if branch, ok := must.NotFail(branches.Get(i)).(*types.Document); ok {
					if err := validateExprs(branch); err != nil {
						return err
					}
				}
			}
		}
	}
	return nil
}

func containsKey(v any, key string) bool {
	switch v := v.(type) {
	case *types.Document:
		for _, k := range v.Keys() {
			if k == key || containsKey(must.NotFail(v.Get(k)), key) {
				return true
			}
		}
	case *types.Array:
		for i := 0; i < v.Len(); i++ {
			if containsKey(must.NotFail(v.Get(i)), key) {
				return true
			}
		}
	}
	return false
}
