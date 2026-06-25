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

package support

import (
	"context"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// AssertRootishRejected verifies that any operation on an invalid rootish
// returns OperationFailed (code 96) at parse time.
func AssertRootishRejected(tb testing.TB, db *mongo.Database, op string) {
	tb.Helper()

	ctx := context.Background()
	_, err := db.Collection("col").Find(ctx, bson.D{})
	require.Error(tb, err, "%s: expected parse-time rejection", op)

	cmdErr, ok := err.(mongo.CommandError)
	require.True(tb, ok, "%s: expected CommandError, got %T: %v", op, err, err)
	assert.EqualValues(tb, 96, cmdErr.Code,
		"%s: expected OperationFailed (96), got %d: %s", op, cmdErr.Code, cmdErr.Message)
}

// AssertWriteBlockedOperationFailed verifies that err is a MongoDB CommandError
// with code 96 (OperationFailed), as expected for writes to read-only rootish
// connections.
func AssertWriteBlockedOperationFailed(tb testing.TB, err error, op string) {
	tb.Helper()

	require.Error(tb, err, "%s on read-only rootish must return an error", op)

	cmdErr, ok := err.(mongo.CommandError)
	require.True(tb, ok, "%s: expected mongo.CommandError, got %T: %v", op, err, err)
	assert.EqualValues(tb, 96, cmdErr.Code,
		"%s: expected OperationFailed (96), got code %d: %s", op, cmdErr.Code, cmdErr.Message)
}
