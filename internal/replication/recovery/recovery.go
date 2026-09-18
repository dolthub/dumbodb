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

package recovery

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"
	"github.com/google/uuid"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/replication/transport"
	"github.com/dolthub/dumbodb/internal/types"
)

var ErrInitialSyncRequired = errors.New("rollback has no safely restorable common point")

type backend interface {
	backends.Backend
	backends.VersioningBackend
}

type sourceClient interface {
	Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error)
	Close() error
}

type connectorClient struct {
	connector *transport.Connector
}

func newSourceClient(source, memberHost string) sourceClient {
	return &connectorClient{connector: transport.NewMemberConnector(source, memberHost, []string{"snappy", "zstd", "zlib"})}
}

func (c *connectorClient) Request(ctx context.Context, request *wire.OpMsg) (*wire.OpMsg, error) {
	connection, err := c.connector.Connection(ctx)
	if err != nil {
		return nil, err
	}
	response, err := connection.Request(ctx, request)
	if err == nil {
		return response, nil
	}
	_, _ = c.connector.Replace(ctx, connection)
	return nil, err
}

func (c *connectorClient) Close() error {
	return c.connector.Close()
}

type Recovery struct {
	backend backend
	store   *control.Store
	manager *topology.Manager
	client  func(string, string) sourceClient
}

func New(backendValue backends.Backend, store *control.Store, manager *topology.Manager) (*Recovery, error) {
	versioned, ok := backendValue.(backend)
	if !ok || store == nil || manager == nil {
		return nil, errors.New("rollback recovery requires versioned backend, control store, and topology manager")
	}
	return &Recovery{backend: versioned, store: store, manager: manager, client: newSourceClient}, nil
}

func (r *Recovery) Run(ctx context.Context) error {
	if pending, ok := r.store.PendingRollback(); ok {
		return r.apply(ctx, pending)
	}
	state := r.manager.Snapshot()
	if state.SyncSource == "" {
		return errors.New("rollback recovery requires a sync source")
	}
	client := r.client(state.SyncSource, state.MemberHost)
	defer client.Close()
	rbid, err := sourceRBID(ctx, client)
	if err != nil {
		return err
	}
	intervals := r.store.CommitIntervals()
	var common control.CommitInterval
	for index := len(intervals) - 1; index >= 0; index-- {
		present, err := sourceHasOpTime(ctx, client, intervals[index].Last)
		if err != nil {
			return err
		}
		if present && r.store.CanRollbackTo(intervals[index].Last) {
			common = intervals[index]
			break
		}
	}
	if common.CommitID == "" {
		if err := r.preserveAllHeads(ctx, auditBranchName(state.Checkpoint.Applied, fmt.Sprintf("r%d", rbid))); err != nil {
			return err
		}
		if err := r.manager.ResetInitialSync(""); err != nil {
			return err
		}
		return ErrInitialSyncRequired
	}
	attempt, err := r.plan(ctx, state.SyncSource, rbid, common)
	if err != nil {
		return err
	}
	if err := r.store.BeginRollback(attempt); err != nil {
		return err
	}
	return r.apply(ctx, attempt)
}

func (r *Recovery) plan(ctx context.Context, source string, rbid int64, common control.CommitInterval) (control.RollbackAttempt, error) {
	result, err := r.backend.ListDatabases(ctx, nil)
	if err != nil {
		return control.RollbackAttempt{}, err
	}
	heads := r.store.DatabaseHeadsAt(common.Last)
	targets := make(map[string]string, len(heads))
	for _, commit := range heads {
		targets[commit.Database] = commit.CommitID
	}
	databases := make([]control.RollbackDatabase, 0, len(result.Databases))
	for _, database := range result.Databases {
		if !replicatedDatabase(database.Name) {
			continue
		}
		target := targets[database.Name]
		if target == "" {
			target, err = r.initialCommit(ctx, database.Name)
			if err != nil {
				return control.RollbackAttempt{}, err
			}
		}
		databases = append(databases, control.RollbackDatabase{Database: database.Name, CommitID: target})
	}
	slices.SortFunc(databases, func(left, right control.RollbackDatabase) int {
		return strings.Compare(left.Database, right.Database)
	})
	attemptID := uuid.NewString()
	return control.RollbackAttempt{
		ID: attemptID, Source: source, SourceRBID: rbid,
		CommitID: common.CommitID, OpTime: common.Last,
		AuditBranch: auditBranchName(common.Last, attemptID), Databases: databases,
	}, nil
}

