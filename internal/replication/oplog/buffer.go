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
	"sync"

	"github.com/dolthub/dumbodb/internal/replication/control"
)

var (
	ErrBufferFull    = errors.New("oplog buffer is full")
	ErrEntryTooLarge = errors.New("oplog entry exceeds buffer byte limit")
	ErrOutOfOrder    = errors.New("oplog entry is not after buffered tail")
)

type BufferLimits struct {
	Entries int
	Bytes   int64
}

type BufferStats struct {
	Entries int
	Bytes   int64
}

type Buffer struct {
	mu      sync.Mutex
	limits  BufferLimits
	entries []Entry
	bytes   int64
	space   chan struct{}
	data    chan struct{}
}

func NewBuffer(limits BufferLimits) (*Buffer, error) {
	if limits.Entries <= 0 || limits.Bytes <= 0 {
		return nil, errors.New("oplog buffer entry and byte limits must be positive")
	}
	return &Buffer{limits: limits, space: make(chan struct{}), data: make(chan struct{})}, nil
}

func (b *Buffer) TryAppend(entry Entry) error {
	size := int64(len(entry.RawBSON))
	if size > b.limits.Bytes {
		return ErrEntryTooLarge
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) != 0 && entry.OpTime.Compare(b.entries[len(b.entries)-1].OpTime) <= 0 {
		return ErrOutOfOrder
	}
	if len(b.entries) == b.limits.Entries || b.bytes+size > b.limits.Bytes {
		return ErrBufferFull
	}
	entry.RawBSON = append([]byte(nil), entry.RawBSON...)
	b.entries = append(b.entries, entry)
	b.bytes += size
	close(b.data)
	b.data = make(chan struct{})
	return nil
}

func (b *Buffer) Drain(maxEntries int, maxBytes int64) []Entry {
	return b.drain(maxEntries, maxBytes, nil)
}

// DrainThrough removes a bounded prefix whose entries do not follow stop.
func (b *Buffer) DrainThrough(stop control.OpTime, maxEntries int, maxBytes int64) []Entry {
	return b.drain(maxEntries, maxBytes, &stop)
}

func (b *Buffer) drain(maxEntries int, maxBytes int64, stop *control.OpTime) []Entry {
	if maxEntries <= 0 || maxBytes <= 0 {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	count := 0
	var bytes int64
	for count < len(b.entries) && count < maxEntries {
		if stop != nil && b.entries[count].OpTime.Compare(*stop) > 0 {
			break
		}
		size := int64(len(b.entries[count].RawBSON))
		if count != 0 && bytes+size > maxBytes {
			break
		}
		bytes += size
		count++
	}
	if count == 0 {
		return nil
	}
	result := append([]Entry(nil), b.entries[:count]...)
	b.entries = append([]Entry(nil), b.entries[count:]...)
	b.bytes -= bytes
	close(b.space)
	b.space = make(chan struct{})
	return result
}

// WaitForData blocks until the buffer contains at least one entry.
func (b *Buffer) WaitForData(ctx context.Context) error {
	for {
		b.mu.Lock()
		if len(b.entries) != 0 {
			b.mu.Unlock()
			return nil
		}
		data := b.data
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-data:
		}
	}
}

func (b *Buffer) WaitForSpace(ctx context.Context, bytes int64) error {
	for {
		b.mu.Lock()
		if len(b.entries) < b.limits.Entries && b.bytes+bytes <= b.limits.Bytes {
			b.mu.Unlock()
			return nil
		}
		space := b.space
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-space:
		}
	}
}

func (b *Buffer) Stats() BufferStats {
	b.mu.Lock()
	defer b.mu.Unlock()
	return BufferStats{Entries: len(b.entries), Bytes: b.bytes}
}

func (b *Buffer) Tail() (control.OpTime, bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.entries) == 0 {
		return control.OpTime{}, false
	}
	return b.entries[len(b.entries)-1].OpTime, true
}
