// Copyright 2024 Dolt Inc.
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

package dolt

import (
	"context"
	"fmt"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	"github.com/dolthub/dolt/go/store/val"
)

// keyDesc describes the key tuple: one Int64 field for RecordID.
var keyDesc = val.NewTupleDescriptor(val.Type{Enc: val.Int64Enc, Nullable: false})

// valDesc describes the value tuple: one BytesAddr field for the BSON blob hash.
var valDesc = val.NewTupleDescriptor(val.Type{Enc: val.BytesAddrEnc, Nullable: false})

// bufPool is a global buffer pool for building tuples.
var bufPool = pool.NewBuffPool()

// newEmptyMap creates an empty prolly.Map with our schema.
func newEmptyMap(ctx context.Context, ns tree.NodeStore) (prolly.Map, error) {
	return prolly.NewMapFromTuples(ctx, ns, keyDesc, valDesc)
}

// openMap opens a prolly.Map for a collection identified by its root hash.
func openMap(ctx context.Context, ns tree.NodeStore, rootHash hash.Hash) (prolly.Map, error) {
	node, err := ns.Read(ctx, rootHash)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("dolt: reading map node: %w", err)
	}

	return prolly.NewMap(node, ns, keyDesc, valDesc), nil
}

// buildKey creates a key tuple for a RecordID.
func buildKey(recordID int64) (val.Tuple, error) {
	tb := val.NewTupleBuilder(keyDesc, nil)
	tb.PutInt64(0, recordID)

	tup, err := tb.Build(bufPool)
	if err != nil {
		return nil, fmt.Errorf("dolt: building key tuple: %w", err)
	}

	return tup, nil
}

// buildValue creates a value tuple storing a hash to BSON bytes.
func buildValue(bsonHash hash.Hash) (val.Tuple, error) {
	tb := val.NewTupleBuilder(valDesc, nil)
	tb.PutBytesAddr(0, bsonHash)

	tup, err := tb.Build(bufPool)
	if err != nil {
		return nil, fmt.Errorf("dolt: building value tuple: %w", err)
	}

	return tup, nil
}

// updateAddressMap applies a mutation to the collections address map and
// persists it as a new dolt commit on the "heads/main" dataset.
// The caller must hold state.mu (write lock).
func (state *dbState) updateAddressMap(ctx context.Context, fn func(prolly.AddressMapEditor) error) error {
	editor := state.am.Editor()

	if err := fn(editor); err != nil {
		return err
	}

	newAM, err := editor.Flush(ctx)
	if err != nil {
		return fmt.Errorf("dolt: flushing address map: %w", err)
	}

	// Commit the updated collections AM as a new dolt commit.
	// datas.Database.Commit manages the STRT root format automatically.
	meta, err := datas.NewCommitMeta("dongo", "dongo@localhost", "update")
	if err != nil {
		return fmt.Errorf("dolt: creating commit meta: %w", err)
	}

	newDS, err := state.doltDB.Commit(ctx, state.ds, tree.ValueFromNode(newAM.Node()), datas.CommitOptions{
		Meta: meta,
	})
	if err != nil {
		return fmt.Errorf("dolt: committing collections AM: %w", err)
	}

	state.am = newAM
	state.ds = newDS

	return nil
}
