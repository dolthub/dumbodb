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
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/replication/catalog"
	"github.com/dolthub/dumbodb/internal/replication/control"
)

// CatalogMaterialization reports ordinary clone work and databases requiring special translation.
type CatalogMaterialization struct {
	Collections      []MaterializeResult
	SpecialDatabases []Database
}

// MaterializeOrdinaryCatalog preflights and clones every ordinary source collection in stable catalog order.
func MaterializeOrdinaryCatalog(
	ctx context.Context,
	client requestClient,
	catalogApplier *catalog.Applier,
	databases []Database,
	createOpTime control.OpTime,
	limits LoaderLimits,
) (CatalogMaterialization, error) {
	if client == nil || catalogApplier == nil || createOpTime == (control.OpTime{}) {
		return CatalogMaterialization{}, errors.New("catalog materialization requires client, catalog applier, and create optime")
	}
	result := CatalogMaterialization{}
	for _, database := range databases {
		if database.Name == "" {
			return CatalogMaterialization{}, errors.New("source catalog contains an unnamed database")
		}
		if database.Special {
			result.SpecialDatabases = append(result.SpecialDatabases, database)
			continue
		}
		for _, collection := range database.Collections {
			if _, err := catalog.PreflightCollection(database.Name, collection.Name, collection.Options, collection.Indexes); err != nil {
				return CatalogMaterialization{}, fmt.Errorf("preflighting initial sync catalog: %w", err)
			}
		}
	}
	for _, database := range databases {
		if database.Special {
			continue
		}
		for _, collection := range database.Collections {
			materialized, err := MaterializeCollection(ctx, client, catalogApplier, database.Name, collection, createOpTime, limits, nil)
			if err != nil {
				return result, err
			}
			result.Collections = append(result.Collections, materialized)
		}
	}
	return result, nil
}
