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

package oplog

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"time"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/replication/transport"
	"github.com/dolthub/dumbodb/internal/types"
)

var (
	ErrContinuityLost = errors.New("oplog continuity lost")
	ErrTooStale       = errors.New("requested oplog history is no longer retained")
	ErrSourceChanged  = errors.New("oplog sync source changed")
)

type fetchClient interface {
	Request(context.Context, *wire.OpMsg) (*wire.OpMsg, error)
	Exhaust(context.Context, *wire.OpMsg, func(*wire.OpMsg) (bool, error)) error
	Close() error
}

type connectorFetchClient struct {
	connector *transport.Connector
}

func newConnectorFetchClient(source, memberHost string) fetchClient {
	return &connectorFetchClient{connector: transport.NewMemberConnector(source, memberHost, []string{"snappy", "zstd", "zlib"})}
}

func (c *connectorFetchClient) Request(ctx context.Context, message *wire.OpMsg) (*wire.OpMsg, error) {
	connection, err := c.connector.Connection(ctx)
	if err != nil {
		return nil, err
	}
	response, err := connection.Request(ctx, message)
	if err == nil {
		return response, nil
	}
	_, _ = c.connector.Replace(ctx, connection)
	return nil, err
}

func (c *connectorFetchClient) Exhaust(ctx context.Context, message *wire.OpMsg, consume func(*wire.OpMsg) (bool, error)) error {
	connection, err := c.connector.Connection(ctx)
	if err != nil {
		return err
	}
	if err := connection.Exhaust(ctx, message, consume); err != nil {
		_, _ = c.connector.Replace(ctx, connection)
		return err
	}
	return nil
}

func (c *connectorFetchClient) Close() error {
	return c.connector.Close()
}

type Fetcher struct {
	manager   *topology.Manager
	buffer    *Buffer
	logger    *slog.Logger
	newClient func(string, string) fetchClient
	exhaust   bool
	retryWait time.Duration
}

func NewFetcher(manager *topology.Manager, buffer *Buffer, logger *slog.Logger) (*Fetcher, error) {
	if manager == nil || buffer == nil {
		return nil, errors.New("oplog fetcher requires topology manager and buffer")
	}
	if logger == nil {
		logger = slog.Default()
	}
	if _, ok := buffer.Tail(); !ok {
		if err := manager.ResetFetchProgress(); err != nil {
			return nil, err
		}
	}
	return &Fetcher{
		manager:   manager,
		buffer:    buffer,
		logger:    logger,
		newClient: newConnectorFetchClient,
		exhaust:   true,
		retryWait: time.Second,
	}, nil
}

