package app

import (
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestMetricsExposeRequiredOperationalSignals(t *testing.T) {
	metrics := NewMetrics(nil)
	metrics.RecordTransaction("PROCESSED", false, 25*time.Millisecond)
	metrics.RecordTransaction("REJECTED", true, 10*time.Millisecond)
	metrics.RecordRetry("sqs")
	metrics.RecordRetry("outbox")
	metrics.RecordDLQMessage()
	metrics.RecordConcurrencyConflict()
	metrics.RecordReconciliationDivergence()
	metrics.RecordOutboxPublication(50 * time.Millisecond)

	recorder := httptest.NewRecorder()
	metrics.Handler(recorder, httptest.NewRequest("GET", "/metrics", nil))
	body := recorder.Body.String()
	for _, signal := range []string{
		`wager_transactions_total{status="processed"} 1`,
		`wager_transactions_total{status="rejected"} 1`,
		"wager_idempotency_duplicates_total 1",
		`wager_retries_total{component="sqs"} 1`,
		`wager_retries_total{component="outbox"} 1`,
		"wager_dlq_messages_total 1",
		"wager_concurrency_conflicts_total 1",
		"wager_processing_duration_seconds_count 2",
		"wager_outbox_oldest_pending_seconds",
		"wager_outbox_publication_delay_seconds_count 1",
		"wallet_reconciliation_divergences_total 1",
	} {
		if !strings.Contains(body, signal) {
			t.Fatalf("metrics output does not contain %q:\n%s", signal, body)
		}
	}
}
