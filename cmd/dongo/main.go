// Copyright 2024 Dolt Inc.
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

// Package main is the entry point for the Dongo server.
package main

import (
	"context"
	"flag"
	"log"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/dolthub/dongo/internal/clientconn"
	"github.com/dolthub/dongo/internal/clientconn/connmetrics"
	"github.com/dolthub/dongo/internal/handler/registry"
	"github.com/dolthub/dongo/internal/util/logging"
	"github.com/dolthub/dongo/internal/util/state"
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
	flag.Parse()

	stateProvider, err := state.NewProvider("")
	if err != nil {
		return err
	}

	metrics := connmetrics.NewListenerMetrics()

	h, closeBackend, err := registry.NewHandler("dolt", &registry.NewHandlerOpts{
		Logger:        logger,
		ConnMetrics:   metrics.ConnMetrics,
		StateProvider: stateProvider,
		TCPHost:       "127.0.0.1",
		ReplSetName:   "",
		DoltDataDir:   *dataDir,
	})
	if err != nil {
		return err
	}
	defer closeBackend()

	listener, err := clientconn.Listen(&clientconn.NewListenerOpts{
		TCP:     *addr,
		Mode:    clientconn.NormalMode,
		Metrics: metrics,
		Handler: h,
		Logger:  logger,
	})
	if err != nil {
		return err
	}

	logger.Info("Dongo server started", "addr", *addr, "data-dir", *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	listener.Run(ctx)
	return nil
}
