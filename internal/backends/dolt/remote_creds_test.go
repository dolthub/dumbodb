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
	"path/filepath"
	"testing"

	"github.com/dolthub/dolt/go/libraries/doltcore/creds"
	"github.com/dolthub/dolt/go/libraries/doltcore/dbfactory"
	"github.com/dolthub/dolt/go/libraries/utils/config"
	"github.com/dolthub/dolt/go/libraries/utils/filesys"
)

// writeDoltLogin lays out a .dolt home under root exactly as `dolt login` would:
// a generated key pair written as <kid>.jwk in creds/, and user.creds set to that
// key id in config_global.json.
func writeDoltLogin(t *testing.T, root string) creds.DoltCreds {
	t.Helper()

	dc, err := creds.GenerateCredentials()
	if err != nil {
		t.Fatalf("generate creds: %v", err)
	}

	credsDir := filepath.Join(root, dbfactory.DoltDir, credsDirName)
	if err := filesys.LocalFS.MkDirs(credsDir); err != nil {
		t.Fatalf("mkdir creds: %v", err)
	}
	if _, err := creds.JWKCredsWriteToDir(filesys.LocalFS, credsDir, dc); err != nil {
		t.Fatalf("write jwk: %v", err)
	}

	gcfgPath := filepath.Join(root, dbfactory.DoltDir, "config_global.json")
	gcfg, err := config.NewFileConfig(gcfgPath, filesys.LocalFS, map[string]string{
		config.UserCreds: dc.KeyIDBase32Str(),
	})
	if err != nil {
		t.Fatalf("write global config: %v", err)
	}
	if got, _ := gcfg.GetString(config.UserCreds); got != dc.KeyIDBase32Str() {
		t.Fatalf("global config user.creds = %q, want %q", got, dc.KeyIDBase32Str())
	}
	return dc
}

func TestLoadDoltCreds_FromDoltLogin(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOLT_ROOT_PATH", root)
	want := writeDoltLogin(t, root)

	dc, ok, err := loadDoltCreds()
	if err != nil {
		t.Fatalf("loadDoltCreds: %v", err)
	}
	if !ok {
		t.Fatal("loadDoltCreds: ok = false, want a credential")
	}
	if dc.KeyIDBase32Str() != want.KeyIDBase32Str() {
		t.Errorf("loaded kid = %s, want %s", dc.KeyIDBase32Str(), want.KeyIDBase32Str())
	}
	if !dc.IsPrivKeyValid() || !dc.IsPubKeyValid() {
		t.Error("loaded credential key pair is invalid")
	}
}

func TestLoadDoltCreds_NoLogin(t *testing.T) {
	t.Setenv("DOLT_ROOT_PATH", t.TempDir())

	_, ok, err := loadDoltCreds()
	if err != nil {
		t.Fatalf("loadDoltCreds: %v", err)
	}
	if ok {
		t.Error("loadDoltCreds: ok = true with no dolt login, want false")
	}
}

// TestRemoteDBParams_HTTPSRequiresCreds verifies a secure remote is rejected with
// a login hint when no credential is configured, while insecure http is allowed
// to proceed without one.
func TestRemoteDBParams_HTTPSRequiresCreds(t *testing.T) {
	t.Setenv("DOLT_ROOT_PATH", t.TempDir())

	if _, err := newTestBackend(t).remoteDBParams(dbfactory.HTTPSScheme); err == nil {
		t.Error("remoteDBParams(https) with no creds: want error, got nil")
	}

	params, err := newTestBackend(t).remoteDBParams(dbfactory.HTTPScheme)
	if err != nil {
		t.Fatalf("remoteDBParams(http): %v", err)
	}
	if _, ok := params[dbfactory.GRPCDialProviderParam]; !ok {
		t.Error("remoteDBParams(http): missing GRPCDialProviderParam")
	}
}

func TestRemoteDBParams_HTTPSWithCreds(t *testing.T) {
	root := t.TempDir()
	t.Setenv("DOLT_ROOT_PATH", root)
	writeDoltLogin(t, root)

	params, err := newTestBackend(t).remoteDBParams(dbfactory.HTTPSScheme)
	if err != nil {
		t.Fatalf("remoteDBParams(https) with creds: %v", err)
	}
	dp, ok := params[dbfactory.GRPCDialProviderParam].(grpcDialProvider)
	if !ok {
		t.Fatal("remoteDBParams(https): missing grpcDialProvider")
	}
	if !dp.hasCreds {
		t.Error("grpcDialProvider.hasCreds = false, want true")
	}
}
