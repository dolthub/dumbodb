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

package main

import (
	"errors"
	"fmt"
	"testing"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

func TestExpectedVCSContention(t *testing.T) {
	dirtyTarget := mongo.CommandError{
		Code:    96,
		Message: "DumboDBMerge: fast-forward: target has uncommitted changes. --force required to overwrite",
	}
	optimisticLock := mongo.CommandError{
		Code:    96,
		Message: "optimistic lock failed on database Root update",
	}
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{name: "dirty target", err: dirtyTarget, want: true},
		{name: "optimistic lock", err: optimisticLock, want: true},
		{name: "wrapped", err: fmt.Errorf("commit: %w", optimisticLock), want: true},
		{name: "different code", err: mongo.CommandError{Code: 112, Message: optimisticLock.Message}, want: false},
		{name: "unrelated code 96", err: mongo.CommandError{Code: 96, Message: "genuine merge failure"}, want: false},
		{name: "plain text", err: errors.New(dirtyTarget.Message), want: false},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := expectedVCSContention(test.err); got != test.want {
				t.Fatalf("expectedVCSContention() = %t, want %t", got, test.want)
			}
		})
	}
}

func TestRecordVCSFailureSuppressesOnlyExpectedContention(t *testing.T) {
	stats := newErrorStats()
	recordVCSFailure(stats, "vcs-merge", mongo.CommandError{
		Code:    96,
		Message: "fast-forward: target has uncommitted changes",
	})
	recordVCSFailure(stats, "vcs-commit", mongo.CommandError{
		Code:    96,
		Message: "optimistic lock failed on database Root update",
	})
	if got := stats.count(); got != 0 {
		t.Fatalf("benign contention count = %d, want 0", got)
	}

	recordVCSFailure(stats, "vcs-merge", mongo.CommandError{Code: 96, Message: "genuine merge failure"})
	if got := stats.count(); got != 1 {
		t.Fatalf("genuine failure count = %d, want 1", got)
	}
}
