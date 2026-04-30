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
	"log/slog"
	"math"
	"strconv"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/dolthub/dumbodb/internal/handler/commonpath"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// DistinctParams contains `distinct` command parameters supported by at least one handler.
//
//nolint:vet // for readability
type DistinctParams struct {
	DB         string          `dumbo:"$db"`
	Collection string          `dumbo:"distinct,collection"`
	Key        string          `dumbo:"key"`
	Filter     *types.Document `dumbo:"-"`
	Comment    string          `dumbo:"comment,opt"`

	Query any `dumbo:"query,opt"`

	Collation *types.Document `dumbo:"collation,unimplemented"`

	ReadConcern    *types.Document `dumbo:"readConcern,ignored"`
	LSID           any             `dumbo:"lsid,ignored"`
	ClusterTime    any             `dumbo:"$clusterTime,ignored"`
	ReadPreference *types.Document `dumbo:"$readPreference,ignored"`

	ApiVersion           string `dumbo:"apiVersion,ignored"`
	ApiStrict            bool   `dumbo:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `dumbo:"apiDeprecationErrors,ignored"`
}

// GetDistinctParams returns `distinct` command parameters.
func GetDistinctParams(document *types.Document, l *slog.Logger) (*DistinctParams, error) {
	var dp DistinctParams

	err := handlerparams.ExtractParams(document, "distinct", &dp, l)
	if err != nil {
		return nil, err
	}

	switch filter := dp.Query.(type) {
	case *types.Document:
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

// decimal128ToFloat64 converts a Decimal128 to float64 via its string representation.
// Returns (value, true) on success, or (0, false) for NaN/Infinity or parse errors.
func decimal128ToFloat64(d types.Decimal128) (float64, bool) {
	s := bson.NewDecimal128(d.H, d.L).String()
	switch s {
	case "NaN", "Infinity", "-Infinity":
		return 0, false
	}
	f, err := strconv.ParseFloat(s, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// distinctKey returns a canonical byte key for v such that two values produce
// the same key iff distinct should treat them as equal. Returns ok=false for
// composite types (Document, Array) where structural comparison is required;
// callers must fall back to a linear scan for those.
//
// The canonical form mirrors compareScalars semantics:
//   - int32(5), int64(5), float64(5.0) all hash to the same key (numeric
//     equality across types).
//   - Decimal128 hashes by numeric value when convertible to float64; NaN /
//     Infinity fall back to bit-level equality. Decimal128 only collides with
//     other Decimal128s — matching the prior distinctContains carve-out.
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

	case int32:
		return numericIntKey(int64(val)), true

	case int64:
		return numericIntKey(val), true

	case float64:
		if math.IsNaN(val) {
			return []byte{tagNumeric, 'N', 'a', 'N'}, true
		}
		if !math.IsInf(val, 0) && val == math.Trunc(val) && val >= math.MinInt64 && val <= math.MaxInt64 {
			return numericIntKey(int64(val)), true
		}
		return numericFloatKey(val), true

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

	case types.MaxKeyType:
		return []byte{tagMaxKey}, true

	case types.Decimal128:
		// Decimal128 only collides with other Decimal128s. Within that bucket,
		// equality is by numeric value when convertible; NaN / Infinity fall
		// back to bit-level equality.
		b := make([]byte, 1+16)
		b[0] = tagDecimal128
		if f, ok := decimal128ToFloat64(val); ok {
			binary.BigEndian.PutUint64(b[1:9], math.Float64bits(f))
			// remaining 8 bytes left zero — convertible decimals share key
		} else {
			binary.BigEndian.PutUint64(b[1:9], val.H)
			binary.BigEndian.PutUint64(b[9:17], val.L)
		}
		return b, true
	}

	return nil, false
}

// Tag bytes that prefix each distinctKey, ensuring values from different type
// buckets never collide.
const (
	tagNull       byte = 0x00
	tagBool       byte = 0x01
	tagNumeric    byte = 0x02
	tagString     byte = 0x03
	tagObjectID   byte = 0x04
	tagDate       byte = 0x05
	tagBinary     byte = 0x06
	tagTimestamp  byte = 0x07
	tagRegex      byte = 0x08
	tagMinKey     byte = 0x09
	tagMaxKey     byte = 0x0a
	tagDecimal128 byte = 0x0b
)

// numericIntKey encodes an integer in the numeric bucket. int32(n), int64(n),
// and float64(n) (when n is exactly integer-valued) all hash to this form.
func numericIntKey(n int64) []byte {
	b := make([]byte, 1+1+8)
	b[0] = tagNumeric
	b[1] = 'i'
	binary.BigEndian.PutUint64(b[2:], uint64(n))
	return b
}

// numericFloatKey encodes a non-integer float in the numeric bucket.
func numericFloatKey(f float64) []byte {
	b := make([]byte, 1+1+8)
	b[0] = tagNumeric
	b[1] = 'f'
	binary.BigEndian.PutUint64(b[2:], math.Float64bits(f))
	return b
}

// FilterDistinctValues returns distinct values from the given slice of documents with the given key.
//
// If the key is not found in the document, the document is ignored.
//
// If the key is found in the document, and the value is an array, each element of the array is added to the result.
// Otherwise, the value itself is added to the result.
func FilterDistinctValues(iter types.DocumentsIterator, key string) (*types.Array, error) {
	defer iter.Close()

	dedup := newDistinctSet()

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
	SortArray(distinct, types.Ascending)

	return distinct, nil
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
	SortArray(out, types.Ascending)
	return out, nil
}

// distinctSet collects unique BSON values in insertion order.
//
// Hashable scalars are deduped through a map keyed on distinctKey; composites
// (Document, Array) and any other unkeyable types fall back to linear scan
// against a smaller "complex" slice. The fast path turns the previous O(n²)
// distinct accumulator into O(n) for the common case of scalar fields.
type distinctSet struct {
	seen     map[string]struct{}
	complex  []any
	values   []any
}

func newDistinctSet() *distinctSet {
	return &distinctSet{seen: make(map[string]struct{})}
}

// add appends v to the set if not already present.
func (s *distinctSet) add(v any) {
	if k, ok := distinctKey(v); ok {
		ks := string(k)
		if _, dup := s.seen[ks]; dup {
			return
		}
		s.seen[ks] = struct{}{}
		s.values = append(s.values, v)
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

// array returns an unsorted *types.Array containing every unique value the
// set has accepted, in insertion order.
func (s *distinctSet) array() *types.Array {
	arr := types.MakeArray(len(s.values))
	for _, v := range s.values {
		arr.Append(v)
	}
	return arr
}
