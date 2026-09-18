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

package publication

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"time"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/initialsync"
	"github.com/dolthub/dumbodb/internal/replication/topology"
)

const replicationCommitAuthor = "MongoDB Replication <replication@dumbodb>"

type commitBackend interface {
	DumboDBCommit(context.Context, *backends.CommitParams) (*backends.CommitResult, error)
	DumboDBLog(context.Context, *backends.LogParams) (*backends.LogResult, error)
}

type Publisher struct {
	backend commitBackend
	store   *control.Store
	manager *topology.Manager
}

var _ initialsync.CompletionPublisher = (*Publisher)(nil)

func NewPublisher(backend commitBackend, store *control.Store, manager *topology.Manager) (*Publisher, error) {
	if backend == nil || store == nil || manager == nil {
		return nil, errors.New("replication publisher requires backend, control store, and topology manager")
	}
	return &Publisher{backend: backend, store: store, manager: manager}, nil
}

func (p *Publisher) PublishInitialSync(ctx context.Context, publication initialsync.InitialSyncPublication) error {
	databases := make([]string, 0, len(publication.Catalog.Collections)+1)
	for _, collection := range publication.Catalog.Collections {
		databases = append(databases, collection.Location.Database)
	}
	for _, database := range publication.Catalog.SpecialDatabases {
		if database.Name == "admin" {
			databases = append(databases, database.Name)
		}
	}
	return p.Publish(ctx, publication.Attempt.BeginApply, publication.Attempt.Stop, databases, publication.Checkpoint)
}

func (p *Publisher) Publish(ctx context.Context, first, last control.OpTime, databases []string, checkpoint control.Checkpoint) error {
	publicationID, err := p.Begin(first, last, databases, checkpoint)
	if err != nil {
		if errors.Is(err, control.ErrPublicationComplete) {
			return nil
		}
		return err
	}
	if err := p.MarkReady(publicationID); err != nil {
		return err
	}
	return p.Complete(ctx, publicationID)
}

func (p *Publisher) Begin(first, last control.OpTime, databases []string, checkpoint control.Checkpoint) (string, error) {
	databases = sortedDatabases(databases)
	publicationID := publicationID(first, last, databases)
	pending := control.PendingPublication{
		ID: publicationID, First: first, Last: last, Databases: databases,
		Commits: make(map[string]string), Checkpoint: checkpoint,
	}
	if err := p.store.BeginPublication(pending); err != nil {
		return publicationID, err
	}
	return publicationID, nil
}

func (p *Publisher) MarkReady(publicationID string) error {
	return p.store.MarkPublicationReady(publicationID)
}

func (p *Publisher) Complete(ctx context.Context, publicationID string) error {
	pending, ok := p.store.PendingPublication()
	if !ok || pending.ID != publicationID {
		return fmt.Errorf("publication %q is not pending", publicationID)
	}
	if !pending.Ready {
		return fmt.Errorf("publication %q is not ready", publicationID)
	}
	databases := pending.Databases
	checkpoint := pending.Checkpoint
	first := pending.First
	last := pending.Last
	message := publicationMessage(publicationID)
	for _, database := range databases {
		current, ok := p.store.PendingPublication()
		if !ok || current.ID != publicationID {
			return fmt.Errorf("publication %q disappeared before database %q", publicationID, database)
		}
		if current.Commits[database] != "" {
			continue
		}
		commitID, err := p.existingCommit(ctx, database, message)
		if err != nil {
			return err
		}
		if commitID == "" {
			result, err := p.backend.DumboDBCommit(ctx, &backends.CommitParams{
				DBName: database, Branch: "main", Message: message,
				Author: replicationCommitAuthor, Timestamp: time.Unix(int64(last.Seconds), 0).UTC(), AllowEmpty: true,
			})
			if err != nil {
				return fmt.Errorf("committing replication publication %q for database %q: %w", publicationID, database, err)
			}
			if result == nil || result.CommitID == "" {
				return fmt.Errorf("committing replication publication %q for database %q returned no commit ID", publicationID, database)
			}
			commitID = result.CommitID
		}
		if err := p.store.RecordPublicationCommit(publicationID, database, commitID); err != nil {
			return err
		}
	}
	completed, ok := p.store.PendingPublication()
	if !ok || completed.ID != publicationID {
		return fmt.Errorf("publication %q disappeared before completion", publicationID)
	}
	commits := make([]control.DatabaseCommit, 0, len(databases))
	for _, database := range databases {
		commits = append(commits, control.DatabaseCommit{Database: database, CommitID: completed.Commits[database]})
	}
	return p.manager.PublishCommit(control.CommitInterval{
		First: first, Last: last, CommitID: publicationID, Commits: commits,
	}, checkpoint)
}

func (p *Publisher) existingCommit(ctx context.Context, database, message string) (string, error) {
	result, err := p.backend.DumboDBLog(ctx, &backends.LogParams{DBName: database, Branch: "main", Limit: 1})
	if err != nil {
		return "", fmt.Errorf("reading database %q publication head: %w", database, err)
	}
	if len(result.Commits) != 0 && result.Commits[0].Message == message {
		return result.Commits[0].CommitID, nil
	}
	return "", nil
}

func sortedDatabases(databases []string) []string {
	result := append([]string(nil), databases...)
	slices.Sort(result)
	return slices.Compact(result)
}

func publicationID(first, last control.OpTime, databases []string) string {
	hash := sha256.New()
	_, _ = fmt.Fprintf(hash, "%d:%d:%d/%d:%d:%d", first.Term, first.Seconds, first.Increment, last.Term, last.Seconds, last.Increment)
	for _, database := range databases {
		hash.Write([]byte{0})
		hash.Write([]byte(database))
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func publicationMessage(publicationID string) string {
	return "MongoDB replication publication " + publicationID
}
