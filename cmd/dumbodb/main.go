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
	"sync/atomic"
	"syscall"

	doltevents "github.com/dolthub/dolt/go/libraries/events"
	eventsapi "github.com/dolthub/eventsapi_schema/dolt/services/eventsapi/v1alpha1"

	"github.com/dolthub/dumbodb/internal/backends"
	"github.com/dolthub/dumbodb/internal/backends/dolt"
	"github.com/dolthub/dumbodb/internal/clientconn"
	"github.com/dolthub/dumbodb/internal/handler/registry"
	"github.com/dolthub/dumbodb/internal/metrics"
	"github.com/dolthub/dumbodb/internal/replication/control"
	"github.com/dolthub/dumbodb/internal/replication/membership"
	replicationruntime "github.com/dolthub/dumbodb/internal/replication/runtime"
	"github.com/dolthub/dumbodb/internal/replication/topology"
	"github.com/dolthub/dumbodb/internal/replication/transport"
	"github.com/dolthub/dumbodb/internal/util/logging"
	"github.com/dolthub/dumbodb/internal/util/state"
	"github.com/dolthub/dumbodb/internal/util/tlsutil"
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
	tlsCertificateKeyFilePassword := fs.String("tlsCertificateKeyFilePassword", "", "password for an encrypted PKCS#8 TLS private key")
	tlsCAFile := fs.String("tlsCAFile", "", "certificate authority PEM file for client certificate verification")
	tlsCRLFile := fs.String("tlsCRLFile", "", "certificate revocation list for client certificate verification")
	tlsDisabledProtocols := fs.String("tlsDisabledProtocols", "", "comma-separated TLS protocol versions to disable")
	tlsAllowConnectionsWithoutCertificates := fs.Bool("tlsAllowConnectionsWithoutCertificates", false, "allow TLS clients without certificates")
	tlsClusterFile := fs.String("tlsClusterFile", "", "certificate and private key PEM file for replica-set TLS")
	tlsClusterPassword := fs.String("tlsClusterPassword", "", "password for an encrypted replica-set TLS private key")
	tlsClusterCAFile := fs.String("tlsClusterCAFile", "", "certificate authority PEM file for replica-set TLS")
	tlsClusterAuthX509ExtensionValue := fs.String("tlsClusterAuthX509ExtensionValue", "", "cluster membership certificate extension value")
	tlsClusterAuthX509Attributes := fs.String("tlsClusterAuthX509Attributes", "", "cluster membership certificate subject attributes")
	registerUnsupportedTLSFlags(fs)
	logLevel := fs.String("log-level", "info", "log level (debug, info, warn, error)")
	autoCommit := fs.Bool("auto-commit", false, "automatically commit each write (insert/update/delete) to Dolt history")
	// Session isolation (per-connection working-set overlay, doltCommit merges)
	// is disabled, not removed: the backend and wire paths still accept the
	// switch, so re-enabling is a flag away once there is demand for it.
	const sessionIsolation = false
	sessionTimeout := fs.Duration("session-timeout", 0, "idle timeout for lsid-keyed sessions; default is 30m (matches MongoDB logicalSessionTimeoutMinutes)")
	sessionSweepPeriod := fs.Duration("session-sweep-period", 0, "how often to walk the session registry looking for idle entries; default 1m")
	pprofAddr := fs.String("pprof-addr", "", "if non-empty, expose net/http/pprof on this address (e.g. 127.0.0.1:6060)")
	noMetrics := fs.Bool("no-metrics", false, "disable anonymous daily usage metrics reported to DoltHub")
	auth := fs.Bool("auth", false, "enable access control (forced login; an authenticated connection has full access)")
	replSetName := fs.String("replSet", "", "replica set name for inbound MongoDB replication")
	keyFile := fs.String("keyFile", "", "shared key file for replica-set membership authentication")
	clusterAuthModeValue := fs.String("clusterAuthMode", string(membership.AuthModeKeyFile), "replica-set authentication mode")
	maxConns := fs.Int("maxConns", 0, "maximum concurrent client connections (default: 1000000 or 80% of the open-file limit, whichever is lower)")
	fs.Parse(os.Args[1:])

	if err := rejectUnsupportedTLSFlags(fs); err != nil {
		return err
	}
	tlsDisabledProtocolsSet := flagWasSet(fs, "tlsDisabledProtocols")
	disabledProtocols, parseErr := parseTLSDisabledProtocols(*tlsDisabledProtocols, tlsDisabledProtocolsSet)
	if parseErr != nil {
		return parseErr
	}
	clusterAuthModeSet := flagWasSet(fs, "clusterAuthMode")
	clusterAuthMode, err := membership.ParseAuthMode(*clusterAuthModeValue)
	if err != nil {
		return err
	}
	if err := validateMembershipFlags(*auth, *replSetName, *keyFile, clusterAuthMode, clusterAuthModeSet, *tlsMode); err != nil {
		return err
	}
	if *keyFile != "" || clusterAuthModeSet {
		*auth = true
	}
	if err := validateTLSFlags(*tlsMode, *tlsCertificateKeyFile, *tlsCertificateKeyFilePassword, *tlsCAFile, *tlsCRLFile, tlsDisabledProtocolsSet, *tlsAllowConnectionsWithoutCertificates); err != nil {
		return err
	}
	if err := validateClusterTLSFlags(*tlsMode, *replSetName, *tlsClusterFile, *tlsClusterPassword, *tlsClusterCAFile); err != nil {
		return err
	}
	if err := validateClusterX509Flags(clusterAuthMode, clusterAuthModeSet, *tlsClusterAuthX509Attributes, *tlsClusterAuthX509ExtensionValue); err != nil {
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
	var membershipCredentials *membership.Credentials
	if *keyFile != "" {
		membershipCredentials, err = membership.LoadKeyFile(*keyFile)
		if err != nil {
			return err
		}
	}
	activeAuthMode := membership.AuthMode("")
	if membershipCredentials != nil || clusterAuthModeSet {
		activeAuthMode = clusterAuthMode
	}
	memberOptions := transport.MemberOptions{Credentials: membershipCredentials, AuthMode: activeAuthMode}

	var serverTLSConfig atomic.Pointer[tls.Config]
	var listenerTLSConfig *tls.Config
	serverTLSOptions := tlsutil.ServerConfigOptions{
		CertificateFile:                     *tlsCertificateKeyFile,
		KeyFile:                             *tlsCertificateKeyFile,
		KeyPassword:                         *tlsCertificateKeyFilePassword,
		CAFile:                              *tlsCAFile,
		CRLFile:                             *tlsCRLFile,
		AllowConnectionsWithoutCertificates: *tlsAllowConnectionsWithoutCertificates,
		DisabledProtocols:                   disabledProtocols,
	}
	if *tlsMode != "disabled" {
		configured, configErr := tlsutil.ServerConfig(serverTLSOptions)
		if configErr != nil {
			return configErr
		}
		serverTLSConfig.Store(configured)
		listenerTLSConfig = tlsutil.DynamicServerConfig(serverTLSConfig.Load)
	}

	var memberTLSConfig atomic.Pointer[tls.Config]
	var memberCertificateFile, memberCertificatePassword, memberCAFile string
	if replicationEnabled && (*tlsMode == "preferTLS" || *tlsMode == "requireTLS") {
		memberCertificateFile = *tlsClusterFile
		memberCertificatePassword = *tlsClusterPassword
		memberCAFile = *tlsClusterCAFile
		if memberCertificateFile == "" {
			memberCertificateFile = *tlsCertificateKeyFile
			memberCertificatePassword = *tlsCertificateKeyFilePassword
		}
		if memberCAFile == "" {
			memberCAFile = *tlsCAFile
		}
		configured, configErr := tlsutil.ClientConfigWithPassword(memberCertificateFile, memberCertificateFile, memberCertificatePassword, memberCAFile)
		if configErr != nil {
			return fmt.Errorf("replica-set TLS configuration: %w", configErr)
		}
		memberTLSConfig.Store(configured)
		memberOptions.TLSConfigProvider = memberTLSConfig.Load
	}

	var x509Policy *membership.X509Policy
	if activeAuthMode.AllowsX509() {
		serverConfig := serverTLSConfig.Load()
		memberConfig := memberTLSConfig.Load()
		if serverConfig == nil || len(serverConfig.Certificates) == 0 || serverConfig.Certificates[0].Leaf == nil ||
			memberConfig == nil || len(memberConfig.Certificates) == 0 || memberConfig.Certificates[0].Leaf == nil {
			return fmt.Errorf("X.509 cluster authentication requires a replica-set TLS certificate")
		}
		x509Policy, err = membership.NewX509Policy(serverConfig.Certificates[0].Leaf, *tlsClusterAuthX509Attributes, *tlsClusterAuthX509ExtensionValue)
		if err != nil {
			return err
		}
		if !x509Policy.Matches(memberConfig.Certificates[0].Leaf) {
			return fmt.Errorf("replica-set TLS certificate does not satisfy the cluster X.509 identity policy")
		}
	}

	var rotateCertificates func() error
	if *tlsMode != "disabled" {
		rotateCertificates = func() error {
			newServerConfig, reloadErr := tlsutil.ServerConfig(serverTLSOptions)
			if reloadErr != nil {
				return reloadErr
			}
			var newMemberConfig *tls.Config
			var newPolicy *membership.X509Policy
			if memberTLSConfig.Load() != nil {
				newMemberConfig, reloadErr = tlsutil.ClientConfigWithPassword(memberCertificateFile, memberCertificateFile, memberCertificatePassword, memberCAFile)
				if reloadErr != nil {
					return fmt.Errorf("replica-set TLS configuration: %w", reloadErr)
				}
				if x509Policy != nil {
					newPolicy, reloadErr = membership.NewX509Policy(newServerConfig.Certificates[0].Leaf, *tlsClusterAuthX509Attributes, *tlsClusterAuthX509ExtensionValue)
					if reloadErr != nil {
						return reloadErr
					}
					if !newPolicy.Matches(newMemberConfig.Certificates[0].Leaf) {
						return fmt.Errorf("replica-set TLS certificate does not satisfy the cluster X.509 identity policy")
					}
				}
			}
			serverTLSConfig.Store(newServerConfig)
			if newMemberConfig != nil {
				memberTLSConfig.Store(newMemberConfig)
			}
			if newPolicy != nil {
				x509Policy.Replace(newPolicy)
			}
			return nil
		}
	}
	var replicationTopology *topology.Manager
	var replicationControlStore *control.Store
	var handlerBackend backends.Backend
	if replicationEnabled {
		handlerBackend, err = dolt.NewBackend(*dataDir, logger, *autoCommit, sessionIsolation, *sessionTimeout, *sessionSweepPeriod)
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
		Backend:               handlerBackend,
		Logger:                logger,
		StateProvider:         stateProvider,
		TCPHost:               *addr,
		ReplSetName:           *replSetName,
		ReplicationTopology:   replicationTopology,
		MembershipCredentials: membershipCredentials,
		MembershipAuthMode:    activeAuthMode,
		MembershipX509Policy:  x509Policy,
		RotateCertificates:    rotateCertificates,
		DoltDataDir:           *dataDir,
		AutoCommit:            *autoCommit,
		SessionIsolation:      sessionIsolation,
		SessionTimeout:        *sessionTimeout,
		SessionSweepPeriod:    *sessionSweepPeriod,
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
		TLSKeyPassword:                         *tlsCertificateKeyFilePassword,
		TLSCAFile:                              *tlsCAFile,
		TLSCRLFile:                             *tlsCRLFile,
		TLSAllowConnectionsWithoutCertificates: *tlsAllowConnectionsWithoutCertificates,
		TLSDisabledProtocols:                   disabledProtocols,
		TLSAcceptPlaintext:                     *tlsMode == "allowTLS" || *tlsMode == "preferTLS",
		TLSConfig:                              listenerTLSConfig,
		MaxConns:                               *maxConns,
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
		go topology.NewHeartbeatMesh(replicationTopology, logger, memberOptions).Run(ctx)
		runtime, err := replicationruntime.New(h.Backend, replicationControlStore, replicationTopology, logger, h.BumpAuthGeneration, memberOptions)
		if err != nil {
			return err
		}
		go runtime.Run(ctx)
	}

	listener.Run(ctx)
	return nil
}

