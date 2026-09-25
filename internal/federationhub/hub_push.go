package federationhub

// /spoke/push HTTP handling: payload decode, validation routing, identity
// binding, rate limiting, and the structured reject contract. Split from
// hub.go (#168) — same-package move, no behaviour change.

import (
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"time"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/propagation"

	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/tracing"
)

// Per-push payload caps. These bound the size of a single spoke push and are
// federation-specific (not per-field). The per-field byte caps live in
// internal/limits because they are shared with the snapshot loader; raising
// them in one place without the other would silently desynchronise wire-format
// acceptance and on-disk validation. See limits.MaxDeviceIDBytes etc.
//
// The count caps and the byte cap are deliberately sized together: a payload
// at both count limits with realistic field contents serialises to ~19 MiB of
// JSON (measured), so maxPushPayloadBytes at 32 MiB leaves ~40% headroom and
// the advertised count limits are actually reachable — even by an
// uncompressed (compression: none) spoke. Pathologically large per-field
// values can still hit the byte cap before the count caps; the byte cap is
// the canonical bound. If you raise the count caps, re-measure and re-size.
const (
	maxDevicesPerPush = 10_000
	maxEdgesPerPush   = 50_000

	// maxPushPayloadBytes caps both the wire bytes read from the request
	// body (any Content-Encoding) and the decompressed output of a gzip
	// body. The dual application means a gzip bomb is bounded on both
	// sides: ≤32 MiB may enter from the network, and inflation stops at
	// 32 MiB of JSON regardless of the theoretical expansion ratio.
	maxPushPayloadBytes = 32 << 20
)

// errDecompressedPayloadTooLarge is the sentinel surfaced by cappedReader
// when a gzip push inflates past maxPushPayloadBytes. handlePush maps it to
// 413, distinguishing "your topology is too big" from a generic decode error.
var errDecompressedPayloadTooLarge = errors.New("decompressed payload exceeds limit")

// cappedReader is io.LimitReader with a loud failure mode: instead of
// silently truncating at n bytes (which would surface as a confusing JSON
// "unexpected EOF"), it returns errDecompressedPayloadTooLarge so the
// handler can answer 413 with an actionable message.
type cappedReader struct {
	r io.Reader
	n int64 // bytes remaining
}

func (c *cappedReader) Read(p []byte) (int, error) {
	if c.n <= 0 {
		return 0, errDecompressedPayloadTooLarge
	}
	if int64(len(p)) > c.n {
		p = p[:c.n]
	}
	rn, err := c.r.Read(p)
	c.n -= int64(rn)
	return rn, err
}

// Package-local aliases for the typed metrics.RejectReason constants. The
// authoritative declarations — including the doc comments on each constant,
// the underlying wire strings, and the Valid() defense-in-depth check —
// live in internal/metrics/reject_reason.go. They are re-exported here so
// existing call sites in this package read naturally and a future reader can
// still find every reject-emitting path with
// `grep rejectReason internal/federation`.
//
// These are typed metrics.RejectReason values, not bare strings: callers
// that need the wire string (Prometheus WithLabelValues, JSON body) convert
// explicitly at the boundary via string(...), which surfaces any future
// untyped-string smuggling at compile time.
//
// New values are added only in a release that ships emission code + tests;
// see docs/operator/federation.md "Spoke push response contract".
const (
	rejectReasonStaleGeneration    = metrics.RejectReasonStaleGeneration
	rejectReasonSizeBudgetExceeded = metrics.RejectReasonSizeBudgetExceeded
	rejectReasonInvalidLabelKey    = metrics.RejectReasonInvalidLabelKey
	rejectReasonInvalidLabelValue  = metrics.RejectReasonInvalidLabelValue
	rejectReasonStructuralInvalid  = metrics.RejectReasonStructuralInvalid
)

