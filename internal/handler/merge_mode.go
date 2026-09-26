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
	"fmt"
	"strings"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/handler/handlererrors"
)

// parseMergeMode reads a collection's mergeMode option. The wire value is
// always the name, never a number, so an unknown name is rejected rather than
// silently resolving to the default -- a client that misspells a mode must not
// believe it got the mode it asked for.
func parseMergeMode(value any, command string) (string, error) {
	name, ok := value.(string)
	if !ok {
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrTypeMismatch,
			fmt.Sprintf("'mergeMode' must be a string (%s)", strings.Join(backends.MergeModeNames(), ", ")),
			command,
		)
	}
	if !backends.ValidMergeMode(name) {
		return "", handlererrors.NewCommandErrorMsgWithArgument(
			handlererrors.ErrBadValue,
			fmt.Sprintf("'mergeMode' must be one of: %s", strings.Join(backends.MergeModeNames(), ", ")),
			command,
		)
	}
	return name, nil
}
