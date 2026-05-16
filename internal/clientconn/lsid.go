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

package clientconn

import (
	"context"
	"encoding/hex"

	"github.com/dolthub/dumbodb/internal/clientconn/conninfo"
	"github.com/dolthub/dumbodb/internal/types"
)

// extractAndSetLSID pulls the MongoDB logical session ID out of a decoded
// command document and records it on the connection's ConnInfo.
//
// The wire format is:
//
//	{ ...command..., lsid: { id: UUID(...) } }
//
// where the UUID is BSON Binary subtype 4 (16 bytes). We hex-encode the
// 16 bytes for a stable 32-char string identifier that the backend can
// use as a per-session map key.
//
// Calls with no lsid (mongosh without sessions, hello/isMaster) are
// silently ignored; conninfo.Owner() falls back to a synthetic per-
// connection id in that case.
func extractAndSetLSID(ctx context.Context, doc *types.Document) {
	if doc == nil || !doc.Has("lsid") {
		return
	}
	v, err := doc.Get("lsid")
	if err != nil {
		return
	}
	lsidDoc, ok := v.(*types.Document)
	if !ok || !lsidDoc.Has("id") {
		return
	}
	idVal, err := lsidDoc.Get("id")
	if err != nil {
		return
	}
	bin, ok := idVal.(types.Binary)
	if !ok {
		return
	}

	conninfo.Get(ctx).SetLSID(hex.EncodeToString(bin.B))
}
