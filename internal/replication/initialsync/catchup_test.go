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
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/oplog"
)

type recordingEntryApplier struct {
	entries []oplog.Entry
}

func (a *recordingEntryApplier) Apply(_ context.Context, entry oplog.Entry) error {
	a.entries = append(a.entries, entry)
	return nil
}

func TestApplyBufferedThroughSkipsBoundaryWaitsForStopAndPreservesLaterEntries(t *testing.T) {
	buffer, err := oplog.NewBuffer(oplog.BufferLimits{Entries: 10, Bytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	for increment := uint32(1); increment <= 3; increment++ {
		if err := buffer.TryAppend(catchUpEntry(increment)); err != nil {
			t.Fatal(err)
		}
	}
	applier := &recordingEntryApplier{}
	resultChannel := make(chan CatchUpResult, 1)
	errorChannel := make(chan error, 1)
	go func() {
		result, err := ApplyBufferedThrough(context.Background(), buffer, applier, catchUpOpTime(2), catchUpOpTime(4), CatchUpLimits{Entries: 2, Bytes: 20})
		resultChannel <- result
		errorChannel <- err
	}()
	select {
	case err := <-errorChannel:
		t.Fatalf("catch-up returned before stop arrived: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	if err := buffer.TryAppend(catchUpEntry(4)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(catchUpEntry(5)); err != nil {
		t.Fatal(err)
	}
	result := <-resultChannel
	if err := <-errorChannel; err != nil {
		t.Fatal(err)
	}
	if result.Applied != 2 || result.Last != catchUpOpTime(4) {
		t.Fatalf("catch-up result = %+v", result)
	}
	if len(applier.entries) != 2 || applier.entries[0].OpTime != catchUpOpTime(3) || applier.entries[1].OpTime != catchUpOpTime(4) {
		t.Fatalf("applied entries = %+v", applier.entries)
	}
	remaining := buffer.Drain(10, 100)
	if len(remaining) != 1 || remaining[0].OpTime != catchUpOpTime(5) {
		t.Fatalf("remaining entries = %+v", remaining)
	}
}

func TestApplyBufferedThroughRejectsMissingStopPosition(t *testing.T) {
	buffer, err := oplog.NewBuffer(oplog.BufferLimits{Entries: 10, Bytes: 100})
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(catchUpEntry(3)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(catchUpEntry(5)); err != nil {
		t.Fatal(err)
	}
	result, err := ApplyBufferedThrough(context.Background(), buffer, &recordingEntryApplier{}, catchUpOpTime(2), catchUpOpTime(4), CatchUpLimits{Entries: 10, Bytes: 100})
	if !errors.Is(err, ErrStopPositionMissing) {
		t.Fatalf("missing stop error = %v", err)
	}
	if result.Applied != 1 || result.Last != catchUpOpTime(3) {
		t.Fatalf("partial catch-up result = %+v", result)
	}
}

func catchUpEntry(increment uint32) oplog.Entry {
	return oplog.Entry{OpTime: catchUpOpTime(increment), RawBSON: []byte{1}}
}

func catchUpOpTime(increment uint32) control.OpTime {
	return control.OpTime{Seconds: 100, Increment: increment, Term: 8}
}
