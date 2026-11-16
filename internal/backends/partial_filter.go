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

package backends

import (
	"fmt"
	"log/slog"
	"strings"
	"sync/atomic"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
)

// partialFilterMatcher holds the function used to evaluate a partial-index
// filter expression against a document. The handler layer registers
// common.FilterDocument here at process init so that the backend can rebuild
// IndexInfo.MatchesPartialFilter closures (e.g. after restart from disk)
// without importing handler/common (which would create a circular dependency).
//
// The registered function takes (doc, filter, cmp)  -- same order as
// common.FilterDocumentColl  -- so it can be passed by value without a wrapper.
var partialFilterMatcher atomic.Pointer[func(doc, filter *types.Document, cmp *collation.Comparator) (bool, error)]

// RegisterPartialFilterMatcher installs fn as the global predicate used to
// evaluate partial-index filter expressions. The handler layer must call this
// once at process init before any backend opens existing data.
func RegisterPartialFilterMatcher(fn func(doc, filter *types.Document, cmp *collation.Comparator) (bool, error)) {
	partialFilterMatcher.Store(&fn)
}

// MatchPartialFilter reports whether doc satisfies filter under the simple
// collation, using the registered predicate. It returns an error if no
// predicate has been registered, which indicates a misconfigured binary
// (handler/common was not linked in).
func MatchPartialFilter(doc, filter *types.Document) (bool, error) {
	return matchPartialFilterColl(doc, filter, nil)
}

func matchPartialFilterColl(doc, filter *types.Document, cmp *collation.Comparator) (bool, error) {
	fn := partialFilterMatcher.Load()
	if fn == nil {
		return false, fmt.Errorf("backends: partial filter matcher not registered")
	}
	return (*fn)(doc, filter, cmp)
}

// filterCanonicalizer and partialFilterCompiler are set once by the handler
// layer's init, which Go runs before any reader.
var (
	filterCanonicalizer   func(filter *types.Document) *types.Document
	partialFilterCompiler func(pfe *types.Document, cmp *collation.Comparator) func(doc *types.Document) (bool, error)
)

// RegisterFilterCanonicalizer installs fn, which returns a copy of a filter
// whose document values have the sorted-key form documents are stored in.
// The handler layer must call this once at process init.
func RegisterFilterCanonicalizer(fn func(filter *types.Document) *types.Document) {
	filterCanonicalizer = fn
}

// RegisterPartialFilterCompiler installs fn, which returns the membership
// predicate of a partial filter, matching documents whether or not their
// keys are sorted yet. The handler layer must call this once at process init.
func RegisterPartialFilterCompiler(fn func(pfe *types.Document, cmp *collation.Comparator) func(doc *types.Document) (bool, error)) {
	partialFilterCompiler = fn
}

// CanonicalPartialFilter returns pfe in the form that matches stored
// documents, leaving pfe itself as written for listIndexes. It returns an
// error if no canonicalizer has been registered.
func CanonicalPartialFilter(pfe *types.Document) (*types.Document, error) {
	if filterCanonicalizer == nil {
		return nil, fmt.Errorf("backends: partial filter canonicalizer not registered")
	}
	if pfe == nil {
		return nil, nil
	}
	return filterCanonicalizer(pfe), nil
}

// PartialFilterMembership returns IndexInfo.MatchesPartialFilter for a partial
// index with filter expression pfe and collation indexCollation: as in mongod,
// the expression is evaluated under the index's collation.
func PartialFilterMembership(pfe, indexCollation *types.Document) func(doc *types.Document) (bool, error) {
	if partialFilterCompiler == nil {
		return func(*types.Document) (bool, error) {
			return false, fmt.Errorf("backends: partial filter compiler not registered")
		}
	}
	return partialFilterCompiler(pfe, collation.Parse(indexCollation).Comparator())
}

// StalePartialFilterMembership is IndexInfo.MatchesPartialFilter for the stale
// partial index name; it logs the index once.
func StalePartialFilterMembership(name string, pfe *types.Document) func(doc *types.Document) (bool, error) {
	slog.Warn("partial index built by an older version; writes to its collection fail until it is dropped and recreated",
		"index", name, "partialFilterExpression", types.FormatAnyValue(pfe))
	return func(*types.Document) (bool, error) {
		return false, StalePartialIndexError(name)
	}
}

// StalePartialIndexError returns index.ErrStalePartialIndex for the index name.
func StalePartialIndexError(name string) error {
	return fmt.Errorf("%w: %q", index.ErrStalePartialIndex, name)
}

// FilterComparesDocuments reports whether a filter compares a field with a
// document value, at any depth.
func FilterComparesDocuments(v any) bool {
	switch v := v.(type) {
	case *types.Document:
		for _, key := range v.Keys() {
			value, _ := v.Get(key)
			if d, ok := value.(*types.Document); ok && !strings.HasPrefix(key, "$") && !isOperatorDocument(d) {
				return true
			}
			switch key {
			case "$eq", "$ne", "$gt", "$gte", "$lt", "$lte":
				if _, ok := value.(*types.Document); ok {
					return true
				}
			}
			if FilterComparesDocuments(value) {
				return true
			}
		}
	case *types.Array:
		for i := range v.Len() {
			elem, _ := v.Get(i)
			if _, ok := elem.(*types.Document); ok {
				return true
			}
			if FilterComparesDocuments(elem) {
				return true
			}
		}
	}
	return false
}

func isOperatorDocument(doc *types.Document) bool {
	return doc.Len() > 0 && strings.HasPrefix(doc.Keys()[0], "$")
}
