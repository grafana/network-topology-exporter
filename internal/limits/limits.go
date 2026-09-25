// Package limits holds the per-field byte caps shared by the federation push
// validator (internal/federation) and the snapshot loader (internal/snapshot).
//
// These are the canonical byte caps shared by the federation push validator
// and the snapshot loader. Raising any of these affects both wire-format
// acceptance and on-disk validation simultaneously: bumping a value in one
// place without the other will produce a configuration where the hub accepts
// a push that the snapshot loader will reject on the next process restart
// (or vice-versa). Keep the two paths locked together by importing from
// here, never by copying the constant into a new package.
package limits

// MaxDeviceIDBytes caps the byte length of a spoke-supplied device_id. The
// same cap applies to the on-disk snapshot loader so a snapshot written by an
// older release with a relaxed cap is rejected at load time rather than
// silently re-emitted.
const MaxDeviceIDBytes = 256

// MaxPortNameBytes caps the byte length of spoke-supplied port names and
// other edge / OOS string fields (src_device, src_port, dst_device, dst_port,
// discovery_proto, link_kind, reporting_device, reporting_port,
// neighbour_hint, proto). Same value as MaxDeviceIDBytes today, but kept
// distinct so the two can diverge if a future discovery protocol pushes the
// port-name shape beyond 256 bytes.
const MaxPortNameBytes = 256

// MaxLabelKeyBytes and MaxLabelValueBytes cap individual spoke-supplied
// label inputs before per-rune validation iterates the string. The
// http.MaxBytesReader on the push body bounds total payload size at 32 MiB,
// but a single multi-MiB label value would still force ~4M rune iterations in
// validateLabelValue — a CPU-DoS vector even against an mTLS-authenticated
// spoke. Prometheus / OpenMetrics impose no formal max on label values
// (docs/remediation.md §3), but client_golang defaults and Grafana Cloud Mimir
// limits operate well under 4 KiB per value, so values exceeding 4096
// bytes are far outside any legitimate topology label and safe to reject.
const (
	MaxLabelKeyBytes   = 256
	MaxLabelValueBytes = 4096
)

// MaxIfIndex is the practical upper bound applied to a spoke-supplied
// SrcIfIndex/DstIfIndex, in addition to the "must not be negative" check
// every ifIndex-consuming path already enforces. IF-MIB (RFC 2863) defines
// ifIndex as `InterfaceIndex ::= INTEGER (1..2147483647)` — the full
// positive Integer32 range — so this uses that spec ceiling rather than a
// tighter platform-specific guess such as 65535 (uint16): several vendors
// (observed on Cisco IOS/IOS-XE/IOS-XR) assign ifIndex values for
// port-channel members, sub-interfaces, and VLAN SVIs well above 16 bits, so
// a uint16 cap would reject legitimate real-world ifIndex values and break
// the snmp_exporter join key this project depends on (see
// docs/proposals/snmp-exporter-label-alignment.md). 0 is reserved by this
// codebase as the "unresolved" sentinel (ifIndexLabel in
// internal/metrics/topology_collector.go) and is accepted even though the
// MIB's own range starts at 1.
const MaxIfIndex = 2147483647 // math.MaxInt32; RFC 2863 InterfaceIndex ceiling.
