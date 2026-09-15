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

package oplog

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/dolthub/dumbodb/internal/replication/control"
)

func TestBufferEnforcesBothLimitsAndResumes(t *testing.T) {
	buffer, err := NewBuffer(BufferLimits{Entries: 2, Bytes: 5})
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(testEntry(1, 3)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(testEntry(2, 2)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(testEntry(3, 1)); !errors.Is(err, ErrBufferFull) {
		t.Fatalf("full buffer error = %v", err)
	}
	waited := make(chan error, 1)
	go func() { waited <- buffer.WaitForSpace(context.Background(), 1) }()
	select {
	case err := <-waited:
		t.Fatalf("WaitForSpace returned before drain: %v", err)
	case <-time.After(10 * time.Millisecond):
	}
	drained := buffer.Drain(1, 5)
	if len(drained) != 1 || drained[0].OpTime.Increment != 1 {
		t.Fatalf("drained = %+v", drained)
	}
	if err := <-waited; err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(testEntry(3, 1)); err != nil {
		t.Fatal(err)
	}
	if stats := buffer.Stats(); stats.Entries != 2 || stats.Bytes != 3 {
		t.Fatalf("buffer stats = %+v", stats)
	}
}

func TestBufferRejectsOversizedAndOutOfOrderEntries(t *testing.T) {
	buffer, err := NewBuffer(BufferLimits{Entries: 2, Bytes: 4})
	if err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(testEntry(1, 5)); !errors.Is(err, ErrEntryTooLarge) {
		t.Fatalf("oversized entry error = %v", err)
	}
	if err := buffer.TryAppend(testEntry(2, 1)); err != nil {
		t.Fatal(err)
	}
	if err := buffer.TryAppend(testEntry(2, 1)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("duplicate entry error = %v", err)
	}
	if err := buffer.TryAppend(testEntry(1, 1)); !errors.Is(err, ErrOutOfOrder) {
		t.Fatalf("out-of-order entry error = %v", err)
	}
}

func testEntry(increment uint32, size int) Entry {
	return Entry{OpTime: control.OpTime{Seconds: 100, Increment: increment, Term: 8}, RawBSON: make([]byte, size)}
}
