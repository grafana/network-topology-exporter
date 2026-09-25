// Package snmp walks SNMP SYSTEM-group data and shared table helpers.
//
// Invariants:
// - Scalar SYSTEM OIDs are fetched via GET; table data uses BulkWalk/Walk fallback.
// - Context cancellation must interrupt blocked SNMP reads via connection deadlines.
// - Each goroutine owns its own GoSNMP session (client instances are not shared).
// - sysObjectID vendor mapping uses IANA enterprise prefixes only.
package snmp

import (
	"context"
	"fmt"
	"log/slog"
	"net"
	"regexp"
	"runtime/debug"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	g "github.com/gosnmp/gosnmp"
	"golang.org/x/time/rate"

	"github.com/grafana/network-topology-exporter/internal/discovery"
)

// WalkerMetrics is the observability sink for per-protocol walker outcomes.
// Discovery modules call RecordWalkerOutcome from their walk paths; the
// implementation (typically wired in cmd/topology-exporter/main.go) bridges
// the call to whatever counter/registry the process is using.
//
// The interface deliberately lives in this package — next to Params — so that
// discovery modules do NOT need to import the prometheus client library. This
// keeps the dependency edge pointing into discovery from main, not the other
// way around, and makes test injection a single struct literal.
//
// Walkers MUST tolerate a nil Params.WalkerMetrics by dropping the increment
// (no panic). The nil-tolerant snmputil.Record*WalkerOutcome forwarders (see
// outcome.go) encapsulate that nil-check; modules call them rather than calling
// Inc directly on a possibly-nil handle.
// RecordWalkerOutcome routes to the BGP-specific counter
// (network_topology_bgp_walker_outcome_total); RecordProtocolWalkerOutcome
// routes to the generic counter (network_topology_walker_outcome_total) used
// by the LLDP, CDP, OSPF, and FDB walkers (issue #98). The two are kept
// separate so the long-standing BGP series operators alert on is never
// renamed. Callers reach these through the nil-tolerant forwarders
// snmputil.RecordBGPWalkerOutcome / RecordProtocolWalkerOutcome (outcome.go): a
// nil Params or nil Params.WalkerMetrics drops the increment rather than
// panicking.
//
// RecordDegraded increments network_topology_discovery_degraded_total for a
// (module, reason) tuple. It exists for the zero-edge degraded case (issue
// #100): when a sub-walk fails and the module returns no edge to stamp with
// degraded metadata, the orchestrator's edge-metadata path (see
// internal/app/cycle.go CollectDegradedReasons) has nothing to carry the
// signal. Modules call this directly so a degraded run is still observable.
// Same nil-tolerance contract as the outcome methods.
//
// RecordSystemWalkAnomaly increments network_topology_system_walk_anomaly_total
// for a low-cardinality reason (issue #101). The system walk (Walk, below)
// has two outcomes that silently degrade downstream behaviour with no metric
// otherwise: an empty/garbage sysName that falls back to the management IP as
// the device ID, and a sysObjectID that resolves to no known vendor (leaving
// Vendor="unknown", which skips the vendor-specific BGP4-V2 walker). reason is
// a closed two-value set — see the systemWalkAnomaly* constants. Same
// nil-tolerance contract as the outcome methods.
type WalkerMetrics interface {
	RecordWalkerOutcome(walker, outcome string)
	RecordProtocolWalkerOutcome(walker, outcome string)
	RecordDegraded(module, reason string)
	RecordSystemWalkAnomaly(reason string)
}

// WarnLimiter is the per-key Warn rate-limiter sink used by discovery
// modules to suppress chronic same-key Warn emissions (issue #16). The
// concrete implementation lives in internal/loglimit; defining the
// interface here keeps the discovery dependency edge pointing the same
// direction as WalkerMetrics — modules call up to the limiter through an
// interface, never the other way around.
//
// key is the suppression identity (typically site-identifier + device +
// failure-dimension). Within a Limiter-configured cooldown, repeats of
// the same key are suppressed; the first occurrence and the post-cooldown
// re-emit pass through to the wrapped slog.Logger.
//
// Walkers MUST tolerate a nil Params.WarnLimiter — pass nil through to
// the inner logger's WarnContext rather than panicking.
type WarnLimiter interface {
	Warn(ctx context.Context, key, msg string, attrs ...any) bool
}

