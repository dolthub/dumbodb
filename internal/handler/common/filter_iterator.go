// Copyright 2021 FerretDB Inc.
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
	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// FilterIterator returns an iterator that filters out documents that don't match the filter.
// It will be added to the given closer.
//
// Next method returns the next document that matches the filter.
//
// Close method closes the underlying iterator.
func FilterIterator(iter types.DocumentsIterator, closer *iterator.MultiCloser, filter *types.Document) types.DocumentsIterator {
	return FilterIteratorColl(iter, closer, filter, nil)
}

// FilterIteratorColl is FilterIterator with an optional collation comparator
// applied to string comparisons in the filter.
func FilterIteratorColl(iter types.DocumentsIterator, closer *iterator.MultiCloser, filter *types.Document, cmp *collation.Comparator) types.DocumentsIterator {
	res := &filterIterator{
		iter:   iter,
		filter: filter,
		cmp:    cmp,
	}
	closer.Add(res)

	return res
}

type filterIterator struct {
	iter   types.DocumentsIterator
	filter *types.Document
	cmp    *collation.Comparator
}

func (iter *filterIterator) Next() (struct{}, *types.Document, error) {
	var unused struct{}

	for {
		_, doc, err := iter.iter.Next()
		if err != nil {
			return unused, nil, lazyerrors.Error(err)
		}

		matches, err := FilterDocumentColl(doc, iter.filter, iter.cmp)
		if err != nil {
			return unused, nil, lazyerrors.Error(err)
		}

		if matches {
			return unused, doc, nil
		}
	}
}

func (iter *filterIterator) Close() {
	iter.iter.Close()
}

var (
	_ types.DocumentsIterator = (*filterIterator)(nil)
)
