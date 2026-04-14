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
	"time"

	"github.com/FerretDB/wire/wirebson"
	fb "github.com/dolthub/flatbuffers/v23/go"

	"github.com/dolthub/dolt/go/gen/fb/serial"
	"github.com/dolthub/dolt/go/store/hash"
	"github.com/dolthub/dolt/go/store/nbs"
	"github.com/dolthub/dolt/go/store/pool"
	"github.com/dolthub/dolt/go/store/prolly"
	"github.com/dolthub/dolt/go/store/prolly/tree"
	dolttypes "github.com/dolthub/dolt/go/store/types"
	"github.com/dolthub/dolt/go/store/val"

	"github.com/dolthub/dumbodb/internal/bson"
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
//   - _id VARBINARY PK → ByteStringEnc (key tuple)
//   - doc JSON         → JSONAddrEnc (value tuple, stores hash to JSON prolly tree)
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
// artifactsHash is the 20-byte noms hash of the ArtifactMap node in the value store;
// pass hash.Hash{} (all zeros) when there are no conflict artifacts.
func buildDoltTableFlatbuffer(m prolly.Map, schemaHash hash.Hash, emptyIndexAM prolly.AddressMap, artifactsHash hash.Hash) serial.Message {
	b := fb.NewBuilder(1024)

	// Primary index: inline TUPM bytes (prolly.Map root node).
	rowBytes := []byte(tree.ValueFromNode(m.Node()).(dolttypes.SerialMessage))

	// Secondary indexes: empty ADRM.
	idxBytes := []byte(tree.ValueFromNode(emptyIndexAM.Node()).(dolttypes.SerialMessage))

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
			return [20]byte{}, fmt.Errorf("dolt: encoding _id document for hash: %w", err)
		}
		wval = wdoc
	case *types.Array:
		warr, err := bson.FromArray(v)
		if err != nil {
			return [20]byte{}, fmt.Errorf("dolt: encoding _id array for hash: %w", err)
		}
		wval = warr
	default:
		return [20]byte{}, fmt.Errorf("dolt: unsupported _id type %T", id)
	}

	if err := doc.Add("_id", wval); err != nil {
		return [20]byte{}, fmt.Errorf("dolt: building _id doc for hash: %w", err)
	}

	raw, err := doc.Encode()
	if err != nil {
		return [20]byte{}, fmt.Errorf("dolt: encoding _id for hash: %w", err)
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

	tup, err := tb.Build(bufPool)
	if err != nil {
		return nil, fmt.Errorf("dolt: building key tuple: %w", err)
	}

	return tup, nil
}

// buildValue creates a value tuple storing a hash to a JSON prolly tree.
func buildValue(jsonHash hash.Hash) (val.Tuple, error) {
	tb := val.NewTupleBuilder(valDesc, nil)
	tb.PutJSONAddr(0, jsonHash)

	tup, err := tb.Build(bufPool)
	if err != nil {
		return nil, fmt.Errorf("dolt: building value tuple: %w", err)
	}

	return tup, nil
}

// dtblHashForMap builds a DTBL from a prolly.Map with no artifacts, writes it to
// the value store, and returns the hash of the DTBL chunk.
func (state *dbState) dtblHashForMap(ctx context.Context, m prolly.Map) (hash.Hash, error) {
	return state.dtblHashForMapWithArtifacts(ctx, m, hash.Hash{})
}

// dtblHashForMapWithArtifacts builds a DTBL from a prolly.Map with the given
// artifactsHash (the noms hash of a written ArtifactMap node; pass hash.Hash{}
// for no artifacts), writes the DTBL to the value store, and returns its hash.
// The DTBL becomes the new ADRM entry for the collection.
func (state *dbState) dtblHashForMapWithArtifacts(ctx context.Context, m prolly.Map, artifactsHash hash.Hash) (hash.Hash, error) {
	dtblMsg := buildDoltTableFlatbuffer(m, state.collSchemaHash, state.emptyIndexAM, artifactsHash)
	ref, err := state.vs.WriteValue(ctx, dolttypes.SerialMessage(dtblMsg))
	if err != nil {
		return hash.Hash{}, fmt.Errorf("writing DTBL: %w", err)
	}
	return ref.TargetHash(), nil
}