func (r *Recovery) apply(ctx context.Context, attempt control.RollbackAttempt) error {
	for _, database := range attempt.Databases {
		if err := r.ensureAuditBranch(ctx, database.Database, attempt.AuditBranch); err != nil {
			return err
		}
	}
	for _, database := range attempt.Databases {
		if _, err := r.backend.DumboDBReset(ctx, &backends.ResetParams{
			DBName: database.Database, Branch: "main", CommitID: database.CommitID, Hard: true,
		}); err != nil {
			return fmt.Errorf("resetting database %q to rollback common point: %w", database.Database, err)
		}
	}
	return r.manager.CompleteRollback(attempt.CommitID, attempt.Source, attempt.SourceRBID)
}

func (r *Recovery) preserveAllHeads(ctx context.Context, branch string) error {
	result, err := r.backend.ListDatabases(ctx, nil)
	if err != nil {
		return err
	}
	for _, database := range result.Databases {
		if !replicatedDatabase(database.Name) {
			continue
		}
		if err := r.ensureAuditBranch(ctx, database.Name, branch); err != nil {
			return err
		}
	}
	return nil
}

func replicatedDatabase(database string) bool {
	return database != "config" && database != "local"
}

func (r *Recovery) ensureAuditBranch(ctx context.Context, database, branch string) error {
	listed, err := r.backend.DumboDBBranch(ctx, &backends.BranchParams{DBName: database, Action: "list"})
	if err != nil {
		return err
	}
	for _, existing := range listed.Branches {
		if existing.Name == branch {
			return nil
		}
	}
	_, err = r.backend.DumboDBBranch(ctx, &backends.BranchParams{
		DBName: database, Action: "add", From: "main", Name: branch,
	})
	return err
}

func (r *Recovery) initialCommit(ctx context.Context, database string) (string, error) {
	from := []string(nil)
	var oldest string
	for {
		result, err := r.backend.DumboDBLog(ctx, &backends.LogParams{
			DBName: database, Branch: "main", Limit: 1000, From: from,
		})
		if err != nil {
			return "", err
		}
		for _, commit := range result.Commits {
			oldest = commit.CommitID
		}
		if len(result.Next) == 0 {
			break
		}
		from = result.Next
	}
	if oldest == "" {
		return "", fmt.Errorf("database %q has no initial commit", database)
	}
	return oldest, nil
}

func auditBranchName(opTime control.OpTime, identity string) string {
	return fmt.Sprintf("mongo-rollback-%d-%d-t%d-%s", opTime.Seconds, opTime.Increment, opTime.Term, identity)
}

func sourceRBID(ctx context.Context, client sourceClient) (int64, error) {
	document, err := requestDocument(ctx, client, wire.MustOpMsg(
		"replSetGetRBID", int32(1), "$replData", int32(1), "$db", "admin",
	))
	if err != nil {
		return 0, err
	}
	value, _ := document.Get("rbid")
	return integer(value)
}

func sourceHasOpTime(ctx context.Context, client sourceClient, opTime control.OpTime) (bool, error) {
	filter := wirebson.MakeDocument(2)
	_ = filter.Add("ts", wirebson.Timestamp(uint64(opTime.Seconds)<<32|uint64(opTime.Increment)))
	_ = filter.Add("t", opTime.Term)
	readConcern := wirebson.MakeDocument(1)
	_ = readConcern.Add("level", "local")
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	document, err := requestDocument(ctx, client, wire.MustOpMsg(
		"find", "oplog.rs", "filter", filter, "limit", int32(1),
		"readConcern", readConcern, "$replData", int32(1), "$oplogQueryData", int32(1),
		"$readPreference", readPreference, "$db", "local",
	))
	if err != nil {
		return false, err
	}
	cursorValue, _ := document.Get("cursor")
	cursor, ok := cursorValue.(*types.Document)
	if !ok {
		return false, fmt.Errorf("oplog common-point cursor has type %T", cursorValue)
	}
	batchValue, _ := cursor.Get("firstBatch")
	batch, ok := batchValue.(*types.Array)
	if !ok {
		return false, fmt.Errorf("oplog common-point batch has type %T", batchValue)
	}
	return batch.Len() != 0, nil
}

func requestDocument(ctx context.Context, client sourceClient, request *wire.OpMsg) (*types.Document, error) {
	response, err := client.Request(ctx, request)
	if err != nil {
		return nil, err
	}
	raw, err := response.RawDocument()
	if err != nil {
		return nil, err
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		return nil, err
	}
	value, _ := document.Get("ok")
	if ok, valid := value.(float64); !valid || ok != 1 {
		message, _ := document.Get("errmsg")
		return nil, fmt.Errorf("MongoDB recovery command failed: %v", message)
	}
	return document, nil
}

func integer(value any) (int64, error) {
	switch value := value.(type) {
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	default:
		return 0, fmt.Errorf("integer has type %T", value)
	}
}
