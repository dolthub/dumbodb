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

package dolt

import (
	"context"
	"encoding/hex"
	"fmt"
	"sort"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/types"
)

const (
	nsMetaTypeKey    = "type"
	nsMetaTypeView   = "view"
	viewOnKey        = "viewOn"
	viewPipelineKey  = "pipeline"
	viewCollationKey = "collation"
)

func writeViewChunk(ctx context.Context, ns tree.NodeStore, vm *viewMeta) (hash.Hash, error) {
	pipeline := vm.Pipeline
	if pipeline == nil {
		pipeline = types.MakeArray(0)
	}
	var collation any = types.Null
	if vm.Collation != nil {
		collation = vm.Collation
	}
	doc, err := types.NewDocument(
		nsMetaTypeKey, nsMetaTypeView,
		viewOnKey, vm.ViewOn,
		viewPipelineKey, pipeline,
		viewCollationKey, collation,
	)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("building view metadata document: %w", err)
	}
	stored, err := docToBSON(doc)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("encoding view metadata BSON: %w", err)
	}
	addr, err := ns.WriteBytes(ctx, stored)
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing view metadata chunk: %w", err)
	}
	return addr, nil
}

func viewMetaToBSONHex(vm *viewMeta) (string, error) {
	pipeline := vm.Pipeline
	if pipeline == nil {
		pipeline = types.MakeArray(0)
	}
	var collation any = types.Null
	if vm.Collation != nil {
		collation = vm.Collation
	}
	doc, err := types.NewDocument(
		nsMetaTypeKey, nsMetaTypeView,
		viewOnKey, vm.ViewOn,
		viewPipelineKey, pipeline,
		viewCollationKey, collation,
	)
	if err != nil {
		return "", err
	}
	stored, err := docToBSON(doc)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(stored), nil
}

func viewMetaFromBSONHex(s string) (*viewMeta, error) {
	stored, err := hex.DecodeString(s)
	if err != nil {
		return nil, err
	}
	doc, err := bsonToDoc(stored)
	if err != nil {
		return nil, err
	}
	vm := &viewMeta{}
	if v, err := doc.Get(viewOnKey); err == nil {
		vm.ViewOn, _ = v.(string)
	}
	if p, err := doc.Get(viewPipelineKey); err == nil {
		vm.Pipeline, _ = p.(*types.Array)
	}
	if c, err := doc.Get(viewCollationKey); err == nil {
		vm.Collation, _ = c.(*types.Document)
	}
	return vm, nil
}

func readViewChunk(ctx context.Context, ns tree.NodeStore, h hash.Hash) (*viewMeta, error) {
	stored, err := ns.ReadBytes(ctx, h)
	if err != nil {
		return nil, fmt.Errorf("reading view metadata chunk: %w", err)
	}
	doc, err := bsonToDoc(stored)
	if err != nil {
		return nil, fmt.Errorf("decoding view metadata BSON: %w", err)
	}
	vm := &viewMeta{}
	if v, err := doc.Get(viewOnKey); err == nil {
		vm.ViewOn, _ = v.(string)
	}
	if p, err := doc.Get(viewPipelineKey); err == nil {
		vm.Pipeline, _ = p.(*types.Array)
	}
	if c, err := doc.Get(viewCollationKey); err == nil {
		vm.Collation, _ = c.(*types.Document)
	}
	return vm, nil
}

func diffViewEntries(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, aAM, bAM prolly.AddressMap) ([]backends.ViewChange, error) {
	names := map[string]struct{}{}
	collect := func(am prolly.AddressMap) error {
		return am.IterAll(ctx, func(name string, h hash.Hash) error {
			isView, err := isViewEntry(ctx, cs, h)
			if err != nil {
				return err
			}
			if isView {
				names[name] = struct{}{}
			}
			return nil
		})
	}
	if err := collect(aAM); err != nil {
		return nil, err
	}
	if err := collect(bAM); err != nil {
		return nil, err
	}

	sorted := make([]string, 0, len(names))
	for n := range names {
		sorted = append(sorted, n)
	}
	sort.Strings(sorted)

	def := func(h hash.Hash) (*backends.ViewDefinition, error) {
		vm, err := readViewChunk(ctx, ns, h)
		if err != nil {
			return nil, err
		}
		return &backends.ViewDefinition{ViewOn: vm.ViewOn, Pipeline: vm.Pipeline}, nil
	}

	var out []backends.ViewChange
	for _, name := range sorted {
		aH, err := aAM.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		bH, err := bAM.Get(ctx, name)
		if err != nil {
			return nil, err
		}
		aIsView, err := isViewEntry(ctx, cs, aH)
		if err != nil {
			return nil, err
		}
		bIsView, err := isViewEntry(ctx, cs, bH)
		if err != nil {
			return nil, err
		}

		switch {
		case aIsView && bIsView:
			if aH == bH {
				continue
			}
			from, err := def(aH)
			if err != nil {
				return nil, err
			}
			to, err := def(bH)
			if err != nil {
				return nil, err
			}
			out = append(out, backends.ViewChange{Name: name, Status: "modified", From: from, To: to})
		case aIsView:
			from, err := def(aH)
			if err != nil {
				return nil, err
			}
			out = append(out, backends.ViewChange{Name: name, Status: "deleted", From: from})
		case bIsView:
			to, err := def(bH)
			if err != nil {
				return nil, err
			}
			out = append(out, backends.ViewChange{Name: name, Status: "added", To: to})
		}
	}
	return out, nil
}

// isViewEntry reports whether the entry at h is a view (BlobFileID chunk) rather
// than a collection (TableFileID DTBL chunk). An empty hash is not a view.
func isViewEntry(ctx context.Context, cs *nbs.GenerationalNBS, h hash.Hash) (bool, error) {
	if h.IsEmpty() {
		return false, nil
	}
	chunk, err := cs.Get(ctx, h)
	if err != nil {
		return false, fmt.Errorf("reading namespace chunk: %w", err)
	}
	return serial.GetFileID(chunk.Data()) == serial.BlobFileID, nil
}
