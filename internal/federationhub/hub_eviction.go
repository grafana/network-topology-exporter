package federationhub

// Spoke eviction (LD-18): periodic removal of spokes that have not pushed
// within federation.spoke_timeout, followed by a rebuild/republish of the
// combined graph. Split from hub.go (#168) — same-package move, no behaviour
// change.

import (
	"context"
	"time"
)

// runEviction periodically evicts spokes that have not pushed within
// federation.spoke_timeout, per LD-18. Spoke liveness and link liveness are
// distinct failure modes; this path handles domain-level silence, not
// individual link instability.
func (h *Hub) runEviction(ctx context.Context) {
	if h.cfg.SpokeTimeout <= 0 {
		return // eviction disabled; avoids time.NewTicker(0) panic in tests
	}
	ticker := time.NewTicker(h.cfg.SpokeTimeout / 2)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// Per-tick recovery: evictSilentSpokes rebuilds and republishes
			// the combined graph, so a panic there (site hub_rebuild) must not
			// kill the eviction loop. Recover, count it, and let the next tick
			// try again. Wrapped in a closure so the deferred recover scopes to
			// one tick rather than the whole goroutine.
			func() {
				defer h.recoverGoroutine("hub_rebuild")
				h.evictSilentSpokes()
			}()
		}
	}
}

func (h *Hub) evictSilentSpokes() {
	// Eviction republishes the combined graph and writes a snapshot, so it is a
	// leader-only path (mirrors the leader gate on handlePush, T3). A DEMOTED
	// leader still holds spoke entries and its now-stale lease epoch; without
	// this guard its next eviction tick would republish its graph and attempt a
	// snapshot write under that stale epoch. Single-hub mode defaults isLeader
	// true (NewHub), so eviction runs exactly as before.
	if !h.isLeader.Load() {
		return
	}
	now := time.Now()
	h.mu.Lock()
	var evicted []string
	for id, entry := range h.spokes {
		if now.Sub(entry.lastSeen) > h.cfg.SpokeTimeout {
			delete(h.spokes, id)
			evicted = append(evicted, id)
		}
	}
	// Delete gauge series while h.mu is still held so a concurrent handlePush
	// cannot race to re-add the series before we remove it. Deletion is cleaner
	// than Set(0): the series disappears immediately on eviction rather than
	// lingering with a stale 0 value until the next scrape interval.
	for _, id := range evicted {
		h.m.FederationSpokeUp.DeleteLabelValues(id)
		h.m.FederationSpokeLastPushUnix.DeleteLabelValues(id)
	}
	h.mu.Unlock()

	for _, id := range evicted {
		h.logger.Warn("hub: spoke evicted (no push within timeout)",
			"spoke_id", id,
			"timeout", h.cfg.SpokeTimeout,
		)
	}
	if len(evicted) > 0 {
		h.mu.Lock()
		spokes := h.spokesSnapshot()
		gen := h.publishGen.Add(1)
		h.mu.Unlock()
		combined, unmatchedCount := h.buildCombinedGraph(spokes)
		if published, _ := h.publishIfWinner(gen, combined, unmatchedCount, nil); published {
			h.writeSnapshotAsync(combined)
		}
	}
}
