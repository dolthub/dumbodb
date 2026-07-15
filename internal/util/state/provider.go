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

package state

import "time"

// Provider provides access to DumboDB process state.
//
// State is set once at construction and never mutated, so all methods are
// safe for concurrent use.
type Provider struct {
	s *State
}

// NewProvider creates a new Provider with the process start time set to now.
func NewProvider() *Provider {
	return &Provider{
		s: &State{Start: time.Now()},
	}
}

// Get returns a copy of the current process state.
func (p *Provider) Get() *State {
	return p.s.deepCopy()
}
