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
	"encoding/hex"
	"fmt"
	"unicode/utf8"

	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/handler/handlerparams"
	"github.com/dolthub/dumbodb/internal/types"
)

// binarySizeOp represents { $binarySize: <expr> }.
type binarySizeOp struct {
	arg any
}

func newBinarySize(args ...any) (Operator, error) {
	if len(args) != 1 {
		return nil, newOperatorError(ErrArgsInvalidLen, "$binarySize",
			fmt.Sprintf("Expression $binarySize takes exactly 1 arguments. %d were passed in.", len(args)))
	}
	return &binarySizeOp{arg: args[0]}, nil
}

// Process follows ExpressionBinarySize::evaluate: a string's size is its
// length in bytes.
func (op *binarySizeOp) Process(doc *types.Document) (any, error) {
	v, err := evalArgValue(op.arg, doc)
	if err != nil {
		return nil, err
	}
	switch v := v.(type) {
	case string:
		return int32(len(v)), nil
	case types.Binary:
		return int32(len(v.B)), nil
	}
	if isNullish(v) {
		return types.Null, nil
	}
	return nil, handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrBinarySizeType,
		"$binarySize requires a string or BinData argument, found: "+handlerparams.AliasFromType(v), "$binarySize")
}

var _ Operator = (*binarySizeOp)(nil)

// convertToObjectID is ExpressionConvert's conversion to objectId: a string
// is parsed with OID::createFromString, and an ObjectId is kept.
func convertToObjectID(v any) (any, error) {
	switch v := v.(type) {
	case types.ObjectID:
		return v, nil
	case string:
		if len(v) != 24 {
			return nil, conversionError(fmt.Sprintf("Failed to parse objectId '%s' in $convert with no onError value: "+
				"Invalid string length for parsing to OID, expected 24 but found %d", v, len(v)))
		}
		var id types.ObjectID
		if _, err := hex.Decode(id[:], []byte(v)); err != nil {
			bad := v
			if invalid, ok := err.(hex.InvalidByteError); ok && utf8.ValidString(string(rune(invalid))) {
				bad = string(rune(invalid))
			}
			return nil, conversionError(fmt.Sprintf("Failed to parse objectId '%s' in $convert with no onError value: "+
				"Invalid character found in hex string: %s", v, bad))
		}
		return id, nil
	default:
		return nil, conversionError(fmt.Sprintf("Unsupported conversion from %s to objectId in $convert with no onError value",
			handlerparams.AliasFromType(v)))
	}
}

func conversionError(msg string) error {
	return handlererrors.NewCommandErrorMsgWithArgument(handlererrors.ErrConversionFailure, msg, "$convert")
}
