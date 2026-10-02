package promwalk_test

import (
	"net"
	"testing"
	"time"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/source/promwalk"
)

func TestEdgesFromLLDPAndNokiaBGP(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	_, cidr, err := net.ParseCIDR("10.9.9.0/24")
	if err != nil {
		t.Fatal(err)
	}
	samples := []promwalk.Sample{
		{
			Name: "snmp_lldpRemSysName",
			Labels: map[string]string{
				"device_name":         "spine1",
				"lldpRemTimeMark":     "0",
				"lldpRemLocalPortNum": "1",
				"lldpRemIndex":        "1",
				"lldpRemSysName":      "leaf1",
				"lldpRemPortId":       "ethernet-1/1",
			},
			Value: 1,
		},
		{
			Name: "snmp_tBgpPeerNgConnState",
			Labels: map[string]string{
				"device_name":       "spine1",
				"tBgpPeerNgAddress": "10.9.9.3",
				"peer_as":           "65001",
				"local_as":          "65000",
				"peer_group":        "eBGP",
			},
			Value: 6,
		},
		{
			Name: "snmp_tBgpPeerNgConnState",
			Labels: map[string]string{
				"device_name":       "spine1",
				"tBgpPeerNgAddress": "10.8.8.1",
			},
			Value: 6,
		},
	}
	edges, oos := promwalk.EdgesFromSamples(samples, now, []*net.IPNet{cidr}, nil)
	if len(edges) != 2 {
		t.Fatalf("edges=%d want 2: %+v", len(edges), edges)
	}
	var sawLLDP, sawBGP bool
	for _, e := range edges {
		switch e.DiscoveryProto {
		case discovery.DiscoveryProtocolLLDP:
			sawLLDP = true
			if e.SrcDevice != "spine1" || e.DstDevice != "leaf1" || e.SrcPort != "1" || e.DstPort != "ethernet-1/1" {
				t.Fatalf("lldp edge: %+v", e)
			}
		case discovery.DiscoveryProtocolBGP:
			sawBGP = true
			if e.DstDevice != "10.9.9.3" || e.SrcDevice != "spine1" || e.SrcPort != "10.9.9.3" {
				t.Fatalf("bgp edge: %+v", e)
			}
			if e.Metadata[discovery.MetadataKeySessionType] != discovery.SessionTypeEBGP ||
				e.Metadata[discovery.MetadataKeyInference] != discovery.InferenceAlloyOTLP ||
				e.Metadata[discovery.MetadataKeyRemoteAS] != "65001" ||
				e.Metadata[discovery.MetadataKeyPeerGroup] != "eBGP" {
				t.Fatalf("bgp provenance: %+v", e.Metadata)
			}
		}
	}
	if !sawLLDP || !sawBGP {
		t.Fatalf("missing proto: %+v", edges)
	}
	if len(oos) != 1 || oos[0].NeighbourHint != "10.8.8.1" {
		t.Fatalf("oos=%+v", oos)
	}
}

func TestBGPIdleSkipped(t *testing.T) {
	edges, oos := promwalk.EdgesFromSamples([]promwalk.Sample{{
		Name:   "snmp_tBgpPeerNgConnState",
		Labels: map[string]string{"device_name": "spine1", "tBgpPeerNgAddress": "10.9.9.3"},
		Value:  1,
	}}, time.Now(), nil, nil)
	if len(edges) != 0 || len(oos) != 0 {
		t.Fatalf("idle should skip: edges=%v oos=%v", edges, oos)
	}
}

func TestBGPJoinsCatalogIP(t *testing.T) {
	edges, oos := promwalk.EdgesFromSamples([]promwalk.Sample{{
		Name: "snmp_tBgpPeerNgConnState",
		Labels: map[string]string{
			"device_name":       "spine1",
			"tBgpPeerNgAddress": "10.0.1.1",
			"peer_as":           "65001",
		},
		Value: 6,
	}}, time.Now(), nil, map[string]string{"10.0.1.1": "leaf1"})
	if len(oos) != 0 || len(edges) != 1 {
		t.Fatalf("edges=%v oos=%v", edges, oos)
	}
	e := edges[0]
	if e.SrcDevice != "spine1" || e.DstDevice != "leaf1" || e.SrcPort != "10.0.1.1" {
		t.Fatalf("joined edge: %+v", e)
	}
}

func TestBGPOverlaySessionType(t *testing.T) {
	edges, _ := promwalk.EdgesFromSamples([]promwalk.Sample{{
		Name: "snmp_tBgpPeerNgConnState",
		Labels: map[string]string{
			"device_name":       "leaf1",
			"tBgpPeerNgAddress": "10.0.2.1",
			"peer_as":           "100",
			"local_as":          "100",
			"peer_group":        "iBGP-overlay",
		},
		Value: 6,
	}}, time.Now(), nil, map[string]string{"10.0.2.1": "spine1"})
	if len(edges) != 1 {
		t.Fatalf("%+v", edges)
	}
	m := edges[0].Metadata
	if m[discovery.MetadataKeySessionType] != discovery.SessionTypeIBGP ||
		m[discovery.MetadataKeyPeerGroup] != "iBGP-overlay" ||
		m[discovery.MetadataKeyInference] != discovery.InferenceAlloyOTLP {
		t.Fatalf("%+v", m)
	}
}

