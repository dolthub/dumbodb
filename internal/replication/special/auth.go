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

// Package special translates MongoDB administrative replication records.
package special

import (
	"encoding/base64"
	"errors"
	"fmt"

	"github.com/dolthub/dumbodb/internal/authz"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
)

// UnsupportedAuthSemanticError stops replication before authorization is weakened.
type UnsupportedAuthSemanticError struct {
	Identity string
	Field    string
	Reason   string
}

func (e *UnsupportedAuthSemanticError) Error() string {
	return fmt.Sprintf("replicated identity %q has unsupported %s: %s", e.Identity, e.Field, e.Reason)
}

// AuthDocument keeps source-owned authorization state separate from local administration.
type AuthDocument struct {
	Owner    string
	Document *types.Document
}

// TranslateUser validates a MongoDB system.users record and marks its replication ownership domain.
func TranslateUser(source *types.Document, owner string) (AuthDocument, error) {
	identity, err := identityName(source, "user")
	if err != nil {
		return AuthDocument{}, err
	}
	if owner == "" {
		return AuthDocument{}, errors.New("replicated user owner is required")
	}
	if err := knownFields(source, identity, map[string]bool{
		"_id": true, "userId": true, "user": true, "db": true, "credentials": true, "roles": true,
		"customData": true, "authenticationRestrictions": true,
	}); err != nil {
		return AuthDocument{}, err
	}
	if err := validateIdentityID(source, identity, "user"); err != nil {
		return AuthDocument{}, err
	}
	userIDValue, _ := source.Get("userId")
	userID, ok := userIDValue.(types.Binary)
	if !ok || userID.Subtype != types.BinaryUUID || len(userID.B) != 16 {
		return AuthDocument{}, unsupported(identity, "userId", "a UUID is required")
	}
	credentialsValue, _ := source.Get("credentials")
	credentials, ok := credentialsValue.(*types.Document)
	if !ok || credentials.Len() == 0 {
		return AuthDocument{}, unsupported(identity, "credentials", "a non-empty document is required")
	}
	for _, mechanism := range credentials.Keys() {
		if mechanism != "SCRAM-SHA-1" && mechanism != "SCRAM-SHA-256" {
			return AuthDocument{}, unsupported(identity, "credentials."+mechanism, "authentication mechanism is not supported")
		}
		value, _ := credentials.Get(mechanism)
		credential, ok := value.(*types.Document)
		if !ok {
			return AuthDocument{}, unsupported(identity, "credentials."+mechanism, "credential must be a document")
		}
		if err := validateSCRAMCredential(identity, mechanism, credential); err != nil {
			return AuthDocument{}, err
		}
	}
	rolesValue, _ := source.Get("roles")
	roles, ok := rolesValue.(*types.Array)
	if !ok {
		return AuthDocument{}, unsupported(identity, "roles", "an array is required")
	}
	if err := validateRoleReferences(identity, roles); err != nil {
		return AuthDocument{}, err
	}
	if err := validateRestrictions(source, identity); err != nil {
		return AuthDocument{}, err
	}
	if customDataValue, customDataErr := source.Get("customData"); customDataErr == nil {
		if _, ok := customDataValue.(*types.Document); !ok {
			return AuthDocument{}, unsupported(identity, "customData", "a document is required")
		}
	}
	return AuthDocument{Owner: owner, Document: source.DeepCopy()}, nil
}

// TranslateRole validates a MongoDB system.roles record and marks its replication ownership domain.
func TranslateRole(source *types.Document, owner string) (AuthDocument, error) {
	identity, err := identityName(source, "role")
	if err != nil {
		return AuthDocument{}, err
	}
	if owner == "" {
		return AuthDocument{}, errors.New("replicated role owner is required")
	}
	if err := knownFields(source, identity, map[string]bool{
		"_id": true, "role": true, "db": true, "privileges": true, "roles": true, "authenticationRestrictions": true,
	}); err != nil {
		return AuthDocument{}, err
	}
	if err := validateIdentityID(source, identity, "role"); err != nil {
		return AuthDocument{}, err
	}
	rolesValue, _ := source.Get("roles")
	roles, ok := rolesValue.(*types.Array)
	if !ok {
		return AuthDocument{}, unsupported(identity, "roles", "an array is required")
	}
	if err := validateRoleReferences(identity, roles); err != nil {
		return AuthDocument{}, err
	}
	privilegesValue, _ := source.Get("privileges")
	privileges, ok := privilegesValue.(*types.Array)
	if !ok {
		return AuthDocument{}, unsupported(identity, "privileges", "an array is required")
	}
	if err := validatePrivileges(identity, privileges); err != nil {
		return AuthDocument{}, err
	}
	if err := validateRestrictions(source, identity); err != nil {
		return AuthDocument{}, err
	}
	return AuthDocument{Owner: owner, Document: source.DeepCopy()}, nil
}

