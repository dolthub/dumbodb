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
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"

	"go.mongodb.org/mongo-driver/v2/mongo"
)

// errorStats tallies workload errors by a normalized signature so the
// report can say what the errors were, not just how many. Each bucket
// keeps one example message verbatim.
type errorStats struct {
	mu     sync.Mutex
	total  int64
	counts map[string]int64
	sample map[string]string
}

func newErrorStats() *errorStats {
	return &errorStats{counts: map[string]int64{}, sample: map[string]string{}}
}

func (e *errorStats) record(op string, err error) {
	if err == nil {
		return
	}
	key := op + " / " + classifyError(err)
	e.mu.Lock()
	e.total++
	e.counts[key]++
	if _, ok := e.sample[key]; !ok {
		e.sample[key] = truncate(strings.Join(strings.Fields(err.Error()), " "), 200)
	}
	e.mu.Unlock()
}

func (e *errorStats) count() int64 {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.total
}

type errorTally struct {
	Key    string
	Count  int64
	Sample string
}

func (e *errorStats) snapshot() []errorTally {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]errorTally, 0, len(e.counts))
	for k, c := range e.counts {
		out = append(out, errorTally{Key: k, Count: c, Sample: e.sample[k]})
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Count != out[j].Count {
			return out[i].Count > out[j].Count
		}
		return out[i].Key < out[j].Key
	})
	return out
}

// classifyError reduces an error to a stable bucket key. Server errors
// key on their name/code, context and network failures on their kind,
// and anything else on a digit-collapsed message so per-id and per-ns
// variation does not fragment the buckets.
func classifyError(err error) string {
	switch {
	case errors.Is(err, context.Canceled):
		return "context canceled"
	case errors.Is(err, context.DeadlineExceeded):
		return "context deadline exceeded"
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) {
		return serverErrorKey(cmdErr.Name, cmdErr.Code)
	}
	var writeErr mongo.WriteException
	if errors.As(err, &writeErr) {
		if len(writeErr.WriteErrors) > 0 {
			we := writeErr.WriteErrors[0]
			return serverErrorKey("", int32(we.Code))
		}
		if writeErr.WriteConcernError != nil {
			return serverErrorKey("", int32(writeErr.WriteConcernError.Code))
		}
	}
	if mongo.IsNetworkError(err) {
		return "network error"
	}
	return collapseDigits(strings.Join(strings.Fields(err.Error()), " "))
}

func serverErrorKey(name string, code int32) string {
	if name != "" {
		return fmt.Sprintf("%s (code %d)", name, code)
	}
	return fmt.Sprintf("code %d", code)
}

// collapseDigits replaces runs of digits with '#' so ids, timestamps
// and ns suffixes collapse into a single bucket, then caps the length.
func collapseDigits(s string) string {
	var b strings.Builder
	prevDigit := false
	for _, r := range s {
		if r >= '0' && r <= '9' {
			if !prevDigit {
				b.WriteByte('#')
			}
			prevDigit = true
			continue
		}
		prevDigit = false
		b.WriteRune(r)
	}
	return truncate(b.String(), 120)
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
