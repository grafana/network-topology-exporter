package federationhub

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/snapshot"
)

func TestHubLeadershipState(t *testing.T) {
	h := NewHub(config.FederationConfig{}, metrics.New(false), nil, "")
	if !h.IsLeader() {
		t.Fatal("non-HA hub must default to leader")
	}
	h.SetLeader(false)
	if h.IsLeader() {
		t.Fatal("SetLeader(false) not reflected")
	}
	h.SetLeader(true)
	if !h.IsLeader() {
		t.Fatal("SetLeader(true) not reflected")
	}
}

// TestFakeElectorDrivesCallbacks exercises the test-only fakeElector so its
// symbols are referenced (and verifies the lead/step → callback wiring that
// T6+ will rely on). Run blocks until ctx is cancelled and returns ctx.Err();
// lead()/step() invoke the recorded callbacks.
func TestFakeElectorDrivesCallbacks(t *testing.T) {
	f := newFakeElector()

	var started, stopped bool
	cb := LeaderCallbacks{
		OnStartedLeading: func(context.Context) { started = true },
		OnStoppedLeading: func() { stopped = true },
	}

	ctx, cancel := context.WithCancel(context.Background())

	// Run records the callbacks then blocks on ctx. Start it, drive the
	// callbacks once it has recorded them (signalled via ready), then cancel.
	ready := make(chan struct{})
	done := make(chan error, 1)
	go func() {
		done <- f.run(ctx, cb, ready)
	}()
	<-ready

	f.lead()
	if !started {
		t.Fatal("lead() did not invoke OnStartedLeading")
	}
	f.step()
	if !stopped {
		t.Fatal("step() did not invoke OnStoppedLeading")
	}

	cancel()
	if err := <-done; err == nil {
		t.Fatal("Run should return ctx.Err() after cancel")
	}
}

// TestEvictionGatedOnLeadership verifies Fix 1 of the Task-6 follow-up: a
// DEMOTED leader (isLeader=false but still holding spoke entries) must NOT
// run eviction-driven republish/snapshot, while a leader still evicts as
// before (single-hub regression check).
func TestEvictionGatedOnLeadership(t *testing.T) {
	t.Run("non-leader does not evict, republish, or snapshot", func(t *testing.T) {
		var snapshotWrites atomic.Int64
		h := NewHub(config.FederationConfig{SpokeTimeout: 100 * time.Millisecond}, metrics.New(false), nil, "")
		h.snapshotWriteFn = func(string, snapshot.File) error {
			snapshotWrites.Add(1)
			return nil
		}
		h.SetLeader(false)

		h.mu.Lock()
		h.spokes["dc-a"] = spokeEntry{
			payload:  federation.SpokePayload{SpokeID: "dc-a"},
			lastSeen: time.Now().Add(-time.Second), // already expired
		}
		h.mu.Unlock()

		genBefore := h.publishGen.Load()
		h.evictSilentSpokes()

		h.mu.Lock()
		_, present := h.spokes["dc-a"]
		h.mu.Unlock()
		if !present {
			t.Error("non-leader must not evict spokes")
		}
		if got := h.publishGen.Load(); got != genBefore {
			t.Errorf("non-leader republished: publishGen %d -> %d", genBefore, got)
		}
		if got := snapshotWrites.Load(); got != 0 {
			t.Errorf("non-leader wrote %d snapshots, want 0", got)
		}
	})

	t.Run("leader still evicts and republishes (regression)", func(t *testing.T) {
		h := NewHub(config.FederationConfig{SpokeTimeout: 100 * time.Millisecond}, metrics.New(false), nil, "")
		// NewHub defaults isLeader=true (single-hub mode).
		h.mu.Lock()
		h.spokes["dc-a"] = spokeEntry{
			payload:  federation.SpokePayload{SpokeID: "dc-a"},
			lastSeen: time.Now().Add(-time.Second), // already expired
		}
		h.mu.Unlock()

		genBefore := h.publishGen.Load()
		h.evictSilentSpokes()

		h.mu.Lock()
		_, present := h.spokes["dc-a"]
		h.mu.Unlock()
		if present {
			t.Error("leader must still evict expired spokes")
		}
		if got := h.publishGen.Load(); got == genBefore {
			t.Errorf("leader did not republish after eviction: publishGen still %d", got)
		}
	})
}

