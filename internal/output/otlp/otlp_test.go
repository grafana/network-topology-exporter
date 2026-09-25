package otlp

import (
	"context"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	otellog "go.opentelemetry.io/otel/log"
	sdklog "go.opentelemetry.io/otel/sdk/log"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/graph"
	"github.com/grafana/network-topology-exporter/internal/otelx"
)

// collectMetrics drains the ManualReader and returns metric name → slice of
// per-data-point attribute maps (string-valued attributes only, which is all
// this exporter emits).
func collectMetrics(t *testing.T, reader metricdataReader) map[string][]map[string]string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := make(map[string][]map[string]string)
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			g, ok := m.Data.(metricdata.Gauge[float64])
			if !ok {
				t.Fatalf("metric %q is %T, want Gauge[float64]", m.Name, m.Data)
			}
			points := make([]map[string]string, 0, len(g.DataPoints))
			for _, dp := range g.DataPoints {
				attrs := make(map[string]string)
				for _, kv := range dp.Attributes.ToSlice() {
					attrs[string(kv.Key)] = kv.Value.String()
				}
				points = append(points, attrs)
			}
			out[m.Name] = points
		}
	}
	return out
}

// resourceAttrs collects the resource attributes attached to the metric stream.
func resourceAttrs(t *testing.T, reader metricdataReader) map[string]string {
	t.Helper()
	var rm metricdata.ResourceMetrics
	if err := reader.Collect(context.Background(), &rm); err != nil {
		t.Fatalf("collect: %v", err)
	}
	out := make(map[string]string)
	if rm.Resource != nil {
		for _, kv := range rm.Resource.Attributes() {
			out[string(kv.Key)] = kv.Value.String()
		}
	}
	return out
}

// metricdataReader is the subset of *sdkmetric.ManualReader the tests use.
type metricdataReader interface {
	Collect(context.Context, *metricdata.ResourceMetrics) error
}

// logAttrs flattens a log record's attributes into a map.
func logAttrs(r sdklog.Record) map[string]string {
	out := make(map[string]string)
	r.WalkAttributes(func(kv otellog.KeyValue) bool {
		out[kv.Key] = kv.Value.AsString()
		return true
	})
	return out
}

// ── PushGraph: two edges + two devices ───────────────────────────────────────

func TestPushGraphTwoEdgesTwoDevices(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	g := discovery.Graph{
		Edges: []discovery.Edge{
			{SrcDevice: "sw-a", SrcPort: "Gi0/1", DstDevice: "sw-b", DstPort: "Gi0/2", DiscoveryProto: "lldp", LinkKind: "ethernet"},
			{SrcDevice: "sw-c", SrcPort: "Eth1", DstDevice: "sw-d", DstPort: "Eth2", DiscoveryProto: "cdp", LinkKind: "ethernet"},
		},
		Devices: []discovery.Device{
			{ID: "sw-a"},
			{ID: "sw-b"},
		},
	}

	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	metrics := collectMetrics(t, reader)

	edgePoints, ok := metrics["network_topology_edge_info"]
	if !ok {
		t.Fatal("metric network_topology_edge_info not found")
	}
	if len(edgePoints) != 2 {
		t.Fatalf("edge data points = %d, want 2", len(edgePoints))
	}
	var foundEdge bool
	for _, pt := range edgePoints {
		if pt["src_device"] == "sw-a" && pt["dst_device"] == "sw-b" {
			foundEdge = true
			if pt["proto"] != "lldp" {
				t.Errorf("proto = %q, want lldp", pt["proto"])
			}
			if pt["link_kind"] != "ethernet" {
				t.Errorf("link_kind = %q, want ethernet", pt["link_kind"])
			}
		}
	}
	if !foundEdge {
		t.Error("sw-a→sw-b edge data point not found")
	}

	devicePoints, ok := metrics["network_topology_device_info"]
	if !ok {
		t.Fatal("metric network_topology_device_info not found")
	}
	if len(devicePoints) != 2 {
		t.Fatalf("device data points = %d, want 2", len(devicePoints))
	}
	deviceIDs := make(map[string]bool)
	for _, pt := range devicePoints {
		deviceIDs[pt["device_id"]] = true
	}
	for _, want := range []string{"sw-a", "sw-b"} {
		if !deviceIDs[want] {
			t.Errorf("device %q not found in data points", want)
		}
	}
}

