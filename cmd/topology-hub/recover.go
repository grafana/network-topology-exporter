package main

import (
	"log/slog"
	"runtime/debug"

	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// recoverGoroutine is the shared panic-recovery body for this binary's two
// long-lived background goroutines (hub_serve, hub_elector). Identical in
// behavior to internal/app.recoverGoroutine, which the pre-split
// "case config.RoleHub" branch used for the same two sites; duplicated here
// rather than imported so this binary does not depend on internal/app. On a
// panic it logs the panic value plus the stack trace at Error level,
// increments network_topology_panics_total{site}, and returns cleanly
// (it does NOT re-panic).
func recoverGoroutine(site string, logger *slog.Logger, m *metrics.Metrics) {
	r := recover()
	if r == nil {
		return
	}
	if logger == nil {
		logger = slog.Default()
	}
	logger.Error("background goroutine panicked; recovered",
		"site", site,
		"panic", r,
		"stack", string(debug.Stack()),
	)
	if m != nil {
		m.PanicsRecoveredTotal.WithLabelValues(site).Inc()
	}
}
