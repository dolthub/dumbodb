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

package topology

import (
	"encoding/hex"
	"errors"
	"fmt"
	"time"

	"github.com/FerretDB/wire"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/types"
)

func heartbeatRequest(state Snapshot) *wire.OpMsg {
	configVersion := int64(-2)
	configTerm := int64(-1)
	from := ""
	fromID := int32(-1)
	if state.Configuration != nil {
		configVersion = state.Configuration.Version
		configTerm = state.Configuration.Term
		from = state.MemberHost
		fromID = int32(state.MemberID)
	}
	return wire.MustOpMsg(
		"replSetHeartbeat", state.SetName,
		"configVersion", configVersion,
		"configTerm", configTerm,
		"hbv", int32(1),
		"from", from,
		"fromId", fromID,
		"term", state.Term,
		"primaryId", int32(state.PrimaryID),
		"maxTimeMSOpOnly", int64(10000),
		"$replData", int32(1),
		"$db", "admin",
	)
}

func parseHeartbeatResponse(message *wire.OpMsg, setName string, memberID int, observedAt time.Time) (Heartbeat, error) {
	raw, err := message.RawDocument()
	if err != nil {
		return Heartbeat{}, fmt.Errorf("reading heartbeat response: %w", err)
	}
	document, err := bson.ToDocument(raw)
	if err != nil {
		return Heartbeat{}, fmt.Errorf("decoding heartbeat response: %w", err)
	}
	if !numericOne(documentValue(document, "ok")) {
		return Heartbeat{}, fmt.Errorf("heartbeat command failed: %v", documentValue(document, "errmsg"))
	}
	responseSet, ok := documentValue(document, "set").(string)
	if !ok || responseSet != setName {
		return Heartbeat{}, fmt.Errorf("heartbeat response set %q does not match %q", responseSet, setName)
	}
	state, err := requiredProtocolInteger(document, "state")
	if err != nil {
		return Heartbeat{}, err
	}
	term, err := requiredProtocolInteger(document, "term")
	if err != nil {
		return Heartbeat{}, err
	}
	primaryID, err := optionalProtocolInteger(document, "primaryId", -1)
	if err != nil {
		return Heartbeat{}, err
	}
	heartbeat := Heartbeat{
		SetName:    responseSet,
		MemberID:   memberID,
		State:      MemberState(state),
		Term:       term,
		PrimaryID:  int(primaryID),
		ObservedAt: observedAt,
	}
	if heartbeat.Applied, err = parseOptionalOpTime(document, "appliedOpTime"); err != nil {
		return Heartbeat{}, err
	}
	if heartbeat.Written, err = parseOptionalOpTime(document, "writtenOpTime"); err != nil {
		return Heartbeat{}, err
	}
	if heartbeat.Durable, err = parseOptionalOpTime(document, "durableOpTime"); err != nil {
		return Heartbeat{}, err
	}
	if configValue := documentValue(document, "config"); configValue != nil {
		configDocument, ok := configValue.(*types.Document)
		if !ok {
			return Heartbeat{}, fmt.Errorf("heartbeat config has type %T, want document", configValue)
		}
		configuration, err := parseReplicaConfiguration(configDocument)
		if err != nil {
			return Heartbeat{}, err
		}
		heartbeat.Configuration = &configuration
	}
	return heartbeat, nil
}

func parseReplicaConfiguration(document *types.Document) (control.ReplicaConfiguration, error) {
	setName, ok := documentValue(document, "_id").(string)
	if !ok || setName == "" {
		return control.ReplicaConfiguration{}, errors.New("replica configuration _id must be a non-empty string")
	}
	version, err := requiredProtocolInteger(document, "version")
	if err != nil {
		return control.ReplicaConfiguration{}, err
	}
	term, err := optionalProtocolInteger(document, "term", -1)
	if err != nil {
		return control.ReplicaConfiguration{}, err
	}
	protocolVersion, err := optionalProtocolInteger(document, "protocolVersion", 1)
	if err != nil {
		return control.ReplicaConfiguration{}, err
	}
	if protocolVersion != 1 {
		return control.ReplicaConfiguration{}, fmt.Errorf("unsupported replica-set protocol version %d", protocolVersion)
	}
	membersValue := documentValue(document, "members")
	membersArray, ok := membersValue.(*types.Array)
	if !ok || membersArray.Len() == 0 {
		return control.ReplicaConfiguration{}, errors.New("replica configuration members must be a non-empty array")
	}
	members := make([]control.MemberConfiguration, 0, membersArray.Len())
	for index := 0; index < membersArray.Len(); index++ {
		value, getErr := membersArray.Get(index)
		if getErr != nil {
			return control.ReplicaConfiguration{}, getErr
		}
		memberDocument, ok := value.(*types.Document)
		if !ok {
			return control.ReplicaConfiguration{}, fmt.Errorf("replica configuration member %d has type %T, want document", index, value)
		}
		member, parseErr := parseMemberConfiguration(memberDocument)
		if parseErr != nil {
			return control.ReplicaConfiguration{}, fmt.Errorf("replica configuration member %d: %w", index, parseErr)
		}
		members = append(members, member)
	}
	settings, ok := documentValue(document, "settings").(*types.Document)
	if !ok {
		return control.ReplicaConfiguration{}, errors.New("replica configuration settings must be a document")
	}
	replicaSetObjectID, ok := documentValue(settings, "replicaSetId").(types.ObjectID)
	if !ok {
		return control.ReplicaConfiguration{}, errors.New("replica configuration settings.replicaSetId must be an ObjectId")
	}
	raw, err := bson.FromDocumentRaw(document)
	if err != nil {
		return control.ReplicaConfiguration{}, fmt.Errorf("encoding replica configuration: %w", err)
	}
	return control.ReplicaConfiguration{
		SetName:         setName,
		Version:         version,
		Term:            term,
		ProtocolVersion: protocolVersion,
		ReplicaSetID:    hex.EncodeToString(replicaSetObjectID[:]),
		Members:         members,
		RawBSON:         raw,
	}, nil
}

