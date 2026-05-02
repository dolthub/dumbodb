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
	"context"
	"fmt"
	"strings"

	"github.com/dolthub/dolt/go/store/datas"
	"github.com/dolthub/dolt/go/store/hash"

	"github.com/dolthub/dumbodb/internal/backends"
)

// tagRefPrefix is the dataset-ID prefix for tags. It matches the namespace used
// by `dolt tag`, so tags created via DumboDBTag are visible to the Dolt CLI and
// vice versa.
const tagRefPrefix = "refs/tags/"

// DumboDBTag implements backends.VersioningBackend.
//
// Operation is selected by params:
//   - Name == "" and Delete == false   -> list all tags in the database.
//   - Name != "" and Delete == false   -> create a tag pointing at the commit that
//     params.Hash resolves to. params.Hash may be a commit hash, branch name,
//     ancestor expression (e.g. "main~3"), or another tag name. If params.Hash
//     is empty, the connection's branch HEAD (params.Branch) is used.
//   - Name != "" and Delete == true    -> delete the named tag.
//
// Tags are stored as Dolt TagValue flatbuffers via datas.Database.Tag, sharing
// the refs/tags/<name> namespace with `dolt tag`.
func (b *Backend) DumboDBTag(ctx context.Context, params *backends.TagParams) (*backends.TagResult, error) {
	db, err := b.getOrOpenDB(ctx, params.DBName, false)
	if err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: opening db %q: %w", params.DBName, err)
	}
	if db == nil {
		return nil, backends.NewError(backends.ErrorCodeDatabaseDoesNotExist,
			fmt.Errorf("dolt: DumboDBTag: database %q does not exist", params.DBName))
	}

	db.mu.Lock()
	defer db.mu.Unlock()

	switch {
	case params.Name == "" && !params.Delete:
		return dumboDBTagList(ctx, db)
	case params.Delete:
		return dumboDBTagDelete(ctx, db, params)
	default:
		return dumboDBTagCreate(ctx, db, params)
	}
}

// dumboDBTagCreate creates a tag at the resolved commit. Caller must hold db.mu.Lock().
func dumboDBTagCreate(ctx context.Context, db *dbState, params *backends.TagParams) (*backends.TagResult, error) {
	rootish := params.Hash
	if rootish == "" {
		rootish = params.Branch
	}
	if rootish == "" {
		rootish = "main"
	}

	commitAddr, err := resolveRootishToCommitHash(ctx, db, rootish)
	if err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: resolving %q: %w", rootish, err)
	}

	tagDS, err := db.doltDB.GetDataset(ctx, tagRefPrefix+params.Name)
	if err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: getting tag dataset %q: %w", params.Name, err)
	}
	if tagDS.HasHead() {
		return nil, fmt.Errorf("dolt: DumboDBTag: tag %q already exists", params.Name)
	}

	taggerName := params.Author
	if taggerName == "" {
		taggerName = "dumbodb"
	}
	taggerEmail := params.Email
	if taggerEmail == "" {
		taggerEmail = "dumbodb@dumbodb"
	}
	meta := datas.NewTagMeta(taggerName, taggerEmail, params.Message)

	if _, err = db.doltDB.Tag(ctx, tagDS, commitAddr, datas.TagOptions{Meta: meta}); err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: creating tag %q: %w", params.Name, err)
	}

	return &backends.TagResult{
		Tags: []backends.TagInfo{{
			Name:      params.Name,
			CommitID:  commitAddr.String(),
			Tagger:    meta.Name,
			Email:     meta.Email,
			Message:   meta.Description,
			Timestamp: int64(meta.Timestamp),
		}},
	}, nil
}

// dumboDBTagDelete deletes the named tag. Caller must hold db.mu.Lock().
func dumboDBTagDelete(ctx context.Context, db *dbState, params *backends.TagParams) (*backends.TagResult, error) {
	tagDS, err := db.doltDB.GetDataset(ctx, tagRefPrefix+params.Name)
	if err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: getting tag dataset %q: %w", params.Name, err)
	}
	if !tagDS.HasHead() {
		return nil, backends.NewError(backends.ErrorCodeCollectionDoesNotExist,
			fmt.Errorf("dolt: DumboDBTag: tag %q does not exist", params.Name))
	}

	commitAddr := tagCommitAddr(tagDS)

	if _, err = db.doltDB.Delete(ctx, tagDS, ""); err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: deleting tag %q: %w", params.Name, err)
	}

	return &backends.TagResult{
		Tags: []backends.TagInfo{{
			Name:     params.Name,
			CommitID: commitAddr.String(),
		}},
	}, nil
}

// dumboDBTagList lists all tags in the database. Caller must hold db.mu.Lock().
func dumboDBTagList(ctx context.Context, db *dbState) (*backends.TagResult, error) {
	dsMap, err := db.doltDB.Datasets(ctx)
	if err != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: listing datasets: %w", err)
	}

	var tagIDs []string
	if iterErr := dsMap.IterAll(ctx, func(id string, _ hash.Hash) error {
		if strings.HasPrefix(id, tagRefPrefix) {
			tagIDs = append(tagIDs, id)
		}
		return nil
	}); iterErr != nil {
		return nil, fmt.Errorf("dolt: DumboDBTag: iterating datasets: %w", iterErr)
	}

	tags := make([]backends.TagInfo, 0, len(tagIDs))
	for _, id := range tagIDs {
		ds, dsErr := db.doltDB.GetDataset(ctx, id)
		if dsErr != nil {
			return nil, fmt.Errorf("dolt: DumboDBTag: loading tag %q: %w", id, dsErr)
		}
		if !ds.HasHead() {
			continue
		}
		name := strings.TrimPrefix(id, tagRefPrefix)
		info := backends.TagInfo{Name: name}
		if ds.IsTag() {
			meta, commitAddr, htErr := ds.HeadTag()
			if htErr != nil {
				return nil, fmt.Errorf("dolt: DumboDBTag: reading tag head %q: %w", name, htErr)
			}
			info.CommitID = commitAddr.String()
			if meta != nil {
				info.Tagger = meta.Name
				info.Email = meta.Email
				info.Message = meta.Description
				info.Timestamp = int64(meta.Timestamp)
			}
		} else {
			// Legacy/test layout: tag dataset points directly at a commit.
			if addr, ok := ds.MaybeHeadAddr(); ok {
				info.CommitID = addr.String()
			}
		}
		tags = append(tags, info)
	}

	return &backends.TagResult{Tags: tags}, nil
}

// tagCommitAddr returns the commit hash referenced by a tag dataset, handling
// both the standard TagValue layout and the legacy direct-commit layout used
// by some tests.
func tagCommitAddr(tagDS datas.Dataset) hash.Hash {
	if tagDS.IsTag() {
		if _, addr, err := tagDS.HeadTag(); err == nil {
			return addr
		}
	}
	if addr, ok := tagDS.MaybeHeadAddr(); ok {
		return addr
	}
	return hash.Hash{}
}