// Params holds the resolved connection parameters for one SNMP walk.
// The caller (discovery loop) resolves credentials to concrete values via
// os.Getenv before building a Params; this package never reads env vars.
// Protocol names follow the config schema: "SHA", "SHA-256", "SHA-384",
// "SHA-512", "MD5" for auth; "AES", "AES-192", "AES-256", "DES" for priv.
type Params struct {
	IP      net.IP
	Port    uint16
	Timeout time.Duration

	// SNMPv2c fields.
	//
	// Community is held as []byte so callers can overwrite it with zeros via
	// Zeroize once the discovery cycle has finished with this Params. See
	// docs/operator/security.md for the threat model and limitations.
	Community []byte

	// SNMPv3 fields.
	//
	// AuthKey and PrivKey are held as []byte for the same reason as Community
	// above. They are converted to string at the gosnmp boundary in
	// buildClient; that conversion makes an immutable copy that Zeroize
	// cannot reach.
	V3          bool
	Username    string
	AuthProto   string // "SHA" | "SHA-256" | "SHA-384" | "SHA-512" | "MD5" | ""
	AuthKey     []byte
	PrivProto   string // "AES" | "AES-192" | "AES-256" | "DES" | ""
	PrivKey     []byte
	ContextName string

	// Retries is the number of SNMP retries per request. 0 means no retries.
	Retries int

	// MaxVlans caps the number of VLANs walked by the FDB module's
	// VLAN community-string path. 0 means use the module default (100).
	MaxVlans int

	// Vendor is the canonical vendor string for the target device (e.g.
	// "cisco", "juniper", "nokia", "arista"). Populated by the discovery
	// loop after sys-group resolution, before any per-module walks run.
	// Empty string means vendor is unknown. Used by modules that dispatch
	// on vendor-specific MIB tables (e.g. BGP4-V2 fallback).
	Vendor string

	// UseBGPV2MIB enables the BGP4-V2-MIB / vendor BGP peer-table walkers
	// that surface IPv6 sessions. When false the BGP module uses only the
	// RFC 4273 IPv4-only path. Defaults to true via config.applyDefaults.
	UseBGPV2MIB bool

	// WalkerMetrics is the observability sink that protocol modules use to
	// record walker outcomes (e.g. edges, mib_unimplemented, walker_drift).
	// May be nil — walkers must drop the increment in that case rather than
	// panic. Replaces the prior package-global counter pointer that lived in
	// internal/discovery/bgp/. See WalkerMetrics interface above.
	WalkerMetrics WalkerMetrics

	// WarnLimiter rate-limits chronic per-cycle Warn emissions (issue #16).
	// May be nil — walkers must fall back to a direct slog.Warn in that
	// case. See WarnLimiter interface above.
	WarnLimiter WarnLimiter

	// PanicReporter is the nil-tolerant observer a walker calls when it
	// recovers a panic in one of its own background goroutines (the FDB
	// per-VLAN walkers spawn one goroutine per VLAN). It mirrors the
	// WalkerMetrics/WarnLimiter injection seam: the discovery layer must not
	// import the prometheus client or internal/app, so the production wiring
	// in internal/app passes a closure that bumps
	// network_topology_panics_total{site}. site is one of the closed,
	// low-cardinality strings documented on metrics.PanicsRecoveredTotal
	// (FDB uses "fdb_vlan_walk"). May be nil — callers MUST nil-check before
	// calling (the recover still happens; only the counter bump is skipped),
	// so a one-off walk without metrics wiring does not panic.
	PanicReporter func(site string)

	// CredentialProfile is the name of the credential profile that won the
	// system-group walk for this target (set in cycle.go from the profileName
	// WalkSystemWithCredentials returns). It is part of the session-pool key
	// (IP, CredentialProfile) so that a credential change for a target yields a
	// different pool entry and InvalidateProfile can evict all sessions for a
	// rotated profile (issue #83). Empty when no pool is in use — the default
	// fresh-open path ignores it.
	CredentialProfile string

	// Pool, when non-nil, opts this Params into the per-target SNMP session
	// pool (issue #83): Acquire checks out a reused session keyed by
	// (IP, CredentialProfile) instead of dialing a fresh socket per walk, and
	// the release func returns it to the pool rather than closing it. When nil
	// (the default), Acquire is byte-identical to Open + Conn.Close — a fresh
	// session per walk, closed on release. The pool itself is concurrency-safe;
	// a single pooled session is only ever handed to one caller at a time.
	Pool *SessionPool
}

