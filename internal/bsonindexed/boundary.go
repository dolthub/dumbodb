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

package bsonindexed

import (
	"crypto/sha512"
	"encoding/binary"
	"math"

	"github.com/zeebo/xxh3"
)

// Chunk-boundary constants and weibull-check ported verbatim from
// dolt's tree/node_splitter.go. Replicated here because the originals
// are package-private; the algorithm must match exactly so that two
// implementations of "where do chunks land" agree byte-for-byte on
// content-defined boundaries.
const (
	MinChunkSize = 1 << 9  // 512 bytes
	MaxChunkSize = 1 << 14 // 16 KiB
	targetSize   = 1 << 12 // 4 KiB
	// L in the weibull distribution; matches dolt's L for compatibility.
	weibullL = targetSize
)

// levelSaltZero is the per-level salt for level-0 chunk-boundary
// hashing. Matches dolt's levelSalt[0]. Computed once at init().
var levelSaltZero = saltFromLevel(1)

// maxUint32 is the divisor used to normalise an xxHash32 result into
// the unit interval for weibullCheck. Mirrors dolt's usage.
const maxUint32 = float64(math.MaxUint32)

// CrossesBoundary reports whether a tentative chunk of size totalSize,
// ending at the location encoded by key, should become an actual chunk
// boundary. Below MinChunkSize the answer is always false; above
// MaxChunkSize it's always true; in between the weibull check decides.
func CrossesBoundary(key []byte, totalSize uint32) bool {
	if totalSize < MinChunkSize {
		return false
	}
	if totalSize > MaxChunkSize {
		return true
	}
	h := xxHash32(key, levelSaltZero)
	return weibullCheck(totalSize, totalSize, h)
}

// weibullCheck is the dolt boundary decision: at hash h, with a
// candidate chunk of size totalSize whose final record is thisSize
// bytes wide, do we split? The probability is shaped by a weibull
// distribution so chunks cluster near the target size.
func weibullCheck(totalSize, thisSize, h uint32) bool {
	pow := float64(totalSize-thisSize) / weibullL
	start := -math.Expm1(-(pow * pow * pow * pow))
	pow = float64(totalSize) / weibullL
	end := -math.Expm1(-(pow * pow * pow * pow))
	p := float64(h) / maxUint32
	d := 1 - start
	if d <= 0 {
		return true
	}
	target := (end - start) / d
	return p < target
}

// xxHash32 is the dolt-compatible 32-bit hash of b with the given
// salt. Implemented atop xxh3 for compatibility with dolt's choice.
func xxHash32(b []byte, salt uint64) uint32 {
	return uint32(xxh3.HashSeed(b, salt))
}

// saltFromLevel derives the level salt the same way dolt does:
// SHA-512 of the single byte 'level', take the first 8 bytes as a
// little-endian uint64.
func saltFromLevel(level uint8) uint64 {
	full := sha512.Sum512([]byte{level})
	return binary.LittleEndian.Uint64(full[:8])
}
