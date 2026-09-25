package federationhub

// Tests split from hub_test.go (#168); see hub_push.go.
import (
	"bytes"
	"compress/gzip"
	"crypto/tls"
	"crypto/x509"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/limits"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// TestHubHandlePushRejectsBadSpokeID verifies that handlePush returns HTTP 400
// for a spoke_id containing invalid characters or exceeding the length limit.
func TestHubHandlePushRejectsBadSpokeID(t *testing.T) {
	h := newTestHub(nil)

	cases := []struct {
		name    string
		spokeID string
	}{
		{"space in spoke_id", "dc a"},
		{"spoke_id too long", strings.Repeat("a", 129)},
		{"slash in spoke_id", "dc/a"},
		{"at-sign in spoke_id", "dc@a"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payload := federation.SpokePayload{
				SpokeID: tc.spokeID,
				CycleAt: time.Now(),
			}
			body, err := json.Marshal(payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.handlePush(rec, req)
			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400", rec.Code)
			}
		})
	}
}

// TestHubHandlePushSuccessStoresSpokeAndSetsGauges sends a valid payload and
// verifies the spoke is stored and the spoke-up gauge is set to 1.
func TestHubHandlePushSuccessStoresSpokeAndSetsGauges(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{SpokeTimeout: 5 * time.Minute},
		m, nil, "",
	)

	payload := federation.SpokePayload{
		SpokeID: "dc-valid",
		CycleAt: time.Now(),
		Devices: []discovery.Device{{ID: "sw-1"}},
		Edges:   []discovery.Edge{},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	h.mu.Lock()
	entry, ok := h.spokes["dc-valid"]
	h.mu.Unlock()
	if !ok {
		t.Fatal("spoke dc-valid not found in h.spokes after successful push")
	}
	if len(entry.payload.Devices) != 1 {
		t.Errorf("stored device count = %d, want 1", len(entry.payload.Devices))
	}
	if got := testutil.ToFloat64(m.FederationSpokeUp.WithLabelValues("dc-valid")); got != 1 {
		t.Errorf("FederationSpokeUp{dc-valid} = %v, want 1", got)
	}
}

// TestHubHandlePushRejectsBadJSON verifies that a body that cannot be decoded
// as JSON returns HTTP 400.
func TestHubHandlePushRejectsBadJSON(t *testing.T) {
	h := newTestHub(nil)
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", strings.NewReader("{not json"))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for bad JSON", rec.Code)
	}
}

// TestHubHandlePushRejectsMethodNotAllowed verifies that non-POST methods get
// HTTP 405.
func TestHubHandlePushRejectsMethodNotAllowed(t *testing.T) {
	h := newTestHub(nil)
	req := httptest.NewRequest(http.MethodGet, "/spoke/push", nil)
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusMethodNotAllowed {
		t.Errorf("status = %d, want 405", rec.Code)
	}
}

// TestHubHandlePushRejectsStaleCycleAt verifies that a payload whose cycle_at
// is older than the spoke_timeout is rejected with HTTP 400.
func TestHubHandlePushRejectsStaleCycleAt(t *testing.T) {
	h := newTestHub(nil)
	h.cfg.SpokeTimeout = time.Minute

	payload := federation.SpokePayload{
		SpokeID: "dc-stale",
		CycleAt: time.Now().Add(-2 * time.Minute), // older than spoke_timeout
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for stale cycle_at", rec.Code)
	}
}

// TestHubHandlePushMissingCycleAt verifies that a payload with zero CycleAt
// is rejected with HTTP 400.
func TestHubHandlePushMissingCycleAt(t *testing.T) {
	h := newTestHub(nil)

	payload := federation.SpokePayload{SpokeID: "dc-no-time"} // CycleAt is zero
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for missing cycle_at", rec.Code)
	}
}

// TestHubHandlePushRejectsFutureCycleAt verifies that a cycle_at more than
// 5 minutes in the future is rejected with HTTP 400.
func TestHubHandlePushRejectsFutureCycleAt(t *testing.T) {
	h := newTestHub(nil)

	payload := federation.SpokePayload{
		SpokeID: "dc-future",
		CycleAt: time.Now().Add(10 * time.Minute),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for future cycle_at", rec.Code)
	}
}