func parseMemberConfiguration(document *types.Document) (control.MemberConfiguration, error) {
	memberID, err := requiredProtocolInteger(document, "_id")
	if err != nil {
		return control.MemberConfiguration{}, err
	}
	host, ok := documentValue(document, "host").(string)
	if !ok || host == "" {
		return control.MemberConfiguration{}, errors.New("host must be a non-empty string")
	}
	hidden, err := optionalProtocolBool(document, "hidden", false)
	if err != nil {
		return control.MemberConfiguration{}, err
	}
	priority, err := optionalProtocolNumber(document, "priority", 1)
	if err != nil {
		return control.MemberConfiguration{}, err
	}
	votes, err := optionalProtocolInteger(document, "votes", 1)
	if err != nil {
		return control.MemberConfiguration{}, err
	}
	return control.MemberConfiguration{
		MemberID: int(memberID),
		Host:     host,
		Hidden:   hidden,
		Priority: priority,
		Votes:    int(votes),
	}, nil
}

func parseOptionalOpTime(document *types.Document, field string) (control.OpTime, error) {
	value := documentValue(document, field)
	if value == nil {
		return control.OpTime{}, nil
	}
	opTimeDocument, ok := value.(*types.Document)
	if !ok {
		return control.OpTime{}, fmt.Errorf("heartbeat %s has type %T, want document", field, value)
	}
	timestamp, ok := documentValue(opTimeDocument, "ts").(types.Timestamp)
	if !ok {
		return control.OpTime{}, fmt.Errorf("heartbeat %s.ts must be a timestamp", field)
	}
	term, err := requiredProtocolInteger(opTimeDocument, "t")
	if err != nil {
		return control.OpTime{}, fmt.Errorf("heartbeat %s: %w", field, err)
	}
	return control.OpTime{Seconds: uint32(uint64(timestamp) >> 32), Increment: uint32(timestamp), Term: term}, nil
}

func documentValue(document *types.Document, field string) any {
	value, _ := document.Get(field)
	return value
}

func requiredProtocolInteger(document *types.Document, field string) (int64, error) {
	value := documentValue(document, field)
	if value == nil {
		return 0, fmt.Errorf("missing required integer field %s", field)
	}
	return protocolInteger(field, value)
}

func optionalProtocolInteger(document *types.Document, field string, defaultValue int64) (int64, error) {
	value := documentValue(document, field)
	if value == nil {
		return defaultValue, nil
	}
	return protocolInteger(field, value)
}

func protocolInteger(field string, value any) (int64, error) {
	switch value := value.(type) {
	case int32:
		return int64(value), nil
	case int64:
		return value, nil
	default:
		return 0, fmt.Errorf("field %s has type %T, want integer", field, value)
	}
}

func optionalProtocolNumber(document *types.Document, field string, defaultValue float64) (float64, error) {
	value := documentValue(document, field)
	if value == nil {
		return defaultValue, nil
	}
	switch value := value.(type) {
	case int32:
		return float64(value), nil
	case int64:
		return float64(value), nil
	case float64:
		return value, nil
	default:
		return 0, fmt.Errorf("field %s has type %T, want number", field, value)
	}
}

func optionalProtocolBool(document *types.Document, field string, defaultValue bool) (bool, error) {
	value := documentValue(document, field)
	if value == nil {
		return defaultValue, nil
	}
	result, ok := value.(bool)
	if !ok {
		return false, fmt.Errorf("field %s has type %T, want boolean", field, value)
	}
	return result, nil
}

func numericOne(value any) bool {
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