// getOrInitBranchAM returns the current working-set AddressMap for branch.
// For "main" it returns state.am. For other branches it checks state.branchAMs
// and initializes from the branch HEAD if not already cached.
// The caller must hold state.mu (write lock).
func (state *dbState) getOrInitBranchAM(ctx context.Context, branch string) (prolly.AddressMap, error) {
	if branch == "main" {
		return state.am, nil
	}
	if am, ok := state.branchAMs[branch]; ok {
		return am, nil
	}
	// Initialize from the branch HEAD commit.
	am, err := amFromRootish(ctx, state, branch)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("initializing branch AM for %q: %w", branch, err)
	}
	state.branchAMs[branch] = am
	return am, nil
}

// headRootAMForBranch returns the collections AddressMap from a branch HEAD's rootValue.
// For "main" it delegates to headRootAM (uses state.ds). For other branches it loads
// the branch dataset from doltDB and reads its HEAD. If the branch has no commits,
// an empty AddressMap is returned (suitable as the initial staged root).
// The caller must hold state.mu (read or write lock).
func headRootAMForBranch(ctx context.Context, state *dbState, branch string) (prolly.AddressMap, error) {
	if branch == "main" {
		return state.headRootAM(ctx)
	}
	branchDS, err := state.doltDB.GetDataset(ctx, "refs/heads/"+branch)
	if err != nil || !branchDS.HasHead() {
		return prolly.NewEmptyAddressMap(state.ns)
	}
	headValue, _, err := branchDS.MaybeHeadValue()
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("reading HEAD value for branch %q: %w", branch, err)
	}
	headMsg, ok := headValue.(dolttypes.SerialMessage)
	if !ok {
		return prolly.AddressMap{}, fmt.Errorf("unexpected HEAD value type %T for branch %q", headValue, branch)
	}
	rtvl, err := serial.TryGetRootAsRootValue([]byte(headMsg), serial.MessagePrefixSz)
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing HEAD RTVL for branch %q: %w", branch, err)
	}
	amNode, _, err := tree.NodeFromBytes(rtvl.TablesBytes())
	if err != nil {
		return prolly.AddressMap{}, fmt.Errorf("parsing AM from HEAD RTVL for branch %q: %w", branch, err)
	}
	return prolly.NewAddressMap(amNode, state.ns)
}

// updateAddressMap applies a mutation to the collections address map for branch and
// persists it to the dolt working set only. HEAD stays at the last explicit
// commit; only working_root_addr advances. staged_root_addr stays at HEAD's
// rootValue so that `dolt status` shows "Changes not staged for commit".
// The caller must hold state.mu (write lock).
func (state *dbState) updateAddressMap(ctx context.Context, branch string, fn func(prolly.AddressMapEditor) error) error {
	// Get the current AM for this branch (initializes from HEAD if needed).
	currentAM, err := state.getOrInitBranchAM(ctx, branch)
	if err != nil {
		return err
	}

	editor := currentAM.Editor()

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

	// Get the staged AM from the branch HEAD's rootValue. Staged stays at HEAD until an
	// explicit stage operation advances it.
	stagedAM, err := headRootAMForBranch(ctx, state, branch)
	if err != nil {
		return fmt.Errorf("dolt: reading HEAD AM for staged root: %w", err)
	}

	if err := updateWorkingSet(ctx, state.doltDB, newAM, stagedAM, branch); err != nil {
		return fmt.Errorf("dolt: updating working set: %w", err)
	}

	// Persist the updated AM.
	if branch == "main" {
		state.am = newAM
	} else {
		state.branchAMs[branch] = newAM
	}

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

