package federationhub

// Payload validation: the Prometheus/OpenMetrics line-protocol safety checks
// and structural invariants applied to every spoke push before it can touch
// hub state. Split from hub.go (#168) — same-package move, no behaviour
// change. The *validationError invariant documented on validateSpokePayload
// is enforced by handlePush in hub_push.go.

import (
	"fmt"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/limits"
	"github.com/grafana/network-topology-exporter/internal/metrics"
)

// labelKeyPattern is the canonical Prometheus / OpenMetrics label-name shape:
// an ASCII letter or underscore followed by any number of ASCII letters,
// digits, or underscores. Names starting with `__` are reserved by Prometheus
// and rejected separately below. Compiled once at package init.
var labelKeyPattern = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// validationError wraps a validateSpokePayload failure with a machine-parseable
// reject reason. Handlers unwrap this to route the reject through the
// structured pushRejection JSON response so spokes (and dashboards) can branch
// on reason rather than parsing free-form message text. msg is the
// human-readable detail logged and included in the rejection detail map.
type validationError struct {
	reason metrics.RejectReason
	msg    string
}

func (e *validationError) Error() string { return e.msg }

func newValidationError(reason metrics.RejectReason, format string, args ...any) *validationError {
	return &validationError{reason: reason, msg: fmt.Sprintf(format, args...)}
}

// validateLabelKey enforces the Prometheus label-name grammar plus the
// reserved-namespace rule. A malformed key would break /metrics line protocol
// on every subsequent scrape — the hub is the only enforcement point because
// mTLS authenticates WHO can push, not WHAT they push.
func validateLabelKey(k string) error {
	if k == "" {
		return newValidationError(rejectReasonInvalidLabelKey, "label key must not be empty")
	}
	// Size cap runs before the regex match so a multi-MiB key cannot force the
	// regex engine (or any future per-rune check) to walk the whole string.
	if len(k) > limits.MaxLabelKeyBytes {
		return newValidationError(rejectReasonInvalidLabelKey,
			"label key exceeds %d bytes", limits.MaxLabelKeyBytes)
	}
	if strings.HasPrefix(k, "__") {
		return newValidationError(rejectReasonInvalidLabelKey,
			"label key %q starts with reserved prefix \"__\"", k)
	}
	if !labelKeyPattern.MatchString(k) {
		return newValidationError(rejectReasonInvalidLabelKey,
			"label key %q does not match %s", k, labelKeyPattern.String())
	}
	return nil
}

// validateLabelValue rejects values containing characters that corrupt the
// OpenMetrics exposition line protocol (NUL, newline, carriage return) or
// other control characters that pass through Prometheus client_golang's
// escaping but render label-based dashboards unreadable. Non-control UTF-8
// (including quotes and backslashes — which the client library escapes
// correctly on emission) is allowed. The caller has already checked that the
// string is valid UTF-8 and within length bounds.
func validateLabelValue(v string) error {
	// Size cap runs before per-rune iteration so a multi-MiB value cannot force
	// ~4M iterations of the control-char check. Prometheus / OpenMetrics
	// impose no formal max on label value length (docs/remediation.md §3), but
	// client_golang defaults and Grafana Cloud Mimir limits operate well
	// under 4 KiB per value, so 4096 bytes is a safe upper bound.
	if len(v) > limits.MaxLabelValueBytes {
		return newValidationError(rejectReasonInvalidLabelValue,
			"label value exceeds %d bytes", limits.MaxLabelValueBytes)
	}
	for _, r := range v {
		if r == 0x00 || r == '\n' || r == '\r' {
			return newValidationError(rejectReasonInvalidLabelValue,
				"label value contains forbidden control char %#U", r)
		}
		// Reject all C0 controls (0x00..0x1F) and DEL (0x7F). The explicit
		// cases above are listed first so error messages point at the most
		// common injection vectors with their familiar names.
		if r < 0x20 || r == 0x7F {
			return newValidationError(rejectReasonInvalidLabelValue,
				"label value contains forbidden control char %#U", r)
		}
	}
	return nil
}

