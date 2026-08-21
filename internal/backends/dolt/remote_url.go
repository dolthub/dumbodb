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

package dolt

import (
	"fmt"
	"net/url"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
)

// remoteURL is a parsed, validated remote location for push/fetch.
type remoteURL struct {
	Raw    string
	Parsed *url.URL
	Scheme string
}

// knownRemoteSchemes are the URL schemes DumboDB recognizes. A scheme outside
// this set is rejected by parseRemoteURL. Being known does not imply push/fetch
// support yet; see remoteURL.supported.
var knownRemoteSchemes = map[string]struct{}{
	dbfactory.FileScheme:  {},
	dbfactory.HTTPScheme:  {},
	dbfactory.HTTPSScheme: {},
	dbfactory.AWSScheme:   {},
	dbfactory.GSScheme:    {},
	dbfactory.AzScheme:    {},
	dbfactory.OCIScheme:   {},
	dbfactory.OSSScheme:   {},
	dbfactory.SSHScheme:   {},
	dbfactory.MemScheme:   {}, // test-only; not advertised as a supported remote
}

// implementedRemoteSchemes are the schemes wired into push/fetch today. mem is
// included for hermetic tests only.
var implementedRemoteSchemes = map[string]struct{}{
	dbfactory.FileScheme: {},
	dbfactory.MemScheme:  {},
}

// parseRemoteURL validates raw and returns its parsed form. It errors on empty
// input, an unparseable URL, a missing scheme, or a scheme DumboDB does not
// recognize. The scheme is normalized to lower case.
func parseRemoteURL(raw string) (*remoteURL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("remote url is empty")
	}

	u, err := url.Parse(trimmed)
	if err != nil {
		return nil, fmt.Errorf("invalid remote url %q: %w", trimmed, err)
	}

	if u.Scheme == "" {
		return nil, fmt.Errorf("remote url %q has no scheme (expected e.g. file://, https://)", trimmed)
	}

	scheme := strings.ToLower(u.Scheme)
	if _, ok := knownRemoteSchemes[scheme]; !ok {
		return nil, fmt.Errorf("unrecognized remote scheme %q in %q", u.Scheme, trimmed)
	}

	return &remoteURL{Raw: trimmed, Parsed: u, Scheme: scheme}, nil
}

// supported reports whether push/fetch can currently use this remote's scheme.
// A known-but-unimplemented scheme parses successfully but is not yet usable.
func (r *remoteURL) supported() bool {
	_, ok := implementedRemoteSchemes[r.Scheme]
	return ok
}
