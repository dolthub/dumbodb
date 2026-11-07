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
	"container/heap"
	"context"
	"math"

	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"

	"github.com/dolthub/dumbodb/internal/backends"
	idxpkg "github.com/dolthub/dumbodb/internal/index"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/must"
)

// A geo index keys each point by its cell in a grid of geoCellBits bits per
// axis over longitude and latitude (2dsphere) or both coordinates in
// [-180, 180] (2d), as a Morton code: every quadtree cell of the grid is a
// contiguous range of keys.
const geoCellBits = 24

// geoOtherCell keys a value the grid holds no point for: a non-point geometry
// or a point outside the grid. Every near read considers its documents.
const geoOtherCell = int64(-1)

// earthRadiusMeters is the radius the handler measures spherical distances
// with.
const earthRadiusMeters = 6378100.0

func isGeoKey(kp backends.IndexKeyPair) bool { return kp.Geo2D || kp.Geo2DSphere }

// geoIndexValue returns the key a geo index on kp holds for v, a field's
// value: its point's cell, geoOtherCell, or null for a missing field.
func geoIndexValue(v any, kp backends.IndexKeyPair) any {
	if _, missing := v.(types.NullType); missing {
		return v
	}
	x, y, ok := geoPoint(v)
	if !ok {
		return geoOtherCell
	}
	nx, ny, ok := geoNormalize(x, y, kp.Geo2DSphere)
	if !ok {
		return geoOtherCell
	}
	return geoCellKey(nx, ny)
}

// geoPoint returns the point a GeoJSON Point, a legacy [x, y] pair, or a
// legacy {x, y} document holds.
func geoPoint(v any) (x, y float64, ok bool) {
	switch v := v.(type) {
	case *types.Array:
		if v.Len() < 2 {
			return 0, 0, false
		}
		return geoNumberPair(must.NotFail(v.Get(0)), must.NotFail(v.Get(1)))
	case *types.Document:
		if t, err := v.Get("type"); err == nil {
			coords, _ := v.Get("coordinates")
			arr, isArr := coords.(*types.Array)
			if t != "Point" || !isArr || arr.Len() < 2 {
				return 0, 0, false
			}
			return geoNumberPair(must.NotFail(arr.Get(0)), must.NotFail(arr.Get(1)))
		}
		if values := v.Values(); len(values) >= 2 {
			return geoNumberPair(values[0], values[1])
		}
	}
	return 0, 0, false
}

func geoNumberPair(a, b any) (float64, float64, bool) {
	x, okX := geoNumber(a)
	y, okY := geoNumber(b)
	return x, y, okX && okY
}

func geoNumber(v any) (float64, bool) {
	switch n := v.(type) {
	case float64:
		return n, !math.IsNaN(n) && !math.IsInf(n, 0)
	case int32:
		return float64(n), true
	case int64:
		return float64(n), true
	}
	return 0, false
}

// geoNormalize maps a point into the grid's unit square.
func geoNormalize(x, y float64, sphere bool) (nx, ny float64, ok bool) {
	yMin, ySpan := -180.0, 360.0
	if sphere {
		yMin, ySpan = -90, 180
	}
	nx, ny = (x+180)/360, (y-yMin)/ySpan
	return nx, ny, nx >= 0 && nx <= 1 && ny >= 0 && ny <= 1
}

// geoCellKey is the Morton code of the finest cell holding (nx, ny).
func geoCellKey(nx, ny float64) int64 {
	const cells = 1 << geoCellBits
	ix := min(uint32(nx*cells), cells-1)
	iy := min(uint32(ny*cells), cells-1)
	return int64(interleave(ix, iy, geoCellBits))
}

// interleave alternates the low bits of x and y, x's first, from the top.
func interleave(x, y uint32, bits int) uint64 {
	var out uint64
	for i := bits - 1; i >= 0; i-- {
		out = out<<2 | uint64(x>>i&1)<<1 | uint64(y>>i&1)
	}
	return out
}

// geoCell is a quadtree cell of the grid: level bits per axis, at (ix, iy).
type geoCell struct {
	level  int
	ix, iy uint32
}

// keyRange is the range of keys geoCellKey gives the points in c.
func (c geoCell) keyRange() (lo, hi int64) {
	shift := 2 * (geoCellBits - c.level)
	prefix := interleave(c.ix, c.iy, c.level)
	return int64(prefix << shift), int64((prefix + 1) << shift)
}

func (c geoCell) parent() geoCell { return geoCell{c.level - 1, c.ix >> 1, c.iy >> 1} }

// geoBox is a rectangle of the unit square.
type geoBox struct{ x0, x1, y0, y1 float64 }

func (b geoBox) whole() bool { return b.x0 <= 0 && b.x1 >= 1 && b.y0 <= 0 && b.y1 >= 1 }

