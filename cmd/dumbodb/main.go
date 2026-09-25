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
	"crypto/tls"
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
	"strings"
	"syscall"

	doltevents "github.com/dolthub/dolt/go/libraries/events"
	eventsapi "github.com/dolthub/eventsapi_schema/dolt/services/eventsapi/v1alpha1"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/clientconn"
	"github.com/dolthub/dumbodb/internal/handler/registry"
	"github.com/dolthub/dumbodb/internal/metrics"
	"github.com/dolthub/dumbodb/internal/replication/control"
	replicationruntime "github.com/dolthub/dumbodb/internal/replication/runtime"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/util/logging"
	"github.com/dolthub/dumbodb/internal/util/state"
	"github.com/dolthub/dumbodb/internal/version"
)

// Tag any events emitted through dolt's global events machinery as DumboDB.
// Our own reporter sets the app id explicitly; this covers any dolt library
// code path that might emit through the shared global.
func init() {
	doltevents.Application = eventsapi.AppID_APP_DUMBODB
}

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
			fmt.Printf("dumbodb %s\n", version.Get().Version)
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
	tlsMode := fs.String("tlsMode", "disabled", "TLS mode (disabled, allowTLS, preferTLS, or requireTLS)")
	tlsCertificateKeyFile := fs.String("tlsCertificateKeyFile", "", "certificate and private key PEM file for TLS")
	tlsCAFile := fs.String("tlsCAFile", "", "certificate authority PEM file for client certificate verification")
	tlsCRLFile := fs.String("tlsCRLFile", "", "certificate revocation list for client certificate verification")
	tlsDisabledProtocols := fs.String("tlsDisabledProtocols", "", "comma-separated TLS protocol versions to disable")
	tlsAllowConnectionsWithoutCertificates := fs.Bool("tlsAllowConnectionsWithoutCertificates", false, "allow TLS clients without certificates")
	registerUnsupportedTLSFlags(fs)
	logLevel := fs.String("log-level", "info", "log level (debug, info, warn, error)")
	autoCommit := fs.Bool("auto-commit", false, "automatically commit each write (insert/update/delete) to Dolt history")
	sessionIsolation := fs.Bool("session-isolation", false, "run in version-control-native isolation mode: per-connection working-set overlay, doltCommit merges, startTransaction rejected")
	sessionTimeout := fs.Duration("session-timeout", 0, "idle timeout for lsid-keyed sessions; default is 30m (matches MongoDB logicalSessionTimeoutMinutes)")
	sessionSweepPeriod := fs.Duration("session-sweep-period", 0, "how often to walk the session registry looking for idle entries; default 1m")
	pprofAddr := fs.String("pprof-addr", "", "if non-empty, expose net/http/pprof on this address (e.g. 127.0.0.1:6060)")
	noMetrics := fs.Bool("no-metrics", false, "disable anonymous daily usage metrics reported to DoltHub")
	auth := fs.Bool("auth", false, "enable access control (forced login; an authenticated connection has full access)")
	replSetName := fs.String("replSet", "", "replica set name for inbound MongoDB replication")
	fs.Parse(os.Args[1:])

	if err := rejectUnsupportedTLSFlags(fs); err != nil {
		return err
	}
	tlsDisabledProtocolsSet := flagWasSet(fs, "tlsDisabledProtocols")
	disabledProtocols, parseErr := parseTLSDisabledProtocols(*tlsDisabledProtocols, tlsDisabledProtocolsSet)
	if parseErr != nil {
		return parseErr
	}
	if *autoCommit && *sessionIsolation {
		return fmt.Errorf("--auto-commit and --session-isolation are mutually exclusive: auto-commit commits every write at the command boundary, while session-isolation defers commits to an explicit doltCommit merge")
	}
	if err := validateTLSFlags(*tlsMode, *tlsCertificateKeyFile, *tlsCAFile, *tlsCRLFile, tlsDisabledProtocolsSet, *tlsAllowConnectionsWithoutCertificates); err != nil {
		return err
	}

	metricsEnabled := !*noMetrics && !envDisablesMetrics()

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

	replicationConfiguration, replicationEnabled := replicationControlConfiguration(*replSetName, *addr)
	var replicationTopology *topology.Manager
	var replicationControlStore *control.Store
	var handlerBackend backends.Backend
	var err error
	if replicationEnabled {
		handlerBackend, err = dolt.NewBackend(*dataDir, logger, *autoCommit, *sessionIsolation, *sessionTimeout, *sessionSweepPeriod)
		if err != nil {
			return err
		}
		controlStore, err := control.Open(handlerBackend, replicationConfiguration)
		if err != nil {
			handlerBackend.Close()
			return err
		}
		replicationControlStore = controlStore
		defer replicationControlStore.Close()
		replicationTopology = topology.New(controlStore)
		logger.Info("replication control state opened", "replSet", *replSetName)
	}

	stateProvider := state.NewProvider()

	h, closeBackend, err := registry.NewHandler("dolt", &registry.NewHandlerOpts{
		Backend:             handlerBackend,
		Logger:              logger,
		StateProvider:       stateProvider,
		TCPHost:             *addr,
		ReplSetName:         *replSetName,
		ReplicationTopology: replicationTopology,
		DoltDataDir:         *dataDir,
		AutoCommit:          *autoCommit,
		SessionIsolation:    *sessionIsolation,
		SessionTimeout:      *sessionTimeout,
		SessionSweepPeriod:  *sessionSweepPeriod,
		TestOpts: registry.TestOpts{
			EnableNewAuth: *auth,
		},
	})
	if err != nil {
		return err
	}
	defer closeBackend()

	listener, err := clientconn.Listen(&clientconn.NewListenerOpts{
		TCP:                                    *addr,
		TLS:                                    *tlsMode != "disabled",
		TLSCertFile:                            *tlsCertificateKeyFile,
		TLSKeyFile:                             *tlsCertificateKeyFile,
		TLSCAFile:                              *tlsCAFile,
		TLSCRLFile:                             *tlsCRLFile,
		TLSAllowConnectionsWithoutCertificates: *tlsAllowConnectionsWithoutCertificates,
		TLSDisabledProtocols:                   disabledProtocols,
		TLSAcceptPlaintext:                     *tlsMode == "allowTLS" || *tlsMode == "preferTLS",
		Mode:                                   clientconn.NormalMode,
		Handler:                                h,
		Logger:                                 logger,
	})
	if err != nil {
		return err
	}

	logger.Info("DumboDB server started", "addr", *addr, "tlsMode", *tlsMode, "data-dir", *dataDir)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if metricsEnabled {
		logger.Info("anonymous usage metrics enabled; disable with --no-metrics or DUMBODB_NO_METRICS=1")
	} else {
		logger.Info("anonymous usage metrics disabled")
	}
	go metrics.RunReporter(ctx, logger, version.Get().Version, metricsEnabled)
	if replicationTopology != nil {
		go topology.NewHeartbeatMesh(replicationTopology, logger).Run(ctx)
		runtime, err := replicationruntime.New(h.Backend, replicationControlStore, replicationTopology, logger, h.BumpAuthGeneration)
		if err != nil {
			return err
		}
		go runtime.Run(ctx)
	}

	listener.Run(ctx)
	return nil
}

