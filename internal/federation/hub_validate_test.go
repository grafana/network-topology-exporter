package federation

// Tests split from hub_test.go (#168); see hub_validate.go.
import (
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/limits"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// TestValidateSpokePayload covers the semantic validation rules enforced by
// validateSpokePayload: empty/overlong/invalid-UTF-8/duplicate device IDs,
// required edge fields, self-edges, and overlong/invalid-UTF-8 port names.
func TestValidateSpokePayload(t *testing.T) {
	validDevice := discovery.Device{ID: "sw-1"}
	validEdge := discovery.Edge{
		SrcDevice: "sw-1", SrcPort: "Gi0/1",
		DstDevice: "sw-2", DstPort: "Gi0/2",
	}

	cases := []struct {
		name    string
		payload SpokePayload
		wantErr bool
	}{
		{
			name: "empty device ID",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: ""}},
			},
			wantErr: true,
		},
		{
			name: "overlong device ID (257 bytes)",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: strings.Repeat("a", 257)}},
			},
			wantErr: true,
		},
		{
			name: "invalid UTF-8 device ID",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "\xff\xfe"}},
			},
			wantErr: true,
		},
		{
			name: "duplicate device IDs",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-dup"}, {ID: "sw-dup"}},
			},
			wantErr: true,
		},
		{
			name: "empty src_device",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "", SrcPort: "Gi0/1", DstDevice: "sw-2"},
				},
			},
			wantErr: true,
		},
		{
			name: "empty src_port",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "sw-1", SrcPort: "", DstDevice: "sw-2"},
				},
			},
			wantErr: true,
		},
		{
			name: "empty dst_device",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "sw-1", SrcPort: "Gi0/1", DstDevice: ""},
				},
			},
			wantErr: true,
		},
		{
			name: "self-edge (src_device == dst_device)",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "sw-1", SrcPort: "Gi0/1", DstDevice: "sw-1", DstPort: "Gi0/2"},
				},
			},
			wantErr: true,
		},
		{
			name: "negative src_if_index",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "sw-1", SrcPort: "Gi0/1", SrcIfIndex: -1, DstDevice: "sw-2", DstPort: "Gi0/2"},
				},
			},
			wantErr: true,
		},
		{
			name: "overlong src_port",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "sw-1", SrcPort: strings.Repeat("p", 257), DstDevice: "sw-2"},
				},
			},
			wantErr: true,
		},
		{
			name: "valid minimal payload (one device, one valid edge)",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges:   []discovery.Edge{validEdge},
			},
			wantErr: false,
		},
		{
			name: "valid payload with empty DstPort (DstPort is optional)",
			payload: SpokePayload{
				Devices: []discovery.Device{validDevice},
				Edges: []discovery.Edge{
					{SrcDevice: "sw-1", SrcPort: "Gi0/1", DstDevice: "sw-2", DstPort: ""},
				},
			},
			wantErr: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpokePayload(tc.payload)
			if tc.wantErr && err == nil {
				t.Error("validateSpokePayload() = nil, want error")
			}
			if !tc.wantErr && err != nil {
				t.Errorf("validateSpokePayload() = %v, want nil", err)
			}
		})
	}
}

// TestValidateSpokePayloadRejectsEmptyLabelKey verifies that validateSpokePayload
// returns an error when a Device's Labels map contains an empty string key.
// An empty label key would produce an invalid Prometheus label at emit time.
func TestValidateSpokePayloadRejectsEmptyLabelKey(t *testing.T) {
	payload := SpokePayload{
		Devices: []discovery.Device{
			{
				ID:     "sw-1",
				Labels: map[string]string{"": "value"},
			},
		},
	}
	err := validateSpokePayload(payload)
	if err == nil {
		t.Error("validateSpokePayload() = nil, want error for empty label key")
	}
}

