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
	"os"
	"os/signal"
	"strconv"
	"syscall"

	"github.com/dolthub/dumbodb/internal/clientconn"
	"github.com/dolthub/dumbodb/internal/handler/registry"
	"github.com/dolthub/dumbodb/internal/util/logging"
	"github.com/dolthub/dumbodb/internal/util/state"
)

func main() {
	logging.Setup(&logging.NewHandlerOpts{
		Base:  "text",
		Level: slog.LevelInfo,
	}, "")
	logger := slog.Default()

	if err := run(logger); err != nil {
		log.Fatal(err)
	}
}

func run(logger *slog.Logger) error {
	dataDir := flag.String("data-dir", "data", "directory for storing Dolt data")
	addr := flag.String("addr", "127.0.0.1:27017", "listen address")
	port := flag.Int("port", 0, "listen port (overrides port in --addr if set)")
	logLevel := flag.String("log-level", "info", "log level (debug, info, warn, error)")
	autoCommit := flag.Bool("auto-commit", false, "automatically commit each write (insert/update/delete) to Dolt history")
	flag.Parse()

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
		flag.Visit(func(f *flag.Flag) {
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
		Logger:        logger,
		StateProvider: stateProvider,
		TCPHost:       *addr,
		ReplSetName:   "",
		DoltDataDir:   *dataDir,
		AutoCommit:    *autoCommit,
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
