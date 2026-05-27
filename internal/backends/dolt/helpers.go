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
	"crypto/sha512"
	"fmt"
	"strings"
	"time"

	"github.com/FerretDB/wire/wirebson"
	fb "github.com/dolthub/flatbuffers/v23/go"
	"github.com/dolthub/go-mysql-server/sql"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/libraries/doltcore/doltdb"
	doltref "github.com/dolthub/dolt/go/libraries/doltcore/ref"
	"github.com/dolthub/dolt/go/libraries/doltcore/dsess"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/sqlctx"
	"github.com/dolthub/dumbodb/internal/types"
)

// emptyArtifactMapSentinel is a zero hash indicating no artifact map is set.
var emptyArtifactMapSentinel [hash.ByteLen]byte

// keyDesc describes the key tuple: one binary(20) field for the SHA-512[:20] encoded MongoDB _id.
var keyDesc = val.NewTupleDescriptor(val.Type{Enc: val.ByteStringEnc, Nullable: false})

// valDesc describes the value tuple: one JSONAddr field for the JSON document hash.
var valDesc = val.NewTupleDescriptor(val.Type{Enc: val.JSONAddrEnc, Nullable: false})

// bufPool is a global buffer pool for building tuples.
var bufPool = pool.NewBuffPool()

// newEmptyMap creates an empty prolly.Map with our schema.
func newEmptyMap(ctx context.Context, ns tree.NodeStore) (prolly.Map, error) {
	return prolly.NewMapFromTuples(ctx, ns, keyDesc, valDesc)
}

// openCollection opens a prolly.Map for a collection from a hash stored in the ADRM.
// Handles both the current DTBL format and the legacy TUPM format for migration.
func openCollection(ctx context.Context, cs *nbs.GenerationalNBS, ns tree.NodeStore, collHash hash.Hash) (prolly.Map, error) {
	chunk, err := cs.Get(ctx, collHash)
	if err != nil {
		return prolly.Map{}, fmt.Errorf("reading collection chunk: %w", err)
	}

	fileID := serial.GetFileID(chunk.Data())
	switch fileID {
	case serial.ProllyTreeNodeFileID:
		// Legacy TUPM format: ADRM entry points directly to prolly.Map root node.
		node, _, err := tree.NodeFromChunk(&chunk)
		if err != nil {
			return prolly.Map{}, fmt.Errorf("parsing TUPM node: %w", err)
		}
		return prolly.NewMap(node, ns, keyDesc, valDesc), nil

	case serial.TableFileID:
		// DTBL format: read primary_index inline bytes to reconstruct the prolly.Map.
		tbl, err := serial.TryGetRootAsTable(chunk.Data(), serial.MessagePrefixSz)
		if err != nil {
			return prolly.Map{}, fmt.Errorf("parsing DTBL: %w", err)
		}
		node, _, err := tree.NodeFromBytes(tbl.PrimaryIndexBytes())
		if err != nil {
			return prolly.Map{}, fmt.Errorf("parsing DTBL primary_index: %w", err)
		}
		return prolly.NewMap(node, ns, keyDesc, valDesc), nil

	default:
		return prolly.Map{}, fmt.Errorf("unexpected file ID %q for collection (want DTBL or TUPM)", fileID)
	}
}

