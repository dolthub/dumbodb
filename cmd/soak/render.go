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
	"fmt"
	"html"
	"strings"
	"time"
)

// htmlReport builds a self-contained HTML email body: a styled stats
// header followed by an inline SVG line chart of RSS over time.
// Inline SVG is supported by Gmail web, Outlook 365, Apple Mail, and
// every modern client we care about; the text body remains the
// fallback for everything else.
func htmlReport(title string, stats []string, samples []sample) string {
	var b strings.Builder
	b.WriteString(`<!DOCTYPE html><html><body style="font-family:-apple-system,BlinkMacSystemFont,Segoe UI,Helvetica,Arial,sans-serif;font-size:13px;color:#333;margin:16px">`)
	fmt.Fprintf(&b, `<h2 style="margin:0 0 8px 0;font-size:15px">%s</h2>`, html.EscapeString(title))
	b.WriteString(`<table style="border-collapse:collapse;margin-bottom:12px">`)
	for _, s := range stats {
		k, v, _ := strings.Cut(s, ":")
		fmt.Fprintf(&b,
			`<tr><td style="padding:2px 12px 2px 0;color:#666;white-space:nowrap">%s</td><td style="padding:2px 0;font-family:ui-monospace,SFMono-Regular,Menlo,monospace">%s</td></tr>`,
			html.EscapeString(strings.TrimSpace(k)), html.EscapeString(strings.TrimSpace(v)))
	}
	b.WriteString(`</table>`)
	if chart := svgChart(samples, 640, 240); chart != "" {
		b.WriteString(chart)
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

// svgChart renders an RSS-over-time line chart as an inline SVG.
// Returns empty string for too-few samples to plot. Y is RSS in MB; X
// is wall-clock from the first to last sample.
func svgChart(samples []sample, width, height int) string {
	if len(samples) < 2 {
		return ""
	}
	t0 := samples[0].t
	tEnd := samples[len(samples)-1].t
	durSec := tEnd.Sub(t0).Seconds()
	if durSec <= 0 {
		return ""
	}

	minY, maxY := samples[0].rssKB, samples[0].rssKB
	for _, s := range samples {
		if s.rssKB < minY {
			minY = s.rssKB
		}
		if s.rssKB > maxY {
			maxY = s.rssKB
		}
	}
	yRange := maxY - minY
	if yRange == 0 {
		yRange = 1
	}
	yMin := minY - yRange/20
	yMax := maxY + yRange/20
	if yMin < 0 {
		yMin = 0
	}

	const (
		leftMargin   = 70
		rightMargin  = 20
		topMargin    = 16
		bottomMargin = 32
	)
	plotW := width - leftMargin - rightMargin
	plotH := height - topMargin - bottomMargin

	var pts strings.Builder
	for i, s := range samples {
		x := leftMargin + int(float64(plotW)*s.t.Sub(t0).Seconds()/durSec)
		y := topMargin + plotH - int(float64(plotH)*float64(s.rssKB-yMin)/float64(yMax-yMin))
		if i > 0 {
			pts.WriteByte(' ')
		}
		fmt.Fprintf(&pts, "%d,%d", x, y)
	}

	yLabel := func(kb int64) string { return fmt.Sprintf("%d MB", kb/1024) }
	xLabel := func(t time.Time) string { return t.UTC().Format("15:04:05Z") }
	mid := (yMin + yMax) / 2

	var s strings.Builder
	fmt.Fprintf(&s, `<svg xmlns="http://www.w3.org/2000/svg" width="%d" height="%d" viewBox="0 0 %d %d" style="border:1px solid #ddd;background:#fafafa">`, width, height, width, height)
	// Y-axis gridlines + labels at min, mid, max.
	for _, p := range []struct {
		v   int64
		lbl string
	}{
		{yMax, yLabel(yMax)},
		{mid, yLabel(mid)},
		{yMin, yLabel(yMin)},
	} {
		y := topMargin + plotH - int(float64(plotH)*float64(p.v-yMin)/float64(yMax-yMin))
		fmt.Fprintf(&s, `<line x1="%d" y1="%d" x2="%d" y2="%d" stroke="#e0e0e0"/>`,
			leftMargin, y, leftMargin+plotW, y)
		fmt.Fprintf(&s, `<text x="%d" y="%d" font-size="11" font-family="monospace" fill="#666" text-anchor="end">%s</text>`,
			leftMargin-6, y+4, p.lbl)
	}
	// X axis labels at endpoints.
	fmt.Fprintf(&s, `<text x="%d" y="%d" font-size="11" font-family="monospace" fill="#666">%s</text>`,
		leftMargin, height-bottomMargin/2+4, xLabel(t0))
	fmt.Fprintf(&s, `<text x="%d" y="%d" font-size="11" font-family="monospace" fill="#666" text-anchor="end">%s</text>`,
		leftMargin+plotW, height-bottomMargin/2+4, xLabel(tEnd))
	// The polyline itself.
	fmt.Fprintf(&s, `<polyline points="%s" stroke="#0066cc" fill="none" stroke-width="2"/>`, pts.String())
	// Endpoint dots so a single-segment chart is visible.
	for i := range samples {
		if i != 0 && i != len(samples)-1 {
			continue
		}
		x := leftMargin + int(float64(plotW)*samples[i].t.Sub(t0).Seconds()/durSec)
		y := topMargin + plotH - int(float64(plotH)*float64(samples[i].rssKB-yMin)/float64(yMax-yMin))
		fmt.Fprintf(&s, `<circle cx="%d" cy="%d" r="3" fill="#0066cc"/>`, x, y)
	}
	s.WriteString(`</svg>`)
	return s.String()
}
