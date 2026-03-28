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

package operators

import (
	"errors"
	"math"
	"testing"

	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/util/must"

	"github.com/dolthub/dongo/internal/types"
)

// TestProbeModNaNDivisor is a regression test for do-9ni.
// When $mod is used with a NaN divisor, dongo must return error code 2 (BadValue),
// not error code 0 (success / no error).
func TestProbeModNaNDivisor(t *testing.T) {
	t.Parallel()

	op, err := newMod(float64(10), math.NaN())
	if err != nil {
		t.Fatalf("newMod: unexpected error: %v", err)
	}

	doc := must.NotFail(types.NewDocument("x", float64(10)))

	_, err = op.Process(doc)
	if err == nil {
		t.Fatal("$mod with NaN divisor: expected an error, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("$mod with NaN divisor: expected *CommandError, got %T: %v", err, err)
	}

	if cmdErr.Code() != handlererrors.ErrBadValue {
		t.Errorf("$mod with NaN divisor: expected error code %d (BadValue), got %d",
			handlererrors.ErrBadValue, cmdErr.Code())
	}
}
