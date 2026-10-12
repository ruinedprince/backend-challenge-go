package app

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// Metrics exposes the operational signals required to run the service. The
// Prometheus text format keeps collection independent from a vendor or SDK.
type Metrics struct {
	pool *pgxpool.Pool

	mu                            sync.Mutex
	transactionsByStatus          map[string]uint64
	retriesByComponent            map[string]uint64
	idempotencyDuplicates         uint64
	dlqMessages                   uint64
	concurrencyConflicts          uint64
	reconciliationDivergences     uint64
	processingDurationNanoseconds uint64
	processingCount               uint64
	outboxDelayNanoseconds        uint64
	outboxPublicationCount        uint64
}

func NewMetrics(pool *pgxpool.Pool) *Metrics {
	return &Metrics{pool: pool, transactionsByStatus: make(map[string]uint64), retriesByComponent: make(map[string]uint64)}
}

func (m *Metrics) RecordTransaction(status string, duplicate bool, duration time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.transactionsByStatus[strings.ToLower(status)]++
	if duplicate {
		m.idempotencyDuplicates++
	}
	m.processingDurationNanoseconds += uint64(duration)
	m.processingCount++
}

func (m *Metrics) RecordRetry(component string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.retriesByComponent[component]++
}

func (m *Metrics) RecordDLQMessage() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.dlqMessages++
}

func (m *Metrics) RecordConcurrencyConflict() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.concurrencyConflicts++
}

func (m *Metrics) RecordOutboxPublication(delay time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.outboxDelayNanoseconds += uint64(delay)
	m.outboxPublicationCount++
}

func (m *Metrics) RecordReconciliationDivergence() {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.reconciliationDivergences++
}

func (m *Metrics) Handler(writer http.ResponseWriter, request *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	m.mu.Lock()
	defer m.mu.Unlock()

	var oldestOutboxDelay float64
	if m.pool != nil {
		if err := m.pool.QueryRow(request.Context(), `SELECT COALESCE(EXTRACT(EPOCH FROM now() - MIN(occurred_at)), 0) FROM outbox_events WHERE published_at IS NULL`).Scan(&oldestOutboxDelay); err != nil {
			oldestOutboxDelay = 0
		}
	}

	_, _ = fmt.Fprintln(writer, "# HELP wager_transactions_total Total completed transaction attempts by final status.")
	_, _ = fmt.Fprintln(writer, "# TYPE wager_transactions_total counter")
	writeLabeledCounters(writer, "wager_transactions_total", "status", m.transactionsByStatus)
	_, _ = fmt.Fprintf(writer, "# HELP wager_idempotency_duplicates_total Total operations recognized as idempotent replays.\n# TYPE wager_idempotency_duplicates_total counter\nwager_idempotency_duplicates_total %d\n", m.idempotencyDuplicates)
	_, _ = fmt.Fprintln(writer, "# HELP wager_retries_total Total retries scheduled by component.")
	_, _ = fmt.Fprintln(writer, "# TYPE wager_retries_total counter")
	writeLabeledCounters(writer, "wager_retries_total", "component", m.retriesByComponent)
	_, _ = fmt.Fprintf(writer, "# HELP wager_dlq_messages_total Total failed SQS deliveries that will be moved to the DLQ.\n# TYPE wager_dlq_messages_total counter\nwager_dlq_messages_total %d\n", m.dlqMessages)
	_, _ = fmt.Fprintf(writer, "# HELP wager_concurrency_conflicts_total Total conflicting concurrent/idempotency operations.\n# TYPE wager_concurrency_conflicts_total counter\nwager_concurrency_conflicts_total %d\n", m.concurrencyConflicts)
	_, _ = fmt.Fprintf(writer, "# HELP wager_processing_duration_seconds Processing latency for completed operations.\n# TYPE wager_processing_duration_seconds summary\nwager_processing_duration_seconds_sum %.9f\nwager_processing_duration_seconds_count %d\n", float64(m.processingDurationNanoseconds)/float64(time.Second), m.processingCount)
	_, _ = fmt.Fprintf(writer, "# HELP wager_outbox_oldest_pending_seconds Age of the oldest unpublished outbox event.\n# TYPE wager_outbox_oldest_pending_seconds gauge\nwager_outbox_oldest_pending_seconds %.6f\n", oldestOutboxDelay)
	_, _ = fmt.Fprintf(writer, "# HELP wager_outbox_publication_delay_seconds Delay between event creation and publication.\n# TYPE wager_outbox_publication_delay_seconds summary\nwager_outbox_publication_delay_seconds_sum %.9f\nwager_outbox_publication_delay_seconds_count %d\n", float64(m.outboxDelayNanoseconds)/float64(time.Second), m.outboxPublicationCount)
	_, _ = fmt.Fprintf(writer, "# HELP wallet_reconciliation_divergences_total Total reconciliation checks that found a balance divergence.\n# TYPE wallet_reconciliation_divergences_total counter\nwallet_reconciliation_divergences_total %d\n", m.reconciliationDivergences)
}

func writeLabeledCounters(writer http.ResponseWriter, name, label string, values map[string]uint64) {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		_, _ = fmt.Fprintf(writer, `%s{%s=%q} %d`+"\n", name, label, key, values[key])
	}
}