// TestHubHandlePushRejectsEmptySpokeID verifies that a payload without a
// spoke_id is rejected with HTTP 400.
func TestHubHandlePushRejectsEmptySpokeID(t *testing.T) {
	h := newTestHub(nil)

	payload := federation.SpokePayload{SpokeID: "", CycleAt: time.Now()}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for empty spoke_id", rec.Code)
	}
}

// TestHubHandlePushRejectsCertCNMismatch verifies that handlePush returns
// HTTP 403 when the client certificate's CN does not match payload.SpokeID
// (LD-21: spoke_id must be bound to the presenting mTLS identity).
func TestHubHandlePushRejectsCertCNMismatch(t *testing.T) {
	h := newTestHub(nil)

	// Cert has CN "dc-a"; payload claims spoke_id "dc-b" — mismatch.
	payload := federation.SpokePayload{SpokeID: "dc-b", CycleAt: time.Now()}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{makeCert(t, "dc-a")},
	}
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusForbidden {
		t.Errorf("status = %d, want 403 for cert CN / spoke_id mismatch", rec.Code)
	}
}

// TestHubHandlePushAcceptsCertCNMatch verifies that handlePush accepts a push
// when the client certificate's CN exactly matches payload.SpokeID (LD-21).
func TestHubHandlePushAcceptsCertCNMatch(t *testing.T) {
	h := newTestHub(nil)

	payload := federation.SpokePayload{SpokeID: "dc-match", CycleAt: time.Now()}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.TLS = &tls.ConnectionState{
		PeerCertificates: []*x509.Certificate{makeCert(t, "dc-match")},
	}
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Errorf("status = %d, want 204 for matching cert CN / spoke_id; body: %s",
			rec.Code, rec.Body.String())
	}
}

// TestHubHandlePushRejectsOversizedPayload verifies that a payload exceeding
// the per-push device or edge limits is rejected with HTTP 413.
func TestHubHandlePushRejectsOversizedPayload(t *testing.T) {
	h := newTestHub(nil)

	// Build a device slice just over maxDevicesPerPush.
	devices := make([]discovery.Device, maxDevicesPerPush+1)
	for i := range devices {
		devices[i] = discovery.Device{ID: fmt.Sprintf("sw-%d", i)}
	}
	payload := federation.SpokePayload{
		SpokeID: "dc-big",
		CycleAt: time.Now(),
		Devices: devices,
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal oversized payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for oversized payload", rec.Code)
	}
}

// TestHubHandlePushRejectedGraphDoesNotMarkSpokeUp verifies that when
// publishIfWinner rejects the combined graph (size budget exceeded), the spoke
// is NOT registered in h.spokes and FederationSpokeUp is NOT set to 1.
// This guards against the inconsistency where a spoke appears "up" in Prometheus
// but contributes zero edges to the topology because its graph was rejected.
func TestHubHandlePushRejectedGraphDoesNotMarkSpokeUp(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{
			SpokeTimeout: 5 * time.Minute,
			Hub: config.FederationHubConfig{
				// Set a tight edge budget so the combined graph is rejected.
				// The spoke payload will have more edges than this limit.
				MaxGraphEdges: 1,
			},
		},
		m, nil, "",
	)

	// Build a payload whose edges will exceed MaxGraphEdges after reconciliation.
	payload := federation.SpokePayload{
		SpokeID: "dc-rejected",
		CycleAt: time.Now(),
		Devices: []discovery.Device{
			{ID: "sw-a"},
			{ID: "sw-b"},
			{ID: "sw-c"},
		},
		Edges: []discovery.Edge{
			{
				SrcDevice: "sw-a", SrcPort: "Gi0/1",
				DstDevice: "sw-b", DstPort: "Gi0/2",
				DiscoveryProto: "lldp",
				Direction:      discovery.DirectionBidirectional,
				LinkKind:       "ethernet",
			},
			{
				SrcDevice: "sw-b", SrcPort: "Gi0/3",
				DstDevice: "sw-c", DstPort: "Gi0/4",
				DiscoveryProto: "lldp",
				Direction:      discovery.DirectionBidirectional,
				LinkKind:       "ethernet",
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	// MaxGraphEdges=1 forces the size-budget reject path, which must return
	// 413 Payload Too Large with a machine-parseable JSON body. A 204 here
	// would silently lie to the spoke that its data was accepted.
	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q, want application/json", got)
	}
	var rej pushRejection
	if err := json.Unmarshal(rec.Body.Bytes(), &rej); err != nil {
		t.Fatalf("response body is not valid JSON: %v; body=%s", err, rec.Body.String())
	}
	if rej.Reason != rejectReasonSizeBudgetExceeded {
		t.Errorf("reject reason = %q, want %q", rej.Reason, rejectReasonSizeBudgetExceeded)
	}

	// Spoke must NOT be registered in h.spokes.
	h.mu.Lock()
	_, present := h.spokes["dc-rejected"]
	h.mu.Unlock()
	if present {
		t.Error("spoke dc-rejected should NOT be in h.spokes when graph publish was rejected")
	}

	// FederationSpokeUp must NOT be set to 1.
	if got := testutil.ToFloat64(m.FederationSpokeUp.WithLabelValues("dc-rejected")); got != 0 {
		t.Errorf("FederationSpokeUp{dc-rejected} = %v, want 0 when graph was rejected", got)
	}

	// GraphUpdatesRejectedTotal must have been incremented.
	if got := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(rejectReasonSizeBudgetExceeded))); got != 1 {
		t.Errorf("GraphUpdatesRejectedTotal{reason=size_budget_exceeded} = %v, want 1", got)
	}
}

