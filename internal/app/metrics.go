package app

import (
	"fmt"
	"net/http"
	"sync/atomic"
)

// Metrics contains the counters currently exposed by the application. The
// Prometheus text format lets local Docker users and production collectors
// scrape the same endpoint without another dependency.
type Metrics struct {
	reconciliationDivergences atomic.Uint64
}

func NewMetrics() *Metrics {
	return &Metrics{}
}

func (m *Metrics) RecordReconciliationDivergence() {
	m.reconciliationDivergences.Add(1)
}

func (m *Metrics) Handler(writer http.ResponseWriter, _ *http.Request) {
	writer.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	_, _ = fmt.Fprintf(writer, "# HELP wallet_reconciliation_divergences_total Total reconciliation checks that found a balance divergence.\n# TYPE wallet_reconciliation_divergences_total counter\nwallet_reconciliation_divergences_total %d\n", m.reconciliationDivergences.Load())
}
