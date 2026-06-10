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
	"log"
	"os/exec"
	"strings"

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
}

type notifier func(ctx context.Context, msg alertMessage)

func buildNotifier(alertCmd, emailFrom, emailTo, emailRegion string) notifier {
	backends := []notifier{}
	if alertCmd != "" {
		backends = append(backends, cmdNotifier(alertCmd))
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
		raw := buildRawEmail(from, toHeader, msg.Subject, msg.HTMLBody, msg.ChartPNG)
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