func registerUnsupportedTLSFlags(fs *flag.FlagSet) {
	fs.String("tlsLogVersions", "", "unsupported MongoDB TLS option")
	fs.Bool("tlsOnNormalPorts", false, "unsupported MongoDB TLS option")
	fs.Bool("tlsAllowInvalidCertificates", false, "unsupported MongoDB replica-set TLS option")
	fs.Bool("tlsAllowInvalidHostnames", false, "unsupported MongoDB replica-set TLS option")
}

func rejectUnsupportedTLSFlags(fs *flag.FlagSet) error {
	var unsupported *flag.Flag
	fs.Visit(func(f *flag.Flag) {
		if unsupported == nil {
			switch f.Name {
			case "tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames", "tlsLogVersions",
				"tlsOnNormalPorts":
				unsupported = f
			}
		}
	})
	if unsupported == nil {
		return nil
	}
	switch unsupported.Name {
	case "tlsAllowInvalidCertificates", "tlsAllowInvalidHostnames":
		return fmt.Errorf("--%s is a MongoDB replica-set TLS option that DumboDB does not support", unsupported.Name)
	default:
		return fmt.Errorf("--%s is a MongoDB TLS option that DumboDB does not support", unsupported.Name)
	}
}

func validateMembershipFlags(auth bool, replSetName, keyFile string, mode membership.AuthMode, modeSet bool, tlsMode string) error {
	if modeSet && replSetName == "" {
		return fmt.Errorf("--clusterAuthMode requires --replSet")
	}
	if keyFile != "" && replSetName == "" {
		return fmt.Errorf("--keyFile requires --replSet")
	}
	if mode.SendsKeyFile() && (auth || modeSet) && replSetName != "" && keyFile == "" {
		return fmt.Errorf("--keyFile is required when --auth and --replSet are enabled")
	}
	if mode == membership.AuthModeSendX509 && keyFile == "" {
		return fmt.Errorf("--clusterAuthMode=sendX509 requires --keyFile")
	}
	if mode == membership.AuthModeX509 && keyFile != "" {
		return fmt.Errorf("--keyFile cannot be used with --clusterAuthMode=x509")
	}
	if modeSet && mode.AllowsX509() && tlsMode != "preferTLS" && tlsMode != "requireTLS" {
		return fmt.Errorf("--clusterAuthMode=%s requires --tlsMode=preferTLS or requireTLS", mode)
	}
	return nil
}