// buildCollectionTableSchema builds a DSCH (TableSchema) flatbuffer for the
// dumbodb collection schema: _id VARBINARY NOT NULL PK, doc JSON NOT NULL.
//
// This schema is shared across all collections within a database; the DSCH
// chunk is written once to the value store and its hash is stored in every DTBL.
//
// The physical encodings match our prolly.Map descriptors:
//   - _id VARBINARY PK -> ByteStringEnc (key tuple)
//   - doc JSON         -> JSONAddrEnc (value tuple, stores hash to JSON prolly tree)
func buildCollectionTableSchema() serial.Message {
	b := fb.NewBuilder(512)

	// Pre-build all strings before starting any object.
	idName := b.CreateString("_id")
	idSqlType := b.CreateString("binary(20)")
	docName := b.CreateString("doc")
	docSqlType := b.CreateString("json")

	// Column 0: _id BINARY(20) NOT NULL PK
	serial.ColumnStart(b)
	serial.ColumnAddName(b, idName)
	serial.ColumnAddSqlType(b, idSqlType)
	serial.ColumnAddTag(b, 1) // stable column tag
	serial.ColumnAddEncoding(b, serial.EncodingBytes)
	serial.ColumnAddPrimaryKey(b, true)
	serial.ColumnAddNullable(b, false)
	serial.ColumnAddDisplayOrder(b, 0)
	col0 := serial.ColumnEnd(b)

	// Column 1: doc JSON NOT NULL
	serial.ColumnStart(b)
	serial.ColumnAddName(b, docName)
	serial.ColumnAddSqlType(b, docSqlType)
	serial.ColumnAddTag(b, 2) // stable column tag
	serial.ColumnAddEncoding(b, serial.EncodingJSONAddr)
	serial.ColumnAddPrimaryKey(b, false)
	serial.ColumnAddNullable(b, false)
	serial.ColumnAddDisplayOrder(b, 1)
	col1 := serial.ColumnEnd(b)

	// Clustered index: key=[col0], value=[col1].
	// FlatBuffers vectors are prepended in reverse order.
	serial.IndexStartIndexColumnsVector(b, 1)
	b.PrependUint16(0) // column 0 (_id)
	koCols := b.EndVector(1)

	serial.IndexStartValueColumnsVector(b, 1)
	b.PrependUint16(1) // column 1 (doc)
	voCols := b.EndVector(1)

	serial.IndexStart(b)
	serial.IndexAddIndexColumns(b, koCols)
	serial.IndexAddKeyColumns(b, koCols)
	serial.IndexAddValueColumns(b, voCols)
	serial.IndexAddPrimaryKey(b, true)
	serial.IndexAddUniqueKey(b, true)
	clusteredIdx := serial.IndexEnd(b)

	// Columns vector [col0, col1]  -- prepend in reverse order.
	serial.TableSchemaStartColumnsVector(b, 2)
	b.PrependUOffsetT(col1)
	b.PrependUOffsetT(col0)
	colsVec := b.EndVector(2)

	// Empty secondary indexes vector.
	serial.TableSchemaStartSecondaryIndexesVector(b, 0)
	emptyIdxVec := b.EndVector(0)

	// Empty checks vector.
	serial.TableSchemaStartChecksVector(b, 0)
	emptyChecksVec := b.EndVector(0)

	serial.TableSchemaStart(b)
	serial.TableSchemaAddColumns(b, colsVec)
	serial.TableSchemaAddClusteredIndex(b, clusteredIdx)
	serial.TableSchemaAddSecondaryIndexes(b, emptyIdxVec)
	serial.TableSchemaAddChecks(b, emptyChecksVec)
	serial.TableSchemaAddCollation(b, serial.Collationutf8mb4_0900_bin)
	root := serial.TableSchemaEnd(b)

	return serial.FinishMessage(b, root, []byte(serial.TableSchemaFileID))
}

