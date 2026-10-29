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
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// ValidateValidatorExpressions returns the error MongoDB raises for an
// expression a collection validator may not use: $rand, which is
// non-deterministic.
func ValidateValidatorExpressions(validator *types.Document) error {
	if containsKey(validator, "$rand") {
		return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrRandInValidator,
			"$rand not allowed inside collection validators", "validator")
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
