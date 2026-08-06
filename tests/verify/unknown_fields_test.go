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

package verify

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// TestDumboUnknownFieldRejected asserts every dumbo* versioning command rejects
// an unrecognized top-level field with IDLUnknownField (40415), matching real
// MongoDB's strict IDL parsing. The check precedes all parameter reads, so a
// bogus field alone triggers it regardless of the command's other requirements.
// dumbo*/dolt* commands have no MongoDB counterpart, so this is a DumboDB test.
func TestDumboUnknownFieldRejected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()
	db := env.Client.Database("unknownfields_vdb@main")

	commands := []string{
		"dumboBranch", "dumboBranchStatus", "dumboCherryPick", "dumboConflicts",
		"dumboDiff", "dumboLog", "dumboMerge", "dumboRebase", "dumboReset",
		"dumboResolveConflict", "dumboRevert", "dumboStatus", "dumboTag",
		"dumboCommit", "dumboGC",
		// dolt* alias resolves to the same handler; prove one carries the check.
		"doltMerge",
	}

	for _, cmd := range commands {
		t.Run(cmd, func(t *testing.T) {
			err := db.RunCommand(ctx, bson.D{
				{Key: cmd, Value: int32(1)},
				{Key: "notARealField", Value: int32(1)},
			}).Err()
			require.Error(t, err, "%s must reject an unknown field", cmd)

			ce, ok := err.(mongo.CommandError)
			require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
			assert.EqualValues(t, 40415, ce.Code, "%s: want IDLUnknownField (40415), got %d: %s", cmd, ce.Code, ce.Message)
			assert.Contains(t, ce.Message, fmt.Sprintf("BSON field '%s.notARealField' is an unknown field.", cmd), cmd)
		})
	}
}

// TestDumboMergeMisspelledNoFF is the workspace-b1d repro: a misspelled noFF
// (noff) must be rejected loudly, not silently dropped into a fast-forward.
func TestDumboMergeMisspelledNoFF(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	err := env.Client.Database("b1d_vdb@main").RunCommand(ctx, bson.D{
		{Key: "doltMerge", Value: int32(1)},
		{Key: "noff", Value: true},
		{Key: "mergeIn", Value: "feature"},
	}).Err()
	require.Error(t, err, "misspelled noff must be rejected")

	ce, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 40415, ce.Code)
	assert.Contains(t, ce.Message, "BSON field 'doltMerge.noff' is an unknown field.")
}

// TestDumboUndropUnknownFieldRejected covers dumboUndrop, which lives in a
// separate handler and is admin-guarded; the unknown-field check runs before
// the admin check.
func TestDumboUndropUnknownFieldRejected(t *testing.T) {
	env := startDumboDB(t)
	ctx := context.Background()

	err := env.Client.Database("admin").RunCommand(ctx, bson.D{
		{Key: "dumboUndrop", Value: int32(1)},
		{Key: "notARealField", Value: int32(1)},
	}).Err()
	require.Error(t, err)

	ce, ok := err.(mongo.CommandError)
	require.True(t, ok, "expected mongo.CommandError, got %T: %v", err, err)
	assert.EqualValues(t, 40415, ce.Code, "want IDLUnknownField (40415), got %d: %s", ce.Code, ce.Message)
	assert.Contains(t, ce.Message, "BSON field 'dumboUndrop.notARealField' is an unknown field.")
}
