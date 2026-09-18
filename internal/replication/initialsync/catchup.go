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

package initialsync

import (
	"context"
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
)

var ErrStopPositionMissing = errors.New("initial sync stop position is missing from the oplog buffer")

type oplogEntryApplier interface {
	Apply(context.Context, oplog.Entry) error
}

type CatchUpLimits struct {
	Entries int
	Bytes   int64
}

type CatchUpResult struct {
	Applied int64
	Last    control.OpTime
}

func ApplyBufferedThrough(
	ctx context.Context,
	buffer *oplog.Buffer,
	applier oplogEntryApplier,
	beginApply control.OpTime,
	stop control.OpTime,
	limits CatchUpLimits,
) (CatchUpResult, error) {
	if buffer == nil || applier == nil || beginApply == (control.OpTime{}) || limits.Entries <= 0 || limits.Bytes <= 0 {
		return CatchUpResult{}, errors.New("initial sync catch-up requires buffer, applier, begin position, and positive limits")
	}
	if stop.Compare(beginApply) < 0 {
		return CatchUpResult{}, fmt.Errorf("initial sync stop position %v precedes begin-apply position %v", stop, beginApply)
	}
	result := CatchUpResult{Last: beginApply}
	if stop == beginApply {
		return result, nil
	}
	for result.Last.Compare(stop) < 0 {
		entries := buffer.DrainThrough(stop, limits.Entries, limits.Bytes)
		if len(entries) == 0 {
			if tail, ok := buffer.Tail(); ok && tail.Compare(stop) > 0 {
				return result, fmt.Errorf("%w: selected %v, next buffered entry is %v", ErrStopPositionMissing, stop, tail)
			}
			if err := buffer.WaitForData(ctx); err != nil {
				return result, err
			}
			continue
		}
		for _, entry := range entries {
			if entry.OpTime.Compare(beginApply) <= 0 {
				continue
			}
			if entry.OpTime.Compare(result.Last) <= 0 {
				return result, fmt.Errorf("initial sync oplog entry %v does not follow %v", entry.OpTime, result.Last)
			}
			if err := applier.Apply(ctx, entry); err != nil {
				return result, fmt.Errorf("applying initial sync oplog entry %v: %w", entry.OpTime, err)
			}
			result.Applied++
			result.Last = entry.OpTime
		}
	}
	return result, nil
}
