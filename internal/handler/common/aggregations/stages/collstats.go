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
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/common"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// collStats represents $collStats stage.
type collStats struct {
	storageStats   *storageStats
	count          bool
	latencyStats   bool
	histograms     bool
	queryExecStats bool
}

// storageStats represents $collStats.storageStats field.
type storageStats struct {
	scale int32
}

func newCollStats(stage *types.Document) (aggregations.Stage, error) {
	fields, err := common.GetRequiredParam[*types.Document](stage, "$collStats")
	if err != nil {
		// Format as "key: value" without surrounding braces, matching MongoDB's error format.
		stageValue, _ := stage.Get("$collStats")
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrStageCollStatsInvalidArg,
			fmt.Sprintf("$collStats must take a nested object but found: $collStats: %s", types.FormatAnyValue(stageValue)),
			"$collStats (stage)",
		)
	}

	var cs collStats

	var present bool
	if present, err = collStatsOption(fields, "count", true); err != nil {
		return nil, err
	}
	cs.count = present

	if v, getErr := fields.Get("latencyStats"); getErr == nil && v != types.Null {
		spec, ok := v.(*types.Document)
		if !ok {
			return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
				fmt.Sprintf("BSON field '$collStats.latencyStats' is the wrong type '%s', expected type 'object'",
					handlerparams.AliasFromType(v)), "$collStats (stage)")
		}
		for _, k := range spec.Keys() {
			if k != "histograms" {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrIDLUnknownField,
					fmt.Sprintf("BSON field '$collStats.latencyStats.%s' is an unknown field.", k), "$collStats (stage)")
			}
			h, isBool := must.NotFail(spec.Get(k)).(bool)
			if !isBool {
				return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
					"Field 'histograms' should be a boolean value, but found: "+handlerparams.AliasFromType(must.NotFail(spec.Get(k))),
					"$collStats (stage)")
			}
			cs.histograms = h
		}
		cs.latencyStats = true
	}

	if cs.queryExecStats, err = collStatsOption(fields, "queryExecStats", true); err != nil {
		return nil, err
	}

	if present, err = collStatsOption(fields, "storageStats", false); err != nil {
		return nil, err
	}
	if present {
		cs.storageStats = new(storageStats)

		storageStatsFields := must.NotFail(fields.Get("storageStats")).(*types.Document)

		cs.storageStats.scale = 1

		var s any
		if s, err = storageStatsFields.Get("scale"); err == nil {
			scale, err := handlerparams.GetValidatedNumberParamWithMinValue(
				"$collStats.storageStats", "scale", s, 1,
			)
			if err != nil {
				return nil, err
			}

			cs.storageStats.scale = int32(scale)
		}
	}

	return &cs, nil
}

// collStatsOption reports whether $collStats asks for the option name: an
// object does, null and absence do not, and anything else is TypeMismatch.
// An option that takes no settings must be an empty object.
func collStatsOption(fields *types.Document, name string, mustBeEmpty bool) (bool, error) {
	v, err := fields.Get(name)
	if err != nil || v == types.Null {
		return false, nil
	}
	spec, ok := v.(*types.Document)
	if !ok {
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrTypeMismatch,
			fmt.Sprintf("BSON field '$collStats.%s' is the wrong type '%s', expected type 'object'",
				name, handlerparams.AliasFromType(v)), "$collStats (stage)")
	}
	if mustBeEmpty && spec.Len() > 0 {
		return false, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrCollStatsExpectedEmptyObject,
			"expected an empty object, but got "+types.FormatAnyValue(spec), "$collStats (stage)")
	}
	return true, nil
}

// Process implements Stage interface.
//
// Processing consists of modification of the input document, so it contains all the necessary fields
// and the data is modified according to the given request.
func (c *collStats) Process(ctx context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	_, res, err := iter.Next()
	if errors.Is(err, iterator.ErrIteratorDone) {
		// For non-shared collections, it must contain a single document.
		return nil, lazyerrors.New("collStatsStage: Process: expected 1 document, got none")
	}

	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	if c.storageStats != nil {
		scale := c.storageStats.scale

		if c.storageStats.scale > 1 {
			scalable := []string{"size", "storageSize", "freeStorageSize", "totalIndexSize", "totalSize"}
			for _, key := range scalable {
				path := types.NewStaticPath("storageStats", key)
				val := must.NotFail(res.GetByPath(path))
				must.NoError(res.SetByPath(path, val.(int32)/scale))
			}

			path := types.NewStaticPath("storageStats", "indexSizes")
			indexSizesDoc := must.NotFail(res.GetByPath(path)).(*types.Document)

			for _, name := range indexSizesDoc.Keys() {
				size := must.NotFail(indexSizesDoc.Get(name)).(int32)
				indexSizesDoc.Set(name, size/scale)
			}

			must.NoError(res.SetByPath(path, indexSizesDoc))
		}

		must.NoError(res.SetByPath(types.NewStaticPath("storageStats", "scaleFactor"), scale))
	}

	if c.latencyStats {
		// DumboDB does not time operations, so every counter is zero.
		stats := types.MakeDocument(4)
		for _, kind := range []string{"commands", "reads", "writes", "transactions"} {
			counters := types.MakeDocument(4)
			if c.histograms {
				counters.Set("histogram", types.MakeArray(0))
			}
			counters.Set("latency", int64(0))
			counters.Set("ops", int64(0))
			counters.Set("queryableEncryptionLatencyMicros", int64(0))
			stats.Set(kind, counters)
		}
		res.Set("latencyStats", stats)
	}

	if _, _, err := iter.Next(); err == nil || !errors.Is(err, iterator.ErrIteratorDone) {
		// For non-shared collections, it contains only a single document.
		return nil, lazyerrors.New("collStatsStage: Process: expected 1 document, got more")
	}

	iter = iterator.Values(iterator.ForSlice([]*types.Document{res}))
	closer.Add(iter)

	return iter, nil
}

var (
	_ aggregations.Stage = (*collStats)(nil)
)
