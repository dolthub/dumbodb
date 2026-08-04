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
	"bytes"
	"encoding/base64"
	"fmt"
	"html"
	"image"
	"image/color"
	"image/png"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"
)

// chartCID and diskChartCID are the Content-IDs under which the two
// PNG attachments are referenced from the HTML body.
const (
	chartCID     = "soak-chart@dumbodb"
	diskChartCID = "soak-disk-chart@dumbodb"
)

// chartRef names an inline image the HTML body should embed. The
// bytes are attached separately in the multipart/related MIME wrapper
// under the matching Content-ID.
type chartRef struct {
	cid string
	alt string
}

// inlineChart pairs a Content-ID with the PNG bytes attached under it.
type inlineChart struct {
	cid  string
	data []byte
}

// htmlReport builds the HTML body: stats table plus one <img cid:>
// tag per chart. The caller attaches the PNG bytes separately in a
// multipart/related MIME wrapper.
func htmlReport(title string, stats []string, charts []chartRef) string {
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
	for _, c := range charts {
		fmt.Fprintf(&b, `<img src="cid:%s" alt="%s" style="display:block;max-width:100%%;border:1px solid #ddd;margin-top:8px"/>`,
			c.cid, html.EscapeString(c.alt))
	}
	b.WriteString(`</body></html>`)
	return b.String()
}

// pngChart renders an RSS-over-time line chart to PNG.
func pngChart(samples []sample, width, height int) []byte {
	return pngSeriesChart(samples, func(s sample) int64 { return s.rssKB },
		color.NRGBA{0x00, 0x66, 0xcc, 0xff}, width, height)
}

// pngDiskChart renders a data-directory-size-over-time line chart to
// PNG.
func pngDiskChart(samples []sample, width, height int) []byte {
	return pngSeriesChart(samples, func(s sample) int64 { return s.diskKB },
		color.NRGBA{0x33, 0x99, 0x33, 0xff}, width, height)
}

