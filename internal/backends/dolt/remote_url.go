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
	"path"
	"path/filepath"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/utils/config"
	"github.com/dolthub/dolt/go/libraries/utils/earl"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
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
	dbfactory.FileScheme:    {},
	dbfactory.HTTPScheme:    {},
	dbfactory.HTTPSScheme:   {},
	dbfactory.S3Scheme:      {},
	dbfactory.AWSScheme:     {},
	dbfactory.GSScheme:      {},
	dbfactory.AzScheme:      {},
	dbfactory.OCIScheme:     {},
	dbfactory.OSSScheme:     {},
	dbfactory.SSHScheme:      {},
	dbfactory.GitFileScheme:  {},
	dbfactory.GitHTTPScheme:  {},
	dbfactory.GitHTTPSScheme: {},
	dbfactory.GitSSHScheme:   {},
	dbfactory.LocalBSScheme:  {}, // test-only local blobstore (s3:// code path)
	dbfactory.MemScheme:      {}, // test-only; not advertised as a supported remote
}

// implementedRemoteSchemes are the schemes wired into push/fetch today. mem and
// localbs are included for hermetic tests only. az shares the s3/gs blob-store
// code path: no dbfactory params, credentials from the ambient environment
// (Azure default credential chain). It has no hermetic fixture and is validated
// only against a live account.
//
// aws is intentionally absent: its aws://[bucket:table] host syntax is rejected
// by net/url.Parse since Go 1.25.2 (golang/go#75678). dolt kludges around it
// with earl.ParseRawWithAWSSupport, which our parse path does not use; enabling
// aws needs a dedicated aws-aware parse end to end. See workspace-1np.8.
var implementedRemoteSchemes = map[string]struct{}{
	dbfactory.FileScheme:    {},
	dbfactory.MemScheme:     {},
	dbfactory.HTTPScheme:    {},
	dbfactory.HTTPSScheme:   {},
	dbfactory.S3Scheme:       {},
	dbfactory.GSScheme:       {},
	dbfactory.AzScheme:       {},
	dbfactory.GitFileScheme:  {},
	dbfactory.GitHTTPScheme:  {},
	dbfactory.GitHTTPSScheme: {},
	dbfactory.GitSSHScheme:   {},
	dbfactory.LocalBSScheme:  {},
}

// parseRemoteURL validates raw and returns its parsed form. Scheme-less input is
// first expanded like the dolt CLI: a bare "org/repo" becomes an https DoltHub
// URL and a scheme-less "host/path" becomes https://host/path (see
// expandSchemelessRemote). It errors on empty input, an unparseable URL, or a
// scheme DumboDB does not recognize. The scheme is normalized to lower case.
func parseRemoteURL(raw string) (*remoteURL, error) {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return nil, fmt.Errorf("remote url is empty")
	}

	trimmed = expandSchemelessRemote(trimmed)

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

// expandSchemelessRemote applies dolt's shorthand for a remote given without a
// scheme, matching env.GetAbsRemoteUrl's scheme-less branches:
//   - "host/path" (a host component is present) -> "https://host/path"
//   - "org/repo"  (path only)                   -> "https://<default_host>/org/repo"
//
// The default host is remotes.default_host from the dolt global config when set,
// otherwise doltremoteapi.dolthub.com. Input that already carries a scheme is
// returned unchanged, so file://, http://, and https:// URLs are never rewritten
// and file remotes keep their exact path (no directory side effects). If the
// input cannot be parsed, it is returned unchanged for parseRemoteURL to reject.
func expandSchemelessRemote(raw string) string {
	u, err := earl.Parse(raw)
	if err != nil || u == nil || u.Scheme != "" {
		return raw
	}
	if u.Host != "" {
		return "https://" + raw
	}
	return "https://" + path.Join(doltHubDefaultHost(), u.Path)
}

// doltHubDefaultHost returns the DoltHub remotes host: remotes.default_host from
// the dolt global config if configured, otherwise the built-in default.
func doltHubDefaultHost() string {
	home, err := env.GetCurrentUserHomeDir()
	if err != nil {
		return env.DefaultRemotesApiHost
	}
	gcfgPath := filepath.Join(home, dbfactory.DoltDir, env.GlobalConfigFile)
	gcfg, err := config.FromFile(gcfgPath, filesys.LocalFS)
	if err != nil {
		return env.DefaultRemotesApiHost
	}
	host, err := gcfg.GetString(config.RemotesApiHostKey)
	if err != nil || strings.TrimSpace(host) == "" {
		return env.DefaultRemotesApiHost
	}
	return strings.TrimSpace(host)
}

// supported reports whether push/fetch can currently use this remote's scheme.
// A known-but-unimplemented scheme parses successfully but is not yet usable.
func (r *remoteURL) supported() bool {
	_, ok := implementedRemoteSchemes[r.Scheme]
	return ok
}
