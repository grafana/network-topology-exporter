package federationhub

// Tests split from hub_test.go (#168); see hub_snapshot.go.
import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/snapshot"
)

// TestHubWriteSnapshotPersistsGraph verifies that writeSnapshot writes a file
// at the configured path that can be loaded back via snapshot.Load.
func TestHubWriteSnapshotPersistsGraph(t *testing.T) {
	dir := t.TempDir()
	snapPath := filepath.Join(dir, "hub.json")

	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, snapPath)

	g := discovery.Graph{
		Devices: []discovery.Device{{ID: "sw-hub-1"}},
		Edges:   []discovery.Edge{},
	}
	h.writeSnapshot(g)

	f, err := snapshot.Load(snapPath)
	if err != nil {
		t.Fatalf("snapshot.Load: %v", err)
	}
	if f == nil {
		t.Fatal("snapshot.Load returned nil, expected written file")
		return
	}
	if len(f.Devices) != 1 || f.Devices[0].ID != "sw-hub-1" {
		t.Errorf("loaded devices = %#v, want [{ID:sw-hub-1}]", f.Devices)
	}
	if got := testutil.ToFloat64(m.SnapshotLastWrittenUnix); got == 0 {
		t.Error("SnapshotLastWrittenUnix not updated after writeSnapshot")
	}
}

// TestWriteSnapshotAsyncIsNonBlocking verifies that writeSnapshotAsync returns
// immediately even when the snapshot channel is already full. The actual
// NFS-stall timeout behaviour (runSnapshotWriter dropping a slow write and
// continuing) is tested in TestRunSnapshotWriterTimeoutContinues.
func TestWriteSnapshotAsyncIsNonBlocking(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, t.TempDir()+"/snap.json")

	// Fill the channel (capacity 1) so the next send would block if async were blocking.
	h.snapshotCh <- discovery.Graph{}

	start := time.Now()
	h.writeSnapshotAsync(discovery.Graph{}) // channel full — must drop and return immediately
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Errorf("writeSnapshotAsync blocked for %v — must return immediately", elapsed)
	}
}

// TestWriteSnapshotAsyncIncrementsDropsOnQueueFull verifies that the
// queue-full drop path on writeSnapshotAsync increments
// SnapshotDropsTotal{reason="queue_full"}. Issue #42.
func TestWriteSnapshotAsyncIncrementsDropsOnQueueFull(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, t.TempDir()+"/snap.json")

	before := testutil.ToFloat64(m.SnapshotDropsTotal.WithLabelValues(
		string(metrics.SnapshotDropReasonQueueFull)))

	h.snapshotCh <- discovery.Graph{}       // fill cap-1 channel
	h.writeSnapshotAsync(discovery.Graph{}) // must drop

	after := testutil.ToFloat64(m.SnapshotDropsTotal.WithLabelValues(
		string(metrics.SnapshotDropReasonQueueFull)))
	if got := after - before; got != 1 {
		t.Errorf("SnapshotDropsTotal{queue_full} delta = %v, want 1", got)
	}
}

