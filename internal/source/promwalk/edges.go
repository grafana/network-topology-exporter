// Package promwalk maps Alloy / snmp_exporter topology metric families onto
// discovery.Edge. Neighbor tables stay a walk result — this is not a PromQL
// extract from Mimir.
package promwalk

import (
	"net"
	"strconv"
	"strings"
	"time"

	dto "github.com/prometheus/client_model/go"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	snmputil "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
)

const (
	lldpPrecedence = 2
	cdpPrecedence  = 3
	bgpPrecedence  = 7
)

// Sample is one labeled point after Alloy enrichment (OTLP attributes or
// Prometheus text labels).
type Sample struct {
	Name   string
	Labels map[string]string
	Value  float64
}

// SamplesFromFamilies flattens prometheus/common metric families.
func SamplesFromFamilies(fams map[string]*dto.MetricFamily) []Sample {
	var out []Sample
	for name, fam := range fams {
		if fam == nil {
			continue
		}
		for _, m := range fam.Metric {
			if m == nil {
				continue
			}
			labels := map[string]string{}
			for _, lp := range m.Label {
				if lp == nil || lp.Name == nil || lp.Value == nil {
					continue
				}
				labels[*lp.Name] = *lp.Value
			}
			var val float64
			switch {
			case m.Gauge != nil && m.Gauge.Value != nil:
				val = *m.Gauge.Value
			case m.Untyped != nil && m.Untyped.Value != nil:
				val = *m.Untyped.Value
			case m.Counter != nil && m.Counter.Value != nil:
				val = *m.Counter.Value
			}
			out = append(out, Sample{Name: name, Labels: labels, Value: val})
		}
	}
	return out
}

// HarvestIPAliases copies device_name → IP mappings from cold-style labels
// (ip_addr / ipAdEntAddr) into ipToID. Safe no-op when ipToID is nil.
func HarvestIPAliases(samples []Sample, ipToID map[string]string) {
	if ipToID == nil {
		return
	}
	for _, s := range samples {
		id := deviceName(s.Labels)
		if id == "" {
			continue
		}
		for _, k := range []string{"ip_addr", "ipAdEntAddr", "ipv4_address"} {
			ip := strings.TrimSpace(s.Labels[k])
			if net.ParseIP(ip) == nil {
				continue
			}
			if _, ok := ipToID[ip]; !ok {
				ipToID[ip] = id
			}
		}
	}
}

// EdgesFromSamples maps labeled samples through the family catalog onto
// discovery.Edge. Vendors live in families.yaml; Reconcile stays generic.
// ipToID is optional catalog (and harvested) address → device_name join.
type neighRow struct {
	src, srcPort, dst, dstPort string
}

func EdgesFromSamples(samples []Sample, now time.Time, allowedNets []*net.IPNet, ipToID map[string]string) ([]discovery.Edge, []discovery.OutOfScopeNeighbour) {
	fams := DefaultFamilies()
	buckets := map[string]map[string]*neighRow{}
	var edges []discovery.Edge
	var oos []discovery.OutOfScopeNeighbour

	for _, s := range samples {
		f := matchFamily(s.Name, fams)
		if f == nil {
			continue
		}
		src := pickReporter(s.Labels, f.Reporter)
		if src == "" {
			continue
		}
		switch strings.ToLower(f.Kind) {
		case "session":
			if !sessionUp(s, f.Established) {
				continue
			}
			raw := label(s.Labels, f.Session...)
			if raw == "" {
				continue
			}
			dst := resolvePeer(raw, s.Labels, ipToID)
			e, n, skip := sessionEdge(f, src, dst, raw, s.Labels, now, allowedNets)
			if skip {
				if n != nil {
					oos = append(oos, *n)
				}
				continue
			}
			edges = append(edges, e)
		default:
			key := f.ID + "|" + joinKey(*f, src, s.Labels)
			b := buckets[f.ID]
			if b == nil {
				b = map[string]*neighRow{}
				buckets[f.ID] = b
			}
			row := b[key]
			if row == nil {
				row = &neighRow{src: src, srcPort: label(s.Labels, f.LocalPort...)}
				b[key] = row
			}
			if row.srcPort == "" {
				row.srcPort = label(s.Labels, f.LocalPort...)
			}
			if dst := pickField(s, f.Neighbor); dst != "" {
				row.dst = dst
			}
			if p := pickField(s, f.NeighborPort); p != "" {
				row.dstPort = p
			}
		}
	}

	for _, f := range fams {
		if strings.EqualFold(f.Kind, "session") {
			continue
		}
		for _, row := range buckets[f.ID] {
			e, n, skip := neighborEdge(row, f, now, allowedNets, ipToID)
			if skip {
				if n != nil {
					oos = append(oos, *n)
				}
				continue
			}
			edges = append(edges, e)
		}
	}
	return edges, oos
}

