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

	lru "github.com/hashicorp/golang-lru/v2"

	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
)

var tableChunkCache, _ = lru.New[hash.Hash, []byte](4096)

// readTableChunk returns the data of the chunk at h, which is empty when the
// chunk is missing. Chunks are immutable, so their data is cached by address;
// callers must not modify it.
func readTableChunk(ctx context.Context, cs *nbs.GenerationalNBS, h hash.Hash) ([]byte, error) {
	if data, ok := tableChunkCache.Get(h); ok {
		return data, nil
	}
	chunk, err := cs.Get(ctx, h)
	if err != nil {
		return nil, err
	}
	data := chunk.Data()
	if len(data) > 0 {
		tableChunkCache.Add(h, data)
	}
	return data, nil
}
