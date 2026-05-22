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
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/types"
)

func ResolveFilterToDocIDHashes(filter *types.Document) (ids []hash.Hash, fullCollection bool, err error) {
	if filter == nil || filter.Len() == 0 {
		return nil, true, nil
	}

	idVal, idErr := filter.Get("_id")
	if idErr != nil {
		return nil, true, nil
	}

	if _, isDoc := idVal.(*types.Document); !isDoc {
		h, hashErr := hashID(idVal)
		if hashErr != nil {
			return nil, false, hashErr
		}
		return []hash.Hash{hashFromArray(h)}, false, nil
	}

	idDoc := idVal.(*types.Document)
	if idDoc.Len() != 1 {
		return nil, true, nil
	}
	op, _ := idDoc.Get("$in")
	if op == nil {
		return nil, true, nil
	}
	arr, ok := op.(*types.Array)
	if !ok {
		return nil, true, nil
	}

	ids = make([]hash.Hash, 0, arr.Len())
	for i := 0; i < arr.Len(); i++ {
		v, getErr := arr.Get(i)
		if getErr != nil {
			return nil, false, getErr
		}
		if _, isDoc := v.(*types.Document); isDoc {
			return nil, true, nil
		}
		h, hashErr := hashID(v)
		if hashErr != nil {
			return nil, false, hashErr
		}
		ids = append(ids, hashFromArray(h))
	}
	return ids, false, nil
}

func hashFromArray(b [20]byte) hash.Hash {
	var h hash.Hash
	copy(h[:], b[:])
	return h
}
