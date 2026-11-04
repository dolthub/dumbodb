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
	"context"

	"github.com/dolthub/dumbodb/internal/types"
)

// GeoNearParams configures GeoNearScanner.GeoNear.
type GeoNearParams struct {
	// Field is the geo field to read near; empty picks the collection's only
	// geo index.
	Field string
	// X and Y are the query point: longitude and latitude, or 2d coordinates.
	X, Y float64
	// Spherical measures distances in metres on the sphere; otherwise they
	// are planar, in coordinate units.
	Spherical bool
	// MaxDistance, when positive and finite, ends the read once every document
	// within it has been returned.
	MaxDistance float64
	// Distance returns a document's distance from the query point, in the
	// units Spherical names; math.MaxFloat64 when it has none.
	Distance func(doc *types.Document) float64
}

// GeoNearResult is a read in distance order.
type GeoNearResult struct {
	// Iter yields documents by nondecreasing Distance.
	Iter types.DocumentsIterator
	// Field is the geo field the read used.
	Field string
}

// GeoNearScanner is an optional capability a backend may implement to read
// documents nearest first from a geo index, as MongoDB's GEO_NEAR stages do,
// instead of sorting every document by distance.
//
// Implementations return (nil, nil) when no geo index serves the request; the
// handler then sorts in memory.
type GeoNearScanner interface {
	GeoNear(ctx context.Context, params *GeoNearParams) (*GeoNearResult, error)
}

// GeoNear forwards to the wrapped collection if it implements GeoNearScanner,
// otherwise returns (nil, nil).
func (cc *collectionContract) GeoNear(ctx context.Context, params *GeoNearParams) (*GeoNearResult, error) {
	gs, ok := cc.c.(GeoNearScanner)
	if !ok {
		return nil, nil
	}
	return gs.GeoNear(ctx, params)
}

var _ GeoNearScanner = (*collectionContract)(nil)
