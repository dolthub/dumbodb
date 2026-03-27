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

package stages_test

// Tests for individual aggregation stage parsing and error handling.
// TestAggStage_* tests verify that stage constructors return the correct
// error codes and messages to match MongoDB behavior.

import (
	"errors"
	"testing"

	"github.com/dolthub/dongo/internal/handler/common/aggregations/stages"
	"github.com/dolthub/dongo/internal/handler/handlererrors"
	"github.com/dolthub/dongo/internal/types"
	"github.com/dolthub/dongo/internal/util/must"
)

// TestAggStage_limit_LimitZeroError verifies that $limit: 0 returns error code
// 5107201 (ErrStageLimitInvalidArg) with message "the limit must be positive",
// matching MongoDB 8 behavior.
func TestAggStage_limit_LimitZeroError(t *testing.T) {
	t.Parallel()

	stageDoc := must.NotFail(types.NewDocument("$limit", int32(0)))

	_, err := stages.NewStage(stageDoc)
	if err == nil {
		t.Fatal("expected error for $limit: 0, got nil")
	}

	var cmdErr *handlererrors.CommandError
	if !errors.As(err, &cmdErr) {
		t.Fatalf("expected *handlererrors.CommandError, got %T: %v", err, err)
	}

	if got, want := cmdErr.Code(), handlererrors.ErrStageLimitInvalidArg; got != want {
		t.Errorf("error code: got %v (%d), want %v (%d)", got, got, want, want)
	}

	if got, want := cmdErr.Err().Error(), "the limit must be positive"; got != want {
		t.Errorf("error message: got %q, want %q", got, want)
	}
}
