package metrics

import (
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/limits"
	"github.com/grafana/network-topology-exporter/internal/sanitize"
)

const maxLabelLen = 128

// ifIndexLabel stringifies an ifIndex for a label value, or returns "" for
// the unresolved case (0) — mirrors how SrcPort/DstPort already degrade to
// empty. See docs/proposals/snmp-exporter-label-alignment.md §4.
//
// This is the default single-instance path: discovery.Edge.SrcIfIndex/
// DstIfIndex arrive here straight from the local LLDP/CDP/FDB/ISIS walkers
// (internal/discovery/...), not through the federation hub-ingest
// validator (internal/federation/hub_validate.go), which only runs for
// pushed spoke payloads. Collect cannot return an error — it implements
// prometheus.Collector — so a value a well-behaved walker should never
// produce (negative, or absurdly large) is degraded to the same ""
// "unresolved" label a real never-resolves case gets, rather than being
// stringified verbatim into a Prometheus label or panicking the scrape.
// limits.MaxIfIndex is the same bound the hub-ingest and snapshot-load
// paths enforce (internal/limits), so all three ifIndex validation sites
// agree on what "in range" means.
func ifIndexLabel(idx int) string {
	if idx <= 0 || idx > limits.MaxIfIndex {
		return ""
	}
	return strconv.Itoa(idx)
}

func sanitizeLabel(s string) string {
	s = strings.Map(func(r rune) rune {
		if unicode.IsPrint(r) {
			return r
		}
		return -1
	}, s)
	// Retreat to a UTF-8 rune boundary (RFC 3629) so we never emit an
	// invalid label value to Prometheus.
	return sanitize.TruncateAtRuneBoundary(s, maxLabelLen)
}

// TopologyCollector implements prometheus.Collector. It holds an atomic
// pointer to the current discovery.Graph and generates ConstMetrics at
// scrape time, eliminating the Reset()+repopulate race inherent in GaugeVec.
// Concurrent Collect calls are safe: each reads the same immutable snapshot.
type TopologyCollector struct {
	snap            atomic.Pointer[discovery.Graph]
	emitBoundaryObs bool

	deviceInfoDesc   *prometheus.Desc
	deviceUptimeDesc *prometheus.Desc
	edgeInfoDesc     *prometheus.Desc
	oosCountDesc     *prometheus.Desc
	boundaryObsDesc  *prometheus.Desc
	graphEdgesDesc   *prometheus.Desc
	graphDevicesDesc *prometheus.Desc

	scrapeDuration prometheus.Gauge
	scrapeSamples  prometheus.Gauge
}

func newTopologyCollector(emitBoundaryObs bool, scrapeDuration, scrapeSamples prometheus.Gauge) *TopologyCollector {
	c := &TopologyCollector{
		emitBoundaryObs: emitBoundaryObs,
		scrapeDuration:  scrapeDuration,
		scrapeSamples:   scrapeSamples,
		deviceInfoDesc: prometheus.NewDesc(
			"network_topology_device_info",
			"One series per discovered device. Value is always 1; inventory data is in the labels. "+
				"sys_name is the case-preserving sysName, added for joining against Alloy/snmp_exporter's "+
				"own sysName label — see docs/proposals/snmp-exporter-label-alignment.md.",
			[]string{"device_id", "vendor", "model", "os_version", "site", "sys_name"},
			nil,
		),
		deviceUptimeDesc: prometheus.NewDesc(
			"network_topology_device_uptime_seconds",
			"Per-device uptime from the SNMP SYSTEM group (sysUpTime).",
			[]string{"device_id"},
			nil,
		),
		edgeInfoDesc: prometheus.NewDesc(
			"network_topology_edge_info",
			"One series per discovered topology edge. Value is always 1. src_if_index/dst_if_index carry "+
				"the IF-MIB ifIndex for each endpoint when the discovery protocol resolves one (empty otherwise) — "+
				"the join key against snmp_exporter's ifIndex-keyed if_mib rows.",
			[]string{"src_device", "src_port", "src_if_index", "dst_device", "dst_port", "dst_if_index", "discovery_proto", "link_kind", "direction"},
			nil,
		),
		oosCountDesc: prometheus.NewDesc(
			"network_topology_out_of_scope_neighbours_total",
			"Count of LLDP/CDP-discovered neighbours whose IP falls outside the configured CIDR allow-list. Detail in log lines.",
			nil,
			nil,
		),
		boundaryObsDesc: prometheus.NewDesc(
			"network_topology_boundary_observation_info",
			"Federation uncoordinated mode: one series per out-of-scope boundary observation. "+
				"peer_a is always the alphabetically-smaller endpoint. "+
				"A Mimir recording rule fires count by(peer_a,peer_b,proto)(...)==2 for confirmed cross-boundary edges.",
			[]string{"peer_a", "peer_b", "reporting_device", "src_port", "proto"},
			nil,
		),
		graphEdgesDesc: prometheus.NewDesc(
			"network_topology_graph_edges_total",
			"Current number of reconciled edges in the active topology graph.",
			nil, nil,
		),
		graphDevicesDesc: prometheus.NewDesc(
			"network_topology_graph_devices_total",
			"Current number of devices in the active topology graph.",
			nil, nil,
		),
	}
	empty := discovery.Graph{}
	c.snap.Store(&empty)
	return c
}

