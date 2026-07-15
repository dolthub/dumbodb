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

// Command soak drives a long-running mixed workload against dumbodb,
// samples its memory footprint, and raises an alert when the
// post-GC floor trends upward. Intended for unattended VM use.
//
// By default the soak owns dumbodb's lifecycle: picks a free port,
// starts the binary with a fresh data directory, kills it cleanly on
// exit. Pass -attach-addr and -attach-pid to point at an external
// dumbodb instead.
//
// Linux only: reads VmRSS from /proc/<pid>/status.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/dolthub/dumbodb/internal/version"
)

// buildVersion returns the version under test, matching the Makefile's
// GIT_VERSION (git describe --tags --always --dirty). It prefers the value
// embedded at build time via -ldflags; absent that (e.g. a plain go build), it
// runs the same git command from the current directory.
func buildVersion() string {
	if v := version.GitVersion; v != "" && v != "unknown" {
		return v
	}

	out, err := exec.Command("git", "describe", "--tags", "--always", "--dirty").Output()
	if err != nil {
		return version.GitVersion
	}

	return strings.TrimSpace(string(out))
}

func main() {
	var (
		// Managed-server flags.
		dumbodbBinary = flag.String("dumbodb", "dumbodb", "path to the dumbodb binary; resolved via $PATH if not absolute")
		dumbodbArgs   = flag.String("dumbodb-args", "", "extra args passed through to dumbodb (e.g. \"-pprof-addr 127.0.0.1:6060\")")
		port          = flag.Int("port", 27017, "TCP port dumbodb should listen on")
		dataDir       = flag.String("data-dir", "", "dumbodb data directory; defaults to a tempdir that is deleted on exit")
		keepData      = flag.Bool("keep-data", false, "preserve the data directory and dumbodb log file on exit")
		readyTimeout  = flag.Duration("ready-timeout", 30*time.Second, "wait this long for dumbodb to accept connections at startup")
		stopGrace     = flag.Duration("stop-grace", 10*time.Second, "wait this long for dumbodb to exit after SIGTERM before SIGKILL")

		// Attach-mode flags (override managed server).
		attachAddr = flag.String("attach-addr", "", "MongoDB URI of an externally-managed dumbodb; bypasses the managed-server path")
		attachPid  = flag.Int("attach-pid", 0, "PID of the externally-managed dumbodb when -attach-addr is set")

		// Run shape.
		runtime        = flag.Duration("runtime", 2*time.Hour, "stop after this long; pass 0 to run forever")
		sampleInterval = flag.Duration("sample-interval", time.Minute, "memory-sample period")
		csvPath        = flag.String("csv", "", "if set, append CSV rows here (one per sample)")

		// Workload pools.
		sessionWorkers = flag.Int("workers-session", 4, "short-lived connect/ping/disconnect workers")
		crudWorkers    = flag.Int("workers-crud", 2, "long-lived clients running a mixed-size CRUD rotation")
		bulkWorkers    = flag.Int("workers-bulk", 1, "InsertMany bulk-write workers")
		aggWorkers     = flag.Int("workers-agg", 1, "aggregation-pipeline workers (cursor drainers)")
		indexedWorkers = flag.Int("workers-indexed", 1, "secondary-index create/drop and indexed-find workers")
		vcsWorkers     = flag.Int("workers-vcs", 1, "commit / branch / merge workers")
		txnWorkers     = flag.Int("workers-txn", 1, "multi-op session.WithTransaction workers")
		opsInterval    = flag.Duration("ops-interval", time.Second, "base delay between ops; heavier worker types scale this up internally")
		sessionDelay   = flag.Duration("session-delay", 250*time.Millisecond, "delay between cycles on each session-churn worker")
		collectionCap  = flag.Int("collection-cap", 0, "if >0, hold the shared collection near this many docs via a trim worker; 0 (default) lets it grow unbounded to stress the aggregators")
		trimInterval   = flag.Duration("trim-interval", time.Second, "how often the trim worker checks the collection size and deletes the overflow")

		// Detection.
		slopeWindow      = flag.Duration("slope-window", 6*time.Hour, "rolling window over which the slope is computed")
		slopeThresholdMB = flag.Float64("slope-threshold-mb-per-hour", 1.0, "MB/hour of sustained growth that triggers an alert")
		cooldown         = flag.Duration("cooldown", 24*time.Hour, "minimum delay between alerts")

		// Email alerting via SES.
		emailFrom = flag.String("email-from", "", "SES verified From address")
		emailTo   = flag.String("email-to", "", "alert recipient(s), comma-separated")
		emailRegn = flag.String("email-region", "", "AWS region for SES; default chain region used when blank")
		emailSubj = flag.String("email-subject", "dumbodb soak alert", "Subject line for SES alerts")

		// Local report sink for runs without SES credentials. Writes each
		// alert/summary report (text, html, png) into this directory.
		reportDir = flag.String("report-dir", "", "if set, write each report locally instead of (or in addition to) mailing it")

		// Optional secondary alert channel (e.g. local mailx, ntfy, curl-to-webhook).
		alertCmd = flag.String("alert-cmd", "", "shell command to run on each alert in addition to email; body on stdin")
	)
	flag.Parse()

	// The soak has no value running unattended without a sink for its
	// reports, but any one of SES, a local report dir, or an alert
	// command satisfies that.
	emailEnabled := *emailFrom != "" && *emailTo != ""
	if !emailEnabled && *reportDir == "" && *alertCmd == "" {
		log.Fatal("configure at least one report sink: -email-from/-email-to, -report-dir, or -alert-cmd")
	}
	if (*attachAddr == "") != (*attachPid == 0) {
		log.Fatal("-attach-addr and -attach-pid must be set together (or not at all)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *runtime > 0 {
		ctx, _ = context.WithTimeout(ctx, *runtime)
	}

	addr, pid, srv, stopServer, err := resolveServer(ctx, *attachAddr, *attachPid, *dumbodbBinary, *dumbodbArgs, *dataDir, *port, *keepData, *readyTimeout, *stopGrace)
	if err != nil {
		log.Fatalf("dumbodb: %v", err)
	}
	defer stopServer()

	// serverDied fires when the managed process exits. A nil channel
	// (attach mode) blocks forever, so the select case never runs.
	var serverDied <-chan struct{}
	if srv != nil {
		serverDied = srv.died()
	}

	sampler := newSampler(pid)
	if _, err := sampler.sample(); err != nil {
		log.Fatalf("initial sample for pid %d: %v", pid, err)
	}

	writer, err := newCSVWriter(*csvPath)
	if err != nil {
		log.Fatalf("csv writer: %v", err)
	}
	defer writer.Close()

	notify := buildNotifier(*alertCmd, *emailFrom, *emailTo, *emailRegn, *reportDir)
	detector := newSlopeDetector(*slopeWindow, *slopeThresholdMB, *cooldown)

	wl, err := startWorkload(ctx, workloadConfig{
		URI:            addr,
		SessionWorkers: *sessionWorkers,
		CRUDWorkers:    *crudWorkers,
		BulkWorkers:    *bulkWorkers,
		AggWorkers:     *aggWorkers,
		IndexedWorkers: *indexedWorkers,
		VCSWorkers:     *vcsWorkers,
		TxnWorkers:     *txnWorkers,
		SessionDelay:   *sessionDelay,
		OpsInterval:    *opsInterval,
		CollectionCap:  *collectionCap,
		TrimInterval:   *trimInterval,
	})
	if err != nil {
		log.Fatalf("starting workload: %v", err)
	}
	defer wl.stop()

	hostname, _ := os.Hostname()
	buildVer := buildVersion()
	log.Printf("soak started host=%s build=%s pid=%d addr=%s sample=%s window=%s threshold=%.2fMB/h cooldown=%s",
		hostname, buildVer, pid, addr, *sampleInterval, *slopeWindow, *slopeThresholdMB, *cooldown)

	ticker := time.NewTicker(*sampleInterval)
	defer ticker.Stop()

	startedAt := time.Now()
	var (
		firstSample, lastSample procSample
		sampleCount             int
		alertCount              int
		allSamples              []sample
		serverExit              string
		serverCrashTail         []string
	)

	defer func() {
		// Send a summary at the end of every run, including clean
		// no-alert runs. A silent cron is indistinguishable from a
		// broken cron; the summary email is the heartbeat. On a crash
		// the run exits immediately, so this same summary doubles as
		// the crash report and carries the server-log tail.
		text, htmlBody := summaryReport(hostname, buildVer, pid, addr, startedAt, time.Now(),
			sampleCount, alertCount, firstSample, lastSample, allSamples,
			wl.cycleCount(), wl.errCount(), serverExit, serverCrashTail)
		subj := fmt.Sprintf("dumbodb soak: %d alert(s)", alertCount)
		if alertCount == 0 {
			subj = "dumbodb soak: clean"
		}
		if serverExit != "" {
			subj = "dumbodb soak: SERVER CRASH"
		}
		log.Printf("SUMMARY: %s", strings.ReplaceAll(strings.TrimSpace(text), "\n", " | "))
		// Use a fresh context so summary delivery doesn't get cancelled
		// by the same signal that's tearing us down.
		sctx, scancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer scancel()
		notify(sctx, alertMessage{
			Subject:  subj,
			TextBody: text,
			HTMLBody: htmlBody,
			ChartPNG: pngChart(allSamples, 640, 240),
		})
	}()

	for {
		select {
		case <-ctx.Done():
			log.Printf("soak stopping: %v; cycles=%d errors=%d", ctx.Err(), wl.cycleCount(), wl.errCount())
			return
		case <-serverDied:
			if !srv.crashed() {
				// A stop we initiated; ctx.Done handles the exit.
				continue
			}
			serverExit = srv.exitStatus()
			uptime := roundDuration(time.Since(startedAt))
			log.Printf("SERVER CRASH after %s: %s; cycles=%d errors=%d",
				uptime, serverExit, wl.cycleCount(), wl.errCount())
			serverCrashTail = srv.logTail(200)
			log.Printf("--- last %d line(s) of server log %s ---", len(serverCrashTail), srv.logPath)
			for _, line := range serverCrashTail {
				log.Printf("  | %s", line)
			}
			log.Printf("--- end server log ---")
			// Return so the deferred summary fires now; it carries the
			// crash detail, so no separate alert email is needed.
			return
		case t := <-ticker.C:
			s, err := sampler.sample()
			if err != nil {
				log.Printf("sample error: %v", err)
				continue
			}
			writer.Write(t, s, wl.cycleCount(), wl.errCount())
			log.Printf("sample rss=%d kB anon=%d kB vm=%d kB threads=%d cycles=%d errors=%d",
				s.RssKB, s.RssAnonKB, s.VmSizeKB, s.Threads, wl.cycleCount(), wl.errCount())
			if sampleCount == 0 {
				firstSample = s
			}
			lastSample = s
			sampleCount++
			allSamples = append(allSamples, sample{t: t, rssKB: s.RssKB})

			alert, ok := detector.observe(t, s.RssKB)
			if !ok {
				continue
			}
			alertCount++
			recent := detector.recent()
			text, htmlBody := alertReport(hostname, buildVer, pid, addr, alert, recent, wl.cycleCount(), wl.errCount())
			log.Printf("ALERT: %s", strings.ReplaceAll(strings.TrimSpace(text), "\n", " | "))
			notify(ctx, alertMessage{
				Subject:  *emailSubj,
				TextBody: text,
				HTMLBody: htmlBody,
				ChartPNG: pngChart(recent, 640, 200),
			})
		}
	}
}

// summaryBody renders the end-of-run heartbeat email.
func summaryBody(host string, pid int, addr string, startedAt, endedAt time.Time, samples, alerts int, first, last procSample, recent []sample, cycles, errs int64) string {
	var b strings.Builder
	fmt.Fprintf(&b, "dumbodb soak summary on %s\n", host)
	fmt.Fprintf(&b, "  pid:     %d (%s)\n", pid, addr)
	fmt.Fprintf(&b, "  runtime: %s (started %s, ended %s)\n",
		roundDuration(endedAt.Sub(startedAt)),
		startedAt.UTC().Format(time.RFC3339), endedAt.UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "  samples: %d collected\n", samples)
	fmt.Fprintf(&b, "  alerts:  %d emitted during this run\n", alerts)
	fmt.Fprintf(&b, "  cycles:  %d workload cycles, %d errors\n", cycles, errs)
	if samples > 0 {
		deltaKB := last.RssKB - first.RssKB
		dur := endedAt.Sub(startedAt).Seconds()
		var ratePerHour float64
		if dur > 0 {
			ratePerHour = float64(deltaKB) / 1024.0 * 3600.0 / dur
		}
		fmt.Fprintf(&b, "  rss:     first=%d kB, last=%d kB, delta=%+d kB (%+.2f MB/hour)\n",
			first.RssKB, last.RssKB, deltaKB, ratePerHour)
	}
	if len(recent) > 0 {
		fmt.Fprintln(&b, "  recent samples:")
		for _, s := range recent {
			fmt.Fprintf(&b, "    %s  rss=%6d kB\n", s.t.UTC().Format(time.RFC3339), s.rssKB)
		}
	}
	return b.String()
}

// resolveServer either starts a managed dumbodb (default) or returns
// caller-supplied attach coordinates. The returned stop func is safe
// to call even in attach mode (it's a no-op there).
func resolveServer(ctx context.Context, attachAddr string, attachPid int, binary, extraArgs, dataDir string, port int, keepData bool, readyTimeout, stopGrace time.Duration) (string, int, *server, func(), error) {
	if attachAddr != "" {
		return attachAddr, attachPid, nil, func() {}, nil
	}
	s, err := startServer(binary, extraArgs, dataDir, port, keepData, readyTimeout)
	if err != nil {
		return "", 0, nil, nil, err
	}
	log.Printf("managed dumbodb started pid=%d addr=%s data=%s log=%s", s.pid, s.addr, s.dataDir, s.logPath)
	stop := s.waitOnContext(ctx, stopGrace)
	return s.addr, s.pid, s, stop, nil
}

// alertReport renders the in-flight alert email as both plain text
// and HTML, sharing the same stat lines so the two views agree.
func alertReport(host, buildVer string, pid int, addr string, a alert, recent []sample, cycles, errs int64) (text, htmlBody string) {
	title := fmt.Sprintf("dumbodb soak alert on %s", host)
	stats := []string{
		fmt.Sprintf("build: %s", buildVer),
		fmt.Sprintf("pid: %d (%s)", pid, addr),
		fmt.Sprintf("slope: %+.2f MB/hour over the last %s (threshold %+.2f MB/hour)",
			a.slopeMBPerHour, roundDuration(a.window), a.thresholdMBPerHour),
		fmt.Sprintf("cycles: %d (errors %d) since startup", cycles, errs),
	}
	return textBlock(title, stats, recent), htmlReport(title, stats, len(recent) >= 2)
}

// summaryReport renders the end-of-run summary email as both plain
// text and HTML.
func summaryReport(host, buildVer string, pid int, addr string, startedAt, endedAt time.Time, samples, alerts int, first, last procSample, allSamples []sample, cycles, errs int64, serverExit string, serverCrashTail []string) (text, htmlBody string) {
	title := fmt.Sprintf("dumbodb soak summary on %s", host)
	stats := []string{
		fmt.Sprintf("build: %s", buildVer),
		fmt.Sprintf("pid: %d (%s)", pid, addr),
		fmt.Sprintf("runtime: %s (started %s, ended %s)",
			roundDuration(endedAt.Sub(startedAt)),
			startedAt.UTC().Format(time.RFC3339), endedAt.UTC().Format(time.RFC3339)),
		fmt.Sprintf("samples: %d collected", samples),
		fmt.Sprintf("alerts: %d emitted during this run", alerts),
		fmt.Sprintf("cycles: %d workload cycles, %d errors", cycles, errs),
	}
	if serverExit != "" {
		stats = append(stats, fmt.Sprintf("server: CRASHED - %s", serverExit))
	}
	if samples > 0 {
		deltaKB := last.RssKB - first.RssKB
		dur := endedAt.Sub(startedAt).Seconds()
		var ratePerHour float64
		if dur > 0 {
			ratePerHour = float64(deltaKB) / 1024.0 * 3600.0 / dur
		}
		stats = append(stats, fmt.Sprintf("rss: first=%d kB, last=%d kB, delta=%+d kB (%+.2f MB/hour)",
			first.RssKB, last.RssKB, deltaKB, ratePerHour))
	}
	// On a crash, fold in the tail of the server log so the cause
	// travels in the same email that reports the crash.
	if len(serverCrashTail) > 0 {
		stats = append(stats, "server log tail:")
		for _, line := range lastN(serverCrashTail, 40) {
			stats = append(stats, "  "+line)
		}
	}
	return textBlock(title, stats, allSamples), htmlReport(title, stats, len(allSamples) >= 2)
}

func lastN(lines []string, n int) []string {
	if len(lines) <= n {
		return lines
	}
	return lines[len(lines)-n:]
}

// textBlock renders the plain-text view consumed by -alert-cmd and
// any future plain-text fallback. Replaces the per-sample dump with
// a single Unicode-block sparkline so the body stays bounded even on
// long runs.
func textBlock(title string, stats []string, samples []sample) string {
	var b strings.Builder
	fmt.Fprintln(&b, title)
	for _, s := range stats {
		fmt.Fprintf(&b, "  %s\n", s)
	}
	if line := sparkline(samples, 60); line != "" {
		fmt.Fprintf(&b, "  trend:  %s\n", line)
	}
	return b.String()
}

func roundDuration(d time.Duration) time.Duration {
	if d >= time.Hour {
		return d.Round(time.Minute)
	}
	return d.Round(time.Second)
}
