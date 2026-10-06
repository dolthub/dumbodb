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

package types

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestDocumentGet_MissingKeyMessage(t *testing.T) {
	doc, err := NewDocument("a", int32(1))
	require.NoError(t, err)
	_, err = doc.Get("b")
	require.EqualError(t, err, `types.Document.Get: key not found: "b"`)

	var nilDoc *Document
	_, err = nilDoc.Get("b")
	require.EqualError(t, err, `types.Document.Get: key not found: "b" (nil document)`)
}
