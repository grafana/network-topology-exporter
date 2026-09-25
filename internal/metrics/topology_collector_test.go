package metrics

import (
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/limits"
)

// TestTopologyCollectorBoundaryObsEmission covers the emitBoundaryObs=true path
// (lines ~110-118 in topology_collector.go) and both branches of canonicalPair:
// the no-swap path (ReportingDevice < NeighbourHint) and the swap path
// (ReportingDevice > NeighbourHint).
func TestTopologyCollectorBoundaryObsEmission(t *testing.T) {
	m := New(true) // emitBoundaryObs=true

	m.Topology.Update(discovery.Graph{
		OutOfScope: []discovery.OutOfScopeNeighbour{
			// a < b — no swap: peer_a="alpha-device", peer_b="zeta-device"
			{
				ReportingDevice: "alpha-device",
				ReportingPort:   "Gi0/1",
				NeighbourHint:   "zeta-device",
				Proto:           "lldp",
			},
			// a > b — swap triggered: peer_a="alpha-device", peer_b="zeta-device"
			{
				ReportingDevice: "zeta-device",
				ReportingPort:   "Gi0/2",
				NeighbourHint:   "alpha-device",
				Proto:           "cdp",
			},
		},
	})

	const want = `
# HELP network_topology_boundary_observation_info Federation uncoordinated mode: one series per out-of-scope boundary observation. peer_a is always the alphabetically-smaller endpoint. A Mimir recording rule fires count by(peer_a,peer_b,proto)(...)==2 for confirmed cross-boundary edges.
# TYPE network_topology_boundary_observation_info gauge
network_topology_boundary_observation_info{peer_a="alpha-device",peer_b="zeta-device",proto="cdp",reporting_device="zeta-device",src_port="Gi0/2"} 1
network_topology_boundary_observation_info{peer_a="alpha-device",peer_b="zeta-device",proto="lldp",reporting_device="alpha-device",src_port="Gi0/1"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_boundary_observation_info"); err != nil {
		t.Fatalf("boundary obs metric mismatch: %v", err)
	}
}

// TestTopologyCollectorBoundaryObsCaseFold covers the case-fold fix: mixed-case
// ReportingDevice/NeighbourHint values (as LLDP/CDP actually deliver them — see
// docs/proposals/knowledge-graph-boundary-stitching.md §5) must be folded to
// lowercase in the emitted metric, since PromQL has no lower() and a naive
// Mimir recording rule matching this metric directly needs pre-folded values
// to pair mixed-case fleets correctly. This is deliberately NOT the same as
// asserting discovery.OutOfScopeNeighbour itself gets normalised — it doesn't
// (see internal/federation's own collision-detection diagnostic, which needs
// the raw value); only the Prometheus label values emitted here are folded.
func TestTopologyCollectorBoundaryObsCaseFold(t *testing.T) {
	m := New(true) // emitBoundaryObs=true

	m.Topology.Update(discovery.Graph{
		OutOfScope: []discovery.OutOfScopeNeighbour{
			{
				ReportingDevice: "Alpha-Device", // already lowercase in practice (dev.ID), but fold regardless
				ReportingPort:   "Gi0/1",
				NeighbourHint:   "ZETA-DEVICE", // raw LLDP/CDP-decoded case — never folded upstream
				Proto:           "lldp",
			},
		},
	})

	const want = `
# HELP network_topology_boundary_observation_info Federation uncoordinated mode: one series per out-of-scope boundary observation. peer_a is always the alphabetically-smaller endpoint. A Mimir recording rule fires count by(peer_a,peer_b,proto)(...)==2 for confirmed cross-boundary edges.
# TYPE network_topology_boundary_observation_info gauge
network_topology_boundary_observation_info{peer_a="alpha-device",peer_b="zeta-device",proto="lldp",reporting_device="alpha-device",src_port="Gi0/1"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_boundary_observation_info"); err != nil {
		t.Fatalf("boundary obs case-fold mismatch: %v", err)
	}

	// The underlying graph snapshot itself must stay raw/unfolded — federation's
	// hub_merge.go collision-detection diagnostic depends on seeing the actual
	// walker-decoded value, not a pre-folded one.
	g := m.Topology.CurrentGraph()
	if g.OutOfScope[0].NeighbourHint != "ZETA-DEVICE" {
		t.Errorf("NeighbourHint mutated in the graph snapshot: got %q, want unchanged %q",
			g.OutOfScope[0].NeighbourHint, "ZETA-DEVICE")
	}
	if g.OutOfScope[0].ReportingDevice != "Alpha-Device" {
		t.Errorf("ReportingDevice mutated in the graph snapshot: got %q, want unchanged %q",
			g.OutOfScope[0].ReportingDevice, "Alpha-Device")
	}
}

