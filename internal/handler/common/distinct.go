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
	"errors"
	"fmt"
	"log/slog"
	"strconv"

	"go.mongodb.org/mongo-driver/bson/primitive"

	"github.com/dolthub/docudolt/internal/handler/commonpath"
	"github.com/dolthub/docudolt/internal/handler/handlererrors"
	"github.com/dolthub/docudolt/internal/handler/handlerparams"
	"github.com/dolthub/docudolt/internal/types"
	"github.com/dolthub/docudolt/internal/util/iterator"
	"github.com/dolthub/docudolt/internal/util/lazyerrors"
)

// DistinctParams contains `distinct` command parameters supported by at least one handler.
//
//nolint:vet // for readability
type DistinctParams struct {
	DB         string          `ferretdb:"$db"`
	Collection string          `ferretdb:"distinct,collection"`
	Key        string          `ferretdb:"key"`
	Filter     *types.Document `ferretdb:"-"`
	Comment    string          `ferretdb:"comment,opt"`

	Query any `ferretdb:"query,opt"`

	Collation *types.Document `ferretdb:"collation,unimplemented"`

	ReadConcern    *types.Document `ferretdb:"readConcern,ignored"`
	LSID           any             `ferretdb:"lsid,ignored"`
	ClusterTime    any             `ferretdb:"$clusterTime,ignored"`
	ReadPreference *types.Document `ferretdb:"$readPreference,ignored"`

	ApiVersion           string `ferretdb:"apiVersion,ignored"`
	ApiStrict            bool   `ferretdb:"apiStrict,ignored"`
	ApiDeprecationErrors bool   `ferretdb:"apiDeprecationErrors,ignored"`
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
	s := primitive.NewDecimal128(d.H, d.L).String()
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

// distinctContains checks if arr already contains a value equal to val.
// For Decimal128 values, equality is determined by numeric value rather than
// bit-level representation, so that e.g. Decimal128("42") and Decimal128("42.0")
// are treated as duplicates.
func distinctContains(arr *types.Array, val any) bool {
	d1, isDecimal := val.(types.Decimal128)
	if !isDecimal {
		return arr.Contains(val)
	}

	f1, f1ok := decimal128ToFloat64(d1)
	for i := 0; i < arr.Len(); i++ {
		el, err := arr.Get(i)
		if err != nil {
			continue
		}
		d2, ok := el.(types.Decimal128)
		if !ok {
			continue
		}
		f2, f2ok := decimal128ToFloat64(d2)
		if f1ok && f2ok && f1 == f2 {
			return true
		}
		// fallback: bit-level equality for NaN/Infinity
		if !f1ok && !f2ok && d1 == d2 {
			return true
		}
	}
	return false
}

// FilterDistinctValues returns distinct values from the given slice of documents with the given key.
//
// If the key is not found in the document, the document is ignored.
//
// If the key is found in the document, and the value is an array, each element of the array is added to the result.
// Otherwise, the value itself is added to the result.
func FilterDistinctValues(iter types.DocumentsIterator, key string) (*types.Array, error) {
	distinct := types.MakeArray(0)

	defer iter.Close()

	for {
		_, doc, err := iter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		path, err := types.NewPathFromString(key)
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

					if !distinctContains(distinct, el) {
						distinct.Append(el)
					}
				}

			default:
				if !distinctContains(distinct, v) {
					distinct.Append(v)
				}
			}
		}
	}

	SortArray(distinct, types.Ascending)

	return distinct, nil
}
