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
	"errors"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// maxTopKBuffer bounds the working set TopKIterator will hold. Above it the
// bounded buffer no longer saves memory, so a full sort is used instead.
const maxTopKBuffer = 1 << 20

// TopKIterator returns the first k documents of a stable sort by sortDoc, in
// sorted order, while holding at most ~2k documents in memory rather than the
// whole input. It is equivalent to SortIterator followed by a limit of k,
// including ties: among equal sort keys earlier input documents win, because
// retained documents are always re-sorted ahead of later arrivals. k <= 0 (or a
// k too large to bound usefully) falls back to a full sort.
func TopKIterator(iter types.DocumentsIterator, closer *iterator.MultiCloser, sortDoc *types.Document, k int64) (types.DocumentsIterator, error) { //nolint:lll // for readability
	if k <= 0 || k > maxTopKBuffer || sortDoc.Len() == 0 {
		return SortIterator(iter, closer, sortDoc)
	}

	defer iter.Close()

	trimAt := 2 * k

	initCap := trimAt
	if initCap > 4096 {
		initCap = 4096
	}

	buf := make([]*types.Document, 0, initCap)

	for {
		_, doc, err := iter.Next()
		if err != nil {
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}

			return nil, lazyerrors.Error(err)
		}

		buf = append(buf, doc)

		if int64(len(buf)) >= trimAt {
			if err := SortDocuments(buf, sortDoc); err != nil {
				return nil, lazyerrors.Error(err)
			}

			buf = trimTopK(buf, k)
		}
	}

	if err := SortDocuments(buf, sortDoc); err != nil {
		return nil, lazyerrors.Error(err)
	}

	if int64(len(buf)) > k {
		buf = trimTopK(buf, k)
	}

	res := iterator.Values(iterator.ForSlice(buf))
	closer.Add(res)

	return res, nil
}

// trimTopK keeps the first k documents and releases references to the rest so
// they can be collected, reusing the backing array.
func trimTopK(buf []*types.Document, k int64) []*types.Document {
	for i := k; i < int64(len(buf)); i++ {
		buf[i] = nil
	}

	return buf[:k]
}
