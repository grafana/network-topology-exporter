package federationhub

import (
	"log/slog"
	"testing"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

// recoverInGoroutineBody runs fn under the hub's recoverGoroutine the same way
// a real hub goroutine does, so the test can confirm a panic is swallowed
// rather than propagated.
func (h *Hub) recoverInGoroutineBody(site string, fn func()) {
	defer h.recoverGoroutine(site)
	fn()
}

// TestHubRecoverGoroutine_RecoversAndCounts verifies the hub's recover helper
// swallows a panic and increments network_topology_panics_total{site}.
func TestHubRecoverGoroutine_RecoversAndCounts(t *testing.T) {
	h := newTestHub(nil)

	// If recoverGoroutine re-panicked, the test would crash; reaching the
	// assertion proves it was contained.
	h.recoverInGoroutineBody("hub_rebuild", func() { panic("boom") })

	got := testutil.ToFloat64(h.m.PanicsRecoveredTotal.WithLabelValues("hub_rebuild"))
	if got != 1 {
		t.Fatalf("PanicsRecoveredTotal{site=hub_rebuild} = %v, want 1", got)
	}
}

// TestHubRecoverGoroutine_NilMetrics verifies the helper tolerates a nil
// *metrics.Metrics handle (h.m): it must recover the panic without itself
// panicking on the nil counter. logger is set to a real logger so the Error
// log line does not hit a nil receiver.
func TestHubRecoverGoroutine_NilMetrics(t *testing.T) {
	h := &Hub{logger: slog.Default()} // h.m left nil
	// Must not panic.
	h.recoverInGoroutineBody("hub_snapshot_writer", func() { panic("boom") })
}