// System group OIDs fetched as scalars via SNMP GET (RFC 3418).
const (
	oidSysDescr    = "1.3.6.1.2.1.1.1.0"
	oidSysObjectID = "1.3.6.1.2.1.1.2.0"
	oidSysUpTime   = "1.3.6.1.2.1.1.3.0"
	oidSysName     = "1.3.6.1.2.1.1.5.0"
)

// Pre-built dotted forms for the PDU switch in Walk. Concatenating at the
// call site in a hot loop allocates a new string on every PDU.
const (
	dotOIDSysDescr    = "." + oidSysDescr
	dotOIDSysObjectID = "." + oidSysObjectID
	dotOIDSysUpTime   = "." + oidSysUpTime
	dotOIDSysName     = "." + oidSysName
)

// System-walk anomaly reasons for network_topology_system_walk_anomaly_total.
// This is a CLOSED set of exactly two values; the metric carries only the
// reason label (no device, IP, or sysObjectID — those would be high
// cardinality). See WalkerMetrics.RecordSystemWalkAnomaly (issue #101).
const (
	// systemWalkAnomalyEmptySysName fires when sysName is empty/garbage and the
	// device ID falls back to the management IP string.
	systemWalkAnomalyEmptySysName = "empty_sysname"
	// systemWalkAnomalyUnknownVendor fires when the sysObjectID does not resolve
	// to a known vendor (Vendor stays "unknown").
	systemWalkAnomalyUnknownVendor = "unknown_vendor"
)

// recordSystemWalkAnomaly routes a system-walk anomaly to the injected
// WalkerMetrics sink, tolerating a nil Params.WalkerMetrics by dropping the
// increment (mirrors the snmputil.Record*WalkerOutcome nil-check pattern so the
// discovery layer never panics when observability is not wired).
func recordSystemWalkAnomaly(p Params, reason string) {
	if p.WalkerMetrics == nil {
		return
	}
	p.WalkerMetrics.RecordSystemWalkAnomaly(reason)
}

// Open creates and connects a gosnmp session from the given parameters.
// Callers must close the underlying connection (client.Conn.Close()) when done.
// Each call returns a fresh *GoSNMP; callers must not share instances across
// goroutines (GoSNMP is not goroutine-safe).
func Open(p Params) (*g.GoSNMP, error) {
	client := buildClient(p)
	if err := client.Connect(); err != nil {
		return nil, fmt.Errorf("snmp connect %s: %w", p.IP, err)
	}
	openCount.Add(1)
	return client, nil
}

// openCount counts successful fresh-session opens (socket dials) across the
// package. Each Open == one new UDP session, which is the quantity that drives
// conntrack/socket churn (issue #83). Exposed via OpenCount for the
// session-pool open-reduction test; it is a cheap atomic increment with no
// effect on production behaviour.
var openCount atomic.Int64

// OpenCount returns the cumulative number of fresh SNMP session opens since
// process start. Used by the session-pool ≥80%-open-reduction test (issue #83)
// to compare pooled vs unpooled dial counts deterministically without raw
// syscall counting. Not part of the operational API.
func OpenCount() int64 { return openCount.Load() }

