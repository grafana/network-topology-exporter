// Package otlp implements an OTLP push output for the network topology
// exporter. It serialises topology graphs as OTLP metrics and change events as
// OTLP log records, then exports them to a configurable endpoint (e.g. a
// Grafana Alloy otelcol.receiver.otlp receiver).
//
// Serialisation and transport are handled by the official OpenTelemetry Go
// SDK: a metric.MeterProvider drives the topology gauges and a log.LoggerProvider
// drives the change records. No hand-rolled OTLP/JSON wire encoding lives in
// this package any more — the SDK owns the proto3 mapping, the schema URL, and
// the HTTP/gRPC transport (including retry/backoff).
package otlp

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync/atomic"
	"time"
	"unicode/utf8"

	"go.opentelemetry.io/otel/attribute"
	otellog "go.opentelemetry.io/otel/log"
	otelmetric "go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/sdk/log"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/resource"

	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploggrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlplog/otlploghttp"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetricgrpc"
	"go.opentelemetry.io/otel/exporters/otlp/otlpmetric/otlpmetrichttp"

	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/graph"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/otelx"
)

// Config holds the settings for the OTLP exporter.
type Config struct {
	// Endpoint is the base URL of the OTLP receiver, e.g. "http://alloy:4318"
	// for HTTP or "alloy:4317" (or "http://alloy:4317") for gRPC.
	Endpoint string

	// Timeout caps each individual export. Defaults to 10s when zero.
	Timeout time.Duration

	// InstanceID is the value emitted as the OTLP resource attribute
	// service.instance.id. When empty, falls back to os.Hostname() — which
	// is the container ID under Docker/Kubernetes and is therefore not stable
	// across pod restarts. Federation spoke deployments should pass the
	// configured spoke_id here so the instance identity is stable.
	InstanceID string

	// Protocol selects the OTLP transport: otelx.ProtocolHTTP (default) or
	// otelx.ProtocolGRPC. The empty value is treated as HTTP for backward
	// compatibility with deployments that only set Endpoint.
	//
	// The payload encoding is always protobuf: the OpenTelemetry Go SDK's OTLP
	// exporters only implement protobuf (Content-Type application/x-protobuf
	// for HTTP, protobuf framing for gRPC); there is no OTLP/JSON exporter
	// upstream. This is a wire-format change from the pre-v1.5.0 hand-rolled
	// OTLP/JSON path — receivers must accept protobuf (the OTLP default).
	Protocol otelx.Protocol
}

// Exporter pushes topology data to an OTLP endpoint via the OpenTelemetry SDK.
// It owns a MeterProvider and a LoggerProvider and exposes PushGraph /
// PushChanges helpers that record measurements / emit log records and then
// force-flush them through the configured OTLP exporter.
type Exporter struct {
	meterProvider  *sdkmetric.MeterProvider
	loggerProvider *log.LoggerProvider

	// latest holds the most recent graph pushed via PushGraph. The edge/device
	// metrics are OBSERVABLE gauges whose callbacks read this pointer on every
	// collection, so each export reflects exactly the current graph — a removed
	// edge/device is simply not observed and therefore disappears at the
	// receiver. A synchronous gauge would instead retain every attribute-set
	// ever recorded under cumulative temporality, leaving phantom links in the
	// topology at the collector indefinitely.
	latest atomic.Pointer[discovery.Graph]
	logger otellog.Logger
}