// validateMetricLabelString validates a string that becomes a Prometheus
// label *value* (not key) on a metric with a static label name. Currently
// applies to spoke-supplied edge port/device names and OOS-neighbour fields.
// Same rules as validateLabelValue, which now enforces the
// limits.MaxLabelValueBytes size cap before per-rune iteration — callers
// inherit that cap transitively. Upstream callers also apply field-specific
// caps (limits.MaxPortNameBytes, limits.MaxDeviceIDBytes) which are tighter;
// the cap inside validateLabelValue is the universal floor that prevents CPU
// DoS on any path that reaches here.
func validateMetricLabelString(s string) error {
	return validateLabelValue(s)
}

// validateSpokePayload checks semantic invariants that the JSON decoder and size
// guards cannot catch: empty/duplicate/overlong/non-UTF-8 device IDs, required
// edge fields, self-edges, overlong/non-UTF-8 port names, and Prometheus
// line-protocol safety for every spoke-supplied string that flows into a
// metric label name or value.
//
// Every returned error is a *validationError carrying a stable reject-reason
// code so the caller can route it through the structured pushRejection JSON
// response. Two reason flavours are emitted:
//   - rejectReasonInvalidLabelKey / rejectReasonInvalidLabelValue for failures
//     specifically about Prometheus line-protocol safety on a label name/value.
//   - rejectReasonStructuralInvalid for shape failures (empty required field,
//     oversize, invalid UTF-8 in non-label fields, duplicate device, self-edge).
//
// The handlePush *validationError invariant is load-bearing: handlePush panics
// on any non-*validationError return from this function (issue #19). Every new
// validation site MUST return newValidationError(...) — never plain fmt.Errorf.
func validateSpokePayload(p federation.SpokePayload) error {
	seen := make(map[string]bool, len(p.Devices))
	for i, d := range p.Devices {
		if d.ID == "" {
			return newValidationError(rejectReasonStructuralInvalid,
				"device[%d]: device_id is empty", i)
		}
		if len(d.ID) > limits.MaxDeviceIDBytes {
			return newValidationError(rejectReasonStructuralInvalid,
				"device[%d]: device_id exceeds %d bytes", i, limits.MaxDeviceIDBytes)
		}
		if !utf8.ValidString(d.ID) {
			return newValidationError(rejectReasonStructuralInvalid,
				"device[%d]: device_id is not valid UTF-8", i)
		}
		if err := validateMetricLabelString(d.ID); err != nil {
			return newValidationError(rejectReasonInvalidLabelValue,
				"device[%d]: device_id: %s", i, err.Error())
		}
		if seen[d.ID] {
			return newValidationError(rejectReasonStructuralInvalid,
				"device[%d]: duplicate device_id %q", i, d.ID)
		}
		seen[d.ID] = true
		// Validate inventory string fields that flow into device_info labels
		// (vendor, model, os_version, site). The label *names* are static so
		// only the values need protocol-safety checks.
		for _, f := range []struct{ name, val string }{
			{"vendor", d.Vendor}, {"model", d.Model},
			{"os_version", d.OSVersion}, {"site", d.Site},
		} {
			if !utf8.ValidString(f.val) {
				return newValidationError(rejectReasonInvalidLabelValue,
					"device[%d]: %s is not valid UTF-8", i, f.name)
			}
			if err := validateMetricLabelString(f.val); err != nil {
				return newValidationError(rejectReasonInvalidLabelValue,
					"device[%d]: %s: %s", i, f.name, err.Error())
			}
		}
		for k, v := range d.Labels {
			if err := validateLabelKey(k); err != nil {
				return newValidationError(rejectReasonInvalidLabelKey,
					"device[%d]: %s", i, err.Error())
			}
			if !utf8.ValidString(v) {
				return newValidationError(rejectReasonInvalidLabelValue,
					"device[%d]: label %q value is not valid UTF-8", i, k)
			}
			if err := validateLabelValue(v); err != nil {
				return newValidationError(rejectReasonInvalidLabelValue,
					"device[%d]: label %q: %s", i, k, err.Error())
			}
		}
	}
	for i, e := range p.Edges {
		if e.SrcDevice == "" || e.SrcPort == "" || e.DstDevice == "" {
			return newValidationError(rejectReasonStructuralInvalid,
				"edge[%d]: src_device, src_port, and dst_device are required", i)
		}
		if e.SrcDevice == e.DstDevice {
			return newValidationError(rejectReasonStructuralInvalid,
				"edge[%d]: self-edge (src_device == dst_device == %q)", i, e.SrcDevice)
		}
		for _, f := range []struct{ name, val string }{
			{"src_device", e.SrcDevice}, {"src_port", e.SrcPort},
			{"dst_device", e.DstDevice}, {"dst_port", e.DstPort},
			{"discovery_proto", string(e.DiscoveryProto)}, {"link_kind", string(e.LinkKind)},
		} {
			if len(f.val) > limits.MaxPortNameBytes {
				return newValidationError(rejectReasonStructuralInvalid,
					"edge[%d]: %s exceeds %d bytes", i, f.name, limits.MaxPortNameBytes)
			}
			if !utf8.ValidString(f.val) {
				return newValidationError(rejectReasonStructuralInvalid,
					"edge[%d]: %s is not valid UTF-8", i, f.name)
			}
			if err := validateMetricLabelString(f.val); err != nil {
				return newValidationError(rejectReasonInvalidLabelValue,
					"edge[%d]: %s: %s", i, f.name, err.Error())
			}
		}
		// Edge.Metadata is a map[string]string that flows into OTLP
		// attribute names+values via internal/output/otlp/otlp.go:201 (key
		// prefixed with metadataAttrPrefix). Not a Prometheus label path —
		// TopologyCollector does not emit it. Threat surface is therefore:
		// log-line corruption (control chars), JSON encoding bloat (huge
		// values), and OTLP attribute pollution (oversized keys/values).
		// Validate accordingly:
		//   - Cap key and value size (matches snapshot.go's caps; #22).
		//   - Reject control characters in both keys and values.
		//   - Do NOT enforce the Prometheus label-name grammar on keys:
		//     production discovery code uses dotted keys like
		//     "bgp.remote_as" and "mpls_te.admin_status" that conform to
		//     OTLP attribute-name conventions but not Prometheus's
		//     [a-zA-Z_][a-zA-Z0-9_]* grammar. Enforcing the strict shape
		//     would reject every legitimate BGP/MPLS push.
		// Issue #25 closed a gap left by #4 / D26.
		for k, v := range e.Metadata {
			if k == "" {
				return newValidationError(rejectReasonInvalidLabelKey,
					"edge[%d]: metadata key must not be empty", i)
			}
			if len(k) > limits.MaxLabelKeyBytes {
				return newValidationError(rejectReasonInvalidLabelKey,
					"edge[%d]: metadata key exceeds %d bytes", i, limits.MaxLabelKeyBytes)
			}
			if !utf8.ValidString(k) {
				return newValidationError(rejectReasonInvalidLabelKey,
					"edge[%d]: metadata key is not valid UTF-8", i)
			}
			for _, r := range k {
				if r == 0x00 || r == '\n' || r == '\r' || r < 0x20 || r == 0x7F {
					return newValidationError(rejectReasonInvalidLabelKey,
						"edge[%d]: metadata key contains forbidden control char %#U", i, r)
				}
			}
			if !utf8.ValidString(v) {
				return newValidationError(rejectReasonInvalidLabelValue,
					"edge[%d]: metadata %q value is not valid UTF-8", i, k)
			}
			if err := validateLabelValue(v); err != nil {
				return newValidationError(rejectReasonInvalidLabelValue,
					"edge[%d]: metadata %q: %s", i, k, err.Error())
			}
		}
	}
	for i, n := range p.OutOfScope {
		for _, f := range []struct{ name, val string }{
			{"reporting_device", n.ReportingDevice},
			{"reporting_port", n.ReportingPort},
			{"neighbour_hint", n.NeighbourHint},
			{"proto", n.Proto},
		} {
			if len(f.val) > limits.MaxPortNameBytes {
				return newValidationError(rejectReasonStructuralInvalid,
					"out_of_scope[%d]: %s exceeds %d bytes", i, f.name, limits.MaxPortNameBytes)
			}
			if !utf8.ValidString(f.val) {
				return newValidationError(rejectReasonInvalidLabelValue,
					"out_of_scope[%d]: %s is not valid UTF-8", i, f.name)
			}
			if err := validateMetricLabelString(f.val); err != nil {
				return newValidationError(rejectReasonInvalidLabelValue,
					"out_of_scope[%d]: %s: %s", i, f.name, err.Error())
			}
		}
	}
	return nil
}