func identityName(source *types.Document, nameField string) (string, error) {
	if source == nil {
		return "", errors.New("replicated identity document is required")
	}
	if duplicate, ok := source.FindDuplicateKey(); ok {
		return "", unsupported("unknown", duplicate, "duplicate fields are not supported")
	}
	nameValue, _ := source.Get(nameField)
	name, ok := nameValue.(string)
	databaseValue, _ := source.Get("db")
	database, databaseOK := databaseValue.(string)
	if !ok || name == "" || !databaseOK || database == "" {
		return "", unsupported("unknown", nameField+"/db", "non-empty strings are required")
	}
	return database + "." + name, nil
}

func validateIdentityID(source *types.Document, identity, nameField string) error {
	id, _ := source.Get("_id")
	if id != identity {
		return unsupported(identity, "_id", fmt.Sprintf("must equal db.%s", nameField))
	}
	return nil
}

func knownFields(document *types.Document, identity string, known map[string]bool) error {
	for _, field := range document.Keys() {
		if !known[field] {
			return unsupported(identity, field, "field is not represented by DumboDB")
		}
	}
	return nil
}

func validateSCRAMCredential(identity, mechanism string, credential *types.Document) error {
	if err := knownFields(credential, identity, map[string]bool{
		"iterationCount": true, "salt": true, "storedKey": true, "serverKey": true,
	}); err != nil {
		return err
	}
	iterationValue, _ := credential.Get("iterationCount")
	iterationCount, ok := iterationValue.(int32)
	if !ok || iterationCount <= 0 {
		return unsupported(identity, "credentials."+mechanism+".iterationCount", "a positive int32 is required")
	}
	for _, field := range []string{"salt", "storedKey", "serverKey"} {
		value, _ := credential.Get(field)
		text, ok := value.(string)
		if !ok || text == "" {
			return unsupported(identity, "credentials."+mechanism+"."+field, "a non-empty base64 string is required")
		}
		if _, err := base64.StdEncoding.DecodeString(text); err != nil {
			return unsupported(identity, "credentials."+mechanism+"."+field, "a valid base64 string is required")
		}
	}
	return nil
}

func validateRoleReferences(identity string, roles *types.Array) error {
	for index := 0; index < roles.Len(); index++ {
		value, _ := roles.Get(index)
		role, ok := value.(*types.Document)
		if !ok || role.Len() != 2 {
			return unsupported(identity, fmt.Sprintf("roles[%d]", index), "role and db fields are required")
		}
		roleValue, _ := role.Get("role")
		databaseValue, _ := role.Get("db")
		roleName, roleOK := roleValue.(string)
		database, databaseOK := databaseValue.(string)
		if !roleOK || roleName == "" || !databaseOK || database == "" {
			return unsupported(identity, fmt.Sprintf("roles[%d]", index), "role and db must be non-empty strings")
		}
	}
	return nil
}

func validatePrivileges(identity string, privileges *types.Array) error {
	for index := 0; index < privileges.Len(); index++ {
		value, _ := privileges.Get(index)
		privilege, ok := value.(*types.Document)
		if !ok || privilege.Len() != 2 {
			return unsupported(identity, fmt.Sprintf("privileges[%d]", index), "resource and actions fields are required")
		}
		resourceValue, _ := privilege.Get("resource")
		resource, ok := resourceValue.(*types.Document)
		if !ok || !supportedResource(resource) {
			return unsupported(identity, fmt.Sprintf("privileges[%d].resource", index), "resource pattern is not represented")
		}
		actionsValue, _ := privilege.Get("actions")
		actions, ok := actionsValue.(*types.Array)
		if !ok {
			return unsupported(identity, fmt.Sprintf("privileges[%d].actions", index), "an array is required")
		}
		for actionIndex := 0; actionIndex < actions.Len(); actionIndex++ {
			value, _ := actions.Get(actionIndex)
			action, ok := value.(string)
			if !ok || !supportedAction(authz.Action(action)) {
				return unsupported(identity, fmt.Sprintf("privileges[%d].actions[%d]", index, actionIndex), fmt.Sprintf("action %q is not enforced", action))
			}
		}
	}
	return nil
}