func neighborEdge(row *neighRow, f Family, now time.Time, allowedNets []*net.IPNet, ipToID map[string]string) (discovery.Edge, *discovery.OutOfScopeNeighbour, bool) {
	if row == nil || row.src == "" || row.dst == "" {
		return discovery.Edge{}, nil, true
	}
	if noisyNeighbor(row.dst) || strings.EqualFold(row.src, row.dst) {
		return discovery.Edge{}, nil, true
	}
	if id, ok := lookupIP(ipToID, row.dst); ok {
		row.dst = id
	}
	if remIP := net.ParseIP(row.dst); remIP != nil && len(allowedNets) > 0 && !snmputil.IPInNets(remIP, allowedNets) {
		n := discovery.OutOfScopeNeighbour{
			Proto:           f.Proto,
			ReportingDevice: row.src,
			ReportingPort:   row.srcPort,
			NeighbourHint:   row.dst,
			FirstSeen:       now,
			LastSeen:        now,
		}
		return discovery.Edge{}, &n, true
	}
	return discovery.Edge{
		SrcDevice:      row.src,
		SrcPort:        row.srcPort,
		DstDevice:      row.dst,
		DstPort:        row.dstPort,
		DiscoveryProto: discovery.DiscoveryProtocol(f.Proto),
		Direction:      discovery.DirectionUnidirectional,
		Confidence:     discovery.ConfidenceHigh,
		Adjacency:      discovery.AdjacencyDirect,
		PrecedenceRank: protoRank(f.Proto),
		LinkKind:       protoLink(f.Proto),
		ObservedAt:     now,
		Metadata:       alloyMeta(f.Evidence, nil),
	}, nil, false
}

func noisyNeighbor(name string) bool {
	n := strings.ToLower(strings.TrimSpace(name))
	if n == "" {
		return false
	}
	return strings.Contains(n, "phone") ||
		strings.HasPrefix(n, "ap-") ||
		strings.HasPrefix(n, "wap-") ||
		strings.HasPrefix(n, "sep")
}

func protoRank(proto string) int {
	switch strings.ToLower(proto) {
	case "lldp":
		return lldpPrecedence
	case "cdp":
		return cdpPrecedence
	case "bgp":
		return bgpPrecedence
	default:
		return 9
	}
}

func protoLink(proto string) discovery.LinkKind {
	if strings.EqualFold(proto, "bgp") {
		return discovery.LinkKindIP
	}
	return discovery.LinkKindEthernet
}