// TestRunSnapshotWriterIncrementsDropsOnWriteInFlight verifies that when
// runSnapshotWriter dequeues a snapshot while a previous write goroutine
// is still in-flight, the new snapshot is dropped and
// SnapshotDropsTotal{reason="write_in_flight"} increments. Issue #42.
//
// Test shape mirrors TestRunSnapshotWriterTimeoutContinues: stall the first
// write past the timeout (so writeDone is still non-nil when the next
// snapshot arrives), then enqueue a second snapshot before unblocking the
// first. The writer's `if writeDone != nil` check finds the previous write
// still pending and takes the drop branch.
func TestRunSnapshotWriterIncrementsDropsOnWriteInFlight(t *testing.T) {
	dir := t.TempDir()
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{SpokeTimeout: 5 * time.Minute}, m, nil, filepath.Join(dir, "snap.json"))

	block := make(chan struct{})
	started := make(chan struct{}, 1)
	firstWriteDone := make(chan struct{})
	firstDone := false
	var mu sync.Mutex

	h.snapshotWriteFn = func(_ string, _ snapshot.File) error {
		mu.Lock()
		isFirst := !firstDone
		if isFirst {
			firstDone = true
		}
		mu.Unlock()

		if isFirst {
			select {
			case started <- struct{}{}:
			default:
			}
			<-block
			close(firstWriteDone)
		}
		return nil
	}
	h.snapshotWriteTimeout = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runSnapshotWriter(ctx)

	// First write: enqueue and wait for it to start.
	h.writeSnapshotAsync(discovery.Graph{Devices: []discovery.Device{{ID: "first"}}})
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first write never started")
	}

	// Wait for the writer's outer-select timeout to fire (20ms + margin). The
	// first write goroutine is still blocked on `block`; writeDone is still
	// non-nil from the writer's perspective.
	time.Sleep(100 * time.Millisecond)

	before := testutil.ToFloat64(m.SnapshotDropsTotal.WithLabelValues(
		string(metrics.SnapshotDropReasonWriteInFlight)))

	// Second snapshot arrives while writeDone is still pending — must drop.
	h.writeSnapshotAsync(discovery.Graph{Devices: []discovery.Device{{ID: "second"}}})

	// Give runSnapshotWriter a chance to dequeue + take the drop branch.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		after := testutil.ToFloat64(m.SnapshotDropsTotal.WithLabelValues(
			string(metrics.SnapshotDropReasonWriteInFlight)))
		if after-before >= 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	after := testutil.ToFloat64(m.SnapshotDropsTotal.WithLabelValues(
		string(metrics.SnapshotDropReasonWriteInFlight)))
	if got := after - before; got != 1 {
		t.Errorf("SnapshotDropsTotal{write_in_flight} delta = %v, want 1", got)
	}

	// Unblock the stalled write so the test cleans up without leaking.
	close(block)
	select {
	case <-firstWriteDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled write never unblocked")
	}
}

// TestHubWriteSnapshotNoopWhenPathEmpty verifies that writeSnapshot is a no-op
// when snapshotPath is empty (the normal test configuration).
func TestHubWriteSnapshotNoopWhenPathEmpty(_ *testing.T) {
	h := newTestHub(nil) // snapshotPath = ""
	// Should not panic or error; just return immediately.
	h.writeSnapshot(discovery.Graph{})
}

// TestHubWriteSnapshotErrorDoesNotPanic verifies that writeSnapshot logs the
// error and does not panic or update SnapshotLastWrittenUnix when the snapshot
// directory cannot be created.
func TestHubWriteSnapshotErrorDoesNotPanic(t *testing.T) {
	dir := t.TempDir()
	// Create a file at the location where we'd expect a parent directory so
	// that MkdirAll fails with "not a directory".
	blocker := filepath.Join(dir, "blocker")
	if err := os.WriteFile(blocker, []byte("x"), 0o600); err != nil {
		t.Fatalf("setup: %v", err)
	}

	m := metrics.New(false)
	h := NewHub(config.FederationConfig{}, m, nil, filepath.Join(blocker, "snap.json"))

	h.writeSnapshot(discovery.Graph{})

	if got := testutil.ToFloat64(m.SnapshotLastWrittenUnix); got != 0 {
		t.Errorf("SnapshotLastWrittenUnix = %v after failed write, want 0", got)
	}
}