// Acquire returns an SNMP session for p plus a release func the caller MUST
// defer, passing the walk's final error (nil on success). It is the single
// seam every walker uses to obtain a session (issue #83), replacing the
// historical `client, err := Open(p); defer client.Conn.Close()` pair.
//
// When p carries no pool (p.Pool == nil, the default), Acquire opens a fresh
// session via Open and the returned release closes its connection regardless
// of the error passed — this path is byte-identical to the pre-#83
// Open + Conn.Close behaviour, which is the single most important property of
// this change and is protected by a test.
//
// When a pool is present, Acquire checks out a session reused across this
// target's sequential module walks, keyed by (p.IP, p.CredentialProfile).
// release inspects the walk error (#164): a connection-level error closes and
// evicts the session (reason connection_error) so the next acquire dials
// fresh; any other outcome returns it to the pool WITHOUT closing it. release
// is always non-nil; on the error path it is a no-op so callers may
// `defer release(...)` unconditionally after the error check.
//
// The idiomatic call shape uses a named return so the deferred release sees
// the final error:
//
//	func Walk(...) (_ []discovery.Edge, retErr error) {
//		client, release, err := snmputil.Acquire(p)
//		if err != nil { return nil, err }
//		defer func() { release(retErr) }()
//		...
//	}
//
// gosnmp is not goroutine-safe, but a pooled session is only ever handed to
// one caller at a time (checkout/release), and a target's modules run
// sequentially within its per-device goroutine (see internal/app/cycle.go), so
// reuse across that target's modules within a cycle is safe.
func Acquire(p Params) (*g.GoSNMP, func(error), error) {
	if p.Pool == nil {
		client, err := Open(p)
		if err != nil {
			return nil, func(error) {}, err
		}
		return client, func(error) { _ = client.Conn.Close() }, nil
	}
	return p.Pool.acquire(p)
}

type rateLimiterKey struct{}
type rateLimitWaitObserverKey struct{}

// ContextWithRateLimiter returns a child context carrying a per-target SNMP
// request-rate limiter. BulkWalk consults the limiter before issuing each walk
// so that the steady-state PDU rate against an individual device cannot exceed
// the operator-configured ceiling, preventing a self-DoS of the device's SNMP
// daemon (issue #72). A nil limiter returns the original context unchanged
// (the default — zero overhead, unlimited rate). The limiter is injected per
// device per cycle in internal/app/cycle.go; it deliberately lives behind a
// context seam (mirroring ContextWithDecodeIssueReporter) so that this package
// stays free of any prometheus-client dependency.
func ContextWithRateLimiter(ctx context.Context, l *rate.Limiter) context.Context {
	if l == nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimiterKey{}, l)
}

func rateLimiterFrom(ctx context.Context) *rate.Limiter {
	l, _ := ctx.Value(rateLimiterKey{}).(*rate.Limiter)
	return l
}

// ContextWithRateLimitWaitObserver returns a child context carrying a callback
// invoked with the duration BulkWalk spent blocked on the per-target rate
// limiter. cycle.go wires this to the network_topology_snmp_rate_limit_wait_seconds
// histogram; carrying it as a context callback (rather than a metrics handle)
// keeps the prometheus-client import out of this package. A nil callback
// returns the original context unchanged.
func ContextWithRateLimitWaitObserver(ctx context.Context, fn func(time.Duration)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, rateLimitWaitObserverKey{}, fn)
}

func rateLimitWaitObserverFrom(ctx context.Context) func(time.Duration) {
	fn, _ := ctx.Value(rateLimitWaitObserverKey{}).(func(time.Duration))
	return fn
}

type panicReporterKey struct{}

// ContextWithPanicReporter returns a child context carrying a nil-tolerant
// panic reporter for the raw transport goroutines this package spawns
// (BulkWalk's BulkWalkAll/WalkAll goroutines and Walk's Get goroutine). Those
// goroutines run on their own stacks, so neither the per-device recover in
// internal/app/cycle.go nor app.recoverGoroutine protects them: a panic while
// decoding a hostile/truncated PDU would otherwise crash the whole process on
// the most-exposed path. The deferred recover in each goroutine calls this
// reporter with site "snmp_walk" so the panic is counted as
// network_topology_panics_total{site="snmp_walk"} and converts the walk into a
// normal error rather than a crash.
//
// Like ContextWithRateLimiter/ContextWithDecodeIssueReporter it lives behind a
// context seam so this package stays free of any prometheus-client dependency;
// the production wiring in internal/app/cycle.go injects a closure that bumps
// metrics.PanicsRecoveredTotal. A nil reporter returns the original context
// unchanged (the recover still happens; only the counter bump is skipped).
func ContextWithPanicReporter(ctx context.Context, fn func(site string)) context.Context {
	if fn == nil {
		return ctx
	}
	return context.WithValue(ctx, panicReporterKey{}, fn)
}