// TestTopologyCollectorCanonicalPairSwap directly exercises canonicalPair since
// the test file is in package metrics (white-box).
func TestTopologyCollectorCanonicalPairSwap(t *testing.T) {
	cases := []struct {
		a, b         string
		wantA, wantB string
	}{
		{"z-device", "a-device", "a-device", "z-device"}, // a > b — swap
		{"a-device", "z-device", "a-device", "z-device"}, // a < b — no swap
		{"same", "same", "same", "same"},                 // a == b — no swap
	}
	for _, tc := range cases {
		gotA, gotB := canonicalPair(tc.a, tc.b)
		if gotA != tc.wantA || gotB != tc.wantB {
			t.Errorf("canonicalPair(%q, %q) = (%q, %q), want (%q, %q)",
				tc.a, tc.b, gotA, gotB, tc.wantA, tc.wantB)
		}
	}
}

// TestTopologyCollectorEmptyGraph ensures Collect does not panic and reports
// oosCount=0 when the graph has nil slices (zero value).
func TestTopologyCollectorEmptyGraph(t *testing.T) {
	for _, emit := range []bool{false, true} {
		m := New(emit)
		m.Topology.Update(discovery.Graph{}) // nil Devices, Edges, OutOfScope

		const want = `
# HELP network_topology_out_of_scope_neighbours_total Count of LLDP/CDP-discovered neighbours whose IP falls outside the configured CIDR allow-list. Detail in log lines.
# TYPE network_topology_out_of_scope_neighbours_total gauge
network_topology_out_of_scope_neighbours_total 0
`
		if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want),
			"network_topology_out_of_scope_neighbours_total"); err != nil {
			t.Fatalf("emitBoundaryObs=%v: %v", emit, err)
		}
	}
}

// TestTopologyCollectorConcurrentUpdateCollect verifies no data races occur
// when Update and Collect run concurrently. The -race flag in go test will
// surface any violations.
func TestTopologyCollectorConcurrentUpdateCollect(t *testing.T) {
	t.Helper()
	m := New(true)

	g := discovery.Graph{
		Devices: []discovery.Device{{ID: "dev-1", Uptime: 5 * time.Second}},
		OutOfScope: []discovery.OutOfScopeNeighbour{
			{ReportingDevice: "dev-1", ReportingPort: "Gi0/1", NeighbourHint: "ext-1", Proto: "lldp"},
		},
	}

	var wg sync.WaitGroup
	stop := make(chan struct{})

	// 5 writers.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
					m.Topology.Update(g)
				}
			}
		}()
	}

	// 5 readers.
	for i := 0; i < 5; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch := make(chan prometheus.Metric, 64)
			for {
				select {
				case <-stop:
					return
				default:
					go func() {
						m.Topology.Collect(ch)
					}()
					// drain to avoid goroutine leak on the send side
					for len(ch) > 0 {
						<-ch
					}
				}
			}
		}()
	}

	time.Sleep(10 * time.Millisecond)
	close(stop)
	wg.Wait()
}

