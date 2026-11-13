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

package dolt

import (
	"bytes"
	"context"

	"github.com/dolthub/dolt/go/store/prolly"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
)

// intervalGaps counts the keys a scan of several ranges examines outside
// them, as MongoDB's multi-interval IXSCAN does: after each range it reads the
// next key and counts it, seeking on unless a later range holds it. A scan of
// one range, or under backends.WithSeekEachRange, reads nothing past a range.
type intervalGaps struct {
	idxMap  prolly.Map
	reverse bool
	// last is the key last read past a range; the empty ranges it skips past
	// read it again without counting it.
	last []byte
}

func newIntervalGaps(ctx context.Context, idxMap prolly.Map, ranges [][2][]byte, reverse bool) *intervalGaps {
	if len(ranges) < 2 || backends.SeeksEachRange(ctx) {
		return nil
	}
	return &intervalGaps{idxMap: idxMap, reverse: reverse}
}

// after reports whether finishing done, with rest still to scan, examines a
// key outside every range.
func (g *intervalGaps) after(ctx context.Context, done [2][]byte, rest [][2][]byte) (bool, error) {
	if g == nil {
		return false, nil
	}
	bound := done[1]
	if g.reverse {
		bound = done[0]
	}
	if len(bound) == 0 {
		return false, nil
	}
	k, ok, err := idxpkg.AdjacentKey(ctx, g.idxMap, bound, g.reverse)
	if err != nil || !ok || bytes.Equal(k, g.last) {
		return false, err
	}
	g.last = k
	for _, r := range rest {
		if (len(r[0]) == 0 || bytes.Compare(k, r[0]) >= 0) && (len(r[1]) == 0 || bytes.Compare(k, r[1]) < 0) {
			return false, nil
		}
	}
	return true, nil
}

// gapKeys counts the keys a scan of all of ranges examines outside them.
func gapKeys(ctx context.Context, idxMap prolly.Map, ranges [][2][]byte) (int, error) {
	g := newIntervalGaps(ctx, idxMap, ranges, false)
	n := 0
	for i := range ranges {
		outside, err := g.after(ctx, ranges[i], ranges[i+1:])
		if err != nil {
			return 0, err
		}
		if outside {
			n++
		}
	}
	return n, nil
}
