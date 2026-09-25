// Package federationhub implements the LD-16, LD-18–LD-20 federation hub: the
// pure aggregator that receives spoke pushes over mTLS, combines them into one
// reconciled graph, and (when federation.hub.ha.enabled) coordinates HA leader
// election via a Kubernetes Lease. It was split out of internal/federation
// (which keeps the spoke-side push transport and the shared SpokePayload wire
// type) by the core/hub binary split (docs/proposals/core-hub-split.md §3.2),
// so that k8s.io/client-go — needed only for the opt-in HA elector in
// elector_k8s.go — is imported by this package alone, and linked only into
// cmd/topology-hub, the sole importer of this package.
//
// Core Hub type, constructor, panic recovery, snapshot restore, and the
// leader/lease accessors live in this file. The remaining hub concerns live
// in sibling files (#168 decomposition — same-package moves, no behaviour
// change):
//
//	hub_server.go   — mTLS server bootstrap and lifecycle (Serve)
//	hub_push.go     — /spoke/push handling and the structured reject contract
//	hub_validate.go — payload validation (label safety, structural invariants)
//	hub_merge.go    — combined-graph construction and device-name normalisation
//	hub_eviction.go — LD-18 silent-spoke eviction
//	hub_publish.go  — generation-fenced publishIfWinner commit path
//	hub_snapshot.go — LD-13 snapshot writer goroutine and async enqueue
package federationhub

import (
	"log/slog"
	"runtime/debug"
	"sync"
	"sync/atomic"
	"time"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/snapshot"
)

type spokeEntry struct {
	payload  federation.SpokePayload
	lastSeen time.Time
}

// acceptedPush carries the spoke state to commit atomically with a winning
// publication. entry.lastSeen is the accept time used for the liveness gauge.
// A nil *acceptedPush (e.g. from eviction) publishes the graph without
// registering any spoke or touching liveness metrics.
type acceptedPush struct {
	id    string
	entry spokeEntry
}

// Hub aggregates federation.SpokePayload pushes from spoke instances, reconciles the
// combined edge set across all spoke domains, and updates the shared
// Prometheus metrics with the unified topology. Per LD-16, spokes push;
// the hub never polls spokes.
type Hub struct {
	cfg                  config.FederationConfig
	mu                   sync.Mutex
	spokes               map[string]spokeEntry
	m                    *metrics.Metrics
	logger               *slog.Logger
	snapshotPath         string
	snapshotCh           chan discovery.Graph
	firstLive            atomic.Bool   // set to true on the first live publishIfWinner call
	isLeader             atomic.Bool   // single-hub default true; flipped by the LeaderElector in HA mode
	leaseEpoch           atomic.Uint64 // fence token for shared-snapshot writes (#71 §4.4); 0 = single-hub / never fenced. T6 sets this from the elector.
	snapshotWriteFn      func(string, snapshot.File) error
	snapshotWriteTimeout time.Duration
	publishGen           atomic.Uint64
	// lastPublishedGen is the generation of the last graph actually published.
	// Guarded by mu (read+written only inside publishIfWinner). Plain uint64,
	// not atomic: the CAS loop is gone now that gen comparison happens under mu.
	lastPublishedGen uint64
}

// NewHub constructs a Hub ready to accept spoke pushes. snapshotPath enables
// LD-13 persistence; pass "" to disable snapshot writes (e.g., in tests).
func NewHub(cfg config.FederationConfig, m *metrics.Metrics, logger *slog.Logger, snapshotPath string) *Hub {
	if logger == nil {
		logger = slog.Default()
	}
	h := &Hub{
		cfg:                  cfg,
		spokes:               make(map[string]spokeEntry),
		m:                    m,
		logger:               logger,
		snapshotPath:         snapshotPath,
		snapshotWriteFn:      snapshot.Write,
		snapshotWriteTimeout: 30 * time.Second,
	}
	if snapshotPath != "" {
		h.snapshotCh = make(chan discovery.Graph, 1)
	}
	// Single-hub default: always leader. The elector (HA mode) flips this.
	h.isLeader.Store(true)
	return h
}