func (f *Fetcher) Run(ctx context.Context) error {
	for ctx.Err() == nil {
		err := f.Fetch(ctx)
		switch {
		case err == nil:
			return nil
		case errors.Is(err, ErrTooStale):
			if transitionErr := f.manager.MarkContinuityLost(true); transitionErr != nil {
				return transitionErr
			}
			return err
		case errors.Is(err, ErrContinuityLost), errors.Is(err, topology.ErrSourceRollbackIDChanged):
			if transitionErr := f.manager.MarkContinuityLost(false); transitionErr != nil {
				return transitionErr
			}
			return err
		case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
			return ctx.Err()
		case errors.Is(err, ErrSourceChanged):
		default:
			f.logger.Warn("oplog fetch failed; retrying", "err", err)
		}
		timer := time.NewTimer(f.retryWait)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return ctx.Err()
}

func (f *Fetcher) Fetch(ctx context.Context) error {
	state := f.manager.Snapshot()
	position := state.Checkpoint.Written
	return f.fetchFrom(ctx, state, position)
}

// FetchFrom fetches inclusively from an explicit initial-sync continuity position.
func (f *Fetcher) FetchFrom(ctx context.Context, position control.OpTime) error {
	if position == (control.OpTime{}) {
		return errors.New("explicit oplog fetch position is required")
	}
	return f.fetchFrom(ctx, f.manager.Snapshot(), position)
}

func (f *Fetcher) fetchFrom(ctx context.Context, state topology.Snapshot, position control.OpTime) error {
	source := state.SyncSource
	if source == "" {
		return errors.New("no eligible oplog sync source")
	}
	client := f.newClient(source, state.MemberHost)
	defer client.Close()
	rbid, err := fetchRollbackID(ctx, client)
	if err != nil {
		return err
	}
	if err := f.manager.ObserveSourceRBID(source, rbid); err != nil {
		return err
	}
	if tail, ok := f.buffer.Tail(); ok && tail.Compare(position) > 0 {
		position = tail
	}
	response, err := client.Request(ctx, oplogFindRequest(position, state.Term))
	if err != nil {
		return err
	}
	continuityPending := position != (control.OpTime{})
	cursorID, err := f.consumeResponse(ctx, source, response, position, &continuityPending)
	if err != nil {
		return err
	}
	for cursorID != 0 {
		if f.manager.Snapshot().SyncSource != source {
			return ErrSourceChanged
		}
		request := oplogGetMoreRequest(cursorID, f.manager.Snapshot().Term)
		if !f.exhaust {
			response, err = client.Request(ctx, request)
			if err != nil {
				return err
			}
			cursorID, err = f.consumeResponse(ctx, source, response, position, &continuityPending)
			if err != nil {
				return err
			}
			continue
		}
		request.Flags = wire.OpMsgFlags(wire.OpMsgExhaustAllowed)
		err = client.Exhaust(ctx, request, func(response *wire.OpMsg) (bool, error) {
			var consumeErr error
			cursorID, consumeErr = f.consumeResponse(ctx, source, response, position, &continuityPending)
			if consumeErr != nil {
				return false, consumeErr
			}
			return cursorID != 0 && f.manager.Snapshot().SyncSource == source, nil
		})
		if err != nil {
			return err
		}
	}
	if continuityPending {
		return ErrTooStale
	}
	return io.EOF
}

func (f *Fetcher) consumeResponse(ctx context.Context, source string, response *wire.OpMsg, expected control.OpTime, continuityPending *bool) (int64, error) {
	if f.manager.Snapshot().SyncSource != source {
		return 0, ErrSourceChanged
	}
	document, err := decodeResponse(response)
	if err != nil {
		return 0, err
	}
	metadata, err := parseResponseMetadata(document)
	if err != nil {
		return 0, err
	}
	if err := f.manager.ObserveSourceRBID(source, metadata.RBID); err != nil {
		return 0, err
	}
	state := f.manager.Snapshot()
	if state.Configuration != nil {
		if len(state.Configuration.ReplicaSetID) == 24 && state.Configuration.ReplicaSetID != metadata.ReplicaSetID {
			return 0, fmt.Errorf("%w: source replica-set ID changed", ErrContinuityLost)
		}
		if state.Configuration.Term != metadata.ConfigTerm || state.Configuration.Version != metadata.ConfigVersion {
			return 0, fmt.Errorf("%w: source reports configuration term %d version %d", ErrSourceChanged, metadata.ConfigTerm, metadata.ConfigVersion)
		}
	}
	sourceMemberID := memberIDForSource(state, source)
	if sourceMemberID >= 0 {
		sourceState := topology.StateSecondary
		if metadata.SourceIsPrimary {
			sourceState = topology.StatePrimary
		}
		if err := f.manager.ObserveHeartbeat(source, topology.Heartbeat{
			SetName: state.SetName, MemberID: sourceMemberID, State: sourceState,
			Term: metadata.Term, PrimaryID: memberIDAtConfigurationIndex(state, metadata.PrimaryIndex),
			Applied: metadata.LastApplied, Written: metadata.LastWritten,
		}); err != nil {
			return 0, err
		}
	}
	cursorID, entries, err := responseBatch(document)
	if err != nil {
		return 0, err
	}
	for _, entryDocument := range entries {
		entry, err := ParseEntry(entryDocument)
		if err != nil {
			return 0, err
		}
		if *continuityPending {
			*continuityPending = false
			if entry.OpTime == expected {
				continue
			}
			if timestampCompare(entry.OpTime, expected) > 0 {
				return 0, fmt.Errorf("%w: expected %v, first source entry is %v", ErrTooStale, expected, entry.OpTime)
			}
			return 0, fmt.Errorf("%w: expected %v, first source entry is %v", ErrContinuityLost, expected, entry.OpTime)
		}
		for {
			err = f.buffer.TryAppend(entry)
			if !errors.Is(err, ErrBufferFull) {
				break
			}
			if err = f.buffer.WaitForSpace(ctx, int64(len(entry.RawBSON))); err != nil {
				return 0, err
			}
		}
		if err != nil {
			return 0, err
		}
		if err := f.manager.AdvanceFetched(entry.OpTime, entry.OpTime); err != nil {
			return 0, err
		}
	}
	return cursorID, nil
}

func fetchRollbackID(ctx context.Context, client fetchClient) (int64, error) {
	response, err := client.Request(ctx, wire.MustOpMsg("replSetGetRBID", int32(1), "$replData", int32(1), "$db", "admin"))
	if err != nil {
		return 0, err
	}
	document, err := decodeResponse(response)
	if err != nil {
		return 0, err
	}
	value, _ := document.Get("rbid")
	rbid, err := entryInteger(value)
	if err != nil {
		return 0, fmt.Errorf("replSetGetRBID rbid: %w", err)
	}
	return rbid, nil
}

func oplogFindRequest(position control.OpTime, term int64) *wire.OpMsg {
	gte := wirebson.MakeDocument(1)
	_ = gte.Add("$gte", wirebson.Timestamp(uint64(position.Seconds)<<32|uint64(position.Increment)))
	filter := wirebson.MakeDocument(1)
	_ = filter.Add("ts", gte)
	readConcern := wirebson.MakeDocument(2)
	_ = readConcern.Add("level", "local")
	_ = readConcern.Add("afterClusterTime", wirebson.Timestamp(1))
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	return wire.MustOpMsg(
		"find", "oplog.rs",
		"filter", filter,
		"batchSize", int32(4096),
		"tailable", true,
		"awaitData", true,
		"term", term,
		"maxTimeMS", int64(60000),
		"readConcern", readConcern,
		"$replData", int32(1),
		"$oplogQueryData", int32(1),
		"$readPreference", readPreference,
		"$db", "local",
	)
}

func oplogGetMoreRequest(cursorID, term int64) *wire.OpMsg {
	lastCommitted := wirebson.MakeDocument(2)
	_ = lastCommitted.Add("ts", wirebson.Timestamp(0))
	_ = lastCommitted.Add("t", int64(-1))
	readPreference := wirebson.MakeDocument(1)
	_ = readPreference.Add("mode", "secondaryPreferred")
	return wire.MustOpMsg(
		"getMore", cursorID,
		"collection", "oplog.rs",
		"batchSize", int32(4096),
		"maxTimeMS", int64(5000),
		"term", term,
		"lastKnownCommittedOpTime", lastCommitted,
		"$replData", int32(1),
		"$oplogQueryData", int32(1),
		"$readPreference", readPreference,
		"$db", "local",
	)
}

func decodeResponse(message *wire.OpMsg) (*types.Document, error) {
	raw, err := message.RawDocument()
	if err != nil {
		return nil, err
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		return nil, err
	}
	if !responseOK(document) {
		value, _ := document.Get("errmsg")
		return nil, fmt.Errorf("MongoDB replication command failed: %v", value)
	}
	return document, nil
}

func responseBatch(document *types.Document) (int64, []*types.Document, error) {
	cursorValue, _ := document.Get("cursor")
	cursor, ok := cursorValue.(*types.Document)
	if !ok {
		return 0, nil, fmt.Errorf("oplog response cursor has type %T, want document", cursorValue)
	}
	idValue, _ := cursor.Get("id")
	id, err := entryInteger(idValue)
	if err != nil {
		return 0, nil, fmt.Errorf("oplog cursor id: %w", err)
	}
	batchValue, _ := cursor.Get("firstBatch")
	if batchValue == nil {
		batchValue, _ = cursor.Get("nextBatch")
	}
	batch, ok := batchValue.(*types.Array)
	if !ok {
		return 0, nil, fmt.Errorf("oplog cursor batch has type %T, want array", batchValue)
	}
	entries := make([]*types.Document, 0, batch.Len())
	for index := 0; index < batch.Len(); index++ {
		value, getErr := batch.Get(index)
		if getErr != nil {
			return 0, nil, getErr
		}
		entry, ok := value.(*types.Document)
		if !ok {
			return 0, nil, fmt.Errorf("oplog batch entry %d has type %T, want document", index, value)
		}
		entries = append(entries, entry)
	}
	return id, entries, nil
}

func timestampCompare(left, right control.OpTime) int {
	if left.Seconds != right.Seconds {
		if left.Seconds < right.Seconds {
			return -1
		}
		return 1
	}
	if left.Increment < right.Increment {
		return -1
	}
	if left.Increment > right.Increment {
		return 1
	}
	return 0
}

func memberIDForSource(state topology.Snapshot, source string) int {
	if state.Configuration == nil {
		return -1
	}
	for _, member := range state.Configuration.Members {
		if member.Host == source {
			return member.MemberID
		}
	}
	return -1
}

func memberIDAtConfigurationIndex(state topology.Snapshot, index int) int {
	if state.Configuration == nil || index < 0 || index >= len(state.Configuration.Members) {
		return -1
	}
	return state.Configuration.Members[index].MemberID
}

func responseOK(document *types.Document) bool {
	value, _ := document.Get("ok")
	switch value := value.(type) {
	case int32:
		return value == 1
	case int64:
		return value == 1
	case float64:
		return value == 1
	default:
		return false
	}
}