// TestValidateSpokePayloadRejectsLabelInjection covers the Prometheus
// line-protocol injection vectors enumerated in the issue: label keys that
// violate the Prometheus label-name grammar or use the reserved `__` prefix,
// and label values that contain control characters which would corrupt
// /metrics output on every subsequent scrape. mTLS authenticates the spoke
// identity; this validation is the only barrier against a spoke (compromised
// or buggy) pushing data that breaks the hub's exposition format.
//
// Each rejecting case asserts the typed *validationError surface so callers
// can route the reject through the structured pushRejection JSON response;
// the wire-level check that the reason actually reaches the spoke lives in
// TestHubHandlePushRejectsLabelInjection below.
func TestValidateSpokePayloadRejectsLabelInjection(t *testing.T) {
	deviceWithLabel := func(k, v string) discovery.Device {
		return discovery.Device{
			ID:     "sw-1",
			Labels: map[string]string{k: v},
		}
	}

	cases := []struct {
		name       string
		payload    SpokePayload
		wantReason metrics.RejectReason
	}{
		{
			name:       "device label key with newline",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("bad\nkey", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key with control char (tab)",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("bad\tkey", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key with NUL byte",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("bad\x00key", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key reserved double-underscore prefix",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("__name", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key with space",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("bad key", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key with double-quote",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel(`bad"key`, "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key with colon",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("bad:key", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key starting with digit",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("9bad", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label key with hyphen",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("bad-key", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "device label value with newline",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("k", "v\ninjected")}},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name:       "device label value with NUL byte",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("k", "v\x00injected")}},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name:       "device label value with carriage return",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("k", "v\rinjected")}},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name:       "device label value with control char (DEL)",
			payload:    SpokePayload{Devices: []discovery.Device{deviceWithLabel("k", "v\x7fbad")}},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "device vendor field with newline",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1", Vendor: "Cisco\nrogue"}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "device sys_name field with newline",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1", SysName: "Sw-1\nrogue"}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "edge src_port with newline",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "Gi0/1\ninjected",
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "edge dst_port with NUL byte",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "Gi0/1",
					DstDevice: "sw-2", DstPort: "Gi0/2\x00",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "edge discovery_proto with control char",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "Gi0/1",
					DstDevice: "sw-2", DstPort: "Gi0/2",
					DiscoveryProto: "lldp\nfake",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "oos reporting_device with newline",
			payload: SpokePayload{
				OutOfScope: []discovery.OutOfScopeNeighbour{{
					ReportingDevice: "sw-a\ninjected",
					ReportingPort:   "Gi0/1",
					NeighbourHint:   "sw-b",
					Proto:           "lldp",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "oos neighbour_hint with NUL byte",
			payload: SpokePayload{
				OutOfScope: []discovery.OutOfScopeNeighbour{{
					ReportingDevice: "sw-a",
					ReportingPort:   "Gi0/1",
					NeighbourHint:   "sw-b\x00",
					Proto:           "lldp",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name: "oos proto with newline",
			payload: SpokePayload{
				OutOfScope: []discovery.OutOfScopeNeighbour{{
					ReportingDevice: "sw-a",
					ReportingPort:   "Gi0/1",
					NeighbourHint:   "sw-b",
					Proto:           "lldp\nrogue",
				}},
			},
			wantReason: rejectReasonInvalidLabelValue,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpokePayload(tc.payload)
			if err == nil {
				t.Fatal("validateSpokePayload() = nil, want validationError")
			}
			var verr *validationError
			if !errors.As(err, &verr) {
				t.Fatalf("error type = %T, want *validationError: %v", err, err)
			}
			if verr.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q (msg: %s)", verr.reason, tc.wantReason, verr.msg)
			}
		})
	}
}

// TestValidateSpokePayloadRejectsEdgeMetadataInjection mirrors the
// Device.Labels coverage in TestValidateSpokePayloadRejectsLabelInjection
// for Edge.Metadata, the map[string]string field that flows into
// /metrics labels on the same wire as Device.Labels. Filed as issue #25:
// the original D26 validation hardening (commit d4439a3) covered Labels
// but missed Metadata, leaving a parallel injection vector.
func TestValidateSpokePayloadRejectsEdgeMetadataInjection(t *testing.T) {
	edgeWithMetadata := func(k, v string) discovery.Edge {
		return discovery.Edge{
			SrcDevice: "sw-1", SrcPort: "Gi0/1",
			DstDevice: "sw-2", DstPort: "Gi0/2",
			Metadata: map[string]string{k: v},
		}
	}

	cases := []struct {
		name       string
		payload    SpokePayload
		wantReason metrics.RejectReason
	}{
		{
			name:       "metadata key with newline",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata("bad\nkey", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "metadata key with NUL",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata("bad\x00key", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "metadata key exceeds size cap",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata(strings.Repeat("k", 257), "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "metadata key empty",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata("", "v")}},
			wantReason: rejectReasonInvalidLabelKey,
		},
		{
			name:       "metadata value with newline",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata("k", "good\nbad")}},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name:       "metadata value with NUL",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata("k", "good\x00bad")}},
			wantReason: rejectReasonInvalidLabelValue,
		},
		{
			name:       "metadata value exceeds size cap",
			payload:    SpokePayload{Edges: []discovery.Edge{edgeWithMetadata("k", strings.Repeat("a", 4097))}},
			wantReason: rejectReasonInvalidLabelValue,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpokePayload(tc.payload)
			if err == nil {
				t.Fatalf("expected rejection for %q, got nil", tc.name)
			}
			var verr *validationError
			if !errors.As(err, &verr) {
				t.Fatalf("expected *validationError, got %T: %v", err, err)
			}
			if verr.reason != tc.wantReason {
				t.Errorf("reason = %q, want %q (msg=%q)", verr.reason, tc.wantReason, verr.msg)
			}
		})
	}
}

// TestValidateSpokePayloadAcceptsValidEdgeMetadata mirrors
// TestValidateSpokePayloadAcceptsValidLabels for Edge.Metadata so the new
// validation doesn't over-fit and reject legitimate metadata values that
// the discovery layer already populates (e.g. bgp.remote_as,
// peer_chassis_mac per internal/discovery/discovery.go).
func TestValidateSpokePayloadAcceptsValidEdgeMetadata(t *testing.T) {
	edge := discovery.Edge{
		SrcDevice: "sw-1", SrcPort: "Gi0/1",
		DstDevice: "sw-2", DstPort: "Gi0/2",
		Metadata: map[string]string{
			"bgp.remote_as":    "65001",
			"peer_chassis_mac": "aa:bb:cc:dd:ee:ff",
			"degraded":         "true",
			"degraded_reason":  "optional_table_missing",
			"normal_with_utf8": "São Paulo",
		},
	}
	if err := validateSpokePayload(SpokePayload{Edges: []discovery.Edge{edge}}); err != nil {
		t.Errorf("expected accept for valid metadata, got: %v", err)
	}
}

// TestValidateSpokePayloadAcceptsValidLabels verifies that the validation
// hardening did not over-fit: the allowed shape of Prometheus label names
// (ASCII letter/underscore start, then alnum/underscore) and any UTF-8
// non-control label value remain accepted. These are the cases an operator
// will hit in production after enabling per-target enrichment labels.
func TestValidateSpokePayloadAcceptsValidLabels(t *testing.T) {
	cases := []struct {
		name    string
		payload SpokePayload
	}{
		{
			name: "label key with single underscore prefix",
			payload: SpokePayload{
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"_internal": "ok"},
				}},
			},
		},
		{
			name: "label key snake_case",
			payload: SpokePayload{
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"datacenter_region": "us-east-1"},
				}},
			},
		},
		{
			name: "label key with trailing digits",
			payload: SpokePayload{
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"tier3": "edge"},
				}},
			},
		},
		{
			name: "label value with allowed UTF-8 (non-ASCII, no controls)",
			payload: SpokePayload{
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"site": "São Paulo"},
				}},
			},
		},
		{
			name: "label value containing quotes and backslashes (escaped at emit time)",
			payload: SpokePayload{
				Devices: []discovery.Device{{
					ID:     "sw-1",
					Labels: map[string]string{"note": `contains "quotes" and \backslash`},
				}},
			},
		},
		{
			name: "valid vendor and site inventory fields",
			payload: SpokePayload{
				Devices: []discovery.Device{{
					ID: "sw-1", Vendor: "Cisco", Model: "Catalyst-9300",
					OSVersion: "17.6.4", Site: "dc-a",
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSpokePayload(tc.payload); err != nil {
				t.Errorf("validateSpokePayload() = %v, want nil", err)
			}
		})
	}
}

// TestValidateSpokePayloadStructuralTypedReason verifies that each shape-
// validation path in validateSpokePayload returns a *validationError tagged
// with rejectReasonStructuralInvalid. These paths used to return plain
// fmt.Errorf and were silently mislabeled as invalid_label_value by the
// handlePush defensive fallthrough (issue #19). Function-level coverage
// complements the wire-level TestHubHandlePushRejectsStructuralInvalid and
// includes the invalid-UTF-8 device_id case which cannot be exercised
// through json.Marshal (encoding/json scrubs invalid UTF-8 to U+FFFD).
func TestValidateSpokePayloadStructuralTypedReason(t *testing.T) {
	cases := []struct {
		name    string
		payload SpokePayload
	}{
		{
			name:    "empty device_id",
			payload: SpokePayload{Devices: []discovery.Device{{ID: ""}}},
		},
		{
			name:    "oversize device_id",
			payload: SpokePayload{Devices: []discovery.Device{{ID: strings.Repeat("a", limits.MaxDeviceIDBytes+1)}}},
		},
		{
			name:    "invalid utf-8 device_id",
			payload: SpokePayload{Devices: []discovery.Device{{ID: "\xff\xfe"}}},
		},
		{
			name:    "duplicate device_id",
			payload: SpokePayload{Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-1"}}},
		},
		{
			name: "self-edge",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "Gi0/1",
					DstDevice: "sw-1", DstPort: "Gi0/2",
				}},
			},
		},
		{
			name: "empty edge src_device",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "", SrcPort: "Gi0/1",
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
		},
		{
			name: "oversize edge src_port",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: strings.Repeat("p", limits.MaxPortNameBytes+1),
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
		},
		{
			name: "invalid utf-8 edge src_port",
			payload: SpokePayload{
				Devices: []discovery.Device{{ID: "sw-1"}, {ID: "sw-2"}},
				Edges: []discovery.Edge{{
					SrcDevice: "sw-1", SrcPort: "\xff\xfe",
					DstDevice: "sw-2", DstPort: "Gi0/2",
				}},
			},
		},
		{
			name: "oversize OOS reporting_device",
			payload: SpokePayload{
				OutOfScope: []discovery.OutOfScopeNeighbour{{
					ReportingDevice: strings.Repeat("d", limits.MaxPortNameBytes+1),
				}},
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := validateSpokePayload(tc.payload)
			if err == nil {
				t.Fatal("validateSpokePayload() = nil, want *validationError")
			}
			var verr *validationError
			if !errors.As(err, &verr) {
				t.Fatalf("error type = %T, want *validationError", err)
			}
			if verr.reason != rejectReasonStructuralInvalid {
				t.Errorf("reason = %q, want %q", verr.reason, rejectReasonStructuralInvalid)
			}
		})
	}
}

// TestValidateSpokePayloadRejectsEmptyLabelKeyTypedReason confirms that the
// pre-existing empty-key case (covered by TestValidateSpokePayloadRejectsEmptyLabelKey
// for the err != nil surface) now carries the structured invalid_label_key
// reject reason. Belt-and-suspenders: a contract regression would let an
// empty key escape with a generic 400 and break dashboards that branch on
// the reason enum.
func TestValidateSpokePayloadRejectsEmptyLabelKeyTypedReason(t *testing.T) {
	payload := SpokePayload{
		Devices: []discovery.Device{{
			ID:     "sw-1",
			Labels: map[string]string{"": "value"},
		}},
	}
	err := validateSpokePayload(payload)
	if err == nil {
		t.Fatal("validateSpokePayload() = nil, want validationError for empty label key")
	}
	var verr *validationError
	if !errors.As(err, &verr) {
		t.Fatalf("error type = %T, want *validationError", err)
	}
	if verr.reason != rejectReasonInvalidLabelKey {
		t.Errorf("reason = %q, want %q", verr.reason, rejectReasonInvalidLabelKey)
	}
}

// TestValidateLabelKeyRejectsOversized verifies the size cap added to
// validateLabelKey short-circuits before the regex / reserved-prefix checks
// when a key exceeds limits.MaxLabelKeyBytes. The cap is exclusive: a 256-byte key
// is the largest accepted value; 257 bytes rejects with invalid_label_key.
// Mitigates CPU-DoS via a 16 MiB label key on an mTLS-authenticated spoke
// push (issue #14).
func TestValidateLabelKeyRejectsOversized(t *testing.T) {
	oversized := strings.Repeat("a", limits.MaxLabelKeyBytes+1)
	err := validateLabelKey(oversized)
	if err == nil {
		t.Fatalf("validateLabelKey(%d bytes) = nil, want validationError", len(oversized))
	}
	var verr *validationError
	if !errors.As(err, &verr) {
		t.Fatalf("error type = %T, want *validationError", err)
	}
	if verr.reason != rejectReasonInvalidLabelKey {
		t.Errorf("reason = %q, want %q", verr.reason, rejectReasonInvalidLabelKey)
	}
}

// TestValidateLabelKeyAcceptsBoundary verifies the cap is exclusive (>),
// not inclusive (>=): a key of exactly limits.MaxLabelKeyBytes is accepted. Uses
// only valid label-key runes so the only possible reject path is the size
// cap itself.
func TestValidateLabelKeyAcceptsBoundary(t *testing.T) {
	boundary := strings.Repeat("a", limits.MaxLabelKeyBytes)
	if err := validateLabelKey(boundary); err != nil {
		t.Errorf("validateLabelKey(%d bytes) = %v, want nil", len(boundary), err)
	}
}

// TestValidateLabelValueRejectsOversized verifies the size cap added to
// validateLabelValue short-circuits before the per-rune control-char loop
// when a value exceeds limits.MaxLabelValueBytes. A 4097-byte value rejects with
// invalid_label_value; mitigates the ~4M-rune-iteration vector described in
// issue #14.
func TestValidateLabelValueRejectsOversized(t *testing.T) {
	oversized := strings.Repeat("a", limits.MaxLabelValueBytes+1)
	err := validateLabelValue(oversized)
	if err == nil {
		t.Fatalf("validateLabelValue(%d bytes) = nil, want validationError", len(oversized))
	}
	var verr *validationError
	if !errors.As(err, &verr) {
		t.Fatalf("error type = %T, want *validationError", err)
	}
	if verr.reason != rejectReasonInvalidLabelValue {
		t.Errorf("reason = %q, want %q", verr.reason, rejectReasonInvalidLabelValue)
	}
}

// TestValidateLabelValueAcceptsBoundary verifies the cap is exclusive (>),
// not inclusive (>=): a value of exactly limits.MaxLabelValueBytes is accepted.
// Uses only printable ASCII so the only possible reject path is the size
// cap itself.
func TestValidateLabelValueAcceptsBoundary(t *testing.T) {
	boundary := strings.Repeat("a", limits.MaxLabelValueBytes)
	if err := validateLabelValue(boundary); err != nil {
		t.Errorf("validateLabelValue(%d bytes) = %v, want nil", len(boundary), err)
	}
}

// Ensure unused import is compiled away by the test binary.
var _ = os.DevNull
