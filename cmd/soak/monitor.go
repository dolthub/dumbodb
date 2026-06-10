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

package main

import (
	"bufio"
	"fmt"
	"math"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"
)

type procSample struct {
	RssKB     int64
	VmSizeKB  int64
	RssAnonKB int64
	Threads   int64
}

type sampler struct {
	pid  int
	path string
}

func newSampler(pid int) *sampler {
	return &sampler{pid: pid, path: fmt.Sprintf("/proc/%d/status", pid)}
}

// sample reads /proc/<pid>/status. Returns an error if the file is
// gone (e.g. the target process exited).
func (s *sampler) sample() (procSample, error) {
	f, err := os.Open(s.path)
	if err != nil {
		return procSample{}, err
	}
	defer f.Close()
	var out procSample
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		line := scanner.Text()
		key, val, ok := splitProcLine(line)
		if !ok {
			continue
		}
		switch key {
		case "VmRSS":
			out.RssKB = parseKB(val)
		case "VmSize":
			out.VmSizeKB = parseKB(val)
		case "RssAnon":
			out.RssAnonKB = parseKB(val)
		case "Threads":
			n, _ := strconv.ParseInt(strings.TrimSpace(val), 10, 64)
			out.Threads = n
		}
	}
	return out, scanner.Err()
}

func splitProcLine(line string) (key, val string, ok bool) {
	i := strings.IndexByte(line, ':')
	if i < 0 {
		return "", "", false
	}
	return line[:i], strings.TrimSpace(line[i+1:]), true
}

// parseKB parses values like "12345 kB" into an int64 of kB.
func parseKB(s string) int64 {
	fields := strings.Fields(s)
	if len(fields) == 0 {
		return 0
	}
	n, _ := strconv.ParseInt(fields[0], 10, 64)
	return n
}

// csvWriter appends one row per memory sample.
type csvWriter struct {
	mu sync.Mutex
	f  *os.File
}

func newCSVWriter(path string) (*csvWriter, error) {
	if path == "" {
		return &csvWriter{}, nil
	}
	newFile := false
	if _, err := os.Stat(path); os.IsNotExist(err) {
		newFile = true
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0644)
	if err != nil {
		return nil, err
	}
	w := &csvWriter{f: f}
	if newFile {
		fmt.Fprintln(f, "timestamp,rss_kb,vmsize_kb,rss_anon_kb,threads,cycles,errors")
	}
	return w, nil
}

func (w *csvWriter) Write(t time.Time, s procSample, cycles, errs int64) {
	if w.f == nil {
		return
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	fmt.Fprintf(w.f, "%d,%d,%d,%d,%d,%d,%d\n",
		t.Unix(), s.RssKB, s.VmSizeKB, s.RssAnonKB, s.Threads, cycles, errs)
}

func (w *csvWriter) Close() error {
	if w.f == nil {
		return nil
	}
	return w.f.Close()
}

// sample is the in-memory record used by the slope detector. Keep
// numeric type narrow (kB) to keep the rolling window cheap.
type sample struct {
	t     time.Time
	rssKB int64
}

// slopeDetector keeps a rolling window of samples and, at every
// observation, fits a linear regression to compute the kB/second
// slope. When the slope exceeds the configured threshold and no
// recent alert is within the cooldown, observe returns a populated
// alert.
type slopeDetector struct {
	window             time.Duration
	thresholdKBPerSec  float64
	cooldown           time.Duration
	samples            []sample
	lastAlert          time.Time
}

type alert struct {
	at                 time.Time
	slopeMBPerHour     float64
	thresholdMBPerHour float64
	window             time.Duration
}

func newSlopeDetector(window time.Duration, thresholdMBPerHour float64, cooldown time.Duration) *slopeDetector {
	// kB/sec is the natural unit for the regression because that's the
	// arithmetic mix of kB samples and second timestamps. Convert
	// MB/hour to kB/sec for the comparison: 1 MB/h == 1024 kB / 3600 s.
	return &slopeDetector{
		window:            window,
		thresholdKBPerSec: thresholdMBPerHour * 1024.0 / 3600.0,
		cooldown:          cooldown,
	}
}

func (d *slopeDetector) observe(t time.Time, rssKB int64) (alert, bool) {
	d.samples = append(d.samples, sample{t: t, rssKB: rssKB})
	cutoff := t.Add(-d.window)
	keep := d.samples[:0]
	for _, s := range d.samples {
		if !s.t.Before(cutoff) {
			keep = append(keep, s)
		}
	}
	d.samples = keep

	if len(d.samples) < 3 {
		return alert{}, false
	}
	if t.Sub(d.lastAlert) < d.cooldown {
		return alert{}, false
	}
	if d.samples[len(d.samples)-1].t.Sub(d.samples[0].t) < d.window/2 {
		// Don't fire until the rolling window is at least half full;
		// otherwise startup transients dominate the slope.
		return alert{}, false
	}

	slope := linRegSlope(d.samples)
	if slope < d.thresholdKBPerSec {
		return alert{}, false
	}
	d.lastAlert = t
	return alert{
		at:                 t,
		slopeMBPerHour:     slope * 3600.0 / 1024.0,
		thresholdMBPerHour: d.thresholdKBPerSec * 3600.0 / 1024.0,
		window:             d.samples[len(d.samples)-1].t.Sub(d.samples[0].t),
	}, true
}

// recent returns the trailing samples of the current window for
// inclusion in alert messages. Caps at 10 entries so the body stays
// short under long windows.
func (d *slopeDetector) recent() []sample {
	const maxSamples = 10
	if len(d.samples) <= maxSamples {
		out := make([]sample, len(d.samples))
		copy(out, d.samples)
		return out
	}
	out := make([]sample, maxSamples)
	copy(out, d.samples[len(d.samples)-maxSamples:])
	return out
}

// linRegSlope returns the kB-per-second linear-regression slope of
// rssKB against time. Standard ordinary-least-squares.
func linRegSlope(samples []sample) float64 {
	if len(samples) < 2 {
		return 0
	}
	t0 := samples[0].t
	var sumX, sumY, sumXY, sumX2 float64
	n := float64(len(samples))
	for _, s := range samples {
		x := s.t.Sub(t0).Seconds()
		y := float64(s.rssKB)
		sumX += x
		sumY += y
		sumXY += x * y
		sumX2 += x * x
	}
	den := n*sumX2 - sumX*sumX
	if math.Abs(den) < 1e-9 {
		return 0
	}
	return (n*sumXY - sumX*sumY) / den
}