// buildDoltTableFlatbuffer builds a DTBL (Table) flatbuffer that wraps a prolly.Map
// as a proper dolt SQL table. The schema field stores a 20-byte hash referencing
// the separately-written DSCH chunk; the primary_index field inlines the prolly.Map
// root node bytes (TUPM). The secondary_indexes field inlines the given AddressMap
// (typically state.collIndexAMs[collName] or state.emptyIndexAM if there are
// no secondary indexes).
// artifactsHash is the 20-byte noms hash of the ArtifactMap node in the value store;
// pass hash.Hash{} (all zeros) when there are no conflict artifacts.
func buildDoltTableFlatbuffer(m prolly.Map, schemaHash hash.Hash, indexAM prolly.AddressMap, artifactsHash hash.Hash) serial.Message {
	b := fb.NewBuilder(1024)

	// Primary index: inline TUPM bytes (prolly.Map root node).
	rowBytes := []byte(tree.ValueFromNode(m.Node()).(dolttypes.SerialMessage))

	// Secondary indexes: per-collection ADRM (name -> IndexEntry chunk hash).
	idxBytes := []byte(tree.ValueFromNode(indexAM.Node()).(dolttypes.SerialMessage))

	// Conflict struct fields (legacy format) and violations are always zero hashes.
	var emptyHashBytes [hash.ByteLen]byte

	schOff := b.CreateByteVector(schemaHash[:])
	rowsOff := b.CreateByteVector(rowBytes)
	idxOff := b.CreateByteVector(idxBytes)
	cdataOff := b.CreateByteVector(emptyHashBytes[:])
	coursOff := b.CreateByteVector(emptyHashBytes[:])
	ctheirsOff := b.CreateByteVector(emptyHashBytes[:])
	cancOff := b.CreateByteVector(emptyHashBytes[:])

	serial.ConflictsStart(b)
	serial.ConflictsAddData(b, cdataOff)
	serial.ConflictsAddOurSchema(b, coursOff)
	serial.ConflictsAddTheirSchema(b, ctheirsOff)
	serial.ConflictsAddAncestorSchema(b, cancOff)
	conflictsOff := serial.ConflictsEnd(b)

	violationsOff := b.CreateByteVector(emptyHashBytes[:])
	// artifacts stores the 20-byte hash of the ArtifactMap node; zeros = no artifacts.
	artifactsOff := b.CreateByteVector(artifactsHash[:])

	serial.TableStart(b)
	serial.TableAddSchema(b, schOff)
	serial.TableAddPrimaryIndex(b, rowsOff)
	serial.TableAddSecondaryIndexes(b, idxOff)
	serial.TableAddConflicts(b, conflictsOff)
	serial.TableAddViolations(b, violationsOff)
	serial.TableAddArtifacts(b, artifactsOff)
	return serial.FinishMessage(b, serial.TableEnd(b), []byte(serial.TableFileID))
}

// hashID returns a stable 20-byte primary key for a MongoDB _id value.
// It serialises id to canonical BSON bytes using wirebson, then returns SHA-512(bytes)[:20].
func hashID(id any) ([20]byte, error) {
	doc := wirebson.MakeDocument(1)

	var wval any
	switch v := id.(type) {
	case int32:
		wval = v
	case int64:
		wval = v
	case float64:
		wval = v
	case types.ObjectID:
		wval = wirebson.ObjectID(v)
	case string:
		wval = v
	case types.Binary:
		wval = wirebson.Binary{B: v.B, Subtype: wirebson.BinarySubtype(v.Subtype)}
	case bool:
		wval = v
	case time.Time:
		wval = v
	case types.Decimal128:
		wval = wirebson.Decimal128{L: v.L, H: v.H}
	case *types.Document:
		wdoc, err := bson.FromDocument(v)
		if err != nil {
			return [20]byte{}, fmt.Errorf("encoding _id document for hash: %w", err)
		}
		wval = wdoc
	case *types.Array:
		warr, err := bson.FromArray(v)
		if err != nil {
			return [20]byte{}, fmt.Errorf("encoding _id array for hash: %w", err)
		}
		wval = warr
	default:
		return [20]byte{}, fmt.Errorf("unsupported _id type %T", id)
	}

	if err := doc.Add("_id", wval); err != nil {
		return [20]byte{}, fmt.Errorf("building _id doc for hash: %w", err)
	}

	raw, err := doc.Encode()
	if err != nil {
		return [20]byte{}, fmt.Errorf("encoding _id for hash: %w", err)
	}

	sum := sha512.Sum512(raw)
	var h [20]byte
	copy(h[:], sum[:20])
	return h, nil
}

// buildKey creates a key tuple for the encoded MongoDB _id bytes.
func buildKey(idBytes []byte) (val.Tuple, error) {
	tb := val.NewTupleBuilder(keyDesc, nil)
	tb.PutByteString(0, idBytes)

	tup, err := tb.Build(context.Background(), bufPool)
	if err != nil {
		return nil, fmt.Errorf("building key tuple: %w", err)
	}

	return tup, nil
}

// buildValue creates a value tuple storing a hash to a JSON prolly tree.
func buildValue(jsonHash hash.Hash) (val.Tuple, error) {
	tb := val.NewTupleBuilder(valDesc, nil)
	tb.PutJSONAddr(0, jsonHash)

	tup, err := tb.Build(context.Background(), bufPool)
	if err != nil {
		return nil, fmt.Errorf("building value tuple: %w", err)
	}

	return tup, nil
}

