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

package index

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"runtime/debug"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/val"
)

// MaxTreeLevels is the length of Dolt's table of per-level chunk boundary
// salts. A write that needs a level past it panics with an index out of range.
// The writer can need that level briefly while finishing a tree of
// MaxTreeLevels-1 levels, so trees top out at 14 or 15 levels.
const MaxTreeLevels = 15

// ErrTreeTooDeep reports a write that would grow a prolly tree past
// MaxTreeLevels. It happens only when the tree's keys are long on average.
var ErrTreeTooDeep = errors.New("collection or index too large for its key size")

// TreePut is mut.Put returning ErrTreeTooDeep where Dolt would panic. Put
// applies pending edits to the tree once enough accumulate.
func TreePut(ctx context.Context, mut *prolly.MutableMap, key, value val.Tuple) (err error) {
	defer recoverTreeTooDeep(&err)
	return mut.Put(ctx, key, value)
}

// TreeFlush is mut.Map returning ErrTreeTooDeep where Dolt would panic.
func TreeFlush(ctx context.Context, mut *prolly.MutableMap) (m prolly.Map, err error) {
	defer recoverTreeTooDeep(&err)
	return mut.Map(ctx)
}

// TreeMutateWithIter is prolly.MutateMapWithTupleIter returning
// ErrTreeTooDeep where Dolt would panic.
func TreeMutateWithIter(ctx context.Context, m prolly.Map, iter prolly.TupleIter) (out prolly.Map, err error) {
	defer recoverTreeTooDeep(&err)
	return prolly.MutateMapWithTupleIter(ctx, m, iter)
}

// recoverTreeTooDeep converts Dolt's panic for a tree deeper than
// MaxTreeLevels into ErrTreeTooDeep and re-panics on any other panic. Dolt
// gives the panic no type of its own, so it is recognized by the frame that
// raises it.
func recoverTreeTooDeep(err *error) {
	p := recover()
	if p == nil {
		return
	}
	if bytes.Contains(debug.Stack(), []byte("prolly/tree.newKeySplitter(")) {
		*err = fmt.Errorf(
			"%w: its tree would pass %d levels; its keys are too long on average",
			ErrTreeTooDeep, MaxTreeLevels,
		)
		return
	}
	panic(p)
}
