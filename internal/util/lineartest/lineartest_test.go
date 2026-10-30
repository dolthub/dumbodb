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

package lineartest

import (
	"testing"
)

type recorder struct {
	testing.TB
	failed bool
}

func (r *recorder) Helper()               {}
func (r *recorder) Errorf(string, ...any) { r.failed = true }

var sink int

func spin(iterations int) {
	for i := range iterations {
		sink += i
	}
}

func TestAssert(t *testing.T) {
	linear := &recorder{TB: t}
	Assert(linear, 1000, func(n int) { spin(n * 1000) })
	if linear.failed {
		t.Error("linear work was reported as superlinear")
	}

	quadratic := &recorder{TB: t}
	Assert(quadratic, 100, func(n int) { spin(n * n * 10) })
	if !quadratic.failed {
		t.Error("quadratic work was not reported")
	}
}
