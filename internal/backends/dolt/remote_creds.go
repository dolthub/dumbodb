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
	"net"
	"os"
	"path/filepath"
	"strings"

	"github.com/dolthub/dolt/go/libraries/doltcore/creds"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/doltcore/env"
	"github.com/dolthub/dolt/go/libraries/doltcore/grpcendpoint"
	"github.com/dolthub/dolt/go/libraries/utils/config"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
)

// credsDirName is the directory under .dolt holding client credential JWKs.
const credsDirName = "creds"

// loadDoltCreds loads the active Dolt client credential exactly as the dolt CLI
// does: the key id comes from user.creds in ~/.dolt/config_global.json, then the
// matching <kid>.jwk is read from ~/.dolt/creds. DOLT_ROOT_PATH and HOME are
// honored through env.GetCurrentUserHomeDir, so a server operator provisions
// credentials by running `dolt login` in the server's environment.
//
// ok is false with a nil error when no credential is configured, letting callers
// decide whether the transport requires one (https does, insecure http does not).
func loadDoltCreds() (dc creds.DoltCreds, ok bool, err error) {
	home, err := env.GetCurrentUserHomeDir()
	if err != nil {
		return creds.DoltCreds{}, false, err
	}

	gcfgPath := filepath.Join(home, dbfactory.DoltDir, env.GlobalConfigFile)
	gcfg, err := config.FromFile(gcfgPath, filesys.LocalFS)
	if err != nil {
		// No global config means `dolt login` has never run here.
		return creds.DoltCreds{}, false, nil
	}

	kid, err := gcfg.GetString(config.UserCreds)
	if err != nil || kid == "" {
		return creds.DoltCreds{}, false, nil
	}

	credsPath := filepath.Join(home, dbfactory.DoltDir, credsDirName, kid+creds.JWKFileExtension)
	dc, err = creds.JWKCredsReadFromFile(filesys.LocalFS, credsPath)
	if err != nil {
		return creds.DoltCreds{}, false, fmt.Errorf("reading dolt credential %s: %w", credsPath, err)
	}
	if !dc.IsPrivKeyValid() || !dc.IsPubKeyValid() {
		return creds.DoltCreds{}, false, fmt.Errorf("dolt credential %s is missing a valid key pair", credsPath)
	}
	return dc, true, nil
}

// grpcDialProvider builds the GRPCDialProvider a gRPC remote (http/https) needs
// in the dbfactory params map. TLS and dial-option setup are delegated to Dolt's
// standard provider; this wrapper only injects the loaded client credential as a
// per-RPC bearer credential. hasCreds is false when running against an
// unauthenticated remote (e.g. a local remotesrv over insecure http).
type grpcDialProvider struct {
	dc       creds.DoltCreds
	hasCreds bool
}

var _ dbfactory.GRPCDialProvider = grpcDialProvider{}

func (p grpcDialProvider) GetGRPCDialParams(cfg grpcendpoint.Config) (dbfactory.GRPCRemoteConfig, error) {
	if p.hasCreds {
		cfg.Creds = p.dc.RPCCreds(audienceFromEndpoint(cfg.Endpoint))
	}
	return env.NewGRPCDialProvider().GetGRPCDialParams(cfg)
}

// audienceFromEndpoint returns the JWT audience for a signed credential: the bare
// host of the remote endpoint, matching Dolt's own derivation.
func audienceFromEndpoint(endpoint string) string {
	if host, _, err := net.SplitHostPort(endpoint); err == nil {
		return host
	}
	return endpoint
}

// isGRPCScheme reports whether a remote scheme is served over the remotesapi
// gRPC transport, which requires a GRPCDialProvider and cannot use PrepareDB.
func isGRPCScheme(scheme string) bool {
	return scheme == dbfactory.HTTPScheme || scheme == dbfactory.HTTPSScheme
}

// isGitScheme reports whether a scheme is served by dolt's GitRemoteFactory
// (dolt stored in a git repository). These need a git_cache_root param and shell
// out to the git CLI.
func isGitScheme(scheme string) bool {
	return strings.HasPrefix(scheme, "git+")
}

// gitPreparable reports whether PrepareDB is supported for a git scheme. Only
// git+file supports it (it runs `git init --bare`); git+http/https/ssh error, so
// their remote repository must already exist.
func gitPreparable(scheme string) bool {
	return scheme == dbfactory.GitFileScheme
}

// isCloneableScheme reports whether dumboClone can materialize a new database
// from a remote of this scheme: direct stores (file, s3, gs, az, localbs) and
// the gRPC transports. mem is excluded (in-process only, nothing to clone
// across).
func isCloneableScheme(scheme string) bool {
	switch scheme {
	case dbfactory.FileScheme, dbfactory.S3Scheme, dbfactory.GSScheme,
		dbfactory.AzScheme, dbfactory.LocalBSScheme:
		return true
	default:
		return isGRPCScheme(scheme) || isGitScheme(scheme)
	}
}

// remoteDBParams returns the dbfactory params for opening a remote of the given
// scheme. gRPC remotes get a dial provider carrying any configured credential;
// an https remote with no credential is rejected here with a `dolt login` hint.
func (b *Backend) remoteDBParams(scheme string) (map[string]interface{}, error) {
	params := map[string]interface{}{
		dbfactory.DisableSingletonCacheParam: "true",
	}

	if isGitScheme(scheme) {
		// Git remotes keep a per-remote bare-repo cache under
		// <cacheRoot>/.dolt/git-remote-cache. Point the cache root at a
		// server-owned directory; the factory creates the subtree on demand.
		cacheRoot := filepath.Join(b.dataDir, gitRemoteCacheRoot)
		if err := os.MkdirAll(cacheRoot, 0o755); err != nil {
			return nil, fmt.Errorf("creating git remote cache root: %w", err)
		}
		params[dbfactory.GitCacheRootParam] = cacheRoot
		return params, nil
	}

	if !isGRPCScheme(scheme) {
		return params, nil
	}

	dc, ok, err := loadDoltCreds()
	if err != nil {
		return nil, err
	}
	if !ok && scheme == dbfactory.HTTPSScheme {
		return nil, fmt.Errorf("no Dolt credentials found for a secure remote; run `dolt login` in the server environment")
	}
	params[dbfactory.GRPCDialProviderParam] = grpcDialProvider{dc: dc, hasCreds: ok}
	return params, nil
}

// gitRemoteCacheRoot is the directory under the backend data dir used as the
// git_cache_root for git remotes. The factory creates .dolt/git-remote-cache
// beneath it.
const gitRemoteCacheRoot = ".git-remote-cache"