// edgeAttrs builds the OTLP attribute set for one topology edge.
func edgeAttrs(edge discovery.Edge) []attribute.KeyValue {
	attrs := []attribute.KeyValue{
		attribute.String("src_device", sanitizeUTF8(edge.SrcDevice)),
		attribute.String("src_port", sanitizeUTF8(edge.SrcPort)),
		attribute.String("dst_device", sanitizeUTF8(edge.DstDevice)),
		attribute.String("dst_port", sanitizeUTF8(edge.DstPort)),
		attribute.String("proto", sanitizeUTF8(string(edge.DiscoveryProto))),
		attribute.String("link_kind", sanitizeUTF8(string(edge.LinkKind))),
		attribute.String("direction", sanitizeUTF8(string(edge.Direction))),
		attribute.String("confidence", sanitizeUTF8(string(edge.Confidence))),
		attribute.String("adjacency", sanitizeUTF8(string(edge.Adjacency))),
		attribute.String("precedence_rank", strconv.Itoa(edge.PrecedenceRank)),
	}
	// Omitted (not "0") when unresolved, same as vendor/model/os_version/site
	// below: 0 is not a valid IF-MIB ifIndex, so emitting it as a string would
	// read as real data. New in v1.1 (OTLP schema — docs/otlp-schema.md) — see
	// docs/proposals/snmp-exporter-label-alignment.md §4.
	if edge.SrcIfIndex != 0 {
		attrs = append(attrs, attribute.String("src_if_index", strconv.Itoa(edge.SrcIfIndex)))
	}
	if edge.DstIfIndex != 0 {
		attrs = append(attrs, attribute.String("dst_if_index", strconv.Itoa(edge.DstIfIndex)))
	}
	for k, v := range edge.Metadata {
		attrs = append(attrs, attribute.String(metadataAttrPrefix+k, sanitizeUTF8(v)))
	}
	return attrs
}

// deviceAttrs builds the OTLP attribute set for one device.
func deviceAttrs(dev discovery.Device) []attribute.KeyValue {
	// "device_id" is the aligned name — matches the Prometheus label name,
	// see docs/proposals/snmp-exporter-label-alignment.md §5.1.
	//
	// "device" is DEPRECATED as of this release: it is emitted alongside
	// "device_id" (same value) only for one release's overlap window so
	// existing OTLP consumers built against the pre-#227 schema keep working
	// unchanged while they migrate. Per docs/operator/stability.md's
	// deprecation policy (minimum one full minor release of overlap), do not
	// remove this attribute in this release — it is scheduled for removal in
	// the next MAJOR version, per docs/otlp-schema.md's versioning policy.
	// See CHANGELOG.md's "Deprecated" entry for this release.
	attrs := []attribute.KeyValue{
		attribute.String("device_id", sanitizeUTF8(dev.ID)),
		attribute.String("device", sanitizeUTF8(dev.ID)),
	}
	if dev.SysName != "" {
		attrs = append(attrs, attribute.String("sys_name", sanitizeUTF8(dev.SysName)))
	}
	if dev.Vendor != "" {
		attrs = append(attrs, attribute.String("vendor", sanitizeUTF8(dev.Vendor)))
	}
	if dev.Model != "" {
		attrs = append(attrs, attribute.String("model", sanitizeUTF8(dev.Model)))
	}
	if dev.OSVersion != "" {
		attrs = append(attrs, attribute.String("os_version", sanitizeUTF8(dev.OSVersion)))
	}
	if dev.Site != "" {
		attrs = append(attrs, attribute.String("site", sanitizeUTF8(dev.Site)))
	}
	return attrs
}

const metadataAttrPrefix = "network.topology."

// New constructs an Exporter from cfg, wiring an OTLP metric exporter into a
// MeterProvider and an OTLP log exporter into a LoggerProvider. A zero Timeout
// is replaced with 10 seconds; an empty Protocol defaults to HTTP. The payload
// encoding is always protobuf (the only encoding the OTel Go SDK exporters
// implement).
//
// New returns an error when the OTLP exporters cannot be constructed.
func New(ctx context.Context, cfg Config) (*Exporter, error) {
	if cfg.Timeout == 0 {
		cfg.Timeout = 10 * time.Second
	}
	if cfg.Protocol == "" {
		cfg.Protocol = otelx.ProtocolHTTP
	}
	if !cfg.Protocol.Valid() {
		return nil, fmt.Errorf("otlp: unsupported protocol %q (want http or grpc)", cfg.Protocol)
	}

	res, err := otelx.NewResource(ctx, cfg.InstanceID)
	if err != nil {
		return nil, fmt.Errorf("otlp: build resource: %w", err)
	}

	metricExp, err := newMetricExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otlp: build metric exporter: %w", err)
	}
	logExp, err := newLogExporter(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("otlp: build log exporter: %w", err)
	}

	// A periodic reader wraps the OTLP metric exporter; we flush explicitly from
	// PushGraph so the export cadence stays driven by the discovery loop (the
	// heartbeat_cycles semantics in app.go), exactly as the hand-rolled
	// implementation did.
	reader := sdkmetric.NewPeriodicReader(metricExp)
	return assemble(res, reader, log.NewSimpleProcessor(logExp))
}