// ── PushGraph: empty graph emits no data points but does not error ───────────

func TestPushGraphEmpty(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	if err := exp.PushGraph(context.Background(), discovery.Graph{}); err != nil {
		t.Fatalf("PushGraph empty: %v", err)
	}
	metrics := collectMetrics(t, reader)
	for _, name := range []string{"network_topology_edge_info", "network_topology_device_info"} {
		if pts := metrics[name]; len(pts) != 0 {
			t.Errorf("metric %q = %d data points, want 0 for empty graph", name, len(pts))
		}
	}
}

// ── PushChanges: Added → SeverityInfo ────────────────────────────────────────

func TestPushChangesAdded(t *testing.T) {
	exp, _, logExp := newTestExporter("")

	after := &discovery.Edge{
		SrcDevice: "sw-a", SrcPort: "Gi0/1",
		DstDevice: "sw-b", DstPort: "Gi0/2",
		DiscoveryProto: "lldp",
		ObservedAt:     time.Now(),
	}
	changes := []graph.EdgeChange{{Kind: graph.ChangeAdded, After: after}}

	if err := exp.PushChanges(context.Background(), changes); err != nil {
		t.Fatalf("PushChanges: %v", err)
	}

	recs := logExp.all()
	if len(recs) != 1 {
		t.Fatalf("log records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Severity() != otellog.SeverityInfo {
		t.Errorf("severity = %v, want Info (9)", rec.Severity())
	}
	if !strings.Contains(rec.Body().AsString(), "topology change: added") {
		t.Errorf("body = %q, want to contain %q", rec.Body().AsString(), "topology change: added")
	}
	if attrs := logAttrs(rec); attrs["change_kind"] != "added" {
		t.Errorf("change_kind = %q, want added", attrs["change_kind"])
	}
}

// ── PushChanges: Removed → SeverityWarn ──────────────────────────────────────

func TestPushChangesRemoved(t *testing.T) {
	exp, _, logExp := newTestExporter("")

	before := &discovery.Edge{
		SrcDevice: "sw-x", SrcPort: "eth0",
		DstDevice: "sw-y", DstPort: "eth1",
		DiscoveryProto: "cdp",
		ObservedAt:     time.Now(),
	}
	changes := []graph.EdgeChange{{Kind: graph.ChangeRemoved, Before: before}}

	if err := exp.PushChanges(context.Background(), changes); err != nil {
		t.Fatalf("PushChanges: %v", err)
	}

	recs := logExp.all()
	if len(recs) != 1 {
		t.Fatalf("log records = %d, want 1", len(recs))
	}
	rec := recs[0]
	if rec.Severity() != otellog.SeverityWarn {
		t.Errorf("severity = %v, want Warn (13)", rec.Severity())
	}
	if rec.SeverityText() != "WARN" {
		t.Errorf("severityText = %q, want WARN", rec.SeverityText())
	}
}

// ── Resource attributes carry service identity ───────────────────────────────

func TestServiceResourceAttributes(t *testing.T) {
	exp, reader, _ := newTestExporter("spoke-1")

	if err := exp.PushGraph(context.Background(), discovery.Graph{Devices: []discovery.Device{{ID: "d"}}}); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	attrs := resourceAttrs(t, reader)
	if attrs["service.name"] != otelx.ServiceName {
		t.Errorf("service.name = %q, want %q", attrs["service.name"], otelx.ServiceName)
	}
	if _, ok := attrs["service.version"]; !ok {
		t.Error("service.version attribute missing from resource")
	}
	if attrs["service.instance.id"] != "spoke-1" {
		t.Errorf("service.instance.id = %q, want spoke-1", attrs["service.instance.id"])
	}
}

// ── Per-edge metadata is prefixed and emitted ────────────────────────────────

func TestPushGraphEdgeMetadata(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	g := discovery.Graph{
		Edges: []discovery.Edge{
			{
				SrcDevice: "sw-a", SrcPort: "Gi0/1",
				DstDevice: "sw-b", DstPort: "Gi0/2",
				DiscoveryProto: "lldp", LinkKind: "ethernet",
				Metadata: map[string]string{"vlan": "100", "speed": "1G"},
			},
		},
	}

	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	edgePoints := collectMetrics(t, reader)["network_topology_edge_info"]
	if len(edgePoints) != 1 {
		t.Fatalf("expected 1 edge data point, got %d", len(edgePoints))
	}
	pt := edgePoints[0]
	if pt["network.topology.vlan"] != "100" {
		t.Errorf("network.topology.vlan = %q, want 100", pt["network.topology.vlan"])
	}
	if pt["network.topology.speed"] != "1G" {
		t.Errorf("network.topology.speed = %q, want 1G", pt["network.topology.speed"])
	}
}

// ── Edge reconciliation attributes (LD-10) ───────────────────────────────────

func TestPushGraphEdgeAttributes(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	g := discovery.Graph{
		Edges: []discovery.Edge{
			{
				SrcDevice: "sw-a", SrcPort: "Gi0/1", SrcIfIndex: 101,
				DstDevice: "sw-b", DstPort: "Gi0/2", // DstIfIndex left zero: LLDP never resolves the remote side
				DiscoveryProto: "lldp", LinkKind: "ethernet",
				Direction:      discovery.DirectionBidirectional,
				Confidence:     discovery.ConfidenceHigh,
				Adjacency:      discovery.AdjacencyDirect,
				PrecedenceRank: 1,
			},
		},
	}

	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	edgePoints := collectMetrics(t, reader)["network_topology_edge_info"]
	if len(edgePoints) != 1 {
		t.Fatalf("expected 1 edge data point, got %d", len(edgePoints))
	}
	pt := edgePoints[0]
	for attr, want := range map[string]string{
		"direction": "bidirectional", "confidence": "high",
		"adjacency": "direct", "precedence_rank": "1",
		"src_if_index": "101",
	} {
		if pt[attr] != want {
			t.Errorf("attribute %q = %q, want %q", attr, pt[attr], want)
		}
	}
	if _, ok := pt["dst_if_index"]; ok {
		t.Errorf("dst_if_index present with value %q, want omitted (unresolved, DstIfIndex==0)", pt["dst_if_index"])
	}
}

// ── Device attributes present and omit-empty ─────────────────────────────────

func TestPushGraphDeviceAttributes(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	g := discovery.Graph{
		Devices: []discovery.Device{
			{ID: "rtr-1", SysName: "Rtr-1", Vendor: "Cisco", Model: "ASR1001", OSVersion: "16.9", Site: "dc1"},
		},
	}
	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	devicePoints := collectMetrics(t, reader)["network_topology_device_info"]
	if len(devicePoints) != 1 {
		t.Fatalf("expected 1 device data point, got %d", len(devicePoints))
	}
	pt := devicePoints[0]
	for attr, want := range map[string]string{
		"device_id": "rtr-1", "sys_name": "Rtr-1", "vendor": "Cisco", "model": "ASR1001",
		"os_version": "16.9", "site": "dc1",
	} {
		if pt[attr] != want {
			t.Errorf("attribute %q = %q, want %q", attr, pt[attr], want)
		}
	}
}

// TestPushGraphDeviceAttributesDualEmitsDeprecatedDevice locks in the
// deprecation-window dual-emit behaviour: the deprecated "device" attribute
// must still be emitted, with the same value as "device_id", for at least
// one full release per docs/operator/stability.md's deprecation policy.
// Removing this without a matching CHANGELOG "Removed" entry and a major
// version bump would silently break any consumer still reading "device".
func TestPushGraphDeviceAttributesDualEmitsDeprecatedDevice(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	g := discovery.Graph{
		Devices: []discovery.Device{{ID: "rtr-1"}},
	}
	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	devicePoints := collectMetrics(t, reader)["network_topology_device_info"]
	if len(devicePoints) != 1 {
		t.Fatalf("expected 1 device data point, got %d", len(devicePoints))
	}
	pt := devicePoints[0]
	if pt["device"] != "rtr-1" {
		t.Errorf(`deprecated "device" attribute = %q, want %q`, pt["device"], "rtr-1")
	}
	if pt["device_id"] != "rtr-1" {
		t.Errorf(`"device_id" attribute = %q, want %q`, pt["device_id"], "rtr-1")
	}
}

func TestPushGraphDeviceAttributesOmitEmpty(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	g := discovery.Graph{Devices: []discovery.Device{{ID: "sw-plain"}}}
	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph: %v", err)
	}

	devicePoints := collectMetrics(t, reader)["network_topology_device_info"]
	if len(devicePoints) != 1 {
		t.Fatalf("expected 1 device data point, got %d", len(devicePoints))
	}
	pt := devicePoints[0]
	for _, absent := range []string{"sys_name", "vendor", "model", "os_version", "site"} {
		if _, exists := pt[absent]; exists {
			t.Errorf("attribute %q should be absent for empty device", absent)
		}
	}
}

