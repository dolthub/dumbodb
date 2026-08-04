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
	"context"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/sesv2"
	"github.com/aws/aws-sdk-go-v2/service/sesv2/types"
)

// alertMessage is what notifiers consume. The text body is for
// stdout / -alert-cmd; SES uses the HTML body and the PNG attachment.
type alertMessage struct {
	Subject  string
	TextBody string
	HTMLBody string
	ChartPNG []byte
	DiskPNG  []byte
}

// inlineCharts lists the PNGs to embed, each paired with the
// Content-ID the HTML body references it by. Empty entries are
// dropped so buildRawEmail attaches only what exists.
func (m alertMessage) inlineCharts() []inlineChart {
	var out []inlineChart
	if len(m.ChartPNG) > 0 {
		out = append(out, inlineChart{cid: chartCID, data: m.ChartPNG})
	}
	if len(m.DiskPNG) > 0 {
		out = append(out, inlineChart{cid: diskChartCID, data: m.DiskPNG})
	}
	return out
}

type notifier func(ctx context.Context, msg alertMessage)

func buildNotifier(alertCmd, emailFrom, emailTo, emailRegion, reportDir string) notifier {
	backends := []notifier{}
	if alertCmd != "" {
		backends = append(backends, cmdNotifier(alertCmd))
	}
	if reportDir != "" {
		backends = append(backends, fileNotifier(reportDir))
	}
	if emailFrom != "" && emailTo != "" {
		backends = append(backends, sesNotifier(emailFrom, emailTo, emailRegion))
	}
	return func(ctx context.Context, msg alertMessage) {
		for _, b := range backends {
			b(ctx, msg)
		}
	}
}

// fileNotifier writes each report locally instead of mailing it,
// for unattended runs that lack SES credentials. Each invocation
// produces report-NNN.{txt,html,png} under dir; report-latest.*
// always points at the most recent so a tail-friendly path exists.
func fileNotifier(dir string) notifier {
	var seq atomic.Int64
	return func(ctx context.Context, msg alertMessage) {
		if err := os.MkdirAll(dir, 0755); err != nil {
			log.Printf("report-dir: %v", err)
			return
		}
		n := seq.Add(1)
		writeFile := func(name string, data []byte) {
			if len(data) == 0 {
				return
			}
			if err := os.WriteFile(filepath.Join(dir, name), data, 0644); err != nil {
				log.Printf("report-dir write %s: %v", name, err)
			}
		}
		seqName := fmt.Sprintf("report-%03d", n)
		writeFile(seqName+".txt", []byte(msg.TextBody))
		writeFile(seqName+".html", []byte(msg.HTMLBody))
		writeFile(seqName+".png", msg.ChartPNG)
		writeFile(seqName+"-disk.png", msg.DiskPNG)
		writeFile("report-latest.txt", []byte(msg.TextBody))
		writeFile("report-latest.html", []byte(msg.HTMLBody))
		writeFile("report-latest.png", msg.ChartPNG)
		writeFile("report-latest-disk.png", msg.DiskPNG)
		log.Printf("report written: %s/%s.{txt,html,png}", dir, seqName)
	}
}

// cmdNotifier passes the text body on stdin and the subject in
// ALERT_SUBJECT. HTML and PNG are ignored.
func cmdNotifier(cmdLine string) notifier {
	return func(ctx context.Context, msg alertMessage) {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdLine)
		cmd.Env = append(cmd.Environ(), "ALERT_SUBJECT="+msg.Subject)
		cmd.Stdin = strings.NewReader(msg.TextBody)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("alert-cmd failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
}

// sesNotifier sends the HTML body via SES SendEmail Raw so a PNG
// chart can be embedded inline via cid:. When ChartPNG is nil the
// raw payload is a single-part text/html message.
func sesNotifier(from, toCSV, region string) notifier {
	to := splitAndTrim(toCSV)
	toHeader := strings.Join(to, ", ")
	return func(ctx context.Context, msg alertMessage) {
		opts := []func(*config.LoadOptions) error{}
		if region != "" {
			opts = append(opts, config.WithRegion(region))
		}
		cfg, err := config.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			log.Printf("ses config: %v", err)
			return
		}
		raw := buildRawEmail(from, toHeader, msg.Subject, msg.HTMLBody, msg.inlineCharts())
		client := sesv2.NewFromConfig(cfg)
		_, err = client.SendEmail(ctx, &sesv2.SendEmailInput{
			FromEmailAddress: aws.String(from),
			Destination:      &types.Destination{ToAddresses: to},
			Content: &types.EmailContent{
				Raw: &types.RawMessage{Data: raw},
			},
		})
		if err != nil {
			log.Printf("ses send: %v", err)
		}
	}
}

func splitAndTrim(csv string) []string {
	parts := strings.Split(csv, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		if v := strings.TrimSpace(p); v != "" {
			out = append(out, v)
		}
	}
	return out
}
