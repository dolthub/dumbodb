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

package operators

import (
	"fmt"

	internalbson "github.com/dolthub/dumbodb/internal/bson"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

type bsonSizeOp struct {
	arg any
}

func newBsonSize(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(
			ErrArgsInvalidLen,
			"$bsonSize",
			fmt.Sprintf("Expression $bsonSize takes exactly 1 arguments. %d were passed in.", len(args)),
		)
	}
	return &bsonSizeOp{arg: args[0]}, nil
}

func (b *bsonSizeOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValueWithRoot(b.arg, doc)
	if err != nil {
		return nil, err
	}

	if v == nil || v == types.Null {
		return types.Null, nil
	}

	d, ok := v.(*types.Document)
	if !ok {
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBSONSizeRequiresDocument,
			fmt.Sprintf("$bsonSize requires a document input, found: %s", handlerparams.AliasFromType(v)),
			"$bsonSize",
		)
	}

	wdoc, err := internalbson.FromDocument(d)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	raw, err := wdoc.Encode()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}
	return int32(len(raw)), nil
}

var _ Operator = (*bsonSizeOp)(nil)
