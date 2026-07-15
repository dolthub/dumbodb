// Copyright 2021 FerretDB Inc.
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

// Package state stores DumboDB process state.
package state

import "time"

// State represents DumboDB process state.
type State struct {
	// Start is the time the process started, used to report uptime.
	Start time.Time
}

func (s *State) deepCopy() *State {
	return &State{
		Start: s.Start,
	}
}
