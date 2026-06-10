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
	"os/signal"
	"strings"
	"syscall"
	"time"
)

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

		// Detection.
		slopeWindow      = flag.Duration("slope-window", 6*time.Hour, "rolling window over which the slope is computed")
		slopeThresholdMB = flag.Float64("slope-threshold-mb-per-hour", 1.0, "MB/hour of sustained growth that triggers an alert")
		cooldown         = flag.Duration("cooldown", 24*time.Hour, "minimum delay between alerts")

		// Email alerting (required: the soak has no value running unattended without it).
		emailFrom = flag.String("email-from", "", "SES verified From address (required)")
		emailTo   = flag.String("email-to", "", "alert recipient(s), comma-separated (required)")
		emailRegn = flag.String("email-region", "", "AWS region for SES; default chain region used when blank")
		emailSubj = flag.String("email-subject", "dumbodb soak alert", "Subject line for SES alerts")

		// Optional secondary alert channel (e.g. local mailx, ntfy, curl-to-webhook).
		alertCmd = flag.String("alert-cmd", "", "shell command to run on each alert in addition to email; body on stdin")
	)
	flag.Parse()

	if *emailFrom == "" || *emailTo == "" {
		log.Fatal("-email-from and -email-to are required")
	}
	if (*attachAddr == "") != (*attachPid == 0) {
		log.Fatal("-attach-addr and -attach-pid must be set together (or not at all)")
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()
	if *runtime > 0 {
		ctx, _ = context.WithTimeout(ctx, *runtime)
	}

	addr, pid, stopServer, err := resolveServer(ctx, *attachAddr, *attachPid, *dumbodbBinary, *dumbodbArgs, *dataDir, *port, *keepData, *readyTimeout, *stopGrace)
	if err != nil {
		log.Fatalf("dumbodb: %v", err)
	}
	defer stopServer()

	sampler := newSampler(pid)
	if _, err := sampler.sample(); err != nil {
		log.Fatalf("initial sample for pid %d: %v", pid, err)
	}

	writer, err := newCSVWriter(*csvPath)
	if err != nil {
		log.Fatalf("csv writer: %v", err)
	}
	defer writer.Close()

	notify := buildNotifier(*alertCmd, *emailFrom, *emailTo, *emailRegn)
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
	})
	if err != nil {
		log.Fatalf("starting workload: %v", err)
	}
	defer wl.stop()

	hostname, _ := os.Hostname()
	log.Printf("soak started host=%s pid=%d addr=%s sample=%s window=%s threshold=%.2fMB/h cooldown=%s",
		hostname, pid, addr, *sampleInterval, *slopeWindow, *slopeThresholdMB, *cooldown)

	ticker := time.NewTicker(*sampleInterval)
	defer ticker.Stop()

	startedAt := time.Now()
	var (
		firstSample, lastSample procSample
		sampleCount             int
		alertCount              int
		allSamples              []sample
	)

	defer func() {
		// Send a summary at the end of every run, including clean
		// no-alert runs. A silent cron is indistinguishable from a
		// broken cron; the summary email is the heartbeat.
		text, htmlBody := summaryReport(hostname, pid, addr, startedAt, time.Now(),
			sampleCount, alertCount, firstSample, lastSample, allSamples,
			wl.cycleCount(), wl.errCount())
		subj := fmt.Sprintf("dumbodb soak: %d alert(s)", alertCount)
		if alertCount == 0 {
			subj = "dumbodb soak: clean"
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
		case t := <-ticker.C:
			s, err := sampler.sample()
			if err != nil {
				log.Printf("sample error: %v", err)
				continue
			}
			writer.Write(t, s, wl.cycleCount(), wl.errCount())
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
			text, htmlBody := alertReport(hostname, pid, addr, alert, recent, wl.cycleCount(), wl.errCount())
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
func resolveServer(ctx context.Context, attachAddr string, attachPid int, binary, extraArgs, dataDir string, port int, keepData bool, readyTimeout, stopGrace time.Duration) (string, int, func(), error) {
	if attachAddr != "" {
		return attachAddr, attachPid, func() {}, nil
	}
	s, err := startServer(binary, extraArgs, dataDir, port, keepData, readyTimeout)
	if err != nil {
		return "", 0, nil, err
	}
	log.Printf("managed dumbodb started pid=%d addr=%s data=%s log=%s", s.pid, s.addr, s.dataDir, s.logPath)
	stop := s.waitOnContext(ctx, stopGrace)
	return s.addr, s.pid, stop, nil
}

// alertReport renders the in-flight alert email as both plain text
// and HTML, sharing the same stat lines so the two views agree.
func alertReport(host string, pid int, addr string, a alert, recent []sample, cycles, errs int64) (text, htmlBody string) {
	title := fmt.Sprintf("dumbodb soak alert on %s", host)
	stats := []string{
		fmt.Sprintf("pid: %d (%s)", pid, addr),
		fmt.Sprintf("slope: %+.2f MB/hour over the last %s (threshold %+.2f MB/hour)",
			a.slopeMBPerHour, roundDuration(a.window), a.thresholdMBPerHour),
		fmt.Sprintf("cycles: %d (errors %d) since startup", cycles, errs),
	}
	return textBlock(title, stats, recent), htmlReport(title, stats, len(recent) >= 2)
}

// summaryReport renders the end-of-run summary email as both plain
// text and HTML.
func summaryReport(host string, pid int, addr string, startedAt, endedAt time.Time, samples, alerts int, first, last procSample, allSamples []sample, cycles, errs int64) (text, htmlBody string) {
	title := fmt.Sprintf("dumbodb soak summary on %s", host)
	stats := []string{
		fmt.Sprintf("pid: %d (%s)", pid, addr),
		fmt.Sprintf("runtime: %s (started %s, ended %s)",
			roundDuration(endedAt.Sub(startedAt)),
			startedAt.UTC().Format(time.RFC3339), endedAt.UTC().Format(time.RFC3339)),
		fmt.Sprintf("samples: %d collected", samples),
		fmt.Sprintf("alerts: %d emitted during this run", alerts),
		fmt.Sprintf("cycles: %d workload cycles, %d errors", cycles, errs),
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
	return textBlock(title, stats, allSamples), htmlReport(title, stats, len(allSamples) >= 2)
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
