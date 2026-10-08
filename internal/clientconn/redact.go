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
	"fmt"
	"net/url"
	"strings"

	"github.com/FerretDB/wire"
	"github.com/FerretDB/wire/wirebson"

	"github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/types"
)

// sensitiveFields are document keys whose values are never logged: passwords,
// SASL exchanges and stored credentials.
var sensitiveFields = map[string]bool{
	"credentials":             true,
	"password":                true,
	"payload":                 true,
	"pwd":                     true,
	"speculativeAuthenticate": true,
}

const redacted = "<redacted>"

// redactedBody renders a request or response body for debug logging with
// secrets removed: values of sensitiveFields are replaced and credentials
// embedded in URLs are stripped. Documents are decoded with the
// depth-limited converter, so a deeply nested body cannot exhaust the stack
// here before the command's own nesting check rejects it.
func redactedBody(body wire.MsgBody) string {
	switch b := body.(type) {
	case *wire.OpMsg:
		var parts []string
		for _, section := range b.Sections() {
			for _, raw := range section.Documents() {
				parts = append(parts, renderRedacted(raw))
			}
		}
		return strings.Join(parts, "\n")
	case *wire.OpQuery:
		return renderRedacted(b.Query())
	case *wire.OpReply:
		return renderRedacted(b.RawDocument())
	default:
		return fmt.Sprintf("<%T body not shown>", body)
	}
}

func renderRedacted(d wirebson.AnyDocument) string {
	if d == nil {
		return "<no document>"
	}
	doc, err := bson.ToDocument(d)
	if err != nil {
		return "<undecodable document: " + err.Error() + ">"
	}
	out, err := bson.FromDocument(redactDocument(doc))
	if err != nil {
		return "<unrenderable document: " + err.Error() + ">"
	}
	return wirebson.LogMessage(out)
}

// redactDocument returns a copy of doc with secrets removed. It rebuilds
// rather than mutates so documents with duplicate keys, which the command
// path rejects later, still render.
func redactDocument(doc *types.Document) *types.Document {
	keys, values := doc.Keys(), doc.Values()
	pairs := make([]any, 0, 2*len(keys))
	for i, key := range keys {
		value := values[i]
		if sensitiveFields[key] {
			value = redacted
		} else {
			value = redactValue(value)
		}
		pairs = append(pairs, key, value)
	}
	out, err := types.NewDocument(pairs...)
	if err != nil {
		return types.MakeDocument(0)
	}
	return out
}

func redactValue(value any) any {
	switch v := value.(type) {
	case *types.Document:
		return redactDocument(v)
	case *types.Array:
		out := types.MakeArray(v.Len())
		for i := range v.Len() {
			element, _ := v.Get(i)
			out.Append(redactValue(element))
		}
		return out
	case string:
		return stripURLCredentials(v)
	default:
		return value
	}
}

// stripURLCredentials removes the userinfo of a URL, which may hold a token
// in either the user or the password part.
func stripURLCredentials(s string) string {
	if !strings.Contains(s, "://") || !strings.Contains(s, "@") {
		return s
	}
	u, err := url.Parse(s)
	if err != nil || u.User == nil {
		return s
	}
	u.User = url.User(redacted)
	return u.String()
}
