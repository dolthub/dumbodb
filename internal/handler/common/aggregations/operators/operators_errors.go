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

// operatorErrorCode represents the type of error.
type operatorErrorCode uint

const (
	_ operatorErrorCode = iota

	ErrArgsInvalidLen
	ErrTooManyFields
	ErrNotImplemented
	ErrInvalidExpression

	// ErrInvalidNestedExpression indicates that operator inside the target operator does not exist.
	ErrInvalidNestedExpression

	// ErrGetFieldUnknownArgument indicates that $getField received an argument it does not accept.
	ErrGetFieldUnknownArgument

	// ErrGetFieldMissingField indicates that $getField was used without its required 'field' argument.
	ErrGetFieldMissingField

	// ErrGetFieldMissingInput indicates that the $getField full form omitted the required 'input' argument.
	ErrGetFieldMissingInput

	// ErrSetFieldUnknownArgument indicates that $setField or $unsetField received an argument it does not accept.
	ErrSetFieldUnknownArgument

	// ErrSetFieldMissingField indicates that $setField or $unsetField was used without its required 'field' argument.
	ErrSetFieldMissingField

	// ErrSetFieldMissingInput indicates that $setField or $unsetField was used without its required 'input' argument.
	ErrSetFieldMissingInput

	// ErrSetFieldNonConstantField indicates that a $setField or $unsetField 'field'
	// argument was an expression rather than a constant.
	ErrSetFieldNonConstantField

	// ErrSetFieldFieldPathReference indicates that a $setField or $unsetField 'field'
	// argument was a field path or variable reference.
	ErrSetFieldFieldPathReference

	// ErrSetFieldMissingValue indicates that $setField was used without its required 'value' argument.
	ErrSetFieldMissingValue

	// ErrSetFieldFieldNotString indicates that a $setField or $unsetField 'field'
	// argument was rejected while parsing for not being a string.
	ErrSetFieldFieldNotString
)

func newOperatorError(code operatorErrorCode, name, msg string) error {
	return OperatorError{
		code: code,
		name: name,
		msg:  msg,
	}
}

// OperatorError is used for reporting operator errors.
type OperatorError struct {
	msg  string
	name string
	code operatorErrorCode
}

func (opErr OperatorError) Error() string {
	return opErr.msg
}

func (opErr OperatorError) Code() operatorErrorCode {
	return opErr.code
}

// Name returns the name of the operator (e.g. $sum) that produced an error.
func (opErr OperatorError) Name() string {
	return opErr.name
}