func TestIsReadyRequiresLeadership(t *testing.T) {
	h := NewHub(config.FederationConfig{}, metrics.New(false), nil, "")
	h.firstLive.Store(true)
	h.SetLeader(true)
	if !h.IsReady() {
		t.Fatal("leader with firstLive must be ready")
	}
	h.SetLeader(false)
	if h.IsReady() {
		t.Fatal("non-leader must not be ready (push Service gate)")
	}
}

// TestFailoverTakeover (acceptance, design §13.1) models two hubs' leadership
// transition. It asserts BOTH arms of the flip: after the leader dies and the
// follower is promoted, the new leader accepts AND the demoted leader 503s.
// Uses the fake leadership state (SetLeader) — no live cluster.
func TestFailoverTakeover(t *testing.T) {
	leader := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, "")
	follower := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, "")
	leader.SetLeader(true)
	follower.SetLeader(false)
	pushCode := func(h *Hub) int {
		body, _ := json.Marshal(federation.SpokePayload{SpokeID: "dc-1", CycleAt: time.Now(), Devices: []discovery.Device{{ID: "d1"}}})
		rec := httptest.NewRecorder()
		h.handlePush(rec, httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body)))
		return rec.Code
	}
	if pushCode(follower) != http.StatusServiceUnavailable {
		t.Fatal("follower must 503 while leader leads")
	}
	if pushCode(leader) == http.StatusServiceUnavailable {
		t.Fatal("leader must accept")
	}
	// leader dies → follower promoted
	leader.SetLeader(false)
	follower.SetLeader(true)
	if pushCode(follower) == http.StatusServiceUnavailable {
		t.Fatal("new leader still 503ing")
	}
	if pushCode(leader) != http.StatusServiceUnavailable {
		t.Fatal("demoted leader must 503")
	}
}

// TestNoSplitBrainInvariant (acceptance, design §13.2 / §8) models leadership
// as a single token handed across two hubs and asserts the split-brain
// invariant at EVERY transition: exactly one hub returns non-503 (count == 1),
// and a 503ing follower never reaches publishIfWinner (its Topology is never
// updated). The test would FAIL if the leader gate were removed (both would
// accept ⇒ count == 2) or if two hubs were leader simultaneously.
func TestNoSplitBrainInvariant(t *testing.T) {
	// Each hub gets a snapshotPath so writeSnapshotAsync has a non-nil channel;
	// we replace snapshotWriteFn with a per-hub counter so an applied push (which
	// calls writeSnapshotAsync after publishIfWinner) is observable. A follower
	// that 503s never reaches publishIfWinner → never enqueues a snapshot, so its
	// counter stays 0. This is a per-state publish observer (unlike firstLive,
	// which is sticky once set).
	var pubA, pubB atomic.Int64
	newCountingHub := func(c *atomic.Int64) *Hub {
		dir := t.TempDir()
		h := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, dir+"/snap.json")
		h.snapshotWriteFn = func(string, snapshot.File) error { c.Add(1); return nil }
		return h
	}
	a := newCountingHub(&pubA)
	b := newCountingHub(&pubB)
	hubs := []*Hub{a, b}
	pubCounters := []*atomic.Int64{&pubA, &pubB}
	// Drive the snapshot writer so enqueued writes reach snapshotWriteFn.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.runSnapshotWriter(ctx)
	go b.runSnapshotWriter(ctx)

	pushCode := func(h *Hub) int {
		body, _ := json.Marshal(federation.SpokePayload{SpokeID: "dc-1", CycleAt: time.Now(), Devices: []discovery.Device{{ID: "d1"}}})
		rec := httptest.NewRecorder()
		h.handlePush(rec, httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body)))
		return rec.Code
	}

	// leaderIdx is the single token: exactly one hub holds leadership in each
	// state. Drive a sequence of single-leader transitions.
	setLeader := func(idx int) {
		for i, h := range hubs {
			h.SetLeader(i == idx)
		}
	}

	states := []int{0, 1, 0, 1, 1, 0} // includes a no-op (1→1) to exercise idempotency
	for step, leaderIdx := range states {
		setLeader(leaderIdx)
		// Record each follower's publish count before this state's push; a
		// follower must not publish (counter unchanged) in a state where it is
		// not the leader.
		before := []int64{pubA.Load(), pubB.Load()}
		nonServiceUnavailable := 0
		for i, h := range hubs {
			code := pushCode(h)
			if i == leaderIdx {
				if code == http.StatusServiceUnavailable {
					t.Fatalf("step %d: leader hub[%d] must accept, got 503", step, i)
				}
			} else if code != http.StatusServiceUnavailable {
				t.Fatalf("step %d: follower hub[%d] must 503, got %d", step, i, code)
			}
			if code != http.StatusServiceUnavailable {
				nonServiceUnavailable++
			}
		}
		// The core invariant: exactly one hub accepts. Non-vacuous — it fails if
		// the gate is removed (would be 2) or if no hub leads (would be 0).
		if nonServiceUnavailable != 1 {
			t.Fatalf("step %d: %d hubs accepted, want exactly 1 (split-brain invariant)", step, nonServiceUnavailable)
		}
		// Let the leader's async snapshot land, then assert no follower published
		// in this state. A 503ing follower never reaches publishIfWinner /
		// writeSnapshotAsync, so its counter must not have advanced.
		waitFor(t, func() bool { return pubCounters[leaderIdx].Load() > before[leaderIdx] })
		for i := range hubs {
			if i == leaderIdx {
				continue
			}
			if got := pubCounters[i].Load(); got != before[i] {
				t.Fatalf("step %d: follower hub[%d] published while not leader (split-brain): %d -> %d", step, i, before[i], got)
			}
		}
	}
}