func panicReporterFrom(ctx context.Context) func(site string) {
	fn, _ := ctx.Value(panicReporterKey{}).(func(site string))
	return fn
}

// reportWalkPanic is the shared recover body for the raw transport goroutines.
// It recovers r (does NOT re-panic), reports it through the nil-tolerant
// context seam under site "snmp_walk", and returns a sentinel error the
// goroutine sends on its result channel so the walk returns a normal error
// instead of crashing. Callers invoke it as:
//
//	defer func() {
//		if r := recover(); r != nil {
//			ch <- walkResult{nil, reportWalkPanic(ctx, r)}
//		}
//	}()
func reportWalkPanic(ctx context.Context, r any) error {
	slog.Error("snmp: transport goroutine panicked; recovered",
		"panic", r, "stack", string(debug.Stack()))
	if fn := panicReporterFrom(ctx); fn != nil {
		fn("snmp_walk")
	}
	return fmt.Errorf("snmp walk panic: %v", r)
}

// BulkWalk walks rootOID using BulkWalkAll, falling back to WalkAll for
// devices that reject GetBulk PDUs (some older IOS/JunOS revisions, some AP
// controllers). ctx is checked before each attempt and wraps the blocking
// BulkWalkAll/WalkAll calls so that context cancellation interrupts a
// mid-walk UDP read via SetDeadline.
func BulkWalk(ctx context.Context, client *g.GoSNMP, rootOID string) ([]g.SnmpPDU, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	// Per-target rate limit (issue #72): block here until a token is available
	// before issuing the walk. Wait is ctx-aware — on cancellation/deadline it
	// returns promptly with an error and we do NOT issue the walk. When no
	// limiter is installed (the default), this is a no-op with zero overhead.
	if l := rateLimiterFrom(ctx); l != nil {
		start := time.Now()
		if err := l.Wait(ctx); err != nil {
			return nil, err
		}
		if obs := rateLimitWaitObserverFrom(ctx); obs != nil {
			obs(time.Since(start))
		}
	}

	type walkResult struct {
		pdus []g.SnmpPDU
		err  error
	}

	ch := make(chan walkResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch <- walkResult{nil, reportWalkPanic(ctx, r)}
			}
		}()
		pdus, err := client.BulkWalkAll(rootOID)
		ch <- walkResult{pdus, err}
	}()
	var bulkErr error
	select {
	case r := <-ch:
		if r.err == nil {
			return r.pdus, nil
		}
		bulkErr = r.err
	case <-ctx.Done():
		_ = client.Conn.SetDeadline(time.Now()) // interrupt pending UDP read
		<-ch                                    // wait for goroutine to exit
		return nil, ctx.Err()
	}

	if err := ctx.Err(); err != nil {
		return nil, err
	}
	_ = bulkErr // checked above; fall through to WalkAll

	ch2 := make(chan walkResult, 1)
	go func() {
		defer func() {
			if r := recover(); r != nil {
				ch2 <- walkResult{nil, reportWalkPanic(ctx, r)}
			}
		}()
		pdus, err := client.WalkAll(rootOID)
		ch2 <- walkResult{pdus, err}
	}()
	select {
	case r := <-ch2:
		return r.pdus, r.err
	case <-ctx.Done():
		_ = client.Conn.SetDeadline(time.Now()) // interrupt pending UDP read
		<-ch2                                   // wait for goroutine to exit
		return nil, ctx.Err()
	}
}

