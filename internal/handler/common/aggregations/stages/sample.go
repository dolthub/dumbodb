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

package stages

import (
	"context"
	"math"
	"math/rand/v2"
	"strconv"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
	"go.mongodb.org/mongo-driver/bson/primitive"
)

// sample represents $sample stage.
type sample struct {
	size int64
}

func newSample(stage *types.Document) (aggregations.Stage, error) {
	spec, err := stage.Get("$sample")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	fail := func(code handlererrors.ErrorCode, msg string) error {
		return handlererrors.NewCommandErrorMsgWithArgument(code, msg, "$sample (stage)")
	}

	doc, ok := spec.(*types.Document)
	if !ok {
		return nil, fail(handlererrors.ErrStageSampleInvalidArg, "the $sample stage specification must be an object")
	}

	s := &sample{size: -1}
	for _, key := range doc.Keys() {
		if key != "size" {
			return nil, fail(handlererrors.ErrStageSampleUnknownOption, "unrecognized option to $sample: "+key)
		}
		var size float64
		switch v := must.NotFail(doc.Get(key)).(type) {
		case int32:
			size = float64(v)
		case int64:
			if v < 0 {
				return nil, fail(handlererrors.ErrStageSampleNegativeSize, "size argument to $sample must not be negative")
			}
			s.size = v
			continue
		case float64:
			size = v
		case types.Decimal128:
			size, err = strconv.ParseFloat(primitive.NewDecimal128(v.H, v.L).String(), 64)
			if err != nil {
				size = math.NaN()
			}
		default:
			return nil, fail(handlererrors.ErrStageSampleSizeNotNumber, "size argument to $sample must be a number")
		}
		if size < 0 {
			return nil, fail(handlererrors.ErrStageSampleNegativeSize, "size argument to $sample must not be negative")
		}
		switch {
		case math.IsNaN(size):
			s.size = 0
		case size >= math.MaxInt64:
			s.size = math.MaxInt64
		default:
			s.size = int64(size)
		}
	}
	if s.size < 0 {
		return nil, fail(handlererrors.ErrStageSampleMissingSize, "$sample stage must specify a size")
	}

	return s, nil
}

// Process implements Stage interface.
//
// It consumes all documents from iter, randomly selects up to size of them,
// and returns an iterator over the selected documents.
func (s *sample) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := common.ConsumeDocuments(iter, "$sample")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if s.size == 0 || len(docs) == 0 {
		res := iterator.Values(iterator.ForSlice([]*types.Document{}))
		closer.Add(res)

		return res, nil
	}

	// Fisher-Yates shuffle to randomly pick s.size documents.
	n := int64(len(docs))
	size := s.size

	if size > n {
		size = n
	}

	for i := int64(0); i < size; i++ {
		j := i + rand.Int64N(n-i)
		docs[i], docs[j] = docs[j], docs[i]
	}

	selected := docs[:size]

	res := iterator.Values(iterator.ForSlice(selected))
	closer.Add(res)

	return res, nil
}

var (
	_ aggregations.Stage = (*sample)(nil)
)