// waitFor polls cond up to ~2s; fails the test if it never becomes true. Used
// to await an async snapshot write without a fixed sleep.
func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("condition not met within timeout")
}

// TestSingleHubRegression (acceptance, design §13.3) is the regression gate as
// a test: a default NewHub (no elector) must default to leader, accept pushes,
// and gate IsReady on firstLive only. It would FAIL if isLeader did not default
// to true (the push would 503 and IsReady would stay false after firstLive).
func TestSingleHubRegression(t *testing.T) {
	h := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, "")

	if !h.IsLeader() {
		t.Fatal("single-hub NewHub must default to leader")
	}
	// IsReady is firstLive-gated; before any push it is not ready.
	if h.IsReady() {
		t.Fatal("single-hub must not be ready before firstLive")
	}

	body, _ := json.Marshal(federation.SpokePayload{SpokeID: "dc-1", CycleAt: time.Now(), Devices: []discovery.Device{{ID: "d1"}}})
	rec := httptest.NewRecorder()
	h.handlePush(rec, httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body)))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatalf("single-hub push 503'd (isLeader did not default true): got %d", rec.Code)
	}
	if rec.Code != http.StatusNoContent {
		t.Fatalf("single-hub push: got %d, want 204", rec.Code)
	}
	// After the first accepted push, firstLive is set and the hub is ready.
	if !h.firstLive.Load() {
		t.Fatal("single-hub did not set firstLive after accepted push")
	}
	if !h.IsReady() {
		t.Fatal("single-hub leader with firstLive must be ready")
	}
}

func TestHandlePush503WhenNotLeader(t *testing.T) {
	h := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, "")
	h.SetLeader(false)
	body, _ := json.Marshal(federation.SpokePayload{SpokeID: "dc-x", CycleAt: time.Now(), Devices: []discovery.Device{{ID: "d1"}}})
	rec := httptest.NewRecorder()
	h.handlePush(rec, httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body)))
	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("non-leader push: got %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Connection"); got != "close" {
		t.Fatalf("expected Connection: close, got %q", got)
	}
	// Leader accepts (does not 503).
	h.SetLeader(true)
	rec = httptest.NewRecorder()
	h.handlePush(rec, httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body)))
	if rec.Code == http.StatusServiceUnavailable {
		t.Fatal("leader push wrongly 503'd")
	}
}
