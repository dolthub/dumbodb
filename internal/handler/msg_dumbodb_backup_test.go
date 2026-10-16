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

package handler

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

func TestRequireBackupFields(t *testing.T) {
	full := backends.BackupParams{DBName: "mydb", Name: "nightly", URL: "file:///x", As: "copy"}
	required := map[string][]string{
		"add":      {"db", "name", "url"},
		"remove":   {"db", "name"},
		"sync":     {"db", "name"},
		"sync-url": {"db", "url"},
		"list":     {"db"},
		"restore":  {"url", "as"},
	}
	for action, fields := range required {
		p := full
		p.Action = action
		require.NoError(t, requireBackupFields(&p), action)

		for _, field := range fields {
			p := full
			p.Action = action
			switch field {
			case "db":
				p.DBName = ""
			case "name":
				p.Name = ""
			case "url":
				p.URL = ""
			case "as":
				p.As = ""
			}
			err := requireBackupFields(&p)
			var ce *handlererrors.CommandError
			require.ErrorAs(t, err, &ce, "%s without %s", action, field)
			require.Equal(t, handlererrors.ErrBadValue, ce.Code(), "%s without %s", action, field)
		}
	}

	p := full
	p.Action = "bogus"
	require.Error(t, requireBackupFields(&p))
}
