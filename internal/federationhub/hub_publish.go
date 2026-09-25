package federationhub

// Publication: the generation-fenced publishIfWinner commit path and the
// snapshot-restore metric swap. Split from hub.go (#168) — same-package move,
// no behaviour change.

import (
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// publishMetrics atomically swaps the Topology collector snapshot and, on the
// first live push, clears GraphStale. The atomic pointer swap in
// TopologyCollector.Update means concurrent spoke pushes cannot produce an
// interleaved or empty scrape window.
func (h *Hub) publishMetrics(g discovery.Graph, clearStale bool) {
	if clearStale {
		h.m.GraphStale.Set(0)
	}
	h.m.Topology.Update(g)
}

// publishIfWinner publishes g only when gen is strictly greater than the last
// published generation, preventing a slow concurrent goroutine from overwriting
// a newer combined graph with an older snapshot. It runs ENTIRELY under h.mu;
// the order is load-bearing:
//
//  1. stale-generation check  → reject; generation untouched
//  2. size-budget check       → reject; generation UNTOUCHED  (issue #147 fix)
//  3. win: advance generation, and if accepted != nil commit the spoke entry,
//     its liveness gauges, and first-live/GraphStale — atomically with
//     Topology.Update (issue #147 fix, incl. the eviction-race reverse).
//
// Removing the old CAS loop is safe: publishGen.Add(1) is performed under h.mu
// at the single call site, so two callers can never hold equal generations.
//
// HA note: leadership is NOT re-verified here. A hub demoted mid-request can
// commit a push the new leader never sees. That is acceptable only because
// every spoke push is a full snapshot, never a delta — the spoke re-sends its
// entire graph next cycle, bounding new-leader staleness at one
// discovery.interval (see docs/operator/federation.md "Leader-flip acceptance
// window"). If pushes ever become incremental, add an isLeader re-check here.
func (h *Hub) publishIfWinner(gen uint64, g discovery.Graph, unmatched int, accepted *acceptedPush) (bool, metrics.RejectReason) {
	h.mu.Lock()
	defer h.mu.Unlock()

	if gen <= h.lastPublishedGen {
		return false, rejectReasonStaleGeneration
	}

	maxEdges := h.cfg.Hub.MaxGraphEdges
	maxDevices := h.cfg.Hub.MaxGraphDevices
	if (maxEdges > 0 && len(g.Edges) > maxEdges) || (maxDevices > 0 && len(g.Devices) > maxDevices) {
		h.logger.Warn("graph update rejected: exceeds size budget",
			"edges", len(g.Edges), "max_edges", maxEdges,
			"devices", len(g.Devices), "max_devices", maxDevices)
		h.m.GraphUpdatesRejectedTotal.WithLabelValues(string(rejectReasonSizeBudgetExceeded)).Inc()
		return false, rejectReasonSizeBudgetExceeded // generation NOT advanced
	}

	h.lastPublishedGen = gen

	if accepted != nil {
		// Commit the spoke registration AND its liveness signals under the same
		// lock as the graph publish. If these gauges were set after the lock
		// released, a concurrent evictSilentSpokes could delete the entry+gauge
		// in between and we would resurrect a gauge for a spoke absent from both
		// h.spokes and the published graph (the #147 inconsistency, reversed).
		h.spokes[accepted.id] = accepted.entry
		h.m.FederationSpokeUp.WithLabelValues(accepted.id).Set(1)
		h.m.FederationSpokeLastPushUnix.WithLabelValues(accepted.id).Set(float64(accepted.entry.lastSeen.Unix()))
		if !h.firstLive.Load() {
			h.m.GraphStale.Set(0)
			h.firstLive.Store(true)
		}
	}

	h.m.HubOOSUnmatchedTotal.Set(float64(unmatched))
	h.m.Topology.Update(g)
	return true, ""
}