// pngSeriesChart renders one kB-valued series over time as a line
// chart to PNG. Y-axis labels are in MB. Returns nil for too-few
// samples. Stdlib + basicfont, no external deps.
func pngSeriesChart(samples []sample, valueOf func(sample) int64, lineColor color.NRGBA, width, height int) []byte {
	if len(samples) < 2 {
		return nil
	}
	t0 := samples[0].t
	tEnd := samples[len(samples)-1].t
	durSec := tEnd.Sub(t0).Seconds()
	if durSec <= 0 {
		return nil
	}

	minY, maxY := valueOf(samples[0]), valueOf(samples[0])
	for _, s := range samples {
		v := valueOf(s)
		if v < minY {
			minY = v
		}
		if v > maxY {
			maxY = v
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
		bottomMargin = 28
	)
	plotW := width - leftMargin - rightMargin
	plotH := height - topMargin - bottomMargin

	img := image.NewRGBA(image.Rect(0, 0, width, height))
	fill(img, image.Rect(0, 0, width, height), color.NRGBA{0xfa, 0xfa, 0xfa, 0xff})

	gridColor := color.NRGBA{0xdd, 0xdd, 0xdd, 0xff}
	labelColor := color.NRGBA{0x66, 0x66, 0x66, 0xff}

	for _, v := range []int64{yMin, (yMin + yMax) / 2, yMax} {
		y := topMargin + plotH - int(float64(plotH)*float64(v-yMin)/float64(yMax-yMin))
		drawHLine(img, leftMargin, leftMargin+plotW, y, gridColor)
		label := fmt.Sprintf("%d MB", v/1024)
		drawString(img, leftMargin-6-7*len(label), y+5, label, labelColor)
	}
	drawString(img, leftMargin, topMargin+plotH+16, t0.UTC().Format("15:04:05"), labelColor)
	endLabel := tEnd.UTC().Format("15:04:05")
	drawString(img, leftMargin+plotW-7*len(endLabel), topMargin+plotH+16, endLabel, labelColor)

	var px, py int
	for i, s := range samples {
		x := leftMargin + int(float64(plotW)*s.t.Sub(t0).Seconds()/durSec)
		y := topMargin + plotH - int(float64(plotH)*float64(valueOf(s)-yMin)/float64(yMax-yMin))
		if i > 0 {
			drawLine(img, px, py, x, y, lineColor)
			drawLine(img, px, py+1, x, y+1, lineColor)
		}
		px, py = x, y
	}
	for _, i := range []int{0, len(samples) - 1} {
		x := leftMargin + int(float64(plotW)*samples[i].t.Sub(t0).Seconds()/durSec)
		y := topMargin + plotH - int(float64(plotH)*float64(valueOf(samples[i])-yMin)/float64(yMax-yMin))
		fillDot(img, x, y, 3, lineColor)
	}

	var buf bytes.Buffer
	if err := png.Encode(&buf, img); err != nil {
		return nil
	}
	return buf.Bytes()
}

func fill(img *image.RGBA, r image.Rectangle, c color.NRGBA) {
	for y := r.Min.Y; y < r.Max.Y; y++ {
		for x := r.Min.X; x < r.Max.X; x++ {
			img.Set(x, y, c)
		}
	}
}

func drawHLine(img *image.RGBA, x0, x1, y int, c color.NRGBA) {
	for x := x0; x <= x1; x++ {
		img.Set(x, y, c)
	}
}

// drawLine is Bresenham's; adequate for non-AA chart strokes.
func drawLine(img *image.RGBA, x0, y0, x1, y1 int, c color.NRGBA) {
	dx := abs(x1 - x0)
	dy := abs(y1 - y0)
	sx := 1
	if x0 > x1 {
		sx = -1
	}
	sy := 1
	if y0 > y1 {
		sy = -1
	}
	err := dx - dy
	for {
		img.Set(x0, y0, c)
		if x0 == x1 && y0 == y1 {
			return
		}
		e2 := 2 * err
		if e2 > -dy {
			err -= dy
			x0 += sx
		}
		if e2 < dx {
			err += dx
			y0 += sy
		}
	}
}

func fillDot(img *image.RGBA, cx, cy, r int, c color.NRGBA) {
	for dy := -r; dy <= r; dy++ {
		for dx := -r; dx <= r; dx++ {
			if dx*dx+dy*dy <= r*r {
				img.Set(cx+dx, cy+dy, c)
			}
		}
	}
}

func drawString(img *image.RGBA, x, y int, s string, c color.NRGBA) {
	d := &font.Drawer{
		Dst:  img,
		Src:  image.NewUniform(c),
		Face: basicfont.Face7x13,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(s)
}

func abs(x int) int {
	if x < 0 {
		return -x
	}
	return x
}

// anyDisk reports whether any sample carries a non-zero data-dir
// size, i.e. whether disk sampling was active (managed-server mode).
func anyDisk(samples []sample) bool {
	for _, s := range samples {
		if s.diskKB > 0 {
			return true
		}
	}
	return false
}

// sparkline renders an N-column Unicode-block trend chart of the RSS
// samples. Drops into the plain-text body so consumers of -alert-cmd
// still see the trend.
func sparkline(samples []sample, columns int) string {
	return sparklineOf(samples, columns, func(s sample) int64 { return s.rssKB })
}

// sparklineOf renders one series scaled to its own min/max as an
// N-column Unicode-block trend chart.
func sparklineOf(samples []sample, columns int, valueOf func(sample) int64) string {
	if len(samples) < 2 || columns < 2 {
		return ""
	}
	blocks := []rune{'▁', '▂', '▃', '▄', '▅', '▆', '▇', '█'}
	var minY, maxY int64 = valueOf(samples[0]), valueOf(samples[0])
	for _, s := range samples {
		v := valueOf(s)
		if v < minY {
			minY = v
		}
		if v > maxY {
			maxY = v
		}
	}
	if maxY == minY {
		return strings.Repeat(string(blocks[3]), columns)
	}
	t0 := samples[0].t
	dur := samples[len(samples)-1].t.Sub(t0)
	if dur <= 0 {
		return ""
	}
	sums := make([]float64, columns)
	counts := make([]int, columns)
	for _, s := range samples {
		off := s.t.Sub(t0)
		b := int(float64(columns) * float64(off) / float64(dur))
		if b >= columns {
			b = columns - 1
		}
		if b < 0 {
			b = 0
		}
		sums[b] += float64(valueOf(s))
		counts[b]++
	}
	var out strings.Builder
	var lastIdx int
	for i := 0; i < columns; i++ {
		var idx int
		if counts[i] == 0 {
			idx = lastIdx
		} else {
			avg := sums[i] / float64(counts[i])
			norm := (avg - float64(minY)) / float64(maxY-minY)
			idx = int(norm * 7.999)
			if idx < 0 {
				idx = 0
			}
			if idx > 7 {
				idx = 7
			}
			lastIdx = idx
		}
		out.WriteRune(blocks[idx])
	}
	return out.String()
}

// buildRawEmail composes the MIME message SES needs when we have one
// or more PNGs to embed. Structure: multipart/related wrapping a
// single text/html part plus one attachment per chart. With no
// charts this degrades to a single-part text/html message.
func buildRawEmail(from, to, subject, htmlBody string, charts []inlineChart) []byte {
	var b bytes.Buffer
	fmt.Fprintf(&b, "From: %s\r\n", from)
	fmt.Fprintf(&b, "To: %s\r\n", to)
	fmt.Fprintf(&b, "Subject: %s\r\n", subject)
	b.WriteString("MIME-Version: 1.0\r\n")

	if len(charts) == 0 {
		// Single-part text/html.
		b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
		b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
		b.WriteString(htmlBody)
		b.WriteString("\r\n")
		return b.Bytes()
	}

	const boundary = "rel-boundary-dumbodb-soak"
	fmt.Fprintf(&b, "Content-Type: multipart/related; boundary=\"%s\"\r\n\r\n", boundary)

	fmt.Fprintf(&b, "--%s\r\n", boundary)
	b.WriteString("Content-Type: text/html; charset=\"UTF-8\"\r\n")
	b.WriteString("Content-Transfer-Encoding: 8bit\r\n\r\n")
	b.WriteString(htmlBody)
	b.WriteString("\r\n")

	for _, c := range charts {
		fmt.Fprintf(&b, "--%s\r\n", boundary)
		b.WriteString("Content-Type: image/png\r\n")
		b.WriteString("Content-Transfer-Encoding: base64\r\n")
		fmt.Fprintf(&b, "Content-ID: <%s>\r\n", c.cid)
		fmt.Fprintf(&b, "Content-Disposition: inline; filename=\"%s.png\"\r\n\r\n", c.cid)
		writeBase64Wrapped(&b, c.data)
	}

	fmt.Fprintf(&b, "--%s--\r\n", boundary)
	return b.Bytes()
}

// writeBase64Wrapped emits base64 in 76-char lines, the MIME standard
// line length.
func writeBase64Wrapped(b *bytes.Buffer, data []byte) {
	encoded := base64.StdEncoding.EncodeToString(data)
	for i := 0; i < len(encoded); i += 76 {
		end := i + 76
		if end > len(encoded) {
			end = len(encoded)
		}
		b.WriteString(encoded[i:end])
		b.WriteString("\r\n")
	}
}