func (h *Hub) handlePush(w http.ResponseWriter, r *http.Request) {
	// Issue #68: continue the spoke's trace. Extract the W3C traceparent the
	// spoke injected into the request headers and start hub.handlePush as a
	// child of the spoke.push span, so the hub span shares the spoke's trace
	// ID. When the spoke is not tracing, no traceparent is present and this
	// span starts a fresh (unsampled-by-default) root.
	ctx := otel.GetTextMapPropagator().Extract(r.Context(), propagation.HeaderCarrier(r.Header))
	ctx, span := tracing.Tracer().Start(ctx, "hub.handlePush")
	defer span.End()
	r = r.WithContext(ctx)

	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if !h.isLeader.Load() {
		// HA: only the leader hub accepts pushes; followers 503 (retryable
		// spoke-side, see spoke.go). The Connection:close header makes a spoke
		// pinned via keep-alive to a just-demoted leader re-resolve to the new
		// leader on its next attempt (design §4.3). Single-hub mode is always
		// leader (isLeader defaults true), so this never fires there.
		w.Header().Set("Connection", "close")
		http.Error(w, "not the leader hub", http.StatusServiceUnavailable)
		return
	}
	defer func() { _ = r.Body.Close() }()

	// Wire-side cap on raw request bytes regardless of Content-Encoding.
	body := io.Reader(http.MaxBytesReader(w, r.Body, maxPushPayloadBytes))
	switch enc := r.Header.Get("Content-Encoding"); enc {
	case "", "identity":
		// uncompressed JSON
	case "gzip":
		zr, err := gzip.NewReader(body)
		if err != nil {
			h.logger.Warn("hub: malformed gzip spoke payload", "error", err)
			http.Error(w, "malformed gzip body", http.StatusBadRequest)
			return
		}
		defer func() { _ = zr.Close() }()
		// Decompressed-side cap: bounds inflation of a gzip bomb.
		body = &cappedReader{r: zr, n: maxPushPayloadBytes}
	default:
		http.Error(w, fmt.Sprintf("unsupported Content-Encoding %q (supported: gzip, identity)", enc), http.StatusUnsupportedMediaType)
		return
	}

	var payload federation.SpokePayload
	dec := json.NewDecoder(body)
	if err := dec.Decode(&payload); err != nil {
		h.logger.Warn("hub: malformed spoke payload", "error", err)
		var maxBytesErr *http.MaxBytesError
		if errors.As(err, &maxBytesErr) {
			http.Error(w, "request body too large (max 32 MiB on the wire; enable spoke compression or partition the domain)", http.StatusRequestEntityTooLarge)
			return
		}
		if errors.Is(err, errDecompressedPayloadTooLarge) {
			http.Error(w, "decompressed payload too large (max 32 MiB; partition the domain across more spokes)", http.StatusRequestEntityTooLarge)
			return
		}
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if len(payload.Devices) > maxDevicesPerPush || len(payload.Edges) > maxEdgesPerPush {
		http.Error(w, fmt.Sprintf("payload exceeds limits (max %d devices, %d edges)", maxDevicesPerPush, maxEdgesPerPush), http.StatusRequestEntityTooLarge)
		return
	}
	if err := validateSpokePayload(payload); err != nil {
		h.logger.Warn("hub: spoke payload failed semantic validation",
			"spoke_id", payload.SpokeID, "error", err)
		var verr *validationError
		if !errors.As(err, &verr) {
			// Invariant: every validateSpokePayload error path returns
			// *validationError (enforced by issue #19). If this branch fires,
			// a new validation site was added that bypassed
			// newValidationError — a code defect that would silently
			// mislabel rejects in the GraphUpdatesRejectedTotal counter and
			// strip the structured pushRejection wire contract. Panic so the
			// defect surfaces at the first request that hits it, rather than
			// degrading observability silently. http.Server recovers from
			// handler panics, so this affects only the offending request.
			panic(fmt.Sprintf("federation: validateSpokePayload returned non-*validationError: %T %v", err, err))
		}
		h.m.GraphUpdatesRejectedTotal.WithLabelValues(string(verr.reason)).Inc()
		// Structured reject: spokes branch on the reason enum, not text.
		writePushRejection(w, http.StatusBadRequest, verr.reason, map[string]any{
			"message": verr.msg,
		})
		return
	}
	if payload.SpokeID == "" {
		http.Error(w, "spoke_id required", http.StatusBadRequest)
		return
	}
	if len(payload.SpokeID) > 128 {
		http.Error(w, "spoke_id too long (max 128)", http.StatusBadRequest)
		return
	}
	for _, c := range payload.SpokeID {
		if (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (c < '0' || c > '9') && c != '-' && c != '_' && c != '.' {
			http.Error(w, "spoke_id contains invalid characters (allowed: a-z A-Z 0-9 - _ .)", http.StatusBadRequest)
			return
		}
	}
	// Record spoke.id on the span only AFTER the length and charset checks
	// above. The OTel SDK does not bound attribute value length by default,
	// so attaching the raw field first would let any mTLS cert holder inject
	// up to a body-cap-sized string into the tracing backend (cost
	// amplification + trace injection). Here the value is guaranteed ≤128
	// chars from a safe alphabet.
	span.SetAttributes(attribute.String("spoke.id", payload.SpokeID))

	// LD-21: bind spoke_id to the presenting mTLS client certificate's Common
	// Name so a spoke holding a valid cert cannot overwrite another spoke's
	// topology data. r.TLS is nil in unit tests (httptest has no TLS); in
	// production ClientAuth: RequireAndVerifyClientCert guarantees at least one
	// peer certificate is present.
	if r.TLS != nil && len(r.TLS.PeerCertificates) > 0 {
		certCN := r.TLS.PeerCertificates[0].Subject.CommonName
		if certCN != payload.SpokeID {
			h.logger.Warn("hub: spoke_id/cert CN mismatch — rejecting push",
				"spoke_id", payload.SpokeID,
				"cert_cn", certCN,
			)
			http.Error(w, fmt.Sprintf("spoke_id %q does not match client certificate CN %q", payload.SpokeID, certCN), http.StatusForbidden)
			return
		}
	}

	// Validate CycleAt: must be present, not in the future, and not older than
	// spoke_timeout (which would indicate a lost/replayed push).
	now := time.Now()
	if payload.CycleAt.IsZero() {
		http.Error(w, "cycle_at required", http.StatusBadRequest)
		return
	}
	if payload.CycleAt.After(now.Add(5 * time.Minute)) {
		http.Error(w, "cycle_at is too far in the future", http.StatusBadRequest)
		return
	}
	if now.Sub(payload.CycleAt) > h.cfg.SpokeTimeout {
		h.logger.Warn("hub: rejecting stale spoke payload",
			"spoke_id", payload.SpokeID,
			"cycle_at", payload.CycleAt,
			"age", now.Sub(payload.CycleAt),
			"spoke_timeout", h.cfg.SpokeTimeout,
		)
		http.Error(w, "cycle_at too stale", http.StatusBadRequest)
		return
	}

	// Build the combined graph with the new spoke folded into a COPY of the
	// registry; h.spokes itself is not written until publishIfWinner confirms
	// this graph wins publication. This makes the spoke commit atomic with the
	// publish: a concurrent push's spokesSnapshot() can never observe an entry
	// that has not yet been validated and published.
	h.mu.Lock()
	prevEntry, hadPrev := h.spokes[payload.SpokeID]
	// Defense-in-depth rate limit: reject pushes that arrive sooner than
	// min_push_interval after the previous accepted push from the same
	// spoke_id. Runs inside h.mu so two racing pushes cannot both pass.
	if hadPrev && h.cfg.Hub.MinPushInterval > 0 {
		sinceLast := now.Sub(prevEntry.lastSeen)
		if sinceLast < h.cfg.Hub.MinPushInterval {
			h.mu.Unlock()
			retryAfter := h.cfg.Hub.MinPushInterval - sinceLast
			retrySecs := int(retryAfter.Seconds())
			if retrySecs < 1 {
				retrySecs = 1
			}
			w.Header().Set("Retry-After", strconv.Itoa(retrySecs))
			h.logger.Info("hub: rejecting push within min_push_interval",
				"spoke_id", payload.SpokeID,
				"since_last_push", sinceLast,
				"min_push_interval", h.cfg.Hub.MinPushInterval,
			)
			http.Error(w, "push too soon — observe min_push_interval", http.StatusTooManyRequests)
			return
		}
	}
	entry := spokeEntry{payload: payload, lastSeen: now}
	candidate := h.spokesSnapshot() // copy of h.spokes WITHOUT the new entry
	candidate[payload.SpokeID] = entry
	gen := h.publishGen.Add(1)
	h.mu.Unlock()

	combined, unmatchedCount := h.buildCombinedGraph(candidate)

	// publishIfWinner commits the spoke entry + liveness gauges + Topology.Update
	// atomically under h.mu iff this generation wins and the graph fits the size
	// budget. On reject nothing was written, so there is nothing to roll back.
	published, rejectReason := h.publishIfWinner(gen, combined, unmatchedCount, &acceptedPush{id: payload.SpokeID, entry: entry})
	if published {
		h.writeSnapshotAsync(combined)
		h.logger.Info("hub: spoke push accepted",
			"spoke_id", payload.SpokeID,
			"devices", len(payload.Devices),
			"edges", len(payload.Edges),
			"cycle_at", payload.CycleAt,
		)
		w.WriteHeader(http.StatusNoContent)
		return
	}

	h.logger.Warn("hub: spoke push rejected — combined graph not applied",
		"spoke_id", payload.SpokeID,
		"reject_reason", rejectReason,
		"combined_devices", len(combined.Devices),
		"combined_edges", len(combined.Edges),
		"max_devices", h.cfg.Hub.MaxGraphDevices,
		"max_edges", h.cfg.Hub.MaxGraphEdges,
		"cycle_at", payload.CycleAt,
	)
	writePushRejection(w, statusForRejectReason(rejectReason), rejectReason, map[string]any{
		"combined_devices": len(combined.Devices),
		"combined_edges":   len(combined.Edges),
		"max_devices":      h.cfg.Hub.MaxGraphDevices,
		"max_edges":        h.cfg.Hub.MaxGraphEdges,
	})
}

// pushRejection is the JSON body returned when a spoke push is accepted by the
// transport but the resulting graph is not applied to active hub state. reason
// is a stable machine-parseable code; detail is a free-form map for operator
// context. Schema: {"status":"rejected","reason":"<code>","detail":{...}}.
//
// Reason is typed metrics.RejectReason rather than bare string so the
// encoder cannot accept untyped-string smuggling at the call site; the
// JSON encoding of `type RejectReason string` is byte-identical to a bare
// string (pinned by TestRejectReasonWireValuesPinned in internal/metrics).
type pushRejection struct {
	Status string               `json:"status"` // always "rejected"
	Reason metrics.RejectReason `json:"reason"` // one of the metrics.RejectReason* constants
	Detail map[string]any       `json:"detail,omitempty"`
}

// statusForRejectReason maps a reject reason to its HTTP status code.
// 413 and 409 are 4xx so the spoke's retry policy treats them as fatal for
// this cycle — the next discovery cycle will produce fresh data. 503 is
// documented in the response contract for transient internal failures; no
// path currently emits it, but future reject reasons representing "we
// couldn't apply this right now, try later" (e.g. snapshot back-pressure,
// downstream sink stall) should map to the default arm here.
func statusForRejectReason(reason metrics.RejectReason) int {
	switch reason {
	case rejectReasonSizeBudgetExceeded:
		return http.StatusRequestEntityTooLarge // 413
	case rejectReasonStaleGeneration:
		return http.StatusConflict // 409
	case rejectReasonInvalidLabelKey, rejectReasonInvalidLabelValue, rejectReasonStructuralInvalid:
		return http.StatusBadRequest // 400: fatal — same payload will fail identically
	default:
		return http.StatusServiceUnavailable // 503: documented for transient internal failures
	}
}

func writePushRejection(w http.ResponseWriter, code int, reason metrics.RejectReason, detail map[string]any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(pushRejection{
		Status: "rejected",
		Reason: reason,
		Detail: detail,
	})
}
