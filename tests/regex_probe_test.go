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

package tests

import (
	"context"
	"math"
	"testing"

	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestProbeModNaNDivisor is a regression test for do-10z3 / do-9ni.
// When $mod is used as a QUERY operator with a NaN divisor (e.g. {x: {$mod: [NaN, 0]}}),
// dumbodb must return a BadValue error (code 2) rather than dropping the connection (code 0).
//
// Root cause: wire.CheckNaNs was set to true, causing NaN values to be rejected at the
// wire layer by dropping the connection instead of returning a proper MongoDB error.
// Fix: wire.CheckNaNs left at default (false) so NaN reaches filterFieldMod, which
// returns ErrBadValue for NaN divisors.
func TestProbeModNaNDivisor(t *testing.T) {
	t.Parallel()

	env := startDumboDB(t)
	coll := env.Collection(t)

	insertDocs(t, coll,
		d(e("_id", int32(1)), e("x", float64(10))),
	)

	ctx := context.Background()
	_, err := coll.Find(ctx,
		d(e("x", d(e("$mod", bson.A{math.NaN(), float64(0)})))),
	)
	require.Error(t, err, "$mod with NaN divisor must return an error")

	var cmdErr mongo.CommandError
	require.ErrorAs(t, err, &cmdErr, "error must be a CommandError, got %T: %v", err, err)
	require.Equal(t, int32(2), cmdErr.Code,
		"$mod with NaN divisor must return BadValue (code 2), got code %d: %s", cmdErr.Code, cmdErr.Message)
}