func validateClusterTLSFlags(tlsMode, replSetName, clusterFile, clusterPassword, clusterCAFile string) error {
	configured := clusterFile != "" || clusterPassword != "" || clusterCAFile != ""
	if configured && replSetName == "" {
		return fmt.Errorf("replica-set TLS options require --replSet")
	}
	if configured && tlsMode == "disabled" {
		return fmt.Errorf("replica-set TLS options require TLS to be enabled")
	}
	if clusterPassword != "" && clusterFile == "" {
		return fmt.Errorf("--tlsClusterPassword requires --tlsClusterFile")
	}
	return nil
}

func validateClusterX509Flags(mode membership.AuthMode, modeSet bool, attributes, extensionValue string) error {
	if attributes != "" && extensionValue != "" {
		return fmt.Errorf("--tlsClusterAuthX509Attributes and --tlsClusterAuthX509ExtensionValue are mutually exclusive")
	}
	if (attributes != "" || extensionValue != "") && (!modeSet || !mode.AllowsX509()) {
		return fmt.Errorf("cluster X.509 identity options require an X.509-capable --clusterAuthMode")
	}
	return nil
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

func validateTLSFlags(tlsMode, certificateKeyFile, certificateKeyFilePassword, caFile, crlFile string, disabledProtocolsSet, allowConnectionsWithoutCertificates bool) error {
	switch tlsMode {
	case "disabled":
		if certificateKeyFile != "" || certificateKeyFilePassword != "" || caFile != "" || crlFile != "" || disabledProtocolsSet || allowConnectionsWithoutCertificates {
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
