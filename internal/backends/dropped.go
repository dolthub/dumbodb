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

package backends

import "fmt"

// DatabaseDroppedError is why a command, cursor or iterator reading a database
// was stopped: the database was dropped, or replaced by a restore, under it.
type DatabaseDroppedError struct {
	Database string
	// CollectionUUIDs maps each collection the database held to the UUID
	// listCollections reported for it.
	CollectionUUIDs map[string]string
}

func (e *DatabaseDroppedError) Error() string {
	return fmt.Sprintf("database %q was dropped", e.Database)
}

// CollectionDroppedReason is mongod's QueryPlanKilled reason for work on
// collection that the drop stopped.
func (e *DatabaseDroppedError) CollectionDroppedReason(collection string) string {
	if uuid, ok := e.CollectionUUIDs[collection]; ok && uuid != "" {
		return "collection dropped. UUID " + uuid
	}
	return "collection dropped"
}