// assemble builds an Exporter from an already-constructed resource, metric
// Reader, and log Processor. It is the shared seam between the production
// constructor New and the in-memory test constructor (see export_test.go), so
// tests can drive the exact same PushGraph/PushChanges code paths against
// in-memory readers/exporters without any OTLP transport.
func assemble(res *resource.Resource, reader sdkmetric.Reader, proc log.Processor) (*Exporter, error) {
	mp := sdkmetric.NewMeterProvider(
		sdkmetric.WithResource(res),
		sdkmetric.WithReader(reader),
	)
	lp := log.NewLoggerProvider(
		log.WithResource(res),
		log.WithProcessor(proc),
	)

	e := &Exporter{
		meterProvider:  mp,
		loggerProvider: lp,
		logger:         lp.Logger(otelx.ScopeName),
	}

	meter := mp.Meter(otelx.ScopeName)
	// Observable gauges: the callback reports exactly the current graph on each
	// collection, so removed edges/devices vanish at the receiver instead of
	// lingering as phantom topology under cumulative temporality.
	if _, err := meter.Float64ObservableGauge("network_topology_edge_info",
		otelmetric.WithDescription("Topology edge presence; value 1 with edge attributes."),
		otelmetric.WithFloat64Callback(func(_ context.Context, o otelmetric.Float64Observer) error {
			if g := e.latest.Load(); g != nil {
				for _, edge := range g.Edges {
					o.Observe(1.0, otelmetric.WithAttributes(edgeAttrs(edge)...))
				}
			}
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("otlp: create edge gauge: %w", err)
	}
	if _, err := meter.Float64ObservableGauge("network_topology_device_info",
		otelmetric.WithDescription("Discovered device presence; value 1 with device attributes."),
		otelmetric.WithFloat64Callback(func(_ context.Context, o otelmetric.Float64Observer) error {
			if g := e.latest.Load(); g != nil {
				for _, dev := range g.Devices {
					o.Observe(1.0, otelmetric.WithAttributes(deviceAttrs(dev)...))
				}
			}
			return nil
		}),
	); err != nil {
		return nil, fmt.Errorf("otlp: create device gauge: %w", err)
	}

	return e, nil
}

// newMetricExporter builds the OTLP metric exporter for the configured
// protocol. The payload encoding is always protobuf (the only encoding the
// OTel Go SDK exporters implement).
func newMetricExporter(ctx context.Context, cfg Config) (sdkmetric.Exporter, error) {
	switch cfg.Protocol {
	case otelx.ProtocolGRPC:
		opts := []otlpmetricgrpc.Option{otlpmetricgrpc.WithTimeout(cfg.Timeout)}
		host, insecure := otelx.EndpointHostInsecure(cfg.Endpoint)
		if host != "" {
			opts = append(opts, otlpmetricgrpc.WithEndpoint(host))
		}
		if insecure {
			opts = append(opts, otlpmetricgrpc.WithInsecure())
		}
		return otlpmetricgrpc.New(ctx, opts...)
	default: // otelx.ProtocolHTTP
		opts := []otlpmetrichttp.Option{otlpmetrichttp.WithTimeout(cfg.Timeout)}
		if u, insecure, ok := otelx.EndpointURL(cfg.Endpoint); ok {
			opts = append(opts, otlpmetrichttp.WithEndpointURL(u))
			if insecure {
				opts = append(opts, otlpmetrichttp.WithInsecure())
			}
		}
		return otlpmetrichttp.New(ctx, opts...)
	}
}

// newLogExporter builds the OTLP log exporter for the configured protocol.
func newLogExporter(ctx context.Context, cfg Config) (log.Exporter, error) {
	switch cfg.Protocol {
	case otelx.ProtocolGRPC:
		opts := []otlploggrpc.Option{otlploggrpc.WithTimeout(cfg.Timeout)}
		host, insecure := otelx.EndpointHostInsecure(cfg.Endpoint)
		if host != "" {
			opts = append(opts, otlploggrpc.WithEndpoint(host))
		}
		if insecure {
			opts = append(opts, otlploggrpc.WithInsecure())
		}
		return otlploggrpc.New(ctx, opts...)
	default: // otelx.ProtocolHTTP
		opts := []otlploghttp.Option{otlploghttp.WithTimeout(cfg.Timeout)}
		if u, insecure, ok := otelx.EndpointURL(cfg.Endpoint); ok {
			opts = append(opts, otlploghttp.WithEndpointURL(u))
			if insecure {
				opts = append(opts, otlploghttp.WithInsecure())
			}
		}
		return otlploghttp.New(ctx, opts...)
	}
}

// sanitizeUTF8 replaces sequences of invalid UTF-8 bytes with the Unicode
// replacement character so that attribute values never carry invalid UTF-8
// from SNMP strings sourced from device sysName/ifDescr.
func sanitizeUTF8(s string) string {
	if utf8.ValidString(s) {
		return s
	}
	return strings.ToValidUTF8(s, "�")
}

// PushGraph publishes the current graph as the snapshot the observable edge/
// device gauges report, then force-flushes a collection through the OTLP
// exporter. Because the gauges observe this snapshot (rather than accumulating
// recorded measurements), each push emits exactly the edges/devices present in
// g — edges removed since the last push are not re-emitted.
func (e *Exporter) PushGraph(ctx context.Context, g discovery.Graph) error {
	e.latest.Store(&g)
	if err := e.meterProvider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("otlp: flush metrics: %w", err)
	}
	return nil
}

// PushChanges emits each graph.EdgeChange as an OTLP log record and
// force-flushes them through the LoggerProvider's OTLP exporter.
func (e *Exporter) PushChanges(ctx context.Context, changes []graph.EdgeChange) error {
	for _, c := range changes {
		sev := otellog.SeverityInfo
		sevText := "INFO"
		if c.Kind == graph.ChangeRemoved {
			sev = otellog.SeverityWarn
			sevText = "WARN"
		}

		// Prefer "after" edge for attribute values; fall back to "before".
		var srcDevice, srcPort, dstDevice, dstPort, proto, linkKind string
		switch {
		case c.After != nil:
			srcDevice = c.After.SrcDevice
			srcPort = c.After.SrcPort
			dstDevice = c.After.DstDevice
			dstPort = c.After.DstPort
			proto = string(c.After.DiscoveryProto)
			linkKind = string(c.After.LinkKind)
		case c.Before != nil:
			srcDevice = c.Before.SrcDevice
			srcPort = c.Before.SrcPort
			dstDevice = c.Before.DstDevice
			dstPort = c.Before.DstPort
			proto = string(c.Before.DiscoveryProto)
			linkKind = string(c.Before.LinkKind)
		}

		var ts time.Time
		switch {
		case c.After != nil && !c.After.ObservedAt.IsZero():
			ts = c.After.ObservedAt
		case c.Before != nil && !c.Before.ObservedAt.IsZero():
			ts = c.Before.ObservedAt
		default:
			ts = time.Now()
		}

		var rec otellog.Record
		rec.SetTimestamp(ts)
		rec.SetObservedTimestamp(time.Now())
		rec.SetSeverity(sev)
		rec.SetSeverityText(sevText)
		rec.SetBody(otellog.StringValue("topology change: " + string(c.Kind)))
		rec.AddAttributes(
			otellog.String("change_kind", string(c.Kind)),
			otellog.String("src_device", sanitizeUTF8(srcDevice)),
			otellog.String("src_port", sanitizeUTF8(srcPort)),
			otellog.String("dst_device", sanitizeUTF8(dstDevice)),
			otellog.String("dst_port", sanitizeUTF8(dstPort)),
			otellog.String("proto", sanitizeUTF8(proto)),
			otellog.String("link_kind", sanitizeUTF8(linkKind)),
		)
		e.logger.Emit(ctx, rec)
	}

	if err := e.loggerProvider.ForceFlush(ctx); err != nil {
		return fmt.Errorf("otlp: flush logs: %w", err)
	}
	return nil
}

// Shutdown flushes and releases the underlying providers. Safe to call once at
// process shutdown.
func (e *Exporter) Shutdown(ctx context.Context) error {
	var errs []error
	if err := e.meterProvider.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("otlp: shutdown meter provider: %w", err))
	}
	if err := e.loggerProvider.Shutdown(ctx); err != nil {
		errs = append(errs, fmt.Errorf("otlp: shutdown logger provider: %w", err))
	}
	return errors.Join(errs...)
}

