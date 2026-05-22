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
	"sort"
	"sync"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

type paramMeta struct {
	defaultValue      any
	settableAtRuntime bool
	settableAtStartup bool
}

// knownParameters returns the metadata table for every server parameter
// getParameter/setParameter understand. Built per call because the default
// values include freshly-allocated *types.Document and *types.Array values.
func knownParameters() map[string]paramMeta {
	mechanisms := must.NotFail(types.NewArray("SCRAM-SHA-1", "SCRAM-SHA-256"))
	fcv := must.NotFail(types.NewDocument("version", "7.0"))
	return map[string]paramMeta{
		"authenticationMechanisms":               {defaultValue: mechanisms, settableAtRuntime: false, settableAtStartup: true},
		"authSchemaVersion":                      {defaultValue: int32(5), settableAtRuntime: true, settableAtStartup: true},
		"featureCompatibilityVersion":            {defaultValue: fcv, settableAtRuntime: false, settableAtStartup: false},
		"maxTransactionLockRequestTimeoutMillis": {defaultValue: int64(5), settableAtRuntime: true, settableAtStartup: true},
		"quiet":                                  {defaultValue: false, settableAtRuntime: true, settableAtStartup: true},
	}
}

// ParamSetCode is the outcome of a parameterStore.Set call.
type ParamSetCode int

const (
	ParamSetOK ParamSetCode = iota
	ParamSetUnknown
	ParamSetNotRuntime
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
// the caller how to map a rejection onto a wire-protocol error.
func (s *parameterStore) Set(name string, value any) (prev any, code ParamSetCode) {
	meta, known := knownParameters()[name]
	if !known {
		return nil, ParamSetUnknown
	}
	if !meta.settableAtRuntime {
		return nil, ParamSetNotRuntime
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, has := s.values[name]; has {
		prev = cur
	} else {
		prev = meta.defaultValue
	}
	s.values[name] = value
	return prev, ParamSetOK
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
