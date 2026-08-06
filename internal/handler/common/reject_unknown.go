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

package common

import (
	"fmt"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
)

// protocolEnvelopeFields are the generic fields a driver or the wire protocol
// may append to any command; MongoDB accepts them on every command regardless
// of the command's own parameters (its IDL generic_argument set). They must be
// allowed everywhere or drivers that attach a session id, cluster time, read
// preference, etc. break.
var protocolEnvelopeFields = map[string]struct{}{
	"$db":                  {},
	"lsid":                 {},
	"txnNumber":            {},
	"autocommit":           {},
	"startTransaction":     {},
	"$readPreference":      {},
	"$clusterTime":         {},
	"apiVersion":           {},
	"apiStrict":            {},
	"apiDeprecationErrors": {},
	"comment":              {},
	"maxTimeMS":            {},
	"writeConcern":         {},
	"readConcern":          {},
	"$audit":               {},
	"$client":              {},
	"$configServerState":   {},
}

// RejectUnknownFields returns an ErrIDLUnknownField (40415) naming the first
// top-level field in doc that is neither the command key, a protocol envelope
// field, nor listed in allowed. It matches MongoDB's strict:true IDL parsing,
// including the "BSON field '<command>.<field>' is an unknown field." message.
// Returns nil when every field is recognized.
func RejectUnknownFields(doc *types.Document, allowed ...string) error {
	command := doc.Command()

	allow := make(map[string]struct{}, len(allowed)+1)
	allow[command] = struct{}{}
	for _, a := range allowed {
		allow[a] = struct{}{}
	}

	for _, key := range doc.Keys() {
		if _, ok := allow[key]; ok {
			continue
		}
		if _, ok := protocolEnvelopeFields[key]; ok {
			continue
		}
		return handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrIDLUnknownField,
			fmt.Sprintf("BSON field '%s.%s' is an unknown field.", command, key),
			command,
		)
	}

	return nil
}
