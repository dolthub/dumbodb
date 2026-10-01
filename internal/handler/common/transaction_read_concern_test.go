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

package common

import (
	"io"
	"log/slog"
	"testing"

	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/must"
)

func TestTransactionWritesAcceptReadConcern(t *testing.T) {
	readConcern := must.NotFail(types.NewDocument("afterClusterTime", types.Timestamp(1)))
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	tests := []struct {
		name string
		doc  *types.Document
		read func(*types.Document, *slog.Logger) error
	}{
		{
			name: "insert",
			doc: must.NotFail(types.NewDocument(
				"insert", "col", "$db", "db", "documents", types.MakeArray(0), "readConcern", readConcern,
			)),
			read: func(doc *types.Document, logger *slog.Logger) error {
				_, err := GetInsertParams(doc, logger)
				return err
			},
		},
		{
			name: "update",
			doc: must.NotFail(types.NewDocument(
				"update", "col", "$db", "db", "updates", types.MakeArray(0), "readConcern", readConcern,
			)),
			read: func(doc *types.Document, logger *slog.Logger) error {
				_, err := GetUpdateParams(doc, logger)
				return err
			},
		},
		{
			name: "delete",
			doc: must.NotFail(types.NewDocument(
				"delete", "col", "$db", "db", "deletes", types.MakeArray(0), "readConcern", readConcern,
			)),
			read: func(doc *types.Document, logger *slog.Logger) error {
				_, err := GetDeleteParams(doc, logger)
				return err
			},
		},
		{
			name: "findAndModify",
			doc: must.NotFail(types.NewDocument(
				"findAndModify", "col", "$db", "db", "remove", true, "readConcern", readConcern,
			)),
			read: func(doc *types.Document, logger *slog.Logger) error {
				_, err := GetFindAndModifyParams(doc, logger)
				return err
			},
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if err := test.read(test.doc, logger); err != nil {
				t.Fatal(err)
			}
		})
	}
}