// Walk retrieves the SNMP SYSTEM group from the device at p.IP and returns a
// Device record. Creates a short-lived session per snmp_exporter convention.
//
// Returns a non-nil *Device and a non-nil error when the SNMP session
// succeeds but the device reports unexpected values — the partial Device is
// usable for inventory. Returns (nil, err) only on connection / auth failure.
func Walk(ctx context.Context, p Params) (*discovery.Device, error) {
	client, err := Open(p)
	if err != nil {
		return nil, err
	}
	defer func() { _ = client.Conn.Close() }()

	// Respect ctx cancellation for the connect+GET window.
	done := make(chan struct{})
	var result *g.SnmpPacket
	var getErr error
	go func() {
		defer close(done)
		defer func() {
			if r := recover(); r != nil {
				result = nil
				getErr = reportWalkPanic(ctx, r)
			}
		}()
		result, getErr = client.Get([]string{
			oidSysDescr,
			oidSysObjectID,
			oidSysUpTime,
			oidSysName,
		})
	}()

	select {
	case <-ctx.Done():
		// Interrupt the blocked Get so the goroutine exits before the deferred
		// Conn.Close runs. Without this, Close races with the goroutine's read.
		_ = client.Conn.SetDeadline(time.Now())
		<-done
		return nil, ctx.Err()
	case <-done:
	}

	if getErr != nil {
		return nil, fmt.Errorf("snmp get system group %s: %w", p.IP, getErr)
	}

	dev := &discovery.Device{
		ID:     p.IP.String(), // fallback until sysName is read
		Vendor: "unknown",
	}

	// Track whether sysName resolved to a usable device ID; if not, dev.ID
	// stays at the management-IP fallback and we emit an empty_sysname anomaly
	// after the loop (the PDU may be present-but-garbage or entirely absent —
	// both leave the IP fallback in place).
	gotSysName := false
	for _, pdu := range result.Variables {
		switch pdu.Name {
		case dotOIDSysName:
			// Clean once, derive both forms from the one pass rather than
			// running NormaliseName and NormaliseNamePreserveCase separately
			// over the same raw value. dev.ID therefore lowercases after the
			// 255-byte truncation rather than before it, unlike NormaliseName
			// itself — only observable for a sysName within a few bytes of
			// the cap whose case-folded form changes byte length, which real
			// device hostnames don't.
			if cleaned := NormaliseNamePreserveCase(PDUString(pdu)); cleaned != "" {
				dev.SysName = cleaned
				dev.ID = strings.ToLower(cleaned)
				gotSysName = true
			}
		case dotOIDSysDescr:
			dev.OSVersion = normalizeSysDescr(PDUString(pdu))
		case dotOIDSysObjectID:
			dev.Vendor = VendorFromObjectID(pduOID(pdu))
		case dotOIDSysUpTime:
			// sysUpTime wraps to zero after ~497 days; callers cannot distinguish wrap from reboot.
			if ticks, ok := PDUIntStrict(pdu); ok && ticks >= 0 {
				dev.Uptime = time.Duration(ticks) * 10 * time.Millisecond
				uptimeSec := dev.Uptime.Seconds()
				if uptimeSec < 86400 {
					slog.Debug("snmp: sysUpTime below 24h; may be recent reboot or 497-day counter wrap",
						"device", dev.ID, "uptime_seconds", uptimeSec)
				}
			}
		}
	}

	// Observe the two silently-degrading outcomes (issue #101). Both leave a
	// usable Device for inventory but weaken downstream behaviour: an empty
	// sysName means the device ID is the management IP (unstable across
	// re-addressing), and an unknown vendor means vendorSpecFor("unknown")
	// returns nil so the vendor-specific BGP4-V2 walker is skipped.
	if !gotSysName {
		recordSystemWalkAnomaly(p, systemWalkAnomalyEmptySysName)
	}
	if dev.Vendor == "unknown" {
		recordSystemWalkAnomaly(p, systemWalkAnomalyUnknownVendor)
	}

	return dev, nil
}

// sysDescrVersionRe matches the first version-like token (digits and dots) in a
// sysDescr string, e.g. "15.2.4" in a Cisco IOS sysDescr.
var sysDescrVersionRe = regexp.MustCompile(`\d+\.\d+[\.\d]*`)

