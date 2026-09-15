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
	"encoding/hex"
	"fmt"

	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
)

type ResponseMetadata struct {
	RBID            int64
	Term            int64
	ConfigVersion   int64
	ConfigTerm      int64
	ReplicaSetID    string
	PrimaryIndex    int
	SyncSourceIndex int
	SyncSourceHost  string
	SourceIsPrimary bool
	LastCommitted   control.OpTime
	LastApplied     control.OpTime
	LastWritten     control.OpTime
	LastVisible     control.OpTime
}

func parseResponseMetadata(document *types.Document) (ResponseMetadata, error) {
	oplogDocument, err := requiredMetadataDocument(document, "$oplogQueryData")
	if err != nil {
		return ResponseMetadata{}, err
	}
	replDocument, err := requiredMetadataDocument(document, "$replData")
	if err != nil {
		return ResponseMetadata{}, err
	}
	metadata := ResponseMetadata{}
	if metadata.RBID, err = requiredMetadataInteger(oplogDocument, "rbid"); err != nil {
		return ResponseMetadata{}, err
	}
	primaryIndex, err := requiredMetadataInteger(oplogDocument, "primaryIndex")
	if err != nil {
		return ResponseMetadata{}, err
	}
	metadata.PrimaryIndex = int(primaryIndex)
	syncSourceIndex, err := requiredMetadataInteger(oplogDocument, "syncSourceIndex")
	if err != nil {
		return ResponseMetadata{}, err
	}
	metadata.SyncSourceIndex = int(syncSourceIndex)
	if metadata.SyncSourceHost, err = requiredMetadataString(oplogDocument, "syncSourceHost"); err != nil {
		return ResponseMetadata{}, err
	}
	if metadata.LastCommitted, err = requiredMetadataOpTime(oplogDocument, "lastOpCommitted"); err != nil {
		return ResponseMetadata{}, err
	}
	if metadata.LastApplied, err = requiredMetadataOpTime(oplogDocument, "lastOpApplied"); err != nil {
		return ResponseMetadata{}, err
	}
	metadata.LastWritten, err = optionalMetadataOpTime(oplogDocument, "lastOpWritten", metadata.LastApplied)
	if err != nil {
		return ResponseMetadata{}, err
	}

	if metadata.Term, err = requiredMetadataInteger(replDocument, "term"); err != nil {
		return ResponseMetadata{}, err
	}
	if metadata.ConfigVersion, err = requiredMetadataInteger(replDocument, "configVersion"); err != nil {
		return ResponseMetadata{}, err
	}
	if metadata.ConfigTerm, err = requiredMetadataInteger(replDocument, "configTerm"); err != nil {
		return ResponseMetadata{}, err
	}
	replicaSetID, _ := replDocument.Get("replicaSetId")
	objectID, ok := replicaSetID.(types.ObjectID)
	if !ok {
		return ResponseMetadata{}, fmt.Errorf("$replData.replicaSetId has type %T, want ObjectId", replicaSetID)
	}
	metadata.ReplicaSetID = hex.EncodeToString(objectID[:])
	if metadata.SourceIsPrimary, err = requiredMetadataBool(replDocument, "isPrimary"); err != nil {
		return ResponseMetadata{}, err
	}
	if metadata.LastVisible, err = optionalMetadataOpTime(replDocument, "lastOpVisible", control.OpTime{}); err != nil {
		return ResponseMetadata{}, err
	}
	return metadata, nil
}

func requiredMetadataDocument(document *types.Document, field string) (*types.Document, error) {
	value, _ := document.Get(field)
	result, ok := value.(*types.Document)
	if !ok {
		return nil, fmt.Errorf("%s has type %T, want document", field, value)
	}
	return result, nil
}

func requiredMetadataInteger(document *types.Document, field string) (int64, error) {
	value, _ := document.Get(field)
	result, err := entryInteger(value)
	if err != nil {
		return 0, fmt.Errorf("metadata %s: %w", field, err)
	}
	return result, nil
}

func requiredMetadataString(document *types.Document, field string) (string, error) {
	value, _ := document.Get(field)
	result, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("metadata %s has type %T, want string", field, value)
	}
	return result, nil
}

func requiredMetadataBool(document *types.Document, field string) (bool, error) {
	value, _ := document.Get(field)
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("metadata %s has type %T, want boolean", field, value)
	}
	return result, nil
}

func requiredMetadataOpTime(document *types.Document, field string) (control.OpTime, error) {
	value, _ := document.Get(field)
	opTimeDocument, ok := value.(*types.Document)
	if !ok {
		return control.OpTime{}, fmt.Errorf("metadata %s has type %T, want document", field, value)
	}
	timestampValue, _ := opTimeDocument.Get("ts")
	timestamp, ok := timestampValue.(types.Timestamp)
	if !ok {
		return control.OpTime{}, fmt.Errorf("metadata %s.ts has type %T, want timestamp", field, timestampValue)
	}
	term, err := requiredMetadataInteger(opTimeDocument, "t")
	if err != nil {
		return control.OpTime{}, err
	}
	return control.OpTime{Seconds: uint32(uint64(timestamp) >> 32), Increment: uint32(timestamp), Term: term}, nil
}

func optionalMetadataOpTime(document *types.Document, field string, defaultValue control.OpTime) (control.OpTime, error) {
	value, _ := document.Get(field)
	if value == nil {
		return defaultValue, nil
	}
	return requiredMetadataOpTime(document, field)
}