// dtblHashForCollection builds a DTBL from a prolly.Map for the named
// collection, inlining the collection's persisted secondary-index AddressMap
// (or the shared empty AM if the collection has no secondary indexes).
// Writes the DTBL to the value store and returns its chunk hash.
func (state *dbState) dtblHashForCollection(ctx context.Context, collName string, m prolly.Map, artifactsHash hash.Hash) (hash.Hash, error) {
	idxAM, ok := state.collIndexAMs[collName]
	if !ok {
		idxAM = state.emptyIndexAM
	}
	dtblMsg := buildDoltTableFlatbuffer(m, state.collSchemaHash, idxAM, artifactsHash)
	ref, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(dtblMsg))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing DTBL: %w", err)
	}
	return ref.TargetHash(), nil
}

// amFromWorkingRoot extracts a prolly.AddressMap from a doltdb.RootValue.
// This is a backward-compat bridge for functions that still consume the raw AM
// (e.g. the Dolt commit path). The AM is parsed from the RTVL flatbuffer that
// the RootValue wraps.
func amFromWorkingRoot(ctx context.Context, rv doltdb.RootValue, ns tree.NodeStore) (prolly.AddressMap, error) {
	msg, ok := rv.NomsValue().(dolttypes.SerialMessage)
	if !ok {
		return prolly.AddressMap{}, fmt.Errorf("unexpected RootValue noms type %T", rv.NomsValue())
	}
	rtvl, err := serial.TryGetRootAsRootValue([]byte(msg), serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing RTVL from working root: %w", err)
	}
	amNode, _, err := tree.NodeFromBytes(rtvl.TablesBytes())
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing AM from RTVL: %w", err)
	}
	return prolly.NewAddressMap(amNode, ns)
}

// getOrInitBranchWS returns the *doltdb.WorkingSet for branch, loading it from
// the doltDB if not already cached. The caller must hold state.mu (write lock).
//
// In a txn-bound context (default-mode startTransaction or session-isolation
// mode), the txn overlay lives on the calling session's branchState via
// dsess. The fork point is captured by tx.dbStartPoints when the dsess
// transaction starts (dispatched from clientconn).
func (state *dbState) getOrInitBranchWS(ctx context.Context, branch string) (*doltdb.WorkingSet, error) {
	if _, inTxn := ownerForTxn(ctx, state.backend.sessionIsolation); inTxn {
		sess := sessionFromContext(ctx)
		if sess == nil {
			return nil, fmt.Errorf("getOrInitBranchWS: in-txn write on %q/%q with no session in context", state.name, branch)
		}
		sqlCtx := sqlctx.Wrap(ctx, sess)
		qualified := qualifiedDbName(state.name, branch)
		sessState, ok, err := sess.LookupDbState(sqlCtx, qualified)
		if err != nil {
			return nil, fmt.Errorf("getOrInitBranchWS: LookupDbState for %q: %w", qualified, err)
		}
		if ok {
			if ws := sessState.WorkingSet(); ws != nil {
				return ws, nil
			}
		}
		ws, err := state.loadCommittedWS(ctx, branch)
		if err != nil {
			return nil, err
		}
		if err := sess.SetWorkingSet(sqlCtx, qualified, ws); err != nil {
			return nil, fmt.Errorf("getOrInitBranchWS: SetWorkingSet for %q: %w", qualified, err)
		}
		return ws, nil
	}

	return state.loadCommittedWS(ctx, branch)
}

func (state *dbState) loadCommittedWS(ctx context.Context, branch string) (*doltdb.WorkingSet, error) {
	if ws, ok := state.workingSets[branch]; ok {
		return ws, nil
	}
	wsRef := doltref.NewWorkingSetRef("heads/" + branch)
	ws, err := state.doltDB.ResolveWorkingSet(ctx, wsRef)
	if err != nil {
		// Branch has no working set yet; initialize from HEAD.
		rv, rvErr := headRootValueForBranch(ctx, state, branch)
		if rvErr != nil {
			return nil, fmt.Errorf("initializing working set for %q: %w", branch, rvErr)
		}
		ws = doltdb.EmptyWorkingSet(wsRef).WithWorkingRoot(rv).WithStagedRoot(rv)
	}
	state.workingSets[branch] = ws
	return ws, nil
}