func supportedResource(resource *types.Document) bool {
	if resource == nil {
		return false
	}
	if _, duplicate := resource.FindDuplicateKey(); duplicate {
		return false
	}
	if resource.Len() == 1 {
		cluster, _ := resource.Get("cluster")
		anyResource, _ := resource.Get("anyResource")
		return cluster == true || anyResource == true
	}
	if resource.Len() != 2 {
		return false
	}
	database, databaseOK := valueString(resource, "db")
	_, collectionOK := valueString(resource, "collection")
	return databaseOK && collectionOK && database != "config"
}

func supportedAction(action authz.Action) bool {
	switch action {
	case authz.ActionFind, authz.ActionInsert, authz.ActionUpdate, authz.ActionRemove,
		authz.ActionBypassDocumentValidation, authz.ActionCreateCollection, authz.ActionCreateIndex,
		authz.ActionDropCollection, authz.ActionDropIndex, authz.ActionDropDatabase, authz.ActionCollMod,
		authz.ActionCompact, authz.ActionConvertToCapped, authz.ActionRenameCollectionSameDB,
		authz.ActionCreateSearchIndexes, authz.ActionDropSearchIndex, authz.ActionListSearchIndexes,
		authz.ActionUpdateSearchIndex, authz.ActionKillCursors, authz.ActionListCollections,
		authz.ActionListIndexes, authz.ActionListDatabases, authz.ActionCollStats, authz.ActionDBStats,
		authz.ActionDataSize, authz.ActionValidate, authz.ActionServerStatus, authz.ActionGetParameter,
		authz.ActionSetParameter, authz.ActionHostInfo, authz.ActionTop, authz.ActionGetLog,
		authz.ActionCreateUser, authz.ActionDropUser, authz.ActionChangePassword,
		authz.ActionChangeCustomData, authz.ActionChangeOwnPassword, authz.ActionChangeOwnCustomData,
		authz.ActionGrantRole, authz.ActionRevokeRole, authz.ActionCreateRole, authz.ActionDropRole,
		authz.ActionViewUser, authz.ActionViewRole, authz.ActionSetAuthenticationRestriction, authz.AnyAction:
		return true
	default:
		return false
	}
}

func validateRestrictions(source *types.Document, identity string) error {
	value, err := source.Get("authenticationRestrictions")
	if err != nil {
		return nil
	}
	restrictions, ok := value.(*types.Array)
	if !ok {
		return unsupported(identity, "authenticationRestrictions", "an array is required")
	}
	for index := 0; index < restrictions.Len(); index++ {
		value, _ := restrictions.Get(index)
		restriction, ok := value.(*types.Document)
		if !ok {
			return unsupported(identity, fmt.Sprintf("authenticationRestrictions[%d]", index), "a document is required")
		}
		if err := knownFields(restriction, identity, map[string]bool{"clientSource": true, "serverAddress": true}); err != nil {
			return err
		}
		iter := restriction.Iterator()
		for {
			field, raw, err := iter.Next()
			if errors.Is(err, iterator.ErrIteratorDone) {
				break
			}
			if err != nil {
				iter.Close()
				return err
			}
			addresses, ok := raw.(*types.Array)
			if !ok || !stringArray(addresses) {
				iter.Close()
				return unsupported(identity, "authenticationRestrictions."+field, "an array of strings is required")
			}
		}
		iter.Close()
	}
	return nil
}

func stringArray(array *types.Array) bool {
	for index := 0; index < array.Len(); index++ {
		value, _ := array.Get(index)
		if _, ok := value.(string); !ok {
			return false
		}
	}
	return true
}

func valueString(document *types.Document, field string) (string, bool) {
	value, err := document.Get(field)
	if err != nil {
		return "", false
	}
	text, ok := value.(string)
	return text, ok
}

func unsupported(identity, field, reason string) error {
	return &UnsupportedAuthSemanticError{Identity: identity, Field: field, Reason: reason}
}
