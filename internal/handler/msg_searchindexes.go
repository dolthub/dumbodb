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

package handler

import (
	"context"

	"github.com/FerretDB/wire"

	"github.com/dolthub/docudolt/internal/handler/handlererrors"
	"github.com/dolthub/docudolt/internal/util/lazyerrors"
)

// MsgCreateSearchIndexes implements the `createSearchIndexes` command.
//
// Atlas Search indexes require a MongoDB Atlas deployment and are not
// supported by DocuDolt. This handler returns a clear "not implemented"
// error so clients receive a meaningful message instead of a generic
// "no such command" error.
func (h *Handler) MsgCreateSearchIndexes(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	if _, err := opMsgDocument(msg); err != nil {
		return nil, lazyerrors.Error(err)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"Atlas Search indexes are not supported by DocuDolt",
		"createSearchIndexes",
	)
}

// MsgListSearchIndexes implements the `listSearchIndexes` command.
//
// Atlas Search indexes are not supported by DocuDolt.
func (h *Handler) MsgListSearchIndexes(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	if _, err := opMsgDocument(msg); err != nil {
		return nil, lazyerrors.Error(err)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"Atlas Search indexes are not supported by DocuDolt",
		"listSearchIndexes",
	)
}

// MsgDropSearchIndex implements the `dropSearchIndex` command.
//
// Atlas Search indexes are not supported by DocuDolt.
func (h *Handler) MsgDropSearchIndex(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	if _, err := opMsgDocument(msg); err != nil {
		return nil, lazyerrors.Error(err)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"Atlas Search indexes are not supported by DocuDolt",
		"dropSearchIndex",
	)
}

// MsgUpdateSearchIndex implements the `updateSearchIndex` command.
//
// Atlas Search indexes are not supported by DocuDolt.
func (h *Handler) MsgUpdateSearchIndex(connCtx context.Context, msg *wire.OpMsg) (*wire.OpMsg, error) {
	if _, err := opMsgDocument(msg); err != nil {
		return nil, lazyerrors.Error(err)
	}

	return nil, handlererrors.NewCommandErrorMsgWithArgument(
		handlererrors.ErrNotImplemented,
		"Atlas Search indexes are not supported by DocuDolt",
		"updateSearchIndex",
	)
}
