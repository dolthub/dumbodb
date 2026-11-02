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
	"math"
	"math/big"
	"sort"
	"sync"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type paramMeta struct {
	defaultValue      any
	settableAtRuntime bool
	settableAtStartup bool
	// coerce converts a setParameter value to the stored type; reason is
	// non-empty when the value is rejected. Nil stores values as given.
	coerce func(v any) (coerced any, reason string)
}

// knownParameters returns the metadata table for every server parameter
// getParameter/setParameter understand. Built per call because the default
// values include freshly-allocated *types.Document and *types.Array values.
func knownParameters() map[string]paramMeta {
	mechanisms := must.NotFail(types.NewArray("MONGODB-X509", "SCRAM-SHA-1", "SCRAM-SHA-256"))
	fcv := must.NotFail(types.NewDocument("version", "8.0"))
	return map[string]paramMeta{
		"authenticationMechanisms": {defaultValue: mechanisms, settableAtRuntime: false, settableAtStartup: true},
		"authSchemaVersion":        {defaultValue: int32(5), settableAtRuntime: false, settableAtStartup: false},
		"featureCompatibilityVersion": {
			defaultValue: fcv, settableAtRuntime: false, settableAtStartup: false,
		},
		"maxTransactionLockRequestTimeoutMillis": {
			defaultValue: int32(5), settableAtRuntime: true, settableAtStartup: true, coerce: coerceInt32,
		},
		"quiet": {defaultValue: false, settableAtRuntime: true, settableAtStartup: true, coerce: coerceBool},
	}
}

// ParamSetCode is the outcome of a parameterStore.Set call.
type ParamSetCode int

const (
	ParamSetOK ParamSetCode = iota
	ParamSetUnknown
	ParamSetNotRuntime
	ParamSetBadValue
)

// parameterStore holds the current values for runtime-settable server
// parameters. Read-mostly; protected by RWMutex.
type parameterStore struct {
	mu     sync.RWMutex
	values map[string]any
}

func newParameterStore() *parameterStore {
	return &parameterStore{values: map[string]any{}}
}

// Get returns the current value of name, falling back to the default when
// the value has not been explicitly set. ok is false when name is not a
// known parameter.
func (s *parameterStore) Get(name string) (value any, ok bool) {
	meta, known := knownParameters()[name]
	if !known {
		return nil, false
	}
	s.mu.RLock()
	v, set := s.values[name]
	s.mu.RUnlock()
	if set {
		return v, true
	}
	return meta.defaultValue, true
}

// Set updates name to value, returning the previous value (or default when
// the parameter had not been explicitly set). The ParamSetCode signals to
// the caller how to map a rejection onto a wire-protocol error; reason
// explains a ParamSetBadValue.
func (s *parameterStore) Set(name string, value any) (prev any, code ParamSetCode, reason string) {
	meta, known := knownParameters()[name]
	if !known {
		return nil, ParamSetUnknown, ""
	}
	if !meta.settableAtRuntime {
		return nil, ParamSetNotRuntime, ""
	}
	if meta.coerce != nil {
		if value, reason = meta.coerce(value); reason != "" {
			return nil, ParamSetBadValue, reason
		}
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, has := s.values[name]; has {
		prev = cur
	} else {
		prev = meta.defaultValue
	}
	s.values[name] = value
	return prev, ParamSetOK, ""
}

// coerceInt32 follows mongod's BSONElement coercion to an int parameter:
// numbers truncate toward zero, booleans become 0 or 1.
func coerceInt32(v any) (any, string) {
	const outOfBounds = "Out of bounds coercing to integral value"
	switch v := v.(type) {
	case int32:
		return v, ""
	case int64:
		if v < math.MinInt32 || v > math.MaxInt32 {
			return nil, outOfBounds
		}
		return int32(v), ""
	case float64:
		if math.IsNaN(v) || math.IsInf(v, 0) {
			return nil, "Unable to coerce NaN/Inf to integral type"
		}
		t := math.Trunc(v)
		if t < math.MinInt32 || t > math.MaxInt32 {
			return nil, outOfBounds
		}
		return int32(t), ""
	case types.Decimal128:
		coeff, exp, err := bson.NewDecimal128(v.H, v.L).BigInt()
		if err != nil {
			return nil, "Unable to coerce NaN/Inf to integral type"
		}
		if exp > 0 {
			if coeff.Sign() != 0 {
				return nil, outOfBounds
			}
		} else if exp < 0 {
			coeff.Quo(coeff, new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(-exp)), nil))
		}
		if !coeff.IsInt64() || coeff.Int64() < math.MinInt32 || coeff.Int64() > math.MaxInt32 {
			return nil, outOfBounds
		}
		return int32(coeff.Int64()), ""
	case bool:
		if v {
			return int32(1), ""
		}
		return int32(0), ""
	default:
		return nil, "Unable to coerce value to integral type"
	}
}

// coerceBool stores the BSON truthiness of v.
func coerceBool(v any) (any, string) {
	switch v := v.(type) {
	case bool:
		return v, ""
	case types.NullType, types.UndefinedType:
		return false, ""
	case int32:
		return v != 0, ""
	case int64:
		return v != 0, ""
	case float64:
		return v != 0, ""
	case types.Decimal128:
		coeff, _, err := bson.NewDecimal128(v.H, v.L).BigInt()
		return err != nil || coeff.Sign() != 0, ""
	default:
		return true, ""
	}
}

// buildParameterDoc returns the getParameter response shape for every known
// parameter, using live values from store when available and falling back
// to defaults otherwise. Keys are emitted in alphabetical order to match
// MongoDB's stable output.
func buildParameterDoc(store *parameterStore) *types.Document {
	params := knownParameters()
	names := make([]string, 0, len(params))
	for name := range params {
		names = append(names, name)
	}
	sort.Strings(names)

	doc := must.NotFail(types.NewDocument())
	for _, name := range names {
		meta := params[name]
		value := meta.defaultValue
		if store != nil {
			if v, ok := store.Get(name); ok {
				value = v
			}
		}
		doc.Set(name, must.NotFail(types.NewDocument(
			"value", value,
			"settableAtRuntime", meta.settableAtRuntime,
			"settableAtStartup", meta.settableAtStartup,
		)))
	}
	return doc
}
