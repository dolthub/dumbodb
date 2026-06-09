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

// notifier dispatches one alert to every backend the operator opted
// into. Text body is what -alert-cmd receives on stdin and what we
// log; SES sends the HTML body too when provided.
type notifier func(ctx context.Context, subject, textBody, htmlBody string)

func buildNotifier(alertCmd, emailFrom, emailTo, emailRegion, _ string) notifier {
	backends := []notifier{}
	if alertCmd != "" {
		backends = append(backends, cmdNotifier(alertCmd))
	}
	if emailFrom != "" && emailTo != "" {
		backends = append(backends, sesNotifier(emailFrom, emailTo, emailRegion))
	}
	return func(ctx context.Context, subject, textBody, htmlBody string) {
		for _, b := range backends {
			b(ctx, subject, textBody, htmlBody)
		}
	}
}

// cmdNotifier runs `sh -c <cmd>` with the text body on stdin and the
// subject in ALERT_SUBJECT. HTML body is ignored; shell consumers
// don't render markup.
func cmdNotifier(cmdLine string) notifier {
	return func(ctx context.Context, subject, textBody, _ string) {
		cmd := exec.CommandContext(ctx, "sh", "-c", cmdLine)
		cmd.Env = append(cmd.Environ(), "ALERT_SUBJECT="+subject)
		cmd.Stdin = strings.NewReader(textBody)
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("alert-cmd failed: %v: %s", err, strings.TrimSpace(string(out)))
		}
	}
}

// sesNotifier sends a multipart/alternative email through the default
// AWS credential chain. Receiving clients pick the HTML body when
// they support it and the text body when they don't.
func sesNotifier(from, toCSV, region string) notifier {
	to := splitAndTrim(toCSV)
	return func(ctx context.Context, subject, textBody, htmlBody string) {
		opts := []func(*config.LoadOptions) error{}
		if region != "" {
			opts = append(opts, config.WithRegion(region))
		}
		cfg, err := config.LoadDefaultConfig(ctx, opts...)
		if err != nil {
			log.Printf("ses config: %v", err)
			return
		}
		body := &types.Body{
			Text: &types.Content{Data: aws.String(textBody)},
		}
		if htmlBody != "" {
			body.Html = &types.Content{Data: aws.String(htmlBody)}
		}
		client := sesv2.NewFromConfig(cfg)
		_, err = client.SendEmail(ctx, &sesv2.SendEmailInput{
			FromEmailAddress: aws.String(from),
			Destination:      &types.Destination{ToAddresses: to},
			Content: &types.EmailContent{
				Simple: &types.Message{
					Subject: &types.Content{Data: aws.String(subject)},
					Body:    body,
				},
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
