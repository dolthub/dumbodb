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
	"fmt"
	"math/rand"
	"strings"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
)

// docSize buckets the doc-size distribution. The payload-byte counts
// are chosen so the encoded BSON straddles dumbodb's inline / OOB /
// multi-chunk boundaries.
type docSize int

const (
	sizeXS docSize = iota // ~150B, well inside the inline tuple-builder threshold
	sizeS                 // ~700B, still inline
	sizeM                 // ~3KB, just over the inline threshold (single OOB chunk)
	sizeL                 // ~20KB, multi-chunk OOB
	sizeXL                // ~80KB, many chunks
)

// payloadBytes is the number of padding bytes added to a doc of each
// size. A few hundred bytes of structural overhead is added on top by
// the shape fields below.
var payloadBytes = [...]int{0, 500, 2800, 18000, 75000}

// pickSize samples docSize from a distribution skewed toward small
// docs (which is what dumbodb sees in steady state) with enough
// large-tail draws to keep the OOB / chunked paths exercised.
func pickSize(r *rand.Rand) docSize {
	n := r.Intn(100)
	switch {
	case n < 55:
		return sizeXS
	case n < 80:
		return sizeS
	case n < 93:
		return sizeM
	case n < 99:
		return sizeL
	default:
		return sizeXL
	}
}

// makeDoc emits a document with mixed types, optional nested
// structure, and a payload field sized to the requested bucket.
// Every doc carries _id, email, score, and createdAt so cross-worker
// queries always have something to filter on; everything else is
// randomized in shape and presence.
func makeDoc(r *rand.Rand, id string, size docSize) bson.M {
	d := bson.M{
		"_id":       id,
		"email":     fmt.Sprintf("%s@example.invalid", id),
		"score":     r.Intn(1000),
		"createdAt": time.Now(),
	}
	if r.Intn(2) == 0 {
		d["tags"] = randomTags(r)
	}
	if r.Intn(3) == 0 {
		d["address"] = bson.M{
			"street": fmt.Sprintf("%d %s St", r.Intn(9999), pickWord(r)),
			"city":   pickWord(r),
			"zip":    fmt.Sprintf("%05d", r.Intn(100000)),
		}
	}
	if r.Intn(3) == 0 {
		d["counters"] = bson.M{
			"views":  r.Intn(1_000_000),
			"clicks": r.Intn(10_000),
		}
	}
	if pad := payloadBytes[size]; pad > 0 {
		d["payload"] = randomString(r, pad)
	}
	return d
}

var wordPool = []string{
	"alpha", "beta", "gamma", "delta", "epsilon", "zeta", "eta", "theta",
	"iota", "kappa", "lambda", "mu", "nu", "xi", "omicron", "pi",
	"oak", "elm", "maple", "pine", "cedar", "ash", "birch", "fir",
}

func pickWord(r *rand.Rand) string { return wordPool[r.Intn(len(wordPool))] }

func randomTags(r *rand.Rand) []string {
	n := 2 + r.Intn(4)
	out := make([]string, n)
	for i := range out {
		out[i] = pickWord(r)
	}
	return out
}

// randomString returns a deterministic-per-seed string of exactly n
// bytes. Bytes are 'a'..'z' so the result is plain ASCII (BSON string
// length is precisely n).
func randomString(r *rand.Rand, n int) string {
	if n <= 0 {
		return ""
	}
	var b strings.Builder
	b.Grow(n)
	for i := 0; i < n; i++ {
		b.WriteByte(byte('a' + r.Intn(26)))
	}
	return b.String()
}
