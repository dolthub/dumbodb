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

// ResolveFilterToDocIDHashes analyzes a MongoDB update/delete filter and
// returns the set of doc id hashes the filter is guaranteed to scope to.
//
// Return values:
//   - ids: a set of hash.Hash values, the precise lock set the filter
//     will affect. Each hash matches what hashID() produces for the
//     corresponding raw _id value.
//   - fullCollection: true when the filter cannot be resolved to a small
//     exact set without scanning -- the caller must either lock the whole
//     collection or scan documents to enumerate ids precisely. Empty,
//     missing _id, range operators on _id, and any filter on non-_id
//     fields all set this.
//   - err: only set when an _id value resolves but cannot be hashed (a
//     malformed BSON value).
//
// Cases recognized as precise:
//   - {_id: <scalar>} where scalar is int32/int64/float64/string/bool/
//     ObjectID/Binary/Decimal128/time.Time
//   - {_id: {$in: [<scalar>, <scalar>, ...]}}
//
// Anything else -- {_id: {$gt: X}}, {x: 1}, {}, {$and: [...]}, missing
// _id key -- yields fullCollection=true. DocLockManager.Acquire callers
// translate "fullCollection" into a coarser lock strategy (lock every
// existing doc in the collection at acquire time, or lock at a
// collection level).
//
// Per session-isolation.md 'Open Behavioral Choices': the precise behavior
// when the matched-set changes mid-transaction is governed by parity test
// in dumbodb-parity-testing. This helper deliberately ships the simplest
// correct shape and accepts over-acquisition for non-_id-keyed filters.
func ResolveFilterToDocIDHashes(filter *types.Document) (ids []hash.Hash, fullCollection bool, err error) {
	if filter == nil || filter.Len() == 0 {
		return nil, true, nil
	}

	idVal, idErr := filter.Get("_id")
	if idErr != nil {
		// No _id field in the filter -- could match anything.
		return nil, true, nil
	}

	// Case 1: {_id: <scalar>}. The value is a non-document scalar that
	// hashID can handle directly.
	if _, isDoc := idVal.(*types.Document); !isDoc {
		h, hashErr := hashID(idVal)
		if hashErr != nil {
			return nil, false, hashErr
		}
		return []hash.Hash{hashFromArray(h)}, false, nil
	}

	// Case 2: {_id: {<op>: <val>}}. Only $in produces a precise set; every
	// other operator is treated as range-like and falls through to full-
	// collection.
	idDoc := idVal.(*types.Document)
	if idDoc.Len() != 1 {
		// Compound operators like {_id: {$gt: 1, $lt: 10}} are range-like.
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
			// $in elements should be scalar matches; a sub-document is
			// either an exact-doc match (rare for _id) or a malformed
			// query. Be conservative and over-acquire.
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

// hashFromArray converts hashID()'s [20]byte return into the hash.Hash
// type that DocLockManager expects. hash.Hash is a fixed [20]byte alias
// in store/hash; we copy rather than alias to keep the type boundary
// explicit.
func hashFromArray(b [20]byte) hash.Hash {
	var h hash.Hash
	copy(h[:], b[:])
	return h
}