// normalizeSysDescr collapses the sysDescr string to the first version token
// (M.N or M.N.P) to prevent one Prometheus series per patch release across
// a device fleet. Using the full sysDescr as a label would multiply series
// count by the number of distinct firmware versions deployed. Falls back to
// the first 64 bytes of raw sysDescr when no version token is found.
func normalizeSysDescr(s string) string {
	// Extract first M.N or M.N.P version token
	if m := sysDescrVersionRe.FindString(s); m != "" {
		return m
	}
	if len(s) > 64 {
		n := 64
		for n > 0 && !utf8.RuneStart(s[n]) {
			n--
		}
		return s[:n]
	}
	return s
}

// buildClient constructs a *gosnmp.GoSNMP from the resolved parameters.
// Each call returns a fresh struct; callers must not share instances across
// goroutines (gosnmp is not goroutine-safe on the same *GoSNMP).
func buildClient(p Params) *g.GoSNMP {
	port := p.Port
	if port == 0 {
		port = 161
	}
	timeout := p.Timeout
	if timeout == 0 {
		timeout = 10 * time.Second
	}

	retries := p.Retries
	if retries == 0 {
		retries = 1 // default to 1 retry for backwards compatibility
	}
	client := &g.GoSNMP{
		Target:    p.IP.String(),
		Port:      port,
		Timeout:   timeout,
		Retries:   retries,
		MaxOids:   g.MaxOids,
		Transport: "udp",
	}

	if p.V3 {
		authProto := parseAuthProto(p.AuthProto)
		privProto := parsePrivProto(p.PrivProto)
		client.Version = g.Version3
		client.SecurityModel = g.UserSecurityModel
		client.MsgFlags = authPrivMsgFlags(authProto, privProto)
		// Conversion to string here makes an immutable copy of the credential
		// bytes that gosnmp keeps for the lifetime of the session. Params.Zeroize
		// cannot reach into gosnmp's internal copy; the docs/operator/security.md
		// threat model documents this gap.
		client.SecurityParameters = &g.UsmSecurityParameters{
			UserName:                 p.Username,
			AuthenticationProtocol:   authProto,
			AuthenticationPassphrase: string(p.AuthKey),
			PrivacyProtocol:          privProto,
			PrivacyPassphrase:        string(p.PrivKey),
		}
		if p.ContextName != "" {
			client.ContextName = p.ContextName
		}
	} else {
		client.Version = g.Version2c
		// See comment on the V3 branch: string() copies the bytes into
		// gosnmp's internal state, outside the reach of Zeroize.
		client.Community = string(p.Community)
	}

	return client
}

func authPrivMsgFlags(auth g.SnmpV3AuthProtocol, priv g.SnmpV3PrivProtocol) g.SnmpV3MsgFlags {
	switch {
	case auth == g.NoAuth:
		return g.NoAuthNoPriv
	case priv == g.NoPriv:
		return g.AuthNoPriv
	default:
		return g.AuthPriv
	}
}

// parseAuthProto maps a config-level string to a gosnmp auth protocol constant.
// An empty string means no auth configured; unrecognized values default to SHA.
func parseAuthProto(s string) g.SnmpV3AuthProtocol {
	switch s {
	case "":
		return g.NoAuth
	case "MD5":
		return g.MD5
	case "SHA-256":
		return g.SHA256
	case "SHA-384":
		return g.SHA384
	case "SHA-512":
		return g.SHA512
	default: // config validator should have caught unrecognized values
		return g.SHA
	}
}

// parsePrivProto maps a config-level string to a gosnmp priv protocol constant.
// An empty string means no priv configured; unrecognized values default to AES.
func parsePrivProto(s string) g.SnmpV3PrivProtocol {
	switch s {
	case "":
		return g.NoPriv
	case "DES":
		return g.DES
	case "AES-192":
		return g.AES192
	case "AES-256":
		return g.AES256
	default: // config validator should have caught unrecognized values
		return g.AES
	}
}

func pduOID(pdu g.SnmpPDU) string {
	if oid, ok := pdu.Value.(string); ok {
		return oid
	}
	return ""
}

// enterprisePrefix is one entry in the ordered enterprisePrefixes slice.
// Entries are checked in order; longest (most-specific) prefixes should appear
// first so that a more-specific vendor match wins over a shorter prefix.
type enterprisePrefix struct {
	prefix string
	vendor string
}

