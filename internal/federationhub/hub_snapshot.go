package federationhub

// LD-13 snapshot persistence: the bounded single-writer goroutine and the
// async enqueue path that keeps an NFS stall from accumulating goroutines.
// Split from hub.go (#168) — same-package move, no behaviour change.

import (
	"context"
	"time"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/snapshot"
)

// writeSnapshot persists the hub's current graph to disk (LD-13). A no-op
// when snapshotPath is empty. Hub snapshots omit credential cache and
// unconfirmed-age counters; those are spoke-side concerns.
func (h *Hub) writeSnapshot(g discovery.Graph) {
	if h.snapshotPath == "" {
		return
	}
	f := snapshot.File{
		Devices: g.Devices,
		Edges:   g.Edges,
		// Fence token (#71 §4.4). leaseEpoch defaults 0 ⇒ single-hub writes are
		// never fenced (byte-identical to today). T6 wires the elector's epoch
		// and the real holder identity; for now Holder stays "".
		LeaseEpoch: h.leaseEpoch.Load(),
	}
	if err := h.snapshotWriteFn(h.snapshotPath, f); err != nil {
		h.logger.Error("hub: snapshot write failed", "error", err)
		return
	}
	h.m.SnapshotLastWrittenUnix.Set(float64(time.Now().Unix()))
}

// runSnapshotWriter is the single bounded snapshot writer goroutine for the
// hub. It drains h.snapshotCh one graph at a time, so an NFS stall cannot
// accumulate goroutines across spoke pushes.
func (h *Hub) runSnapshotWriter(ctx context.Context) {
	// Recover a panic in the snapshot writer so a bug in the write path
	// cannot crash the aggregator. One-shot: on recovery the writer exits;
	// subsequent writeSnapshotAsync calls then trip queue_full and count the
	// dropped snapshots, so the failure stays observable.
	defer h.recoverGoroutine("hub_snapshot_writer")
	var writeDone chan struct{} // non-nil while a write goroutine is in flight
	for {
		select {
		case <-ctx.Done():
			return
		case g := <-h.snapshotCh:
			// Collect result from any previously timed-out write that has now finished.
			// writeDone is always reassigned below before the next iteration's check,
			// so prior channel references don't leak across iterations.
			if writeDone != nil {
				select {
				case <-writeDone:
				default:
					h.m.SnapshotDropsTotal.WithLabelValues(string(metrics.SnapshotDropReasonWriteInFlight)).Inc()
					h.logger.Warn("hub: snapshot write still in flight; dropping snapshot (NFS stall?)")
					continue
				}
			}
			writeDone = make(chan struct{}, 1)
			go func(g discovery.Graph, done chan struct{}) {
				// Recover a panic in writeSnapshot so it cannot crash the
				// process; close(done) still runs via defer so the parent's
				// select unblocks on the success branch rather than timing
				// out. Registered first so it runs after the close.
				defer h.recoverGoroutine("hub_snapshot_writer")
				defer close(done)
				h.writeSnapshot(g)
			}(g, writeDone)
			select {
			case <-writeDone:
				// success path — writeDone naturally falls out of scope at the
				// next iteration's `writeDone = make(...)`.
			case <-time.After(h.snapshotWriteTimeout):
				h.logger.Warn("hub: snapshot write timed out (NFS stall?)", "timeout", h.snapshotWriteTimeout)
				// writeDone goroutine still running; next iteration will detect this.
			case <-ctx.Done():
				// writeDone was just assigned a non-nil channel above and the
				// success branch above is the only other case that could have
				// fired; so writeDone is definitively non-nil here. Wait for
				// the in-flight write or shutdown-grace timeout.
				select {
				case <-writeDone:
				case <-time.After(h.snapshotWriteTimeout):
					h.logger.Warn("hub: snapshot write did not complete before shutdown; data may be lost")
				}
				return
			}
		}
	}
}

// writeSnapshotAsync enqueues g for writing by the bounded runSnapshotWriter
// goroutine. If the channel is full (previous write still in flight), the new
// snapshot is dropped rather than spawning an additional goroutine.
func (h *Hub) writeSnapshotAsync(g discovery.Graph) {
	if h.snapshotCh == nil {
		return
	}
	select {
	case h.snapshotCh <- g:
	default:
		h.m.SnapshotDropsTotal.WithLabelValues(string(metrics.SnapshotDropReasonQueueFull)).Inc()
		h.logger.Warn("hub: snapshot write queue full; dropping (NFS stall?)")
	}
}