// GetIfPresent (not Get): background loops (deferredFlushLoop, capped
// cleanup) run without a ConnInfo and would otherwise panic.
//
// In --session-isolation mode every connection is implicitly forked, so
// the InTransaction check is bypassed.
func ownerForTxn(ctx context.Context, sessionIsolation bool) (string, bool) {
	ci := conninfo.GetIfPresent(ctx)
	if ci == nil {
		return "", false
	}
	if sessionIsolation || ci.InTransaction() {
		return ci.Owner(), true
	}
	return "", false
}

// commitDirtyBranchesForSession commits every dirty branch on the given
// session that belongs to this dbState. Each branch is committed via
// dsess.CommitWorkingSet (the per-(db, branch) variant; CommitTransaction's
// single-dirty-branch rule does not apply). After each commit, the
// session's now-clean branchState working set is mirrored back into
// state.workingSets[branch] so non-session readers (still present until
// workspace-qsc.6) see the post-merge state.
//
// Caller must hold state.mu write lock.
func (state *dbState) commitDirtyBranchesForSession(sqlCtx *sql.Context, sess *dsess.DoltSession, tx sql.Transaction) ([]string, error) {
	var branches []string
	for _, qualified := range sess.DirtyBranchRevisions() {
		base, branch := doltdb.SplitRevisionDbName(qualified)
		if branch == "" {
			branch = defaultBranch
		}
		if !strings.EqualFold(base, state.name) {
			continue
		}
		if err := sess.CommitWorkingSet(sqlCtx, qualified, tx); err != nil {
			return branches, fmt.Errorf("commitTransaction: committing %q: %w", qualified, err)
		}
		// Mirror the post-commit working set back into state.workingSets so
		// non-session readers (workingRootViaSession's snapshot path) see
		// the merged result. Removed in workspace-qsc.6.
		sessState, ok, err := sess.LookupDbState(sqlCtx, qualified)
		if err == nil && ok {
			if newWS := sessState.WorkingSet(); newWS != nil {
				state.workingSets[branch] = newWS
			}
		}
		if state.dirtyBranches != nil {
			delete(state.dirtyBranches, branch)
		}
		branches = append(branches, branch)
	}
	return branches, nil
}

// rollbackDirtyBranchesForSession discards every dirty branch on the given
// session that belongs to this dbState. Uses dsess.Rollback to drop the
// session's branchState heads back to the transaction's start point.
//
// Caller must hold state.mu write lock.
func (state *dbState) rollbackDirtyBranchesForSession(sqlCtx *sql.Context, sess *dsess.DoltSession, tx sql.Transaction) error {
	// Rollback is session-wide in dsess; per-(db, branch) selective rollback
	// would require partitioning the session, which we don't do. The caller
	// is responsible for ensuring this is the right granularity.
	if tx == nil {
		return nil
	}
	return sess.Rollback(sqlCtx, tx)
}

// headRootValueForBranch returns the doltdb.RootValue at the HEAD of branch.
// If the branch has no commits, an empty RootValue is returned.
// The caller must hold state.mu (read or write lock).
func headRootValueForBranch(ctx context.Context, state *dbState, branch string) (doltdb.RootValue, error) {
	branchDS, err := state.datasDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil || !branchDS.HasHead() {
		return doltdb.EmptyRootValue(ctx, state.doltDB.ValueReadWriter(), state.doltDB.NodeStore())
	}
	headValue, _, err := branchDS.MaybeHeadValue()
	if err != nil {
		return nil, fmt.Errorf("reading HEAD value for branch %q: %w", branch, err)
	}
	return doltdb.NewRootValue(ctx, state.doltDB.ValueReadWriter(), state.doltDB.NodeStore(), headValue)
}

// headRootAMForBranch returns the collections AddressMap from a branch HEAD's rootValue.
// This is used by the Dolt commit path (which still works in AM terms) to get
// the staged AM. The caller must hold state.mu (read or write lock).
func headRootAMForBranch(ctx context.Context, state *dbState, branch string) (prolly.AddressMap, error) {
	rv, err := headRootValueForBranch(ctx, state, branch)
	if err != nil {
		return prolly.AddressMap{}, err
	}
	return amFromWorkingRoot(ctx, rv, state.ns)
}

