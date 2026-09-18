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

package control

import (
	"io"
	"log/slog"
	"sync"
	"testing"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
)

var testBackends = struct {
	sync.Mutex
	byDirectory map[string]backends.Backend
}{byDirectory: make(map[string]backends.Backend)}

func openTestStore(t testing.TB, directory string, configuration Configuration) (*Store, error) {
	t.Helper()
	return Open(testBackend(t, directory), configuration)
}

func testBackend(t testing.TB, directory string) backends.Backend {
	t.Helper()
	testBackends.Lock()
	backend := testBackends.byDirectory[directory]
	if backend == nil {
		var err error
		backend, err = dolt.NewBackend(directory, slog.New(slog.NewTextHandler(io.Discard, nil)), false, false, 0, 0)
		if err != nil {
			testBackends.Unlock()
			t.Fatal(err)
		}
		testBackends.byDirectory[directory] = backend
		t.Cleanup(func() {
			testBackends.Lock()
			delete(testBackends.byDirectory, directory)
			testBackends.Unlock()
			backend.Close()
		})
	}
	testBackends.Unlock()
	return backend
}
