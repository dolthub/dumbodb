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

package initialsync

import (
	"context"
	"fmt"

	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/special"
	"github.com/dolthub/dumbodb/internal/types"
)

func NewSpecialDatabaseHandler(applier *special.Applier) SpecialDatabaseHandler {
	return func(ctx context.Context, client requestClient, databases []Database, opTime control.OpTime) error {
		return MaterializeSpecialDatabases(ctx, client, applier, databases, opTime)
	}
}

func MaterializeSpecialDatabases(
	ctx context.Context,
	client requestClient,
	applier *special.Applier,
	databases []Database,
	opTime control.OpTime,
) error {
	if client == nil || applier == nil || opTime == (control.OpTime{}) {
		return fmt.Errorf("special database materialization requires client, applier, and optime")
	}
	for _, database := range databases {
		if database.Name != "admin" && database.Name != "config" {
			return fmt.Errorf("database %q is not a supported special database", database.Name)
		}
		for _, collection := range database.Collections {
			namespace := database.Name + "." + collection.Name
			if special.IsIgnoredNamespace(namespace) {
				continue
			}
			if !special.IsSpecialNamespace(namespace) {
				return fmt.Errorf("%w %q", special.ErrUnsupportedSpecialNamespace, namespace)
			}
		}
	}
	for _, database := range databases {
		for _, collection := range database.Collections {
			namespace := database.Name + "." + collection.Name
			if special.IsIgnoredNamespace(namespace) {
				continue
			}
			_, err := CloneDocuments(ctx, client, CloneCursor{
				Database: database.Name, SourceUUID: collection.UUIDBinary,
			}, func(document *types.Document) error {
				return applier.ApplyInitialDocument(ctx, namespace, document, opTime)
			})
			if err != nil {
				return fmt.Errorf("cloning special namespace %s: %w", namespace, err)
			}
		}
	}
	return nil
}
