// Copyright 2024 Dolt Inc.
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

	fb "github.com/dolthub/flatbuffers/v23/go"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"
)

// keyDesc describes the key tuple: one Int64 field for RecordID.
var keyDesc = val.NewTupleDescriptor(val.Type{Enc: val.Int64Enc, Nullable: false})

// valDesc describes the value tuple: one BytesAddr field for the BSON blob hash.
var valDesc = val.NewTupleDescriptor(val.Type{Enc: val.BytesAddrEnc, Nullable: false})

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
// dongo collection schema: _id BIGINT NOT NULL PK, doc LONGBLOB NOT NULL.
//
// This schema is shared across all collections within a database; the DSCH
// chunk is written once to the value store and its hash is stored in every DTBL.
//
// The physical encodings match our existing prolly.Map descriptors:
//   - _id BIGINT PK → Int64Enc (key tuple)
//   - doc LONGBLOB   → BytesAddrEnc (value tuple, stores hash to BSON bytes)
func buildCollectionTableSchema() serial.Message {
	b := fb.NewBuilder(512)

	// Pre-build all strings before starting any object.
	idName := b.CreateString("_id")
	idSqlType := b.CreateString("bigint")
	docName := b.CreateString("doc")
	docSqlType := b.CreateString("longblob")

	// Column 0: _id BIGINT NOT NULL PK
	serial.ColumnStart(b)
	serial.ColumnAddName(b, idName)
	serial.ColumnAddSqlType(b, idSqlType)
	serial.ColumnAddTag(b, 1) // stable column tag
	serial.ColumnAddEncoding(b, serial.EncodingInt64)
	serial.ColumnAddPrimaryKey(b, true)
	serial.ColumnAddNullable(b, false)
	serial.ColumnAddDisplayOrder(b, 0)
	col0 := serial.ColumnEnd(b)

	// Column 1: doc LONGBLOB NOT NULL
	serial.ColumnStart(b)
	serial.ColumnAddName(b, docName)
	serial.ColumnAddSqlType(b, docSqlType)
	serial.ColumnAddTag(b, 2) // stable column tag
	serial.ColumnAddEncoding(b, serial.EncodingBytesAddr)
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

	// Columns vector [col0, col1] — prepend in reverse order.
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
// root node bytes (TUPM). The secondary_indexes field inlines an empty ADRM.
func buildDoltTableFlatbuffer(m prolly.Map, schemaHash hash.Hash, emptyIndexAM prolly.AddressMap) serial.Message {
	b := fb.NewBuilder(1024)

	// Primary index: inline TUPM bytes (prolly.Map root node).
	rowBytes := []byte(tree.ValueFromNode(m.Node()).(dolttypes.SerialMessage))

	// Secondary indexes: empty ADRM.
	idxBytes := []byte(tree.ValueFromNode(emptyIndexAM.Node()).(dolttypes.SerialMessage))

	// All conflict/violation/artifact fields are zero hashes.
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
	artifactsOff := b.CreateByteVector(emptyHashBytes[:])

	serial.TableStart(b)
	serial.TableAddSchema(b, schOff)
	serial.TableAddPrimaryIndex(b, rowsOff)
	serial.TableAddSecondaryIndexes(b, idxOff)
	serial.TableAddConflicts(b, conflictsOff)
	serial.TableAddViolations(b, violationsOff)
	serial.TableAddArtifacts(b, artifactsOff)
	return serial.FinishMessage(b, serial.TableEnd(b), []byte(serial.TableFileID))
}

// buildKey creates a key tuple for a RecordID.
func buildKey(recordID int64) (val.Tuple, error) {
	tb := val.NewTupleBuilder(keyDesc, nil)
	tb.PutInt64(0, recordID)

	tup, err := tb.Build(bufPool)
	if err != nil {
		return nil, fmt.Errorf("dolt: building key tuple: %w", err)
	}

	return tup, nil
}

// buildValue creates a value tuple storing a hash to BSON bytes.
func buildValue(bsonHash hash.Hash) (val.Tuple, error) {
	tb := val.NewTupleBuilder(valDesc, nil)
	tb.PutBytesAddr(0, bsonHash)

	tup, err := tb.Build(bufPool)
	if err != nil {
		return nil, fmt.Errorf("dolt: building value tuple: %w", err)
	}

	return tup, nil
}

// dtblHashForMap builds a DTBL from a prolly.Map, writes it to the value store,
// and returns the hash of the DTBL chunk. The DTBL becomes the new ADRM entry
// for the collection.
func (state *dbState) dtblHashForMap(ctx context.Context, m prolly.Map) (hash.Hash, error) {
	dtblMsg := buildDoltTableFlatbuffer(m, state.collSchemaHash, state.emptyIndexAM)
	ref, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(dtblMsg))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing DTBL: %w", err)
	}
	return ref.TargetHash(), nil
}

// updateAddressMap applies a mutation to the collections address map and
// persists it to the dolt working set only. HEAD stays at the last explicit
// commit; only working_root_addr advances. staged_root_addr stays at HEAD's
// rootValue so that `dolt status` shows "Changes not staged for commit".
// The caller must hold state.mu (write lock).
func (state *dbState) updateAddressMap(ctx context.Context, fn func(prolly.AddressMapEditor) error) error {
	editor := state.am.Editor()

	if err := fn(editor); err != nil {
		return err
	}

	newAM, err := editor.Flush(ctx)
	if err != nil {
		return fmt.Errorf("dolt: flushing address map: %w", err)
	}

	// Write the working RTVL chunk to the value store so the working set reference is
	// resolvable. updateWorkingSet recomputes the same hash deterministically.
	workingRtvlMsg := buildRootValueFlatbuffer(newAM)
	if _, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(workingRtvlMsg)); err != nil {
		return fmt.Errorf("dolt: writing RTVL for working set: %w", err)
	}

	// Get the staged AM from HEAD's rootValue. Staged stays at HEAD until an
	// explicit stage operation advances it.
	stagedAM, err := state.headRootAM(ctx)
	if err != nil {
		return fmt.Errorf("dolt: reading HEAD AM for staged root: %w", err)
	}

	if err := updateWorkingSet(ctx, state.doltDB, newAM, stagedAM); err != nil {
		return fmt.Errorf("dolt: updating working set: %w", err)
	}

	state.am = newAM

	return nil
}

// headRootAM returns the collections AddressMap from HEAD's rootValue.
// This is the correct staged root: staged stays equal to HEAD until an
// explicit stage operation advances it.
// The caller must hold state.mu (read or write lock).
func (state *dbState) headRootAM(ctx context.Context) (prolly.AddressMap, error) {
	if !state.ds.HasHead() {
		return prolly.NewEmptyAddressMap(state.ns)
	}
	headValue, _, err := state.ds.MaybeHeadValue()
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading HEAD value: %w", err)
	}
	headMsg, ok := headValue.(dolttypes.SerialMessage)
	if !ok {
		return prolly.AddressMap{}, fmt.Errorf("unexpected HEAD value type %T", headValue)
	}
	rtvl, err := serial.TryGetRootAsRootValue([]byte(headMsg), serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing HEAD RTVL: %w", err)
	}
	amNode, _, err := tree.NodeFromBytes(rtvl.TablesBytes())
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing AM from HEAD RTVL: %w", err)
	}
	return prolly.NewAddressMap(amNode, state.ns)
}