// updateWorkingRoot applies fn to the current working RootValue for branch,
// stores the updated WorkingSet in state, and persists it via doltDB unless
// skipSync is true (in which case the branch is marked dirty for later flush).
// The caller must hold state.mu (write lock).
func (state *dbState) updateWorkingRoot(ctx context.Context, branch string, fn func(doltdb.RootValue) (doltdb.RootValue, error), skipSync bool) error {
	ws, err := state.getOrInitBranchWS(ctx, branch)
	if err != nil {
		return err
	}

	newRV, err := fn(ws.WorkingRoot())
	if err != nil {
		return err
	}

	stagedRV := ws.StagedRoot()
	if stagedRV == nil {
		// Staged stays at HEAD until an explicit stage; initialize from HEAD.
		stagedRV, err = headRootValueForBranch(ctx, state, branch)
		if err != nil {
			return fmt.Errorf("reading HEAD root for staged: %w", err)
		}
	}

	newWS := ws.WithWorkingRoot(newRV).WithStagedRoot(stagedRV)

	if _, inTxn := ownerForTxn(ctx, state.backend.sessionIsolation); inTxn {
		sess := sessionFromContext(ctx)
		if sess == nil {
			return fmt.Errorf("updateWorkingRoot: in-txn write on %q/%q with no session in context", state.name, branch)
		}
		sqlCtx := sqlctx.Wrap(ctx, sess)
		qualified := qualifiedDbName(state.name, branch)
		if err := sess.SetWorkingSet(sqlCtx, qualified, newWS); err != nil {
			return fmt.Errorf("updateWorkingRoot: SetWorkingSet for %q: %w", qualified, err)
		}
		return nil
	}

	state.workingSets[branch] = newWS

	if skipSync {
		if state.dirtyBranches == nil {
			state.dirtyBranches = make(map[string]struct{})
		}
		state.dirtyBranches[branch] = struct{}{}
		return nil
	}

	if err := updateWorkingSet(ctx, state.doltDB, newWS, branch); err != nil {
		return fmt.Errorf("updating working set: %w", err)
	}

	if state.dirtyBranches != nil {
		delete(state.dirtyBranches, branch)
	}
	return nil
}

// updateAddressMap is a backward-compat wrapper around updateWorkingRoot for
// callers that still use the AddressMapEditor pattern. The fn receives a
// RootValue whose AM is modified and returns the updated RootValue.
// The caller must hold state.mu (write lock).
func (state *dbState) updateAddressMap(ctx context.Context, branch string, fn func(prolly.AddressMapEditor) error) error {
	return state.updateAddressMapWithSync(ctx, branch, fn, false)
}

func (state *dbState) updateAddressMapWithSync(ctx context.Context, branch string, fn func(prolly.AddressMapEditor) error, skipSync bool) error {
	return state.updateWorkingRoot(ctx, branch, func(rv doltdb.RootValue) (doltdb.RootValue, error) {
		am, err := amFromWorkingRoot(ctx, rv, state.ns)
		if err != nil {
			return nil, err
		}
		ed := am.Editor()
		if err := fn(ed); err != nil {
			return nil, err
		}
		newAM, err := ed.Flush(ctx)
		if err != nil {
			return nil, fmt.Errorf("flushing address map: %w", err)
		}
		rtvlMsg := buildRootValueFlatbuffer(newAM)
		return doltdb.NewRootValue(ctx, state.doltDB.ValueReadWriter(), state.doltDB.NodeStore(), dolttypes.SerialMessage(rtvlMsg))
	}, skipSync)
}

func (state *dbState) flushDirtyBranches(ctx context.Context) error {
	if len(state.dirtyBranches) == 0 {
		return nil
	}
	for branch := range state.dirtyBranches {
		ws, ok := state.workingSets[branch]
		if !ok {
			continue
		}
		if err := updateWorkingSet(ctx, state.doltDB, ws, branch); err != nil {
			return fmt.Errorf("updating working set for %q: %w", branch, err)
		}
		delete(state.dirtyBranches, branch)
	}
	return nil
}

// headRootAM returns the collections AddressMap from HEAD's rootValue.
// This is the correct staged root: staged stays equal to HEAD until an
// explicit stage operation advances it.
// The caller must hold state.mu (read or write lock).
func (state *dbState) headRootAM(ctx context.Context) (prolly.AddressMap, error) {
	return headRootAMForBranch(ctx, state, defaultBranch)
}