// coverBox returns the cells, at most a few per side of the box, covering b.
func coverBox(b geoBox) []geoCell {
	if !finiteFloat(b.x0) || !finiteFloat(b.x1) || !finiteFloat(b.y0) || !finiteFloat(b.y1) {
		// No comparison prunes a NaN box; cover it with the root cell.
		return []geoCell{{}}
	}
	size := max(b.x1-b.x0, b.y1-b.y0)
	maxLevel := geoCellBits
	if size > 0 {
		maxLevel = min(geoCellBits, max(0, int(math.Floor(math.Log2(1/size)))+2))
	}
	var out []geoCell
	var walk func(c geoCell)
	walk = func(c geoCell) {
		side := 1 / float64(uint64(1)<<c.level)
		cx0, cy0 := float64(c.ix)*side, float64(c.iy)*side
		cx1, cy1 := cx0+side, cy0+side
		if cx1 <= b.x0 || cx0 > b.x1 || cy1 <= b.y0 || cy0 > b.y1 {
			return
		}
		if c.level == maxLevel || (cx0 >= b.x0 && cx1 <= b.x1 && cy0 >= b.y0 && cy1 <= b.y1) {
			out = append(out, c)
			return
		}
		for i := uint32(0); i < 4; i++ {
			walk(geoCell{c.level + 1, c.ix<<1 | i>>1, c.iy<<1 | i&1})
		}
	}
	walk(geoCell{})
	return out
}

// nearBoxes returns rectangles of the unit square holding every point within
// radius of (x, y), and whether they hold the whole grid.
func nearBoxes(x, y, radius float64, spherical, sphereGrid bool) ([]geoBox, bool) {
	const margin = 1e-9
	var x0, x1, y0, y1 float64
	if !spherical {
		x0, x1, y0, y1 = x-radius, x+radius, y-radius, y+radius
	} else {
		d := radius / earthRadiusMeters
		if d >= math.Pi {
			return nil, true
		}
		dLat := d * 180 / math.Pi
		y0, y1 = y-dLat, y+dLat
		x0, x1 = -180, 180
		if y0 > -90 && y1 < 90 {
			if s := math.Sin(d) / math.Cos(y*math.Pi/180); s < 1 {
				dLng := math.Asin(s) * 180 / math.Pi
				x0, x1 = x-dLng, x+dLng
			}
		}
	}
	var boxes []geoBox
	add := func(x0, x1 float64) {
		nx0, ny0, _ := geoNormalize(x0, y0, sphereGrid)
		nx1, ny1, _ := geoNormalize(x1, y1, sphereGrid)
		boxes = append(boxes, geoBox{nx0 - margin, nx1 + margin, ny0 - margin, ny1 + margin})
	}
	switch {
	case spherical && x0 < -180:
		add(x0+360, 180)
		add(-180, x1)
	case spherical && x1 > 180:
		add(x0, 180)
		add(-180, x1-360)
	default:
		add(x0, x1)
	}
	whole := false
	for _, b := range boxes {
		whole = whole || b.whole()
	}
	return boxes, whole
}

// GeoNear implements backends.GeoNearScanner.
func (c *collection) GeoNear(ctx context.Context, params *backends.GeoNearParams) (*backends.GeoNearResult, error) {
	if params == nil || params.Distance == nil {
		return nil, nil
	}
	// A query validated as MongoDB does never gets here with a non-finite
	// point or bound; decline rather than walk the grid without end.
	if !finiteFloat(params.X) || !finiteFloat(params.Y) || math.IsNaN(params.MaxDistance) {
		return nil, nil
	}
	m, exists, state, err := c.getMap(ctx)
	if err != nil || !exists {
		return nil, err
	}
	state.mu.RLock()
	idxInfos, idxMaps, err := resolveIndexes(ctx, c, state)
	state.mu.RUnlock()
	if err != nil {
		return nil, err
	}
	found := -1
	for i, idx := range idxInfos {
		if len(idx.Key) == 0 || !isGeoKey(idx.Key[0]) || idx.Hidden || idx.PartialFilterExpression != nil {
			continue
		}
		if params.Field != "" && idx.Key[0].Field != params.Field {
			continue
		}
		if found >= 0 && params.Field == "" {
			return nil, nil
		}
		found = i
	}
	if found < 0 {
		return nil, nil
	}
	info := idxInfos[found]
	it := &geoNearIter{
		ctx:     ctx,
		ns:      state.ns,
		primary: m,
		idxMap:  idxMaps[found],
		params:  params,
		sphere:  info.Key[0].Geo2DSphere,
		radius:  -1,
		scanned: make(map[geoCell]struct{}),
		seen:    make(map[string]struct{}),
		release: pinRoots(state, m, idxMaps[found]),
	}
	return &backends.GeoNearResult{Iter: it, Field: info.Key[0].Field}, nil
}