func registerUnsupportedTLSFlags(fs *flag.FlagSet) {
	fs.String("tlsCertificateKeyFilePassword", "", "unsupported MongoDB TLS option")
	fs.String("tlsLogVersions", "", "unsupported MongoDB TLS option")
	fs.Bool("tlsOnNormalPorts", false, "unsupported MongoDB TLS option")
	fs.Bool("tlsAllowInvalidCertificates", false, "unsupported MongoDB replica-set TLS option")
	fs.Bool("tlsAllowInvalidHostnames", false, "unsupported MongoDB replica-set TLS option")
	fs.String("tlsClusterFile", "", "unsupported MongoDB replica-set TLS option")
	fs.String("tlsClusterPassword", "", "unsupported MongoDB replica-set TLS option")
	fs.String("tlsClusterCAFile", "", "unsupported MongoDB replica-set TLS option")
	fs.String("tlsClusterAuthX509ExtensionValue", "", "unsupported MongoDB replica-set TLS option")
	fs.String("tlsClusterAuthX509Attributes", "", "unsupported MongoDB replica-set TLS option")
}

func rejectUnsupportedTLSFlags(fs *flag.FlagSet) error {
	var unsupported *flag.Flag
	fs.Visit(func(f *flag.Flag) {
		if unsupported == nil {
			switch f.Name {
			case "tlsCertificateKeyFilePassword",
				"tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames", "tlsLogVersions",
				"tlsOnNormalPorts", "tlsClusterFile", "tlsClusterPassword", "tlsClusterCAFile",
				"tlsClusterAuthX509ExtensionValue", "tlsClusterAuthX509Attributes":
				unsupported = f
			}
		}
	})
	if unsupported == nil {
		return nil
	}
	switch unsupported.Name {
	case "tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames",
		"tlsClusterFile", "tlsClusterPassword", "tlsClusterCAFile",
		"tlsClusterAuthX509ExtensionValue", "tlsClusterAuthX509Attributes":
		return fmt.Errorf("--%s is a MongoDB replica-set TLS option that DumboDB does not support", unsupported.Name)
	default:
		return fmt.Errorf("--%s is a MongoDB TLS option that DumboDB does not support", unsupported.Name)
	}
}