// ── Change timestamp falls back to time.Now() ────────────────────────────────

func TestPushChangesNoBeforeNoAfterTimestamp(t *testing.T) {
	exp, _, logExp := newTestExporter("")

	changes := []graph.EdgeChange{{Kind: graph.ChangeAdded}}
	if err := exp.PushChanges(context.Background(), changes); err != nil {
		t.Fatalf("PushChanges: %v", err)
	}
	recs := logExp.all()
	if len(recs) != 1 {
		t.Fatalf("log records = %d, want 1", len(recs))
	}
	if recs[0].Timestamp().IsZero() {
		t.Error("timestamp is zero; want the time.Now() fallback")
	}
}

// ── Invalid UTF-8 is sanitized in metric attributes ──────────────────────────

func TestPushGraphInvalidUTF8(t *testing.T) {
	exp, reader, _ := newTestExporter("")

	badSrc := "sw-\xffa"
	badDst := "\xfe\xffsw-b"
	badPort := "Gi0/\xff"
	badDevice := "core-\xff\xfe"

	g := discovery.Graph{
		Edges: []discovery.Edge{
			{
				SrcDevice: badSrc, SrcPort: badPort, DstDevice: badDst, DstPort: "Gi0/2",
				DiscoveryProto: "lldp", LinkKind: "ethernet",
			},
		},
		Devices: []discovery.Device{{ID: badDevice}},
	}
	if err := exp.PushGraph(context.Background(), g); err != nil {
		t.Fatalf("PushGraph with invalid UTF-8: %v", err)
	}

	metrics := collectMetrics(t, reader)
	edgePoints := metrics["network_topology_edge_info"]
	if len(edgePoints) != 1 {
		t.Fatalf("expected 1 edge data point, got %d", len(edgePoints))
	}
	pt := edgePoints[0]
	for key, raw := range map[string]string{"src_device": badSrc, "src_port": badPort, "dst_device": badDst} {
		got := pt[key]
		if got == raw {
			t.Errorf("attribute %q not sanitized: got %q (same as input)", key, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("attribute %q still invalid UTF-8: %q", key, got)
		}
		if !strings.Contains(got, "�") {
			t.Errorf("attribute %q expected replacement char, got %q", key, got)
		}
	}
	devicePoints := metrics["network_topology_device_info"]
	if len(devicePoints) != 1 {
		t.Fatalf("expected 1 device data point, got %d", len(devicePoints))
	}
	devID := devicePoints[0]["device_id"]
	if devID == badDevice {
		t.Errorf("device ID not sanitized: got %q", devID)
	}
	if !utf8.ValidString(devID) {
		t.Errorf("device ID still invalid UTF-8: %q", devID)
	}
}

// ── Invalid UTF-8 is sanitized in log attributes ─────────────────────────────

func TestPushChangesInvalidUTF8(t *testing.T) {
	exp, _, logExp := newTestExporter("")

	badSrc := "sw-\xff"
	badDst := "\xfesw-b"
	after := &discovery.Edge{
		SrcDevice: badSrc, SrcPort: "Gi0/1", DstDevice: badDst, DstPort: "Gi0/2",
		DiscoveryProto: "lldp", ObservedAt: time.Now(),
	}
	changes := []graph.EdgeChange{{Kind: graph.ChangeAdded, After: after}}
	if err := exp.PushChanges(context.Background(), changes); err != nil {
		t.Fatalf("PushChanges with invalid UTF-8: %v", err)
	}

	recs := logExp.all()
	if len(recs) != 1 {
		t.Fatalf("log records = %d, want 1", len(recs))
	}
	attrs := logAttrs(recs[0])
	for key, raw := range map[string]string{"src_device": badSrc, "dst_device": badDst} {
		got, ok := attrs[key]
		if !ok {
			t.Errorf("log attribute %q missing", key)
			continue
		}
		if got == raw {
			t.Errorf("log attribute %q not sanitized: got %q", key, got)
		}
		if !utf8.ValidString(got) {
			t.Errorf("log attribute %q still invalid UTF-8: %q", key, got)
		}
	}
}

// ── Config defaults: backward compatibility ──────────────────────────────────

// TestNewDefaultsHTTPProtobuf proves an existing deployment that only sets
// Endpoint still builds successfully and defaults to OTLP/HTTP + protobuf — the
// pre-SDK behaviour. This is the backward-compatibility guarantee for #82.
func TestNewDefaultsHTTPProtobuf(t *testing.T) {
	exp, err := New(context.Background(), Config{Endpoint: "http://127.0.0.1:4318"})
	if err != nil {
		t.Fatalf("New with only Endpoint set: %v", err)
	}
	defer func() { _ = exp.Shutdown(context.Background()) }()
	// No push is performed (no live collector); construction succeeding with
	// only Endpoint set is the contract under test.
}

// TestNewRejectsUnknownProtocol guards the protocol switch.
func TestNewRejectsUnknownProtocol(t *testing.T) {
	if _, err := New(context.Background(), Config{Endpoint: "x", Protocol: "ftp"}); err == nil {
		t.Fatal("New with Protocol=ftp: want error, got nil")
	}
}

// TestNewGRPCConstructs proves the gRPC protocol path builds (no live
// collector is contacted at construction time).
func TestNewGRPCConstructs(t *testing.T) {
	exp, err := New(context.Background(), Config{Endpoint: "localhost:4317", Protocol: otelx.ProtocolGRPC})
	if err != nil {
		t.Fatalf("New gRPC: %v", err)
	}
	// Bound shutdown: there is no live collector, so a flush would otherwise
	// block on the gRPC dial/retry until its own deadline.
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	_ = exp.Shutdown(ctx)
}

// TestPushGraphDropsStaleEdges proves the observable-gauge fix for the
// cumulative-temporality phantom-edge flaw: pushing a graph with fewer edges
// than the previous push must NOT leave the removed edge reported at the
// receiver. The old synchronous Float64Gauge retained every recorded
// attribute-set and would fail this test.
func TestPushGraphDropsStaleEdges(t *testing.T) {
	exp, reader, _ := newTestExporter("")
	ctx := context.Background()

	two := discovery.Graph{Edges: []discovery.Edge{
		{SrcDevice: "a", SrcPort: "1", DstDevice: "b", DstPort: "2", DiscoveryProto: "lldp", LinkKind: "ethernet"},
		{SrcDevice: "c", SrcPort: "3", DstDevice: "d", DstPort: "4", DiscoveryProto: "lldp", LinkKind: "ethernet"},
	}}
	if err := exp.PushGraph(ctx, two); err != nil {
		t.Fatalf("PushGraph(two): %v", err)
	}
	if pts := collectMetrics(t, reader)["network_topology_edge_info"]; len(pts) != 2 {
		t.Fatalf("first push: edge points = %d, want 2", len(pts))
	}

	// Remove the c→d edge.
	one := discovery.Graph{Edges: []discovery.Edge{
		{SrcDevice: "a", SrcPort: "1", DstDevice: "b", DstPort: "2", DiscoveryProto: "lldp", LinkKind: "ethernet"},
	}}
	if err := exp.PushGraph(ctx, one); err != nil {
		t.Fatalf("PushGraph(one): %v", err)
	}
	pts := collectMetrics(t, reader)["network_topology_edge_info"]
	if len(pts) != 1 {
		t.Fatalf("after edge removal: edge points = %d, want 1 (the removed edge must not persist)", len(pts))
	}
	if pts[0]["src_device"] != "a" {
		t.Errorf("surviving edge src_device = %q, want a", pts[0]["src_device"])
	}
}