// TestTopologyCollectorEdgeMetric covers the g.Edges loop body in Collect,
// which was the only remaining uncovered statement after the initial test pass.
func TestTopologyCollectorEdgeMetric(t *testing.T) {
	m := New(false)

	m.Topology.Update(discovery.Graph{
		Edges: []discovery.Edge{
			{
				SrcDevice:      "dev-a",
				SrcPort:        "Gi0/1",
				SrcIfIndex:     101,
				DstDevice:      "dev-b",
				DstPort:        "Gi0/2",
				DiscoveryProto: "lldp",
				LinkKind:       "ethernet",
				Direction:      discovery.DirectionBidirectional,
			},
			{
				// BGP never resolves an ifIndex or a port on either side —
				// alongside the LLDP edge above, this covers a scrape with
				// both an ifIndex-bearing and a never-resolves-one edge in
				// the same Collect call.
				SrcDevice:      "dev-c",
				DstDevice:      "192.0.2.1",
				DiscoveryProto: "bgp",
				LinkKind:       "ip",
				Direction:      discovery.DirectionUnidirectional,
			},
		},
	})

	// SrcIfIndex is set (LLDP always resolves the local side); DstIfIndex is
	// left zero, exercising ifIndexLabel's empty-label branch — LLDP never
	// resolves the remote side's ifIndex, so dst_if_index="" is the common case
	// in practice, not just a test default.
	const want = `
# HELP network_topology_edge_info One series per discovered topology edge. Value is always 1. src_if_index/dst_if_index carry the IF-MIB ifIndex for each endpoint when the discovery protocol resolves one (empty otherwise) — the join key against snmp_exporter's ifIndex-keyed if_mib rows.
# TYPE network_topology_edge_info gauge
network_topology_edge_info{direction="bidirectional",discovery_proto="lldp",dst_device="dev-b",dst_if_index="",dst_port="Gi0/2",link_kind="ethernet",src_device="dev-a",src_if_index="101",src_port="Gi0/1"} 1
network_topology_edge_info{direction="unidirectional",discovery_proto="bgp",dst_device="192.0.2.1",dst_if_index="",dst_port="",link_kind="ip",src_device="dev-c",src_if_index="",src_port=""} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_edge_info"); err != nil {
		t.Fatalf("edge metric mismatch: %v", err)
	}
}

// TestTopologyCollectorEdgeMetricRejectsOutOfRangeIfIndex is the
// Collect()-level counterpart to TestIfIndexLabel: a negative or
// over-limits.MaxIfIndex value reaching Collect (e.g. from a discovery
// walker bug) must degrade to the same empty label ifIndexLabel(0) produces,
// never a literal negative or out-of-range number on the wire.
func TestTopologyCollectorEdgeMetricRejectsOutOfRangeIfIndex(t *testing.T) {
	m := New(false)

	m.Topology.Update(discovery.Graph{
		Edges: []discovery.Edge{
			{
				SrcDevice:      "dev-a",
				SrcPort:        "Gi0/1",
				SrcIfIndex:     -1,
				DstDevice:      "dev-b",
				DstPort:        "Gi0/2",
				DstIfIndex:     limits.MaxIfIndex + 1,
				DiscoveryProto: "lldp",
				LinkKind:       "ethernet",
				Direction:      discovery.DirectionBidirectional,
			},
		},
	})

	want := `
# HELP network_topology_edge_info One series per discovered topology edge. Value is always 1. src_if_index/dst_if_index carry the IF-MIB ifIndex for each endpoint when the discovery protocol resolves one (empty otherwise) — the join key against snmp_exporter's ifIndex-keyed if_mib rows.
# TYPE network_topology_edge_info gauge
network_topology_edge_info{direction="bidirectional",discovery_proto="lldp",dst_device="dev-b",dst_if_index="",dst_port="Gi0/2",link_kind="ethernet",src_device="dev-a",src_if_index="",src_port="Gi0/1"} 1
`
	if err := testutil.GatherAndCompare(m.Registry(), strings.NewReader(want), "network_topology_edge_info"); err != nil {
		t.Fatalf("edge metric mismatch: %v", err)
	}
}

// TestIfIndexLabel covers ifIndexLabel directly. Previously only idx==101
// (a normal resolved value) and idx==0 (the unresolved sentinel) were
// exercised, via TestTopologyCollectorEdgeMetric's Collect() call — the
// negative and over-limits.MaxIfIndex cases were never tested on this
// default single-instance path, unlike the equivalent federation hub-ingest
// validator (internal/federation/hub_validate.go), which does check them.
// Collect() can't return an error (it implements prometheus.Collector), so
// out-of-range values degrade to the same "" the unresolved sentinel gets,
// rather than being stringified verbatim into a Prometheus label.
func TestIfIndexLabel(t *testing.T) {
	cases := []struct {
		name string
		idx  int
		want string
	}{
		{"zero (unresolved sentinel)", 0, ""},
		{"typical resolved value", 101, "101"},
		{"negative", -1, ""},
		{"large negative", -101, ""},
		{"at MaxIfIndex boundary", limits.MaxIfIndex, strconv.Itoa(limits.MaxIfIndex)},
		{"one past MaxIfIndex", limits.MaxIfIndex + 1, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := ifIndexLabel(tc.idx); got != tc.want {
				t.Errorf("ifIndexLabel(%d) = %q, want %q", tc.idx, got, tc.want)
			}
		})
	}
}

// TestSanitizeLabel covers the non-printable stripping and truncation branches
// of sanitizeLabel that were not reached by earlier tests.
func TestSanitizeLabel(t *testing.T) {
	cases := []struct {
		name  string
		input string
		want  string
	}{
		{
			name:  "non_printable_runes_stripped",
			input: "\x00hello\x01",
			want:  "hello",
		},
		{
			name:  "exactly_128_bytes_unchanged",
			input: strings.Repeat("a", 128),
			want:  strings.Repeat("a", 128),
		},
		{
			name:  "129_bytes_truncated_to_128",
			input: strings.Repeat("b", 129),
			want:  strings.Repeat("b", 128),
		},
		{
			name:  "empty_string",
			input: "",
			want:  "",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := sanitizeLabel(tc.input)
			if got != tc.want {
				t.Errorf("sanitizeLabel(%q) = %q, want %q", tc.input, got, tc.want)
			}
		})
	}
}

// TestSanitizeLabelUTF8Boundary verifies that sanitizeLabel never produces
// invalid UTF-8 when a multi-byte rune straddles the maxLabelLen (128) byte
// boundary. The input is 127 ASCII bytes followed by a 2-byte UTF-8 rune
// (U+00E9, é), placing the second byte of the rune at position 128 — exactly
// at the truncation point.
func TestSanitizeLabelUTF8Boundary(t *testing.T) {
	// Build a string of 127 ASCII 'a' bytes + U+00E9 (é, 2 bytes in UTF-8).
	// Total: 129 bytes. The rune boundary falls at byte 127, so the safe
	// truncation must produce exactly 127 bytes.
	input := strings.Repeat("a", 127) + "é"
	if len(input) != 129 {
		t.Fatalf("test setup: expected 129-byte input, got %d", len(input))
	}

	got := sanitizeLabel(input)

	if len(got) >= 129 {
		t.Errorf("sanitizeLabel: result too long: %d bytes", len(got))
	}
	if !utf8.ValidString(got) {
		t.Errorf("sanitizeLabel: result is not valid UTF-8: %q", got)
	}
}

// buildSyntheticGraph creates a discovery.Graph with the requested number of
// edges. Device IDs and port names are generated deterministically so the
// benchmark is repeatable and allocation-stable across runs.
func buildSyntheticGraph(numEdges int) discovery.Graph {
	devices := make([]discovery.Device, 0, numEdges+1)
	edges := make([]discovery.Edge, 0, numEdges)

	// One fixed source device.
	devices = append(devices, discovery.Device{
		ID:        "sw-001",
		Vendor:    "cisco",
		Model:     "nexus-9000",
		OSVersion: "9.3(10)",
		Site:      "dc1",
		Uptime:    3600 * time.Second,
	})

	for i := range numEdges {
		dstID := fmt.Sprintf("sw-%03d", i+2)
		devices = append(devices, discovery.Device{
			ID:        dstID,
			Vendor:    "arista",
			Model:     "7050cx3",
			OSVersion: "4.28.0F",
			Site:      "dc1",
			Uptime:    time.Duration(i+1) * time.Second,
		})
		edges = append(edges, discovery.Edge{
			SrcDevice:      "sw-001",
			SrcPort:        fmt.Sprintf("GigabitEthernet0/%d", i%48),
			DstDevice:      dstID,
			DstPort:        "GigabitEthernet0/0",
			DiscoveryProto: "lldp",
			LinkKind:       "ethernet",
			Direction:      discovery.DirectionBidirectional,
		})
	}

	return discovery.Graph{Devices: devices, Edges: edges}
}

func BenchmarkCollect1000Edges(b *testing.B) {
	tc := newTopologyCollector(false, nil, nil)
	tc.Update(buildSyntheticGraph(1000))

	ch := make(chan prometheus.Metric, 4096)
	// Drain goroutine so Collect never blocks on a full channel.
	go func() {
		for range ch {
		}
	}()

	b.ResetTimer()
	for b.Loop() {
		tc.Collect(ch)
	}
	b.StopTimer()
	close(ch)
}

func BenchmarkCollect10000Edges(b *testing.B) {
	tc := newTopologyCollector(false, nil, nil)
	tc.Update(buildSyntheticGraph(10000))

	ch := make(chan prometheus.Metric, 32768)
	go func() {
		for range ch {
		}
	}()

	b.ResetTimer()
	for b.Loop() {
		tc.Collect(ch)
	}
	b.StopTimer()
	close(ch)
}

// TestCardinalityBudget asserts that the total number of Prometheus samples
// emitted by TopologyCollector stays within a per-device budget across three
// scale points (100, 500, and 1000 devices). The budget is 5 samples per
// device, which is deliberately conservative: each device contributes 2
// samples (device_info + device_uptime_seconds) and each edge contributes 1
// sample, plus a handful of scalar metrics. If someone introduces an unbounded
// label on a per-device metric, this test will fail at the 100-device mark.
func TestCardinalityBudget(t *testing.T) {
	for _, numDevices := range []int{100, 500, 1000} {
		numDevices := numDevices
		t.Run(fmt.Sprintf("%d_devices", numDevices), func(t *testing.T) {
			// Build a synthetic graph: numDevices devices, 2 bidirectional edges
			// per adjacent device pair (i→i+1 and i+1→i as separate observations,
			// already collapsed to one bidirectional edge after reconciliation in
			// the graph layer). We construct the graph directly here so the test
			// is self-contained.
			devices := make([]discovery.Device, numDevices)
			for i := range devices {
				devices[i] = discovery.Device{
					ID:        fmt.Sprintf("sw-%04d", i),
					Vendor:    "cisco",
					Model:     "nexus-9000",
					OSVersion: "9.3(10)",
					Site:      "dc1",
					Uptime:    time.Duration(i+1) * time.Second,
				}
			}

			edges := make([]discovery.Edge, numDevices)
			for i := range edges {
				srcIdx := i
				dstIdx := (i + 1) % numDevices
				edges[i] = discovery.Edge{
					SrcDevice:      fmt.Sprintf("sw-%04d", srcIdx),
					SrcPort:        fmt.Sprintf("GigabitEthernet0/%d", i%48),
					DstDevice:      fmt.Sprintf("sw-%04d", dstIdx),
					DstPort:        fmt.Sprintf("GigabitEthernet0/%d", (i+1)%48),
					DiscoveryProto: "lldp",
					LinkKind:       "ethernet",
					Direction:      discovery.DirectionBidirectional,
				}
			}

			g := discovery.Graph{Devices: devices, Edges: edges}

			reg := prometheus.NewRegistry()
			tc := newTopologyCollector(false, nil, nil)
			tc.Update(g)
			if err := reg.Register(tc); err != nil {
				t.Fatalf("register collector: %v", err)
			}

			count, err := testutil.GatherAndCount(reg)
			if err != nil {
				t.Fatalf("gather: %v", err)
			}

			// Budget: 2 samples per device (device_info + device_uptime) +
			// 1 sample per edge (edge_info, ring topology gives ~1 edge/device) +
			// a small constant for scalar metrics. 5×/device is a 60% safety margin
			// over the observed ~3×/device. Reduce if the topology expands new per-device metrics.
			budget := numDevices * 5
			if count > budget {
				t.Errorf("cardinality budget exceeded: %d samples for %d devices (budget %d = %d * 5)",
					count, numDevices, budget, numDevices)
			}
		})
	}
}

// TestTopologyCollectorDescribeAllDescriptors verifies that Describe always
// sends exactly 7 descriptors regardless of the emitBoundaryObs flag.
func TestTopologyCollectorDescribeAllDescriptors(t *testing.T) {
	for _, emit := range []bool{false, true} {
		c := newTopologyCollector(emit, nil, nil)
		ch := make(chan *prometheus.Desc, 16)
		c.Describe(ch)
		close(ch)

		var count int
		for range ch {
			count++
		}
		if count != 7 {
			t.Errorf("emitBoundaryObs=%v: got %d descriptors, want 7", emit, count)
		}
	}
}

func TestCurrentGraphReturnsLatest(t *testing.T) {
	c := newTopologyCollector(false,
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "t_dur"}),
		prometheus.NewGauge(prometheus.GaugeOpts{Name: "t_smp"}))
	if g := c.CurrentGraph(); g == nil {
		t.Fatal("CurrentGraph returned nil before any Update (init stores empty graph)")
	}
	c.Update(discovery.Graph{Devices: []discovery.Device{{ID: "x"}}})
	g := c.CurrentGraph()
	if g == nil || len(g.Devices) != 1 || g.Devices[0].ID != "x" {
		t.Fatalf("CurrentGraph did not return the updated graph: %+v", g)
	}
}