// TestRunSnapshotWriterWritesSnapshot verifies that runSnapshotWriter drains
// the snapshot channel and invokes snapshotWriteFn when a graph is enqueued
// via writeSnapshotAsync.
func TestRunSnapshotWriterWritesSnapshot(t *testing.T) {
	dir := t.TempDir()
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{SpokeTimeout: 5 * time.Minute}, m, nil, filepath.Join(dir, "snap.json"))

	written := make(chan discovery.Graph, 1)
	h.snapshotWriteFn = func(_ string, f snapshot.File) error {
		written <- discovery.Graph{Devices: f.Devices, Edges: f.Edges}
		return nil
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runSnapshotWriter(ctx)

	g := discovery.Graph{
		Devices: []discovery.Device{{ID: "sw-snap-1"}},
		Edges:   []discovery.Edge{},
	}
	h.writeSnapshotAsync(g)

	select {
	case got := <-written:
		if len(got.Devices) != 1 || got.Devices[0].ID != "sw-snap-1" {
			t.Errorf("snapshot devices = %#v, want [{ID:sw-snap-1}]", got.Devices)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("runSnapshotWriter did not invoke snapshotWriteFn within 2s")
	}
}

// TestRunSnapshotWriterTimeoutContinues verifies the NFS-stall protection inside
// runSnapshotWriter: when snapshotWriteFn blocks beyond snapshotWriteTimeout the
// writer logs a warning and then continues to process the next enqueued graph.
func TestRunSnapshotWriterTimeoutContinues(t *testing.T) {
	dir := t.TempDir()
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{SpokeTimeout: 5 * time.Minute}, m, nil, filepath.Join(dir, "snap.json"))

	// block gates the first (stalling) write; second signals when the second write runs.
	// Using separate closed-over channels (no shared mutable variable) avoids the
	// data race between the stalled goroutine and the second goroutine spawned after
	// the timeout fires.
	block := make(chan struct{})
	started := make(chan struct{}, 1)     // first write signals it has started
	firstWriteDone := make(chan struct{}) // closed when the first (stalled) write completes
	second := make(chan struct{}, 1)      // second write signals completion
	firstDone := false

	var mu sync.Mutex
	h.snapshotWriteFn = func(_ string, _ snapshot.File) error {
		mu.Lock()
		isFirst := !firstDone
		if isFirst {
			firstDone = true
		}
		mu.Unlock()

		if isFirst {
			select {
			case started <- struct{}{}:
			default:
			}
			<-block               // stall until the test unblocks us
			close(firstWriteDone) // signal that the stalled write has completed
			return nil
		}
		close(second)
		return nil
	}
	h.snapshotWriteTimeout = 20 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go h.runSnapshotWriter(ctx)

	// Enqueue first (blocking) write. The channel has capacity 1 so this lands immediately.
	h.writeSnapshotAsync(discovery.Graph{Devices: []discovery.Device{{ID: "stall"}}})

	// Wait for the first write to start before starting the timeout clock.
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("first write never started")
	}

	// Wait for the timeout to fire (20 ms + margin), then unblock the stalled goroutine.
	time.Sleep(100 * time.Millisecond)
	close(block)

	// Wait until the stalled write goroutine has actually finished before sending
	// the second graph. This replaces the racy fixed sleep: runSnapshotWriter
	// checks writeDone on the next dequeue, so we must ensure writeDone is closed
	// first to avoid the "still in flight; dropping" branch.
	select {
	case <-firstWriteDone:
	case <-time.After(2 * time.Second):
		t.Fatal("stalled write never completed")
	}
	h.writeSnapshotAsync(discovery.Graph{Devices: []discovery.Device{{ID: "ok"}}})

	select {
	case <-second:
		// runSnapshotWriter continued to process after the timeout.
	case <-time.After(2 * time.Second):
		t.Fatal("runSnapshotWriter did not process second snapshot after timeout recovery within 2s")
	}
}

// TestRunSnapshotWriterShutdownUnblocksOnTimeout verifies that cancelling ctx
// causes runSnapshotWriter to return within snapshotWriteTimeout even when the
// in-flight snapshot write goroutine is blocked (e.g. NFS stall).
func TestRunSnapshotWriterShutdownUnblocksOnTimeout(t *testing.T) {
	dir := t.TempDir()
	m := metrics.New(false)
	h := NewHub(config.FederationConfig{SpokeTimeout: 5 * time.Minute}, m, nil, filepath.Join(dir, "snap.json"))

	// writeStarted is closed when the blocking write begins; unblock releases it.
	writeStarted := make(chan struct{})
	unblock := make(chan struct{})

	h.snapshotWriteFn = func(_ string, _ snapshot.File) error {
		close(writeStarted)
		<-unblock // block until the test unblocks or the test ends
		return nil
	}
	// Use a short timeout so the test completes quickly (well under the default 30s).
	h.snapshotWriteTimeout = 100 * time.Millisecond

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	defer close(unblock) // ensure the write goroutine is always released

	writerDone := make(chan struct{})
	go func() {
		h.runSnapshotWriter(ctx)
		close(writerDone)
	}()

	// Enqueue a write so runSnapshotWriter starts the blocking goroutine.
	h.writeSnapshotAsync(discovery.Graph{})

	// Wait for the write to actually start before cancelling.
	select {
	case <-writeStarted:
	case <-time.After(2 * time.Second):
		t.Fatal("snapshot write never started")
	}

	// Cancel the context. runSnapshotWriter must return within snapshotWriteTimeout
	// (100 ms) + a small margin even though the write is still blocked.
	cancel()

	deadline := time.After(500 * time.Millisecond)
	select {
	case <-writerDone:
		// runSnapshotWriter exited — correct behaviour.
	case <-deadline:
		t.Fatal("runSnapshotWriter did not exit within 500ms after ctx cancel (shutdown stall)")
	}
}