func sessionEdge(f *Family, src, dst, peerHint string, labels map[string]string, now time.Time, allowedNets []*net.IPNet) (discovery.Edge, *discovery.OutOfScopeNeighbour, bool) {
	scopeHint := peerHint
	if scopeHint == "" {
		scopeHint = dst
	}
	if ip := net.ParseIP(scopeHint); ip != nil {
		if ip.IsUnspecified() || ip.IsLinkLocalUnicast() {
			return discovery.Edge{}, nil, true
		}
		if len(allowedNets) > 0 && !snmputil.IPInNets(ip, allowedNets) {
			n := discovery.OutOfScopeNeighbour{
				Proto:           f.Proto,
				ReportingDevice: src,
				NeighbourHint:   scopeHint,
				FirstSeen:       now,
				LastSeen:        now,
			}
			return discovery.Edge{}, &n, true
		}
	}
	remoteAS := label(labels, f.RemoteAS...)
	if remoteAS != "" {
		if _, err := strconv.Atoi(remoteAS); err != nil {
			remoteAS = ""
		}
	}
	localAS := label(labels, f.LocalAS...)
	if localAS != "" {
		if _, err := strconv.Atoi(localAS); err != nil {
			localAS = ""
		}
	}
	peerGroup := label(labels, f.PeerGroup...)
	sessionType := discovery.InferSessionType(localAS, remoteAS, peerGroup)
	extra := map[string]string{}
	if remoteAS != "" {
		extra[discovery.MetadataKeyRemoteAS] = remoteAS
	}
	if localAS != "" {
		extra[discovery.MetadataKeyLocalAS] = localAS
	}
	if peerGroup != "" {
		extra[discovery.MetadataKeyPeerGroup] = peerGroup
	}
	if sessionType != "" {
		extra[discovery.MetadataKeySessionType] = sessionType
	}
	metadata := alloyMeta(f.Evidence, extra)
	// Peer address is the session id. Empty SrcPort collapses every BGP
	// adjacency on a box into one neighbour_disagreement.
	session := peerHint
	if session == "" {
		session = dst
	}
	return discovery.Edge{
		SrcDevice:      src,
		SrcPort:        session,
		DstDevice:      dst,
		DiscoveryProto: discovery.DiscoveryProtocol(f.Proto),
		Direction:      discovery.DirectionUnidirectional,
		Confidence:     discovery.ConfidenceLow,
		Adjacency:      discovery.AdjacencyUnknown,
		PrecedenceRank: protoRank(f.Proto),
		LinkKind:       protoLink(f.Proto),
		ObservedAt:     now,
		Metadata:       metadata,
	}, nil, false
}

func resolvePeer(raw string, labels map[string]string, ipToID map[string]string) string {
	if id, ok := lookupIP(ipToID, raw); ok {
		return id
	}
	if name := hostnameish(label(labels, "peer_description")); name != "" {
		return name
	}
	return raw
}

func lookupIP(ipToID map[string]string, raw string) (string, bool) {
	if ipToID == nil || raw == "" {
		return "", false
	}
	if id, ok := ipToID[raw]; ok && id != "" {
		return id, true
	}
	if ip := net.ParseIP(raw); ip != nil {
		if id, ok := ipToID[ip.String()]; ok && id != "" {
			return id, true
		}
	}
	return "", false
}

func hostnameish(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" || net.ParseIP(s) != nil || strings.ContainsAny(s, " /") {
		return ""
	}
	for i, r := range s {
		ok := r == '.' || r == '-' || r == '_' ||
			(r >= 'a' && r <= 'z') || (r >= '0' && r <= '9')
		if i == 0 {
			ok = r >= 'a' && r <= 'z'
		}
		if !ok {
			return ""
		}
	}
	if len(s) < 2 {
		return ""
	}
	return s
}

func alloyMeta(evidence string, extra map[string]string) map[string]string {
	m := map[string]string{
		discovery.MetadataKeyInference: discovery.InferenceAlloyOTLP,
		discovery.MetadataKeyEvidence:  evidence,
	}
	for k, v := range extra {
		if strings.TrimSpace(v) == "" {
			continue
		}
		m[k] = v
	}
	return m
}

func deviceName(labels map[string]string) string {
	return pickReporter(labels, []string{"device_name", "src_device", "source"})
}

func label(m map[string]string, keys ...string) string {
	for _, k := range keys {
		if v := strings.TrimSpace(m[k]); v != "" {
			return v
		}
	}
	return ""
}
