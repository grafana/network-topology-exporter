package promwalk_test

import (
	"testing"
	"time"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/source/promwalk"
)

func TestDefaultFamiliesCoverKnownEncodings(t *testing.T) {
	if n := len(promwalk.DefaultFamilies()); n < 4 {
		t.Fatalf("embedded catalog too small: %d", n)
	}
	now := time.Unix(1_700_000_000, 0)
	cases := []struct {
		name     string
		evidence string
		labels   map[string]string
		value    float64
	}{
		{
			name:     "snmp_lldpRemSysName",
			evidence: discovery.EvidenceLLDP,
			labels:   map[string]string{"device_name": "a", "lldpRemSysName": "b"},
			value:    1,
		},
		{
			name:     "gnmi_lldp_neighbors:lldp_interface_neighbor_system_name",
			evidence: discovery.EvidenceGNMILLDP,
			labels:   map[string]string{"source": "a", "interface_name": "e1", "value": "b"},
			value:    1,
		},
		{
			name:     "snmp_cdpCacheDeviceId",
			evidence: discovery.EvidenceCDP,
			labels:   map[string]string{"device_name": "a", "cdpCacheDeviceId": "b"},
			value:    1,
		},
		{
			name:     "snmp_tBgpPeerNgConnState",
			evidence: discovery.EvidenceNokiaBGP,
			labels:   map[string]string{"device_name": "a", "tBgpPeerNgAddress": "10.0.0.1"},
			value:    6,
		},
	}
	for _, tc := range cases {
		edges, oos := promwalk.EdgesFromSamples([]promwalk.Sample{{
			Name: tc.name, Labels: tc.labels, Value: tc.value,
		}}, now, nil, nil)
		if len(oos) != 0 || len(edges) != 1 {
			t.Fatalf("%s: edges=%+v oos=%+v", tc.name, edges, oos)
		}
		if edges[0].Metadata[discovery.MetadataKeyEvidence] != tc.evidence {
			t.Fatalf("%s evidence=%v want %s", tc.name, edges[0].Metadata, tc.evidence)
		}
	}
}