// geoNearIter yields documents nearest first. It reads the index in rings of
// doubling radius: it fetches the documents of every cell within the ring's
// bounds, and returns, in order of distance, those no farther than its
// radius, as no unread document can be nearer.
type geoNearIter struct {
	ctx     context.Context
	ns      tree.NodeStore
	primary prolly.Map
	idxMap  prolly.Map
	params  *backends.GeoNearParams
	sphere  bool
	// radius is the ring read so far, -1 before the first.
	radius  float64
	done    bool
	scanned map[geoCell]struct{}
	seen    map[string]struct{}
	found   nearHeap
	release func()
}

func (it *geoNearIter) Next() (struct{}, *types.Document, error) {
	for {
		if err := it.ctx.Err(); err != nil {
			return struct{}{}, nil, err
		}
		if len(it.found) > 0 && (it.done || it.found[0].dist <= it.radius) {
			return struct{}{}, heap.Pop(&it.found).(nearDoc).doc, nil
		}
		if it.done {
			return struct{}{}, nil, iterator.ErrIteratorDone
		}
		if err := it.expand(); err != nil {
			return struct{}{}, nil, err
		}
	}
}

func (it *geoNearIter) Close() {
	if it.release != nil {
		it.release()
		it.release = nil
	}
}

// firstRadius is a ring of about a cell of 2^15 per axis.
func (it *geoNearIter) firstRadius() float64 {
	if it.params.Spherical {
		return 2 * math.Pi * earthRadiusMeters / (1 << 15)
	}
	return 360.0 / (1 << 15)
}

// expand reads the next ring: the first, or one of twice the radius.
func (it *geoNearIter) expand() error {
	next := it.firstRadius()
	if it.radius >= 0 {
		next = it.radius * 2
	} else if err := it.scanKeys(geoOtherCell, geoOtherCell+1); err != nil {
		return err
	}
	if limit := it.params.MaxDistance; limit > 0 && !math.IsInf(limit, 1) && next >= limit {
		next, it.done = limit, true
	}
	boxes, whole := nearBoxes(it.params.X, it.params.Y, next, it.params.Spherical, it.sphere)
	if whole || !finiteFloat(next) {
		boxes, it.done = []geoBox{{0, 1, 0, 1}}, true
	}
	for _, b := range boxes {
		for _, cell := range coverBox(b) {
			if err := it.ctx.Err(); err != nil {
				return err
			}
			if it.coveredByScanned(cell) {
				continue
			}
			it.scanned[cell] = struct{}{}
			lo, hi := cell.keyRange()
			if err := it.scanKeys(lo, hi); err != nil {
				return err
			}
		}
	}
	it.radius = next
	return nil
}

func (it *geoNearIter) coveredByScanned(c geoCell) bool {
	for ; ; c = c.parent() {
		if _, ok := it.scanned[c]; ok {
			return true
		}
		if c.level == 0 {
			return false
		}
	}
}

// scanKeys fetches the documents keyed in [lo, hi) not yet seen, adding them
// to found by distance.
func (it *geoNearIter) scanKeys(lo, hi int64) error {
	entries, ok, err := idxpkg.SeekKeyRange(it.ctx, it.idxMap, idxpkg.EncodeValue(lo), idxpkg.EncodeValue(hi))
	if err != nil || !ok {
		return err
	}
	for {
		_, id, _, done, err := nextDistinctEntry(it.ctx, entries)
		if err != nil || done {
			return err
		}
		if _, dup := it.seen[string(id)]; dup {
			continue
		}
		it.seen[string(id)] = struct{}{}
		doc, err := lookupDocFromPrimary(it.ctx, it.primary, it.ns, id, nil)
		if err != nil {
			return err
		}
		if doc == nil {
			continue
		}
		doc.SetRecordID(keyBytesToRecordID(id))
		heap.Push(&it.found, nearDoc{doc: doc, dist: it.params.Distance(doc), order: len(it.seen)})
	}
}

// nearDoc is a fetched document and its distance; order keeps ties in the
// order they were read.
type nearDoc struct {
	doc   *types.Document
	dist  float64
	order int
}

type nearHeap []nearDoc

func (h nearHeap) Len() int { return len(h) }

func (h nearHeap) Less(i, j int) bool {
	if h[i].dist != h[j].dist {
		return h[i].dist < h[j].dist
	}
	return h[i].order < h[j].order
}

func (h nearHeap) Swap(i, j int) { h[i], h[j] = h[j], h[i] }

func (h *nearHeap) Push(x any) { *h = append(*h, x.(nearDoc)) }

func (h *nearHeap) Pop() any {
	old := *h
	x := old[len(old)-1]
	*h = old[:len(old)-1]
	return x
}

var _ backends.GeoNearScanner = (*collection)(nil)

func finiteFloat(f float64) bool { return !math.IsNaN(f) && !math.IsInf(f, 0) }