// TestHubHandlePushRejectedGraphLeavesPreviousEntryIntact verifies that when a
// spoke's push is rejected due to graph size limits, the spoke's PREVIOUS entry
// in h.spokes is left intact (never speculatively overwritten with the new
// payload).
func TestHubHandlePushRejectedGraphLeavesPreviousEntryIntact(t *testing.T) {
	m := metrics.New(false)
	h := NewHub(
		config.FederationConfig{
			SpokeTimeout: 5 * time.Minute,
			Hub:          config.FederationHubConfig{MaxGraphEdges: 1},
		},
		m, nil, "",
	)

	// Seed a prior entry with one device — within the tight edge budget (no edges).
	prior := spokeEntry{
		payload: federation.SpokePayload{
			SpokeID: "dc-rollback",
			Devices: []discovery.Device{{ID: "sw-prior"}},
			Edges:   []discovery.Edge{},
		},
		lastSeen: time.Now().Add(-time.Minute),
	}
	h.mu.Lock()
	h.spokes["dc-rollback"] = prior
	h.mu.Unlock()

	// Push a new payload that will exceed MaxGraphEdges.
	payload := federation.SpokePayload{
		SpokeID: "dc-rollback",
		CycleAt: time.Now(),
		Devices: []discovery.Device{{ID: "sw-a"}, {ID: "sw-b"}, {ID: "sw-c"}},
		Edges: []discovery.Edge{
			{
				SrcDevice: "sw-a", SrcPort: "Gi0/1",
				DstDevice: "sw-b", DstPort: "Gi0/2",
				DiscoveryProto: "lldp",
				Direction:      discovery.DirectionBidirectional,
				LinkKind:       "ethernet",
			},
			{
				SrcDevice: "sw-b", SrcPort: "Gi0/3",
				DstDevice: "sw-c", DstPort: "Gi0/4",
				DiscoveryProto: "lldp",
				Direction:      discovery.DirectionBidirectional,
				LinkKind:       "ethernet",
			},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want 413; body: %s", rec.Code, rec.Body.String())
	}

	// The spoke entry should have been rolled back to the prior payload.
	h.mu.Lock()
	entry, ok := h.spokes["dc-rollback"]
	h.mu.Unlock()

	if !ok {
		t.Fatal("spoke dc-rollback should still be in h.spokes with the prior entry after rollback")
	}
	if len(entry.payload.Devices) != 1 || entry.payload.Devices[0].ID != "sw-prior" {
		t.Errorf("h.spokes[dc-rollback].payload.Devices = %v, want prior entry [{sw-prior}]",
			entry.payload.Devices)
	}
}

// TestHandlePushRejectedLeavesSpokesUntouched pins #147 defect #1: a push whose
// combined graph is rejected (size budget) must leave h.spokes unchanged and
// FederationSpokeUp unset — i.e. the entry is never speculatively written.
func TestHandlePushRejectedLeavesSpokesUntouched(t *testing.T) {
	h := NewHub(config.FederationConfig{
		SpokeTimeout: time.Hour,
		Hub:          config.FederationHubConfig{MaxGraphDevices: 1},
	}, metrics.New(false), nil, "")

	body, _ := json.Marshal(federation.SpokePayload{
		SpokeID: "dc-x",
		CycleAt: time.Now(),
		Devices: []discovery.Device{{ID: "d1"}, {ID: "d2"}}, // 2 > MaxGraphDevices=1
	})
	req := httptest.NewRequest(http.MethodPost, "/federation/push", bytes.NewReader(body))
	rec := httptest.NewRecorder()
	h.handlePush(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("status: got %d, want 413", rec.Code)
	}
	h.mu.Lock()
	_, present := h.spokes["dc-x"]
	h.mu.Unlock()
	if present {
		t.Fatal("rejected push left a spoke entry in h.spokes (speculative write not eliminated)")
	}
	if got := testutil.ToFloat64(h.m.FederationSpokeUp.WithLabelValues("dc-x")); got != 0 {
		t.Fatalf("FederationSpokeUp{dc-x}=%v after reject, want 0", got)
	}
}

// TestHandlePushConcurrentDifferentSpokes drives many concurrent real handlePush
// requests for distinct spokes through the actual handler. Run with -race.
func TestHandlePushConcurrentDifferentSpokes(t *testing.T) {
	h := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, "")
	const n = 32
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			id := fmt.Sprintf("dc-%02d", i)
			body, _ := json.Marshal(federation.SpokePayload{
				SpokeID: id, CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: id + "-r1"}},
			})
			req := httptest.NewRequest(http.MethodPost, "/federation/push", bytes.NewReader(body))
			h.handlePush(httptest.NewRecorder(), req)
		}(i)
	}
	wg.Wait()
	// Concurrent pushes are arbitrated by generation: a push that loses the race
	// is cleanly dropped (no commit, 409) and re-pushed next cycle, so not all n
	// register in a single burst. The invariant the #147 fix guarantees is
	// consistency — a spoke is in h.spokes iff its liveness gauge is set.
	registered := 0
	for i := 0; i < n; i++ {
		id := fmt.Sprintf("dc-%02d", i)
		h.mu.Lock()
		_, present := h.spokes[id]
		h.mu.Unlock()
		gaugeUp := testutil.ToFloat64(h.m.FederationSpokeUp.WithLabelValues(id)) == 1
		if present != gaugeUp {
			t.Fatalf("%s: h.spokes present=%v but gauge up=%v (atomic-commit invariant violated)", id, present, gaugeUp)
		}
		if present {
			registered++
		}
	}
	if registered == 0 {
		t.Fatal("no spokes registered from concurrent pushes — expected at least one winner")
	}
}