// enterprisePrefixes is the ordered list of IANA enterprise OID prefixes.
// Enterprise numbers are disjoint — no entry is a prefix of another, so
// iteration order does not affect correctness. Order is fixed for determinism.
//
// Source: IANA Enterprise Numbers registry
// (https://www.iana.org/assignments/enterprise-numbers)
var enterprisePrefixes = []enterprisePrefix{
	{prefix: "1.3.6.1.4.1.9.", vendor: "cisco"},
	{prefix: "1.3.6.1.4.1.11.", vendor: "hp"},
	{prefix: "1.3.6.1.4.1.14988.", vendor: "mikrotik"},
	{prefix: "1.3.6.1.4.1.2636.", vendor: "juniper"},
	{prefix: "1.3.6.1.4.1.12356.", vendor: "fortinet"},
	{prefix: "1.3.6.1.4.1.8072.", vendor: "net-snmp"},
	{prefix: "1.3.6.1.4.1.890.", vendor: "zyxel"},
	{prefix: "1.3.6.1.4.1.6527.", vendor: "nokia"},
	{prefix: "1.3.6.1.4.1.25506.", vendor: "huawei"},
	{prefix: "1.3.6.1.4.1.2011.", vendor: "huawei"},
	{prefix: "1.3.6.1.4.1.4526.", vendor: "netgear"},
	{prefix: "1.3.6.1.4.1.1916.", vendor: "extreme"},
	{prefix: "1.3.6.1.4.1.1991.", vendor: "brocade"},
	{prefix: "1.3.6.1.4.1.1872.", vendor: "alteon"},
	{prefix: "1.3.6.1.4.1.3375.", vendor: "f5"},
	{prefix: "1.3.6.1.4.1.25461.", vendor: "paloalto"},
	{prefix: "1.3.6.1.4.1.30065.", vendor: "arista"},
	{prefix: "1.3.6.1.4.1.40310.", vendor: "cumulus"},
	{prefix: "1.3.6.1.4.1.6876.", vendor: "vmware"},
	{prefix: "1.3.6.1.4.1.20301.", vendor: "ubiquiti"},
	{prefix: "1.3.6.1.4.1.41112.", vendor: "ubiquiti"},
	{prefix: "1.3.6.1.4.1.674.", vendor: "dell"},
	{prefix: "1.3.6.1.4.1.6486.", vendor: "alcatel-lucent"},
	{prefix: "1.3.6.1.4.1.3076.", vendor: "altiga"},
	{prefix: "1.3.6.1.4.1.232.", vendor: "hpe"},
	{prefix: "1.3.6.1.4.1.236.", vendor: "samsung"},
	{prefix: "1.3.6.1.4.1.3417.", vendor: "bluecoat"},
	{prefix: "1.3.6.1.4.1.5624.", vendor: "enterasys"},
	{prefix: "1.3.6.1.4.1.18928.", vendor: "aerohive"},
	{prefix: "1.3.6.1.4.1.14179.", vendor: "cisco-wlc"},
	{prefix: "1.3.6.1.4.1.45.", vendor: "baynetworks"},
}

// VendorFromObjectID maps the IANA enterprise number prefix of a sysObjectID
// to a canonical vendor string. Only the enterprise prefix matters; the
// remainder encodes model/platform details that belong in a separate
// lookup if needed. Exported for modules that dispatch on vendor (e.g. BGP4-V2
// fallback) and may need to resolve a vendor when Params.Vendor is empty.
//
// Source: IANA Enterprise Numbers registry
// (https://www.iana.org/assignments/enterprise-numbers)
func VendorFromObjectID(oid string) string {
	if oid == "" {
		return "unknown"
	}
	// Strip leading dot so both ".1.3.6.1.4.1.9." and "1.3.6.1.4.1.9." match.
	oid = strings.TrimPrefix(oid, ".")
	for _, e := range enterprisePrefixes {
		if strings.HasPrefix(oid, e.prefix) {
			return e.vendor
		}
	}
	return "unknown"
}