// Update atomically swaps the graph snapshot. The next Collect call reads
// the new graph with no empty-window gap.
func (c *TopologyCollector) Update(g discovery.Graph) {
	c.snap.Store(&g)
}

// CurrentGraph returns the current immutable graph snapshot (the same one
// Collect reads). Never nil: the constructor stores an empty graph at init.
// The returned pointer is safe to read concurrently — the graph is replaced,
// never mutated in place.
func (c *TopologyCollector) CurrentGraph() *discovery.Graph {
	return c.snap.Load()
}

// Describe sends all metric descriptors to ch.
func (c *TopologyCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- c.deviceInfoDesc
	ch <- c.deviceUptimeDesc
	ch <- c.edgeInfoDesc
	ch <- c.oosCountDesc
	ch <- c.boundaryObsDesc
	ch <- c.graphEdgesDesc
	ch <- c.graphDevicesDesc
}

// Collect generates metrics from the current snapshot. May be called
// concurrently by the Prometheus HTTP handler; safe because snap is an
// atomic pointer and ConstMetrics are immutable.
func (c *TopologyCollector) Collect(ch chan<- prometheus.Metric) {
	start := time.Now()
	g := c.snap.Load()

	samples := 0
	for _, d := range g.Devices {
		ch <- prometheus.MustNewConstMetric(
			c.deviceInfoDesc, prometheus.GaugeValue, 1,
			sanitizeLabel(d.ID), sanitizeLabel(d.Vendor), sanitizeLabel(d.Model),
			sanitizeLabel(d.OSVersion), sanitizeLabel(d.Site), sanitizeLabel(d.SysName),
		)
		ch <- prometheus.MustNewConstMetric(
			c.deviceUptimeDesc, prometheus.GaugeValue, d.Uptime.Seconds(),
			sanitizeLabel(d.ID),
		)
		samples += 2
	}

	for _, e := range g.Edges {
		ch <- prometheus.MustNewConstMetric(
			c.edgeInfoDesc, prometheus.GaugeValue, 1,
			sanitizeLabel(e.SrcDevice), sanitizeLabel(e.SrcPort), ifIndexLabel(e.SrcIfIndex),
			sanitizeLabel(e.DstDevice), sanitizeLabel(e.DstPort), ifIndexLabel(e.DstIfIndex),
			sanitizeLabel(string(e.DiscoveryProto)), sanitizeLabel(string(e.LinkKind)), string(e.Direction),
		)
		samples++
	}

	ch <- prometheus.MustNewConstMetric(
		c.oosCountDesc, prometheus.GaugeValue, float64(len(g.OutOfScope)),
	)
	samples++

	if c.emitBoundaryObs {
		for _, n := range g.OutOfScope {
			// Fold reporting_device/peer_a/peer_b to lowercase here only — not in
			// discovery.OutOfScopeNeighbour or the federation wire payload, which
			// keep whatever case the walker produced. LLDP's chassis-ID fallback
			// path is the one hint that's genuinely un-normalised at this point;
			// hub_merge.go's collision diagnostic needs to keep seeing that raw
			// value, so the fold can't happen upstream. See LD-15 in
			// docs/architecture.md.
			reportingDevice := strings.ToLower(n.ReportingDevice)
			neighbourHint := strings.ToLower(n.NeighbourHint)
			peerA, peerB := canonicalPair(
				sanitizeLabel(reportingDevice),
				sanitizeLabel(neighbourHint),
			)
			ch <- prometheus.MustNewConstMetric(
				c.boundaryObsDesc, prometheus.GaugeValue, 1,
				peerA, peerB,
				sanitizeLabel(reportingDevice), sanitizeLabel(n.ReportingPort),
				sanitizeLabel(n.Proto),
			)
			samples++
		}
	}

	ch <- prometheus.MustNewConstMetric(c.graphEdgesDesc, prometheus.GaugeValue, float64(len(g.Edges)))
	ch <- prometheus.MustNewConstMetric(c.graphDevicesDesc, prometheus.GaugeValue, float64(len(g.Devices)))
	samples += 2

	if c.scrapeDuration != nil {
		c.scrapeDuration.Set(time.Since(start).Seconds())
	}
	if c.scrapeSamples != nil {
		c.scrapeSamples.Set(float64(samples))
	}
}

// canonicalPair returns (a, b) with the alphabetically-smaller value first.
// Used by LD-15 boundary observations so the Mimir recording rule matches
// from either side with a stable canonical pair ordering.
func canonicalPair(a, b string) (string, string) {
	if a <= b {
		return a, b
	}
	return b, a
}
