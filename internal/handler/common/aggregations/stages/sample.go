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

package stages

import (
	"context"
	"math/rand/v2"

	"github.com/dolthub/dongo/internal/handler/common/aggregations"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/handler/handlerparams"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/iterator"
	"github.com/dolthub/dongo/internal/util/lazyerrors"
)

// sample represents $sample stage.
type sample struct {
	size int64
}

// newSample creates a new $sample stage.
func newSample(stage *types.Document) (aggregations.Stage, error) {
	spec, err := stage.Get("$sample")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	doc, ok := spec.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageSampleInvalidArg,
			"$sample stage requires a size field, and it must be a non-negative integral number",
			"$sample (stage)",
		)
	}

	sizeVal, err := doc.Get("size")
	if err != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageSampleInvalidArg,
			"$sample stage requires a size field, and it must be a non-negative integral number",
			"$sample (stage)",
		)
	}

	size, parseErr := handlerparams.GetWholeNumberParam(sizeVal)
	if parseErr != nil {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageSampleInvalidArg,
			"$sample stage requires a size field, and it must be a non-negative integral number",
			"$sample (stage)",
		)
	}

	if size < 0 {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageSampleNegativeSize,
			"size argument to $sample must not be negative",
			"$sample (stage)",
		)
	}

	return &sample{size: size}, nil
}

// Process implements Stage interface.
//
// It consumes all documents from iter, randomly selects up to size of them,
// and returns an iterator over the selected documents.
func (s *sample) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
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

// check interfaces
var (
	_ aggregations.Stage = (*sample)(nil)
)