// TestHandlePushEvictionRaceInvariant probes the #147 F1 fold-into-lock fix: the
// FederationSpokeUp gauge and h.spokes membership must stay consistent
// (present iff gauge==1) across concurrent accept/evict. Even iterations race a
// fresh push against eviction (push wins -> present); odd iterations evict an
// aged entry alone (-> absent). Both terminal states must occur, so the
// invariant is asserted in BOTH directions. Run under -race.
func TestHandlePushEvictionRaceInvariant(t *testing.T) {
	h := NewHub(config.FederationConfig{SpokeTimeout: time.Hour}, metrics.New(false), nil, "")
	const id = "dc-race"
	sawPresent, sawAbsent := false, false
	for iter := 0; iter < 200; iter++ {
		// Seed an aged entry + gauge so eviction is eligible to delete it.
		h.mu.Lock()
		h.spokes[id] = spokeEntry{payload: federation.SpokePayload{SpokeID: id}, lastSeen: time.Now().Add(-2 * time.Hour)}
		h.mu.Unlock()
		h.m.FederationSpokeUp.WithLabelValues(id).Set(1)

		race := iter%2 == 0
		var wg sync.WaitGroup
		if race {
			wg.Add(1)
			go func() {
				defer wg.Done()
				body, _ := json.Marshal(federation.SpokePayload{SpokeID: id, CycleAt: time.Now(),
					Devices: []discovery.Device{{ID: "r1"}}})
				h.handlePush(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/p", bytes.NewReader(body)))
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); h.evictSilentSpokes() }()
		wg.Wait()

		h.mu.Lock()
		_, present := h.spokes[id]
		h.mu.Unlock()
		gauge := testutil.ToFloat64(h.m.FederationSpokeUp.WithLabelValues(id))
		if present != (gauge == 1) {
			t.Fatalf("iter %d (race=%v): invariant violated: present=%v gauge=%v", iter, race, present, gauge)
		}
		if present {
			sawPresent = true
		} else {
			sawAbsent = true
		}
	}
	if !sawPresent || !sawAbsent {
		t.Fatalf("test did not exercise both terminal states: sawPresent=%v sawAbsent=%v", sawPresent, sawAbsent)
	}
}

// TestHubHandlePushRejectsLabelInjection verifies the on-the-wire contract:
// a payload with an injected label key/value is rejected by handlePush with
// HTTP 400, Content-Type: application/json, and a body whose `reason` field
// is the documented enum value. This is the surface spokes branch on; the
// counter increment is checked here so a regression in the wiring (forgetting
// to call h.m.GraphUpdatesRejectedTotal.Inc()) is caught.
func TestHubHandlePushRejectsLabelInjection(t *testing.T) {
	cases := []struct {
		name       string
		payload    federation.SpokePayload
		wantReason metrics.RejectReason
	}{
		{
			name: "label key with newline returns invalid_label_key",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"bad\nkey": "v"},
				}},
			},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name: "label key with reserved __ prefix returns invalid_label_key",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"__reserved": "v"},
				}},
			},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name: "label value with NUL returns invalid_label_value",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"k": "v\x00bad"},
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "edge port with newline returns invalid_label_value",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "Gi0/1\ninject",
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New(false)
			h := NewHub(config.FederationConfig{SpokeTimeout: time.Minute}, m, nil, "")

			before := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(tc.wantReason)))
			body, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.handlePush(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var resp pushRejection
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode rejection body: %v; raw=%s", err, rec.Body.String())
			}
			if resp.Status != "rejected" {
				t.Errorf("status field = %q, want \"rejected\"", resp.Status)
			}
			if resp.Reason != tc.wantReason {
				t.Errorf("reason = %q, want %q", resp.Reason, tc.wantReason)
			}
			if after := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(tc.wantReason))); after != before+1 {
				t.Errorf("GraphUpdatesRejectedTotal{reason=%s} delta = %v, want 1", tc.wantReason, after-before)
			}
		})
	}
}

