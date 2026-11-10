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
	"encoding/binary"
	"errors"
	"fmt"
	"github.com/dolthub/dumbodb/internal/index"
	"log/slog"
	"sort"
	"time"

	"github.com/dolthub/dumbodb/internal/collation"
	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

//nolint:vet // for readability
type DistinctParams struct {
	DB         string          `dumbo:"$db"`
	Collection string          `dumbo:"distinct,collection"`
	Key        string          `dumbo:"key"`
	Filter     *types.Document `dumbo:"-"`
	Comment    string          `dumbo:"comment,opt"`

	Query any `dumbo:"query,opt"`

	Collation      *types.Document `dumbo:"collation,opt"`
	MaxTimeMSValue any             `dumbo:"maxTimeMS,opt"`
	MaxTimeMS      int64           `dumbo:"-"`
	Hint           any             `dumbo:"hint,opt"`

	ReadConcern    *types.Document `dumbo:"readConcern,ignored"`
	LSID           any             `dumbo:"lsid,ignored"`
	ClusterTime    any             `dumbo:"$clusterTime,ignored"`
	ReadPreference *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`
}

func GetDistinctParams(document *types.Document, l *slog.Logger) (*DistinctParams, error) {
	var dp DistinctParams

	err := handlerparams.ExtractParams(document, "distinct", &dp, l)
	if err == nil {
		dp.MaxTimeMS, err = handlerparams.CommandMaxTimeMS(dp.MaxTimeMSValue)
	}
	if err != nil {
		return nil, err
	}

	switch filter := dp.Query.(type) {
	case *types.Document:
		CanonicalizeFilter(filter)
		dp.Filter = filter
	case types.NullType, nil:
		dp.Filter = types.MakeDocument(0)
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf(
				"BSON field 'distinct.query' is the wrong type '%s', expected type 'object'",
				handlerparams.AliasFromType(dp.Query),
			),
			"distinct",
		)
	}

	if dp.Key == "" {
		return nil, handlererrors.NewCommandErrorMsg(
			handlererrors.ErrEmptyFieldPath,
			"FieldPath cannot be constructed with empty string",
		)
	}

	return &dp, nil
}

// distinctKey returns a canonical byte key for v such that two values produce
// the same key iff distinct should treat them as equal. Returns ok=false for
// composite types (Document, Array) where structural comparison is required;
// callers must fall back to a linear scan for those.
//
// The canonical form mirrors compareScalars semantics:
//   - Numbers equal in value share a key whatever their types: int32(5),
//     int64(5), 5.0 and NumberDecimal("5.00") are one value, as are 0 and -0.
//   - Strings, ObjectIDs, dates, binary, regex, timestamps, bool, null,
//     min/max keys each occupy distinct buckets via a leading tag byte.
func distinctKey(v any) ([]byte, bool) {
	switch val := v.(type) {
	case nil, types.NullType:
		return []byte{tagNull}, true

	case bool:
		if val {
			return []byte{tagBool, 1}, true
		}
		return []byte{tagBool, 0}, true

	case int32, int64, float64, types.Decimal128:
		// Numbers equal in value share a KeyString, whatever their types.
		return index.AppendValue([]byte{tagNumeric}, val), true

	case string:
		b := make([]byte, 1+len(val))
		b[0] = tagString
		copy(b[1:], val)
		return b, true

	case types.ObjectID:
		b := make([]byte, 1+types.ObjectIDLen)
		b[0] = tagObjectID
		copy(b[1:], val[:])
		return b, true

	case time.Time:
		b := make([]byte, 1+8)
		b[0] = tagDate
		binary.BigEndian.PutUint64(b[1:], uint64(val.UnixMilli()))
		return b, true

	case types.Binary:
		b := make([]byte, 2+len(val.B))
		b[0] = tagBinary
		b[1] = byte(val.Subtype)
		copy(b[2:], val.B)
		return b, true

	case types.Timestamp:
		b := make([]byte, 1+8)
		b[0] = tagTimestamp
		binary.BigEndian.PutUint64(b[1:], uint64(val))
		return b, true

	case types.Regex:
		b := make([]byte, 0, 2+len(val.Pattern)+len(val.Options))
		b = append(b, tagRegex)
		b = append(b, val.Pattern...)
		b = append(b, 0x00)
		b = append(b, val.Options...)
		return b, true

	case types.MinKeyType:
		return []byte{tagMinKey}, true

	case types.UndefinedType:
		return []byte{tagUndefined}, true

	case types.MaxKeyType:
		return []byte{tagMaxKey}, true

	}

	return nil, false
}

// Tag bytes that prefix each distinctKey, ensuring values from different type
// buckets never collide.
const (
	tagNull      byte = 0x00
	tagBool      byte = 0x01
	tagNumeric   byte = 0x02
	tagString    byte = 0x03
	tagObjectID  byte = 0x04
	tagDate      byte = 0x05
	tagBinary    byte = 0x06
	tagTimestamp byte = 0x07
	tagRegex     byte = 0x08
	tagMinKey    byte = 0x09
	tagMaxKey    byte = 0x0a
	tagUndefined byte = 0x0c
)

// FilterDistinctValues returns distinct values from the given slice of documents with the given key.
//
// If the key is not found in the document, the document is ignored.
//
// If the key is found in the document, and the value is an array, each element of the array is added to the result.
// Otherwise, the value itself is added to the result.
func FilterDistinctValues(iter types.DocumentsIterator, key string, cmp *collation.Comparator) (*types.Array, error) {
	defer iter.Close()

	dedup := newDistinctSet()
	dedup.cmp = cmp

	path, err := types.NewPathFromString(key)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		// distinct using dot notation returns the value by valid array index
		// or values for the given key in array's document
		vals, err := commonpath.FindValues(doc, path, &commonpath.FindValuesOpts{
			FindArrayIndex:     true,
			FindArrayDocuments: true,
		})
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		for _, val := range vals {
			switch v := val.(type) {
			case *types.Array:
				for i := 0; i < v.Len(); i++ {
					el, err := v.Get(i)
					if err != nil {
						return nil, lazyerrors.Error(err)
					}
					dedup.add(el)
				}

			default:
				dedup.add(v)
			}
		}
	}

	distinct := dedup.array()
	sortDistinctValues(distinct)

	return distinct, nil
}

// IndexKeyDistinctValues returns the distinct values MongoDB reports when it
// answers distinct from an index on key (DISTINCT_SCAN): every document's index
// keys, so a missing field yields null and an empty array yields undefined. A
// sparse index omits documents in which the path reaches no value.
func IndexKeyDistinctValues(iter types.DocumentsIterator, key string, sparse bool) (*types.Array, error) {
	defer iter.Close()

	path, err := types.NewPathFromString(key)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var keys []any
	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}
		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		if sparse {
			vals, err := commonpath.FindValues(doc, path, &commonpath.FindValuesOpts{
				FindArrayIndex:     true,
				FindArrayDocuments: true,
			})
			if err != nil {
				return nil, lazyerrors.Error(err)
			}
			if len(vals) == 0 {
				continue
			}
		}

		keys = append(keys, commonpath.IndexKeyValues(doc, path)...)
	}

	return DedupDistinctValues(keys)
}

// DedupDistinctValues deduplicates a slice of pre-extracted field values and
// returns them sorted ascending. Used by the backend DistinctScanner fast path,
// where the backend has already iterated documents and emits raw field values.
//
// Array values are flattened element-wise (matching FilterDistinctValues).
func DedupDistinctValues(values []any) (*types.Array, error) {
	dedup := newDistinctSet()

	for _, v := range values {
		switch a := v.(type) {
		case *types.Array:
			for i := 0; i < a.Len(); i++ {
				el, err := a.Get(i)
				if err != nil {
					return nil, lazyerrors.Error(err)
				}
				dedup.add(el)
			}
		default:
			dedup.add(v)
		}
	}

	out := dedup.array()
	sortDistinctValues(out)
	return out, nil
}

// distinctSet collects unique BSON values in insertion order.
//
// Hashable scalars are deduped through a map keyed on distinctKey; composites
// (Document, Array) and any other unkeyable types fall back to linear scan
// against a smaller "complex" slice. The fast path turns the previous O(n^2)
// distinct accumulator into O(n) for the common case of scalar fields.
type distinctSet struct {
	seen    map[string]struct{}
	complex []any
	values  []any
	// cmp, when set, makes strings equal under the collation duplicates.
	cmp *collation.Comparator
	// scratch holds the key of the value being added.
	scratch []byte
}

func newDistinctSet() *distinctSet {
	return &distinctSet{seen: make(map[string]struct{})}
}

// add appends v to the set if not already present.
func (s *distinctSet) add(v any) {
	if str, isString := v.(string); isString && s.cmp != nil {
		s.addKeyed(append([]byte{tagString}, s.cmp.Key(str)...), v)
		return
	}
	// Scalars most often repeat: key them in a reused buffer, so a value
	// already seen allocates nothing.
	switch val := v.(type) {
	case int32, int64, float64, types.Decimal128:
		s.scratch = index.AppendValue(append(s.scratch[:0], tagNumeric), val)
		s.addKeyed(s.scratch, v)
		return
	case string:
		s.scratch = append(append(s.scratch[:0], tagString), val...)
		s.addKeyed(s.scratch, v)
		return
	}
	if k, ok := distinctKey(v); ok {
		s.addKeyed(k, v)
		return
	}

	// Composite / unkeyable: fall back to structural compare against the
	// (typically small) set of composite values already collected.
	for _, c := range s.complex {
		if types.Compare(c, v) == types.Equal {
			return
		}
	}
	s.complex = append(s.complex, v)
	s.values = append(s.values, v)
}

func (s *distinctSet) addKeyed(key []byte, v any) {
	if _, dup := s.seen[string(key)]; dup {
		return
	}
	s.seen[string(key)] = struct{}{}
	s.values = append(s.values, v)
}

// array returns an unsorted *types.Array containing every unique value the
// set has accepted, in insertion order.
func (s *distinctSet) array() *types.Array {
	arr := types.MakeArray(len(s.values))
	for _, v := range s.values {
		arr.Append(v)
	}
	return arr
}

// sortDistinctValues orders values canonically like MongoDB's distinct: by BSON
// type, then value, comparing arrays element-wise rather than by their minimum
// element as a sort does.
func sortDistinctValues(arr *types.Array) {
	vals := make([]any, arr.Len())
	for i := range vals {
		vals[i] = must.NotFail(arr.Get(i))
	}
	sort.SliceStable(vals, func(i, j int) bool {
		return types.CompareForAggregation(vals[i], vals[j]) == types.Less
	})
	for i, v := range vals {
		must.NoError(arr.Set(i, v))
	}
}