func TestBGPUsesPeerDescription(t *testing.T) {
	edges, _ := promwalk.EdgesFromSamples([]promwalk.Sample{{
		Name: "snmp_tBgpPeerNgConnState",
		Labels: map[string]string{
			"device_name":       "spine1",
			"tBgpPeerNgAddress": "10.0.1.1",
			"peer_description":  "leaf1",
		},
		Value: 6,
	}}, time.Now(), nil, nil)
	if len(edges) != 1 || edges[0].DstDevice != "leaf1" {
		t.Fatalf("desc join: %+v", edges)
	}
}

func TestCDPEdges(t *testing.T) {
	edges, _ := promwalk.EdgesFromSamples([]promwalk.Sample{{
		Name: "snmp_cdpCacheDeviceId",
		Labels: map[string]string{
			"device_name":         "core-01",
			"ifIndex":             "2",
			"cdpCacheDeviceIndex": "1",
			"cdpCacheDeviceId":    "access-01",
			"cdpCacheDevicePort":  "Gi0/1",
			"if_interface_name":   "Gi1/0/1",
		},
		Value: 1,
	}}, time.Now(), nil, nil)
	if len(edges) != 1 {
		t.Fatalf("cdp edges: %+v", edges)
	}
	e := edges[0]
	if e.DiscoveryProto != discovery.DiscoveryProtocolCDP ||
		e.SrcDevice != "core-01" || e.DstDevice != "access-01" ||
		e.SrcPort != "Gi1/0/1" || e.DstPort != "Gi0/1" {
		t.Fatalf("cdp edge: %+v", e)
	}
}

func TestEdgesFromGNMILLDP(t *testing.T) {
	now := time.Unix(1_700_000_000, 0)
	edges, oos := promwalk.EdgesFromSamples([]promwalk.Sample{
		{
			Name: "gnmi_lldp_neighbors_srl:lldp_interface_neighbor_system_name",
			Labels: map[string]string{
				"source":         "leaf1",
				"interface_name": "ethernet-1/49",
				"neighbor_id":    "1A:00:08:FF:00:00",
				"value":          "spine1",
			},
			Value: 1,
		},
		{
			Name: "gnmi_lldp_neighbors_srl:lldp_interface_neighbor_port_id",
			Labels: map[string]string{
				"source":         "leaf1",
				"interface_name": "ethernet-1/49",
				"neighbor_id":    "1A:00:08:FF:00:00",
				"value":          "ethernet-1/1",
			},
			Value: 1,
		},
		{
			Name: "gnmi_lldp_neighbors_srl:lldp_interface_neighbor_last_update",
			Labels: map[string]string{
				"source":         "leaf1",
				"interface_name": "ethernet-1/49",
				"neighbor_id":    "1A:00:08:FF:00:00",
				"value":          "2026-09-16T00:51:23.500Z",
			},
			Value: 1,
		},
		{
			Name: "gnmi_lldp_neighbors_srl:lldp_interface_neighbor_system_name",
			Labels: map[string]string{
				"source":         "leaf1",
				"interface_name": "ethernet-1/1",
				"neighbor_id":    "aa:bb",
				"value":          "AP-lobby",
			},
			Value: 1,
		},
	}, now, nil, nil)
	if len(oos) != 0 {
		t.Fatalf("oos=%+v", oos)
	}
	if len(edges) != 1 {
		t.Fatalf("edges=%d want 1: %+v", len(edges), edges)
	}
	e := edges[0]
	if e.SrcDevice != "leaf1" || e.DstDevice != "spine1" ||
		e.SrcPort != "ethernet-1/49" || e.DstPort != "ethernet-1/1" {
		t.Fatalf("gnmi lldp: %+v", e)
	}
	if e.Metadata[discovery.MetadataKeyEvidence] != discovery.EvidenceGNMILLDP ||
		e.Metadata[discovery.MetadataKeyInference] != discovery.InferenceAlloyOTLP {
		t.Fatalf("gnmi provenance: %+v", e.Metadata)
	}
}

func TestGNMIIgnoresPlaceholderNeighborPort(t *testing.T) {
	edges, oos := promwalk.EdgesFromSamples([]promwalk.Sample{
		{
			Name: "gnmi_lldp_neighbors:lldp_interface_neighbor_system_name",
			Labels: map[string]string{
				"source":         "leaf-br1",
				"interface_name": "Eth-1/49",
				"neighbor_id":    "1",
				"value":          "spine1",
			},
			Value: 1,
		},
		{
			Name: "gnmi_lldp_neighbors:lldp_interface_neighbor_port_id",
			Labels: map[string]string{
				"source":         "leaf-br1",
				"interface_name": "Eth-1/49",
				"neighbor_id":    "1",
				"value":          "INTERFACE_NAME",
			},
			Value: 1,
		},
	}, time.Now(), nil, nil)
	if len(oos) != 0 || len(edges) != 1 {
		t.Fatalf("edges=%+v oos=%+v", edges, oos)
	}
	e := edges[0]
	if e.SrcDevice != "leaf-br1" || e.DstDevice != "spine1" || e.SrcPort != "Eth-1/49" {
		t.Fatalf("gnmi edge: %+v", e)
	}
	if e.DstPort != "" {
		t.Fatalf("placeholder INTERFACE_NAME must not become dst_port: %+v", e)
	}
}

func TestHarvestIPAliases(t *testing.T) {
	ipToID := map[string]string{}
	promwalk.HarvestIPAliases([]promwalk.Sample{{
		Name: "snmp_ipAdEntAddr",
		Labels: map[string]string{
			"device_name": "leaf1",
			"ipAdEntAddr": "10.0.1.1",
		},
	}}, ipToID)
	if ipToID["10.0.1.1"] != "leaf1" {
		t.Fatalf("aliases=%v", ipToID)
	}
}