// TestHubHandlePushRejectsStructuralInvalid verifies that paths previously
// returning plain fmt.Errorf — empty device_id, self-edge, empty edge
// endpoints, oversize port name, invalid UTF-8 device_id — now reach the
// wire as a structured pushRejection JSON body with
// `reason: "structural_invalid"` and a 400 status, and that the
// GraphUpdatesRejectedTotal counter increments under the correct label.
//
// Before issue #19 these requests received a plain text/plain 400 and
// (incorrectly) incremented the invalid_label_value counter via the
// defensive fallthrough in handlePush. The fallthrough is now a panic
// guarding the *validationError invariant; this test is the wire-level
// proof that every converted path carries its new typed reason.
func TestHubHandlePushRejectsStructuralInvalid(t *testing.T) {
	cases := []struct {
		name    string
		payload federation.SpokePayload
	}{
		{
			name: "empty device_id",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: ""}},
			},
		},
		{
			name: "duplicate device_id",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-1"}},
			},
		},
		{
			name: "self-edge",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: "sw-1"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "Gi0/1",
					DstDevice: "sw-1", DstPort: "Gi0/2",
				}},
			},
		},
		{
			name: "empty edge src_device",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "", SrcPort: "Gi0/1",
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
		},
		{
			name: "oversize src_port",
			payload: federation.SpokePayload{
				SpokeID: "dc-a",
				CycleAt: time.Now(),
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: strings.Repeat("p", limits.MaxPortNameBytes+1),
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
		},
		// Note: invalid-UTF-8 device_id cannot be exercised via json.Marshal
		// here because encoding/json replaces invalid UTF-8 bytes with U+FFFD
		// during marshal. That path is covered at the function level in
		// TestValidateSpokePayload ("invalid UTF-8 device ID") and at the
		// typed-reason level in TestValidateSpokePayloadStructuralTypedReason
		// below.
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			m := metrics.New(false)
			h := NewHub(config.FederationConfig{SpokeTimeout: time.Minute}, m, nil, "")

			before := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(rejectReasonStructuralInvalid)))
			body, err := json.Marshal(tc.payload)
			if err != nil {
				t.Fatalf("marshal: %v", err)
			}
			req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
			rec := httptest.NewRecorder()
			h.handlePush(rec, req)

			if rec.Code != http.StatusBadRequest {
				t.Errorf("status = %d, want 400; body=%s", rec.Code, rec.Body.String())
			}
			if ct := rec.Header().Get("Content-Type"); ct != "application/json" {
				t.Errorf("Content-Type = %q, want application/json", ct)
			}
			var resp pushRejection
			if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
				t.Fatalf("decode rejection body: %v; raw=%s", err, rec.Body.String())
			}
			if resp.Status != "rejected" {
				t.Errorf("status field = %q, want \"rejected\"", resp.Status)
			}
			if resp.Reason != rejectReasonStructuralInvalid {
				t.Errorf("reason = %q, want %q", resp.Reason, rejectReasonStructuralInvalid)
			}
			if after := testutil.ToFloat64(m.GraphUpdatesRejectedTotal.WithLabelValues(string(rejectReasonStructuralInvalid))); after != before+1 {
				t.Errorf("GraphUpdatesRejectedTotal{reason=%s} delta = %v, want 1", rejectReasonStructuralInvalid, after-before)
			}
		})
	}
}