// recoverGoroutine is the hub's panic-recovery body for its long-lived
// background goroutines (eviction loop, snapshot writer). Without it, a panic
// in any of them crashes the whole aggregator and destroys the combined graph
// for every spoke. It mirrors the per-device recover block in
// internal/app/cycle.go: it logs the panic value plus stack trace at Error
// level, increments network_topology_panics_total{site} so the bug is never
// hidden silently, and returns cleanly (does NOT re-panic) so the goroutine
// dies gracefully. The hub already holds its own *metrics.Metrics handle
// (h.m), so no injection seam is needed here — unlike the discovery modules,
// the federation package legitimately depends on internal/metrics. h.m is
// tolerated as nil (skips the counter) so tests can construct a bare Hub.
//
// Used as the first deferred call at the top of a goroutine body, e.g.
// `defer h.recoverGoroutine("hub_rebuild")`. site must be one of the closed,
// low-cardinality strings documented on metrics.PanicsRecoveredTotal.
func (h *Hub) recoverGoroutine(site string) {
	r := recover()
	if r == nil {
		return
	}
	h.logger.Error("hub background goroutine panicked; recovered",
		"site", site,
		"panic", r,
		"stack", string(debug.Stack()),
	)
	if h.m != nil {
		h.m.PanicsRecoveredTotal.WithLabelValues(site).Inc()
	}
}

// RestoreGraph populates hub metrics from a snapshot loaded at startup so the
// hub can serve stale-but-valid metrics (GraphStale=1) until the first live
// spoke push arrives (LD-13). The caller must set m.GraphStale=1 before
// invoking this; the hub clears it after the first successful push.
func (h *Hub) RestoreGraph(g discovery.Graph) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.publishMetrics(g, false)
}

// IsReady reports whether this hub should receive spoke pushes: it must have
// live data (firstLive) AND be the leader. This gates the leader-only push
// Service, so spokes route only to the leader (HA). Followers report
// NotReady-for-push but stay scrapeable via the separate metrics Service
// (publishNotReadyAddresses), so /metrics is never removed from a follower.
//
// In single-hub mode isLeader defaults true (NewHub stores true), so this is
// identical to the previous firstLive-only semantics: the hub can serve
// /metrics from the startup snapshot immediately, but is only "ready" once at
// least one spoke has confirmed its topology.
func (h *Hub) IsReady() bool {
	return h.firstLive.Load() && h.isLeader.Load()
}

// IsLeader reports whether this hub currently accepts pushes and publishes.
// Always true in single-hub mode; flipped by the LeaderElector in HA mode.
func (h *Hub) IsLeader() bool { return h.isLeader.Load() }

// SetLeader is invoked by the LeaderElector callbacks (HA mode only).
func (h *Hub) SetLeader(v bool) { h.isLeader.Store(v) }

// SetLeaseEpoch records the fence token (#71 §4.4) for shared-snapshot writes.
// Invoked by the LeaderElector wiring on OnStartedLeading with the Lease's
// server-assigned LeaderTransitions count, which orders writes across pods so a
// resumed stale leader carrying a lower epoch has its snapshot write refused
// (snapshot.ErrStaleEpoch). The wiring guarantees the value never decreases for
// a given pod: on a transient epoch-read error it retains the current epoch
// rather than substituting an unrelated local counter (see internal/app). A
// no-op-safe store: single-hub mode never calls it, leaving leaseEpoch 0
// (unfenced).
func (h *Hub) SetLeaseEpoch(epoch uint64) { h.leaseEpoch.Store(epoch) }

// LeaseEpoch returns the current fence token (#71 §4.4). Used by the elector
// wiring to keep the epoch monotonic across re-acquisitions.
func (h *Hub) LeaseEpoch() uint64 { return h.leaseEpoch.Load() }
