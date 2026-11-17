// Copyright 2021 FerretDB Inc.
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

package handler

import (
	"log/slog"
	"time"

	"github.com/dolthub/dolt/go/libraries/doltcore/sqle/dsess"

	"github.com/dolthub/dumbodb/internal/backends"
)

// defaultTransactionLifetimeLimitSeconds is mongod's default for
// transactionLifetimeLimitSeconds.
const defaultTransactionLifetimeLimitSeconds = 60

// transactionReaperPeriod is how often expired transactions are looked for.
const transactionReaperPeriod = time.Second

// runTransactionReaper aborts client transactions open longer than
// transactionLifetimeLimitSeconds, as mongod does, until stop closes. An
// expired transaction would otherwise hold its document locks, and everything
// waiting on them, for as long as its client likes.
func (h *Handler) runTransactionReaper(stop <-chan struct{}) {
	ticker := time.NewTicker(transactionReaperPeriod)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			h.abortExpiredTransactions(time.Now())
		case <-stop:
			return
		}
	}
}

// abortExpiredTransactions aborts every client transaction that started more
// than transactionLifetimeLimitSeconds before now. Each abort runs inside its
// session's command bracket, so it waits for a statement of the transaction
// that is running, and later statements are refused with NoSuchTransaction.
func (h *Handler) abortExpiredTransactions(now time.Time) {
	reg := h.SessionRegistry()
	sab, ok := h.b.(backends.SessionAwareBackend)
	if reg == nil || !ok {
		return
	}
	cutoff := now.Add(-h.transactionLifetimeLimit())
	for owner, shadow := range reg.TransactionsStartedBefore(cutoff) {
		err := shadow.Use(now, func(*dsess.DoltSession) error {
			if !shadow.TransactionStartedBefore(cutoff) {
				return nil
			}
			sab.OnTransactionAbort(owner)
			shadow.MarkTransactionAborted()
			h.L.Info("Aborted a transaction that exceeded transactionLifetimeLimitSeconds.", slog.String("lsid", owner))
			return nil
		})
		if err != nil {
			h.L.Debug("Could not abort an expired transaction; its session is gone.", slog.String("lsid", owner))
		}
	}
}

// transactionLifetimeLimit is the current transactionLifetimeLimitSeconds.
func (h *Handler) transactionLifetimeLimit() time.Duration {
	seconds := int32(defaultTransactionLifetimeLimitSeconds)
	if v, ok := h.paramStore.Get("transactionLifetimeLimitSeconds"); ok {
		if n, ok := v.(int32); ok {
			seconds = n
		}
	}
	return time.Duration(seconds) * time.Second
}
