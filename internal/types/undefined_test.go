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

package types

import (
	"bytes"
	"testing"

	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestUndefinedEncoding(t *testing.T) {
	doc := must.NotFail(NewDocument("u", Undefined))
	got := doc.AppendBSON(nil)
	want := []byte{0x08, 0, 0, 0, 0x06, 'u', 0x00, 0x00}
	if !bytes.Equal(got, want) {
		t.Fatalf("AppendBSON = % x, want % x", got, want)
	}
	if doc.BSONSize() != len(want) {
		t.Fatalf("BSONSize = %d, want %d", doc.BSONSize(), len(want))
	}
}

func TestUndefinedOrdering(t *testing.T) {
	if Compare(Undefined, Undefined) != Equal {
		t.Fatal("undefined must equal undefined")
	}
	if Compare(Undefined, Null) == Equal {
		t.Fatal("undefined must not equal null")
	}
	for _, tc := range []struct {
		lo, hi any
	}{
		{MinKey, Undefined},
		{Undefined, Null},
		{Undefined, int32(0)},
	} {
		if CompareOrder(tc.lo, tc.hi, Ascending) != Less {
			t.Errorf("%v must sort before %v", tc.lo, tc.hi)
		}
	}
}
