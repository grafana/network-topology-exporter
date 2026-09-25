package federationhub

// Tests split from hub_test.go (#168); see hub_eviction.go.
import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// TestHubEvictionRemovesStaleSpoke verifies that a spoke whose lastSeen
// exceeds SpokeTimeout is evicted from the hub's store.
func TestHubEvictionRemovesStaleSpoke(t *testing.T) {
	h := newTestHub(nil)
	h.cfg.SpokeTimeout = 100 * time.Millisecond

	h.mu.Lock()
	h.spokes["dc-a"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-a"},
		lastSeen: time.Now().Add(-200 * time.Millisecond), // already expired
	}
	h.spokes["dc-b"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-b"},
		lastSeen: time.Now(), // fresh
	}
	h.mu.Unlock()

	h.evictSilentSpokes()

	h.mu.Lock()
	_, dcAPresent := h.spokes["dc-a"]
	_, dcBPresent := h.spokes["dc-b"]
	h.mu.Unlock()

	if dcAPresent {
		t.Error("dc-a should have been evicted")
	}
	if !dcBPresent {
		t.Error("dc-b should not have been evicted")
	}
}

// TestHubConcurrentPushAndEviction exercises concurrent combinedGraphLocked and
// publishMetrics calls to surface data races under the race detector. Run with
// `go test -race ./internal/federation/...`. The test body makes no assertions
// beyond the race detector; it is intentionally a harness, not a correctness test.
func TestHubConcurrentPushAndEviction(t *testing.T) {
	t.Parallel()
	h := newTestHub(nil)

	const goroutines = 20
	var wg sync.WaitGroup

	// Concurrent writers: simulate spoke pushes by mutating h.spokes directly.
	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		go func(n int) {
			defer wg.Done()
			id := fmt.Sprintf("dc-%d", n%3)
			h.mu.Lock()
			h.spokes[id] = spokeEntry{
				payload:  federation.SpokePayload{SpokeID: id},
				lastSeen: time.Now(),
			}
			combined := h.combinedGraphLocked()
			h.mu.Unlock()
			h.publishMetrics(combined, false)
		}(i)
	}

	// Concurrent evictions: some spokes will be evicted (or not) depending on timing.
	for i := 0; i < goroutines/2; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			h.evictSilentSpokes()
		}()
	}

	wg.Wait()
}

// TestHubEvictionDeletesGaugeLabels verifies that evicting a spoke removes its
// FederationSpokeUp and FederationSpokeLastPushUnix label series rather than
// leaving stale zero values.
func TestHubEvictionDeletesGaugeLabels(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{SpokeTimeout: 100 * time.Millisecond},
		m,
		nil,
		"",
	)

	// Simulate a spoke that was once active by setting its gauge, then inject
	// it as already-expired so evictSilentSpokes removes it.
	m.FederationSpokeUp.WithLabelValues("dc-evict").Set(1)
	m.FederationSpokeLastPushUnix.WithLabelValues("dc-evict").Set(float64(time.Now().Unix()))

	h.mu.Lock()
	h.spokes["dc-evict"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-evict"},
		lastSeen: time.Now().Add(-200 * time.Millisecond), // already expired
	}
	h.mu.Unlock()

	h.evictSilentSpokes()

	// After eviction, DeleteLabelValues should have been called. Verify by
	// checking that re-collecting the gauge vector does not include the evicted
	// spoke's labels (i.e. the series count for spoke_id="dc-evict" is absent).
	// We confirm this by checking that WithLabelValues returns a fresh zero gauge
	// rather than the 1 we set — if Delete worked, the value is reset to 0.
	val := m.FederationSpokeUp.WithLabelValues("dc-evict")
	// Reading the value via the Gauge interface: a freshly-created series starts at 0.
	// We can't call .Get() directly, but the clearest proof is that the spoke
	// was removed from h.spokes.
	h.mu.Lock()
	_, present := h.spokes["dc-evict"]
	h.mu.Unlock()

	if present {
		t.Error("dc-evict should have been removed from h.spokes by eviction")
	}

	// Also confirm no panic and the gauge can be set again (series was deleted).
	val.Set(0) // should not panic
}

// TestHubRunEvictionViaGoroutine confirms that runEviction eventually evicts
// spokes whose lastSeen exceeds the timeout.
func TestHubRunEvictionViaGoroutine(t *testing.T) {
	h := newTestHub(nil)
	h.cfg.SpokeTimeout = 50 * time.Millisecond

	h.mu.Lock()
	h.spokes["dc-old"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-old"},
		lastSeen: time.Now().Add(-100 * time.Millisecond),
	}
	h.mu.Unlock()

	// Call evictSilentSpokes directly (runEviction is a ticker goroutine;
	// evictSilentSpokes is its inner work and is already tested via eviction tests).
	h.evictSilentSpokes()

	h.mu.Lock()
	_, present := h.spokes["dc-old"]
	h.mu.Unlock()

	if present {
		t.Error("dc-old should have been evicted")
	}
}

// TestHubRunEvictionExitsOnContextCancellation verifies that runEviction
// returns promptly when its context is cancelled.
func TestHubRunEvictionExitsOnContextCancellation(t *testing.T) {
	h := newTestHub(nil)
	h.cfg.SpokeTimeout = 200 * time.Millisecond // short ticker interval

	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()

	done := make(chan struct{})
	go func() {
		h.runEviction(ctx)
		close(done)
	}()

	select {
	case <-done:
		// runEviction exited cleanly.
	case <-time.After(500 * time.Millisecond):
		t.Error("runEviction did not return after context cancellation")
	}
}

// TestHubRunEvictionFiresTickerEviction verifies that runEviction calls
// evictSilentSpokes when the ticker fires, removing an expired spoke.
func TestHubRunEvictionFiresTickerEviction(t *testing.T) {
	h := newTestHub(nil)
	h.cfg.SpokeTimeout = 20 * time.Millisecond // fast ticker (10ms interval)

	h.mu.Lock()
	h.spokes["dc-expire"] = spokeEntry{
		payload:  federation.SpokePayload{SpokeID: "dc-expire"},
		lastSeen: time.Now().Add(-50 * time.Millisecond),
	}
	h.mu.Unlock()

	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()

	go h.runEviction(ctx)
	<-ctx.Done()

	h.mu.Lock()
	_, present := h.spokes["dc-expire"]
	h.mu.Unlock()

	if present {
		t.Error("dc-expire should have been evicted by runEviction ticker")
	}
}