func flagWasSet(fs *flag.FlagSet, name string) bool {
	found := false
	fs.Visit(func(f *flag.Flag) {
		if f.Name == name {
			found = true
		}
	})
	return found
}

func parseTLSDisabledProtocols(value string, enabled bool) ([]uint16, error) {
	if !enabled || value == "none" {
		return nil, nil
	}
	versions := map[string]uint16{
		"TLS1_0":   tls.VersionTLS10,
		"TLS1_1":   tls.VersionTLS11,
		"TLS1_2":   tls.VersionTLS12,
		"TLS1_3":   tls.VersionTLS13,
		"noTLS1_0": tls.VersionTLS10,
		"noTLS1_1": tls.VersionTLS11,
		"noTLS1_2": tls.VersionTLS12,
		"noTLS1_3": tls.VersionTLS13,
	}
	var disabled []uint16
	for _, token := range strings.Split(value, ",") {
		version, ok := versions[token]
		if !ok {
			return nil, fmt.Errorf("unrecognized --tlsDisabledProtocols value %q", token)
		}
		disabled = append(disabled, version)
	}
	return disabled, nil
}

func validateTLSFlags(tlsMode, certificateKeyFile, caFile, crlFile string, disabledProtocolsSet, allowConnectionsWithoutCertificates bool) error {
	switch tlsMode {
	case "disabled":
		if certificateKeyFile != "" || caFile != "" || crlFile != "" || disabledProtocolsSet || allowConnectionsWithoutCertificates {
			return fmt.Errorf("need to enable TLS via --tlsMode when using TLS configuration options")
		}
		return nil
	case "allowTLS", "preferTLS", "requireTLS":
		if certificateKeyFile == "" {
			return fmt.Errorf("--tlsCertificateKeyFile is required when --tlsMode is %s", tlsMode)
		}
		if caFile == "" {
			return fmt.Errorf("--tlsCAFile is required when --tlsMode is %s", tlsMode)
		}
		return nil
	default:
		return fmt.Errorf("invalid --tlsMode %q; supported modes are disabled, allowTLS, preferTLS, and requireTLS", tlsMode)
	}
}

func replicationControlConfiguration(replSetName, memberHost string) (control.Configuration, bool) {
	if replSetName == "" {
		return control.Configuration{}, false
	}
	return control.Configuration{
		SetName:    replSetName,
		MemberHost: memberHost,
	}, true
}

// envDisablesMetrics reports whether DUMBODB_NO_METRICS is set to a truthy value.
func envDisablesMetrics() bool {
	v, ok := os.LookupEnv("DUMBODB_NO_METRICS")
	if !ok {
		return false
	}
	b, err := strconv.ParseBool(v)
	return err == nil && b
}
