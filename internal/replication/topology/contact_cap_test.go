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

package topology

import (
	"fmt"
	"testing"
)

// Before a configuration is installed every inbound heartbeat contact adds a
// host this node then polls. Before the fix the set was unbounded, so a
// stream of forged heartbeats grew memory and made the node dial any number
// of hosts. Contacts are now capped, keeping the most recent ones.
func TestBootstrapContactsAreBounded(t *testing.T) {
	manager := New(openControlStore(t, t.TempDir()))
	for i := range 1000 {
		if err := manager.ObserveMemberContact(fmt.Sprintf("host%d.example:27017", i), i); err != nil {
			t.Fatal(err)
		}
	}

	state := manager.Snapshot()
	if len(state.Members) > MaxBootstrapContacts {
		t.Fatalf("bootstrap contacts = %d, want at most %d", len(state.Members), MaxBootstrapContacts)
	}
	if _, ok := state.Members[999]; !ok {
		t.Fatal("the most recent contact must be kept")
	}
	if targets := heartbeatTargets(state); len(targets) > MaxBootstrapContacts {
		t.Fatalf("heartbeat targets = %d, want at most %d", len(targets), MaxBootstrapContacts)
	}
}
