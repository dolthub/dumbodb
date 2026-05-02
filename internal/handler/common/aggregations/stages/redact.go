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

package stages

import (
	"context"
	"errors"

	"github.com/dolthub/dumbodb/internal/handler/common/aggregations"
	"github.com/dolthub/dumbodb/internal/handler/common/aggregations/operators"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
	"github.com/dolthub/dumbodb/internal/types"
	"github.com/dolthub/dumbodb/internal/util/iterator"
	"github.com/dolthub/dumbodb/internal/util/lazyerrors"
)

// redactStage represents the $redact aggregation stage.
//
//	{ $redact: <expression> }
//
// The expression must evaluate to one of:
//   - "$$PRUNE"    -- exclude this document/sub-document
//   - "$$KEEP"     -- include this document as-is (no recursion)
//   - "$$DESCEND"  -- include this document and recurse into sub-documents
type redactStage struct {
	expr any
}

// newRedact creates a new $redact stage.
func newRedact(stage *types.Document) (aggregations.Stage, error) {
	v, err := stage.Get("$redact")
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	return &redactStage{expr: v}, nil
}

// Process implements Stage interface.
func (r *redactStage) Process(_ context.Context, iter types.DocumentsIterator, closer *iterator.MultiCloser) (types.DocumentsIterator, error) { //nolint:lll // for readability
	docs, err := iterator.ConsumeValues(iter)
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	var out []*types.Document

	for _, doc := range docs {
		result, redactErr := redactDocument(r.expr, doc)
		if redactErr != nil {
			return nil, redactErr
		}

		if result != nil {
			out = append(out, result)
		}
	}

	res := iterator.Values(iterator.ForSlice(out))
	closer.Add(res)

	return res, nil
}

// redactDocument applies the redact expression to a document.
// Returns nil if the document should be pruned, or the (possibly modified) document.
func redactDocument(expr any, doc *types.Document) (*types.Document, error) {
	action, err := evalRedactExpr(expr, doc)
	if err != nil {
		return nil, err
	}

	switch action {
	case "$$PRUNE":
		return nil, nil
	case "$$KEEP":
		return doc, nil
	case "$$DESCEND":
		return descend(expr, doc)
	default:
		return nil, handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$redact expression must evaluate to $$PRUNE, $$KEEP, or $$DESCEND",
			"$redact (stage)",
		)
	}
}

// descend processes each field in the document recursively.
// Sub-documents are recursively redacted; arrays are element-wise redacted; scalars are kept.
func descend(expr any, doc *types.Document) (*types.Document, error) {
	result, err := types.NewDocument()
	if err != nil {
		return nil, lazyerrors.Error(err)
	}

	docIter := doc.Iterator()
	defer docIter.Close()

	for {
		k, v, iterErr := docIter.Next()
		if errors.Is(iterErr, iterator.ErrIteratorDone) {
			break
		}

		if iterErr != nil {
			return nil, lazyerrors.Error(iterErr)
		}

		switch val := v.(type) {
		case *types.Document:
			sub, redactErr := redactDocument(expr, val)
			if redactErr != nil {
				return nil, redactErr
			}

			if sub != nil {
				result.Set(k, sub)
			}
			// If sub is nil (pruned), the field is omitted entirely.

		case *types.Array:
			filtered, filterErr := redactArray(expr, val)
			if filterErr != nil {
				return nil, filterErr
			}

			result.Set(k, filtered)

		default:
			result.Set(k, v)
		}
	}

	return result, nil
}

// redactArray applies the redact expression to each document element of an array.
// Non-document elements are kept as-is.
func redactArray(expr any, arr *types.Array) (*types.Array, error) {
	out := types.MakeArray(arr.Len())

	arrIter := arr.Iterator()
	defer arrIter.Close()

	for {
		_, v, err := arrIter.Next()
		if errors.Is(err, iterator.ErrIteratorDone) {
			break
		}

		if err != nil {
			return nil, lazyerrors.Error(err)
		}

		switch val := v.(type) {
		case *types.Document:
			sub, redactErr := redactDocument(expr, val)
			if redactErr != nil {
				return nil, redactErr
			}

			if sub != nil {
				out.Append(sub)
			}

		default:
			out.Append(v)
		}
	}

	return out, nil
}

// evalRedactExpr evaluates the redact expression against the document and returns the action string.
func evalRedactExpr(expr any, doc *types.Document) (string, error) {
	switch e := expr.(type) {
	case string:
		// Could be a direct system variable like "$$PRUNE", "$$KEEP", "$$DESCEND".
		if e == "$$PRUNE" || e == "$$KEEP" || e == "$$DESCEND" {
			return e, nil
		}

		// Field path expression  -- evaluate it.
		aggExpr, err := aggregations.NewExpression(e, nil)
		if err != nil {
			return "", handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$redact expression must evaluate to $$PRUNE, $$KEEP, or $$DESCEND",
				"$redact (stage)",
			)
		}

		val, err := aggExpr.Evaluate(doc)
		if err != nil {
			return "$$PRUNE", nil
		}

		s, ok := val.(string)
		if !ok {
			return "", handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$redact expression must evaluate to $$PRUNE, $$KEEP, or $$DESCEND",
				"$redact (stage)",
			)
		}

		return s, nil

	case *types.Document:
		if !operators.IsOperator(e) {
			return "", handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$redact expression must be an operator expression",
				"$redact (stage)",
			)
		}

		op, err := operators.NewOperator(e)
		if err != nil {
			return "", lazyerrors.Error(err)
		}

		val, err := op.Process(doc)
		if err != nil {
			return "", lazyerrors.Error(err)
		}

		s, ok := val.(string)
		if !ok {
			return "", handlererrors.NewCommandErrorMsgWithArgument(
				handlererrors.ErrBadValue,
				"$redact expression must evaluate to $$PRUNE, $$KEEP, or $$DESCEND",
				"$redact (stage)",
			)
		}

		return s, nil

	default:
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			"$redact expression must evaluate to $$PRUNE, $$KEEP, or $$DESCEND",
			"$redact (stage)",
		)
	}
}

// check interfaces
var (
	_ aggregations.Stage = (*redactStage)(nil)
)
