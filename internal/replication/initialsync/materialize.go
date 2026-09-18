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
	"github.com/dolthub/dumbodb/internal/types"
)

type MaterializeResult struct {
	Location    catalog.Location
	ResumeToken *types.Document
	Loader      LoaderStats
}

func MaterializeCollection(
	ctx context.Context,
	client requestClient,
	catalogApplier *catalog.Applier,
	database string,
	collection Collection,
	createOpTime control.OpTime,
	limits LoaderLimits,
	resumeAfter *types.Document,
	progress func(LoaderStats),
) (MaterializeResult, error) {
	if client == nil || catalogApplier == nil || database == "" || collection.Name == "" {
		return MaterializeResult{}, errors.New("collection materialization requires client, catalog applier, database, and name")
	}
	plan, err := catalog.PreflightCollection(database, collection.Name, collection.Options, collection.Indexes)
	if err != nil {
		return MaterializeResult{}, err
	}
	if collection.SourceUUID == "" {
		return MaterializeResult{}, errors.New("collection materialization requires a source UUID")
	}
	location, err := catalogApplier.Create(ctx, database, plan.Create, collection.SourceUUID, createOpTime)
	if err != nil {
		return MaterializeResult{}, err
	}
	_, destination, err := catalogApplier.ResolveCollection(ctx, collection.SourceUUID)
	if err != nil {
		return MaterializeResult{}, err
	}
	loader, err := NewBoundedCollectionLoader(destination, limits)
	if err != nil {
		return MaterializeResult{}, err
	}
	result := MaterializeResult{Location: location, ResumeToken: resumeAfter}
	resumeToken, err := CloneDocumentBatches(ctx, client, CloneCursor{
		Database: database, SourceUUID: collection.UUIDBinary, ResumeAfter: resumeAfter,
	}, func(documents []*types.Document, _ *types.Document) error {
		for _, document := range documents {
			if err := loader.Add(ctx, document); err != nil {
				return err
			}
		}
		if err := loader.Flush(ctx); err != nil {
			return err
		}
		if progress != nil {
			progress(loader.Stats())
		}
		return nil
	})
	result.ResumeToken = resumeToken
	result.Loader = loader.Stats()
	if err != nil {
		var unsupported *UnsupportedBSONTypeError
		if errors.As(err, &unsupported) {
			return result, &UnsupportedBSONTypeError{
				Namespace: database + "." + collection.Name,
				BSONType:  unsupported.BSONType,
			}
		}
		return result, fmt.Errorf("cloning %s.%s: %w", database, collection.Name, err)
	}
	if err := catalogApplier.CreateIndexes(ctx, collection.SourceUUID, plan.Indexes); err != nil {
		return result, fmt.Errorf("building %s.%s indexes: %w", database, collection.Name, err)
	}
	return result, nil
}