// ClassifyPushError maps a non-nil error returned by PushGraph or PushChanges
// to a metrics.PushReason for use as the `reason` label on
// network_topology_otlp_push_total{status="error"}. err must be non-nil;
// passing nil panics — call sites should use metrics.ReasonNA for the
// status="ok" path. The returned value is guaranteed to satisfy
// metrics.PushReason.Valid().
//
// Because the OpenTelemetry Go SDK OTLP exporters surface transport failures as
// formatted error strings (there is no exported status-code type), this
// classifier inspects the error text. Categorisation:
//   - context.DeadlineExceeded / "timeout"      → PushReasonTimeout
//   - "tls:" / "x509:" in the message           → PushReasonTLSError
//   - an HTTP 4xx status token in the message    → PushReasonHTTP4xx
//   - an HTTP 5xx status token in the message    → PushReasonHTTP5xx
//   - everything else                            → PushReasonNetwork
func ClassifyPushError(err error) metrics.PushReason {
	if err == nil {
		panic("otlp: ClassifyPushError called with nil error")
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return metrics.PushReasonTimeout
	}
	msg := err.Error()
	lower := strings.ToLower(msg)

	if strings.Contains(lower, "tls:") || strings.Contains(lower, "x509:") ||
		strings.Contains(lower, "certificate") {
		return metrics.PushReasonTLSError
	}
	if strings.Contains(lower, "timeout") || strings.Contains(lower, "deadline exceeded") {
		return metrics.PushReasonTimeout
	}
	switch classifyHTTPStatus(msg) {
	case statusClass4xx:
		return metrics.PushReasonHTTP4xx
	case statusClass5xx:
		return metrics.PushReasonHTTP5xx
	}
	return metrics.PushReasonNetwork
}

type statusClass int

const (
	statusClassNone statusClass = iota
	statusClass4xx
	statusClass5xx
)

// classifyHTTPStatus scans msg for a three-digit HTTP status token (as emitted
// by the SDK via http.Response.Status, e.g. "400 Bad Request") and returns its
// class. It only matches standalone 4xx/5xx tokens to avoid false positives on
// arbitrary numbers (ports, byte counts) in the message.
func classifyHTTPStatus(msg string) statusClass {
	fields := strings.FieldsFunc(msg, func(r rune) bool {
		return r < '0' || r > '9'
	})
	for _, f := range fields {
		if len(f) != 3 {
			continue
		}
		code, err := strconv.Atoi(f)
		if err != nil {
			continue
		}
		switch {
		case code >= 400 && code < 500:
			return statusClass4xx
		case code >= 500 && code < 600:
			return statusClass5xx
		}
	}
	return statusClassNone
}
