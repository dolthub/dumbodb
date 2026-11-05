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

package dolt

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestNewBackendRefusesMissingDataDir(t *testing.T) {
	missingDataDir := filepath.Join(t.TempDir(), "nope")

	b, err := newBackend(missingDataDir, slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), missingDataDir)
	assert.Contains(t, err.Error(), "not found")
	assert.NoDirExists(t, missingDataDir)
}

func TestNewBackendRefusesDataDirThatIsAFile(t *testing.T) {
	dataFile := filepath.Join(t.TempDir(), "file")
	require.NoError(t, os.WriteFile(dataFile, nil, 0o644))

	b, err := newBackend(dataFile, slog.New(slog.NewTextHandler(io.Discard, nil)), false, 0, 0)
	require.Error(t, err)
	assert.Nil(t, b)
	assert.Contains(t, err.Error(), "is not a directory")
}
