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
	"errors"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// argField is one named argument of an operator's argument object.
type argField struct {
	name string

	// set receives the value, in document order; it may validate it.
	set func(value any) error

	// missing is returned when a required field is absent; nil if optional.
	missing error
}

// argObject describes an operator's argument object as MongoDB parses it.
type argObject struct {
	notObject func(arg any) error
	unknown   func(key string) error
	fields    []argField
}

// parse checks that args is one object, passes each field to its set in
// document order, rejects unknown keys there, and then returns the first
// missing required field, in the order fields lists them.
func (a *argObject) parse(args []any) error {
	spec, ok := args[0].(*types.Document)
	if len(args) != 1 || !ok {
		return a.notObject(args[0])
	}

	seen := make([]bool, len(a.fields))
	iter := spec.Iterator()
	defer iter.Close()
	for {
		key, value, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return err
		}
		i := a.index(key)
		if i < 0 {
			return a.unknown(key)
		}
		if err = a.fields[i].set(value); err != nil {
			return err
		}
		seen[i] = true
	}

	for i, f := range a.fields {
		if !seen[i] && f.missing != nil {
			return f.missing
		}
	}
	return nil
}

func (a *argObject) index(key string) int {
	for i, f := range a.fields {
		if f.name == key {
			return i
		}
	}
	return -1
}

// assign returns a set that stores the value in dst and marks present.
func assign(dst *any, present *bool) func(any) error {
	return func(v any) error {
		*dst = v
		if present != nil {
			*present = true
		}
		return nil
	}
}