// gzipBytes compresses b for the Content-Encoding wire tests below.
func gzipBytes(t *testing.T, b []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(b); err != nil {
		t.Fatalf("gzip write: %v", err)
	}
	if err := zw.Close(); err != nil {
		t.Fatalf("gzip close: %v", err)
	}
	return buf.Bytes()
}

// TestHubHandlePushAcceptsGzipBody verifies the hub decompresses and accepts
// a gzip-encoded push (the spoke default since the compression change).
func TestHubHandlePushAcceptsGzipBody(t *testing.T) {
	h := newTestHub(nil)
	payload := federation.SpokePayload{
		SpokeID: "dc-gzip",
		CycleAt: time.Now(),
		Devices: []discovery.Device{{ID: "sw-1"}},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(gzipBytes(t, body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want 204; body: %s", rec.Code, rec.Body.String())
	}
	h.mu.Lock()
	_, ok := h.spokes["dc-gzip"]
	h.mu.Unlock()
	if !ok {
		t.Fatal("spoke dc-gzip not stored after gzip push")
	}
}

// TestHubHandlePushRejectsMalformedGzip verifies that a body advertising gzip
// but containing garbage returns 400, not a 500 or hang.
func TestHubHandlePushRejectsMalformedGzip(t *testing.T) {
	h := newTestHub(nil)
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", strings.NewReader("definitely not gzip"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusBadRequest {
		t.Errorf("status = %d, want 400 for malformed gzip", rec.Code)
	}
}

// TestHubHandlePushRejectsUnsupportedEncoding verifies that an unknown
// Content-Encoding returns 415 rather than feeding compressed bytes to the
// JSON decoder.
func TestHubHandlePushRejectsUnsupportedEncoding(t *testing.T) {
	h := newTestHub(nil)
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", strings.NewReader("{}"))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "br")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusUnsupportedMediaType {
		t.Errorf("status = %d, want 415 for unsupported encoding", rec.Code)
	}
}

// TestHubHandlePushRejectsGzipBomb verifies the decompressed-side cap: a
// small wire body that inflates past maxPushPayloadBytes must 413, not OOM
// the hub. ~33 MiB of repeated spaces compresses to ~33 KiB on the wire.
func TestHubHandlePushRejectsGzipBomb(t *testing.T) {
	h := newTestHub(nil)
	// Leading whitespace is consumed by the JSON decoder before any token,
	// so inflation is driven past the cap without allocating a giant value.
	bomb := append(bytes.Repeat([]byte{' '}, maxPushPayloadBytes+1024), []byte(`{"spoke_id":"dc-bomb"}`)...)
	req := httptest.NewRequest(http.MethodPost, "/spoke/push", bytes.NewReader(gzipBytes(t, bomb)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Content-Encoding", "gzip")
	rec := httptest.NewRecorder()

	h.handlePush(rec, req)

	if rec.Code != http.StatusRequestEntityTooLarge {
		t.Errorf("status = %d, want 413 for gzip bomb; body: %s", rec.Code, rec.Body.String())
	}
}
