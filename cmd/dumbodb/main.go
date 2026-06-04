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

// Package main is the entry point for the DumboDB server.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"log/slog"
	"net"
	"net/http"
	_ "net/http/pprof"
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/dolthub/dumbodb/internal/clientconn"
	"github.com/dolthub/dumbodb/internal/handler/registry"
	"github.com/dolthub/dumbodb/internal/util/logging"
	"github.com/dolthub/dumbodb/internal/util/state"
	"github.com/dolthub/dumbodb/internal/version"
)

func main() {
	handleVersionFlag()

	logging.Setup(&logging.NewHandlerOpts{
		Base:  "text",
		Level: slog.LevelInfo,
	}, "")
	logger := slog.Default()

	if err := run(logger); err != nil {
		log.Fatal(err)
	}
}

// handleVersionFlag prints the version and exits if --version or -v is passed.
// It must be the only argument; combining it with anything else is an error.
func handleVersionFlag() {
	for _, arg := range os.Args[1:] {
		if arg == "--version" || arg == "-v" || arg == "-version" {
			if len(os.Args) != 2 {
				fmt.Fprintln(os.Stderr, "--version/-v cannot be combined with other arguments")
				os.Exit(2)
			}
			fmt.Printf("dumbodb %s (commit %s)\n", version.Version, version.GitVersion)
			os.Exit(0)
		}
	}
}

func run(logger *slog.Logger) error {
	// Use a custom FlagSet to avoid mysql-related flags registered
	// globally by the vitess dependency.
	fs := flag.NewFlagSet("dumbodb", flag.ExitOnError)
	dataDir := fs.String("data-dir", "data", "directory for storing Dolt data")
	addr := fs.String("addr", "127.0.0.1:27017", "listen address")
	port := fs.Int("port", 0, "listen port (overrides port in --addr if set)")
	logLevel := fs.String("log-level", "info", "log level (debug, info, warn, error)")
	autoCommit := fs.Bool("auto-commit", false, "automatically commit each write (insert/update/delete) to Dolt history")
	sessionIsolation := fs.Bool("session-isolation", false, "run in version-control-native isolation mode: per-connection working-set overlay, doltCommit merges, startTransaction rejected")
	sessionTimeout := fs.Duration("session-timeout", 0, "idle timeout for lsid-keyed sessions; default is 30m (matches MongoDB logicalSessionTimeoutMinutes)")
	sessionSweepPeriod := fs.Duration("session-sweep-period", 0, "how often to walk the session registry looking for idle entries; default 1m")
	pprofAddr := fs.String("pprof-addr", "", "if non-empty, expose net/http/pprof on this address (e.g. 127.0.0.1:6060)")
	fs.Parse(os.Args[1:])

	if *pprofAddr != "" {
		go func() {
			logger.Info("pprof listening", "addr", *pprofAddr)
			if err := http.ListenAndServe(*pprofAddr, nil); err != nil {
				logger.Error("pprof server exited", "err", err)
			}
		}()
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(*logLevel)); err != nil {
		return fmt.Errorf("invalid --log-level %q: %w", *logLevel, err)
	}
	logging.Setup(&logging.NewHandlerOpts{
		Base:  "text",
		Level: level,
	}, "")
	logger = slog.Default()

	if *port != 0 {
		addrExplicit := false
		fs.Visit(func(f *flag.Flag) {
			if f.Name == "addr" {
				addrExplicit = true
			}
		})
		if !addrExplicit {
			host, _, err := net.SplitHostPort(*addr)
			if err != nil {
				return fmt.Errorf("invalid --addr %q: %w", *addr, err)
			}
			*addr = net.JoinHostPort(host, strconv.Itoa(*port))
		}
	}

	stateProvider, err := state.NewProvider("")
	if err != nil {
		return err
	}

	h, closeBackend, err := registry.NewHandler("dolt", &registry.NewHandlerOpts{
		Logger:           logger,
		StateProvider:    stateProvider,
		TCPHost:          *addr,
		ReplSetName:      "",
		DoltDataDir:      *dataDir,
		AutoCommit:         *autoCommit,
		SessionIsolation:   *sessionIsolation,
		SessionTimeout:     *sessionTimeout,
		SessionSweepPeriod: *sessionSweepPeriod,
	})
	if err != nil {
		return err
	}
	defer closeBackend()

	listener, err := clientconn.Listen(&clientconn.NewListenerOpts{
		TCP:     *addr,
		Mode:    clientconn.NormalMode,
		Handler: h,
		Logger:  logger,
	})
	if err != nil {
		return err
	}

	logger.Info("DumboDB server started", "addr", *addr, "data-dir", *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener.Run(ctx)
	return nil
}
