package app

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"runtime"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"
	"github.com/prometheus/exporter-toolkit/web"

	"github.com/grafana/network-topology-exporter/internal/app/httpx"
	"github.com/grafana/network-topology-exporter/internal/config"
	"github.com/grafana/network-topology-exporter/internal/credentials"
	snmpwalk "github.com/grafana/network-topology-exporter/internal/discovery/snmp"
	"github.com/grafana/network-topology-exporter/internal/federation"
	"github.com/grafana/network-topology-exporter/internal/loglimit"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/otelx"
	"github.com/grafana/network-topology-exporter/internal/output/otlp"
	yangout "github.com/grafana/network-topology-exporter/internal/output/yang"
	"github.com/grafana/network-topology-exporter/internal/tracing"
	"github.com/grafana/network-topology-exporter/internal/version"
)

// Run is the topology-exporter top-level entry point: it parses argv, loads
// config, wires up the HTTP server with /metrics, /healthz, /readyz, branches
// on federation.role (hub vs spoke/standalone), starts the discovery loop or
// the hub federation listener, blocks until SIGINT/SIGTERM, and drains
// cleanly. Returns the process exit code; main() calls os.Exit on the
// return value.
func Run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("topology-exporter", flag.ContinueOnError)
	var (
		configPath = fs.String("config.file", "/etc/topology-exporter/config.yaml", "Path to the YAML configuration file.")
		listenAddr = fs.String("web.listen-address", ":9100", "Address on which to expose /metrics and /healthz.")
		logLevel   = fs.String("log.level", "info", "Log level: debug | info | warn | error.")
		showVer    = fs.Bool("version", false, "Print version and exit.")
	)
	if err := fs.Parse(args); err != nil {
		return 1
	}

	if *showVer {
		fmt.Printf("topology-exporter %s (%s, built %s)\n", version.Version, version.Commit, version.BuildDate)
		return 0
	}

	logger := NewLogger(*logLevel)
	slog.SetDefault(logger)
	logger.Info("starting",
		"version", version.Version,
		"commit", version.Commit,
		"build_date", version.BuildDate,
		"config", *configPath,
	)

	cfg, err := config.Load(*configPath)
	if err != nil {
		logger.Error("loading config failed", "error", err)
		return 1
	}

	// federation.role: hub is no longer a supported configuration in this
	// binary (docs/proposals/core-hub-split.md §3.2). It used to run a pure
	// aggregator with no local discovery loop via the branch below; that
	// logic (and the internal/federationhub package, and the k8s.io/client-go
	// dependency it pulls in for HA leader election) now lives entirely in
	// cmd/topology-hub, which cmd/topology-exporter never links. Fail fast
	// here with a clear pointer rather than silently doing nothing, so a
	// config file written for the old single-binary world doesn't start a
	// process that never becomes ready.
	if cfg.Federation.Role == config.RoleHub {
		logger.Error("federation.role: hub is not supported by this binary; run cmd/topology-hub instead",
			"config", *configPath)
		return 1
	}
	logger.Info("config loaded",
		"discovery_interval", cfg.Discovery.Interval,
		"parallelism", cfg.Discovery.Parallelism,
		"target_count", len(cfg.Targets),
	)
	if cfg.Modules.FDB.Enabled && !cfg.Modules.ARP.Enabled {
		logger.Warn("FDB is enabled but ARP enrichment is off; DstPort backfill on FDB-only edges will be unavailable. Set modules.arp.enabled: true to re-enable.")
	}

	m := metrics.New(cfg.Federation.Role == config.RoleUncoordinated)
	m.SnapshotLastWrittenUnix.SetToCurrentTime()

	// walkerMetrics adapts m.BGPWalkerOutcomeTotal to the snmputil.WalkerMetrics
	// interface so the bgp package can record walker outcomes without holding a
	// package-global counter handle. Threaded per-cycle into snmputil.Params
	// below; see internal/metrics/walker_metrics_adapter.go for the adapter.
	walkerMetrics := metrics.NewWalkerMetricsAdapter(m)

	// warnLimiter caps chronic same-key Warn emissions (issue #16). Wraps
	// `logger` and is threaded into spoke push, BGP/FDB walks, and the
	// per-cycle NFS-stall sites below. Process-singleton: keys are stable
	// across cycles so a chronic failure does not re-alert on every cycle.
	// DefaultCooldown (1h) keeps operators alerted on first occurrence and
	// re-alerted hourly thereafter without flooding the log.
	warnLimiter := loglimit.New(logger, loglimit.DefaultCooldown)

	// Issue #83: opt-in per-target SNMP session pool. Constructed once and
	// threaded into the discovery loop so each target reuses one session across
	// its modules, cutting socket/conntrack churn on large fleets. nil when
	// disabled (the default) → snmpwalk.Acquire uses the fresh open+close path,
	// byte-identical to pre-#83. Closed during graceful shutdown below.
	var sessionPool *snmpwalk.SessionPool
	if cfg.Discovery.SNMP.SessionPool.Enabled {
		sessionPool = snmpwalk.NewSessionPool(snmpwalk.SessionPoolOptions{
			MaxIdle: cfg.Discovery.SNMP.SessionPool.MaxIdle,
			Metrics: metrics.NewSessionPoolMetricsAdapter(m),
		})
		logger.Info("snmp session pool enabled", "max_idle", cfg.Discovery.SNMP.SessionPool.MaxIdle)
	}

	var status atomic.Pointer[httpx.CycleStatus]
	var ready atomic.Bool // set to true after the first live cycle or spoke push

	// isReadyFn is the readiness check for /readyz. Default: process-local flag.
	// Hub mode replaces this with hub.IsReady before registering the mux handler.
	isReadyFn := ready.Load

	// Liveness staleness gate (ops hardening): /healthz returns 503 once the
	// most recent discovery cycle is older than interval × liveness_max_stale_cycles
	// so Kubernetes restarts a process whose discovery loop has wedged. A maxStale
	// of 0 disables the gate (handler always 200 — prior behaviour). The same
	// value gates the watchdog goroutine below. See livenessMaxStale for the
	// hub-exclusion rule.
	livenessMaxStale := livenessMaxStale(cfg)

	mux := http.NewServeMux()
	mux.Handle("/metrics", httpx.InstrumentMetricsHandler(
		promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{Registry: m.Registry()}),
		m.MetricsRenderDuration,
		m.MetricsPayloadBytes,
	))
	mux.HandleFunc("/healthz", httpx.NewHealthzHandler(&status, livenessMaxStale, time.Now))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "topology-exporter %s\nendpoints: /metrics /healthz /readyz\n", version.Version)
	})

	effectiveAddr := cfg.Listen.Addr
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "web.listen-address" {
			effectiveAddr = *listenAddr
		}
	})

	srv := &http.Server{
		Addr:              effectiveAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Issue #69: opt-in pprof debug endpoint on a SEPARATE listener. Off by
	// default (empty addr): no extra listener, no profiling overhead. When
	// enabled, /debug/pprof/* is served ONLY on this listener — never on the
	// main metrics mux above — and carries no auth/TLS, so it must be bound to
	// localhost or a management interface only (documented in example.yaml).
	var debugSrv *http.Server
	if cfg.Listen.DebugListenAddr != "" {
		// Mutex and block profiles are empty unless sampling is enabled at
		// runtime. Enable conservative sampling ONLY when the debug endpoint is
		// on, so there is zero overhead when it is off. These add minor runtime
		// overhead (per-contention bookkeeping), which is why they are gated
		// behind the opt-in debug listener rather than always on.
		runtime.SetMutexProfileFraction(100) // sample ~1/100 mutex contention events
		runtime.SetBlockProfileRate(10000)   // sample blocking events ~every 10µs of block time
		debugSrv = &http.Server{
			Addr:              cfg.Listen.DebugListenAddr,
			Handler:           newDebugMux(),
			ReadHeaderTimeout: 10 * time.Second,
			// No WriteTimeout: CPU/trace profiles stream for a caller-chosen
			// number of seconds (e.g. ?seconds=30) and a write deadline would
			// truncate them.
			IdleTimeout: 120 * time.Second,
		}
	}

	ctx, cancel := signal.NotifyContext(ctx, syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	var workerDone sync.WaitGroup

	// Issue #68: opt-in OpenTelemetry tracing of the discovery cycle. Installs
	// the global TracerProvider + W3C TraceContext propagator that
	// spoke/standalone mode uses to record discovery.cycle and spoke.push
	// (hub mode's matching hub.handlePush span is installed the same way by
	// cmd/topology-hub, since federation.role: hub no longer runs in this
	// binary — see the rejection above). When traces.enabled is false, no
	// provider is installed and the global tracer stays the OTel no-op, so
	// all instrumentation is a cheap no-op.
	var traceProvider *tracing.Provider
	if cfg.Output.OTLP.Traces.Enabled {
		sampleRate := 0.1
		if cfg.Output.OTLP.Traces.SampleRate != nil {
			sampleRate = *cfg.Output.OTLP.Traces.SampleRate
		}
		var err error
		traceProvider, err = tracing.New(ctx, tracing.Config{
			Endpoint:   cfg.Output.OTLP.Endpoint,
			Timeout:    cfg.Output.OTLP.Timeout,
			Protocol:   otelx.Protocol(cfg.Output.OTLP.Protocol),
			InstanceID: cfg.Federation.Spoke.SpokeID, // empty in non-spoke roles → falls back to hostname
			SampleRate: sampleRate,
		})
		if err != nil {
			logger.Error("building tracer provider", "error", err)
			return 1
		}
		logger.Info("tracing enabled", "sample_rate", sampleRate, "protocol", cfg.Output.OTLP.Protocol)
	}

	// OTLP publisher: a no-op by default (hub mode never enables OTLP), so the
	// shared shutdown drain below never nil-panics. The default branch replaces
	// this with an enabled publisher (exp + sem + wg allocated together) when
	// output.otlp.enabled is true.
	pub := NoopOTLPPublisher()

	// federation.role is standalone, uncoordinated, or spoke at this point
	// (hub was rejected above). This used to be the "default" arm of a
	// switch on cfg.Federation.Role; the hub arm moved to cmd/topology-hub
	// (docs/proposals/core-hub-split.md §3.2), so the switch itself is gone —
	// this is its unconditional body, unchanged.
	//
	// Build the spoke client now so TLS errors surface at startup, not
	// mid-cycle.
	var spoke *federation.Spoke
	if cfg.Federation.Role == config.RoleSpoke {
		var err error
		spoke, err = federation.NewSpoke(cfg.Federation, logger, warnLimiter, m)
		if err != nil {
			logger.Error("building federation spoke", "error", err)
			return 1
		}
	}

	if cfg.Output.OTLP.Enabled {
		otlpExp, err := otlp.New(ctx, otlp.Config{
			Endpoint:   cfg.Output.OTLP.Endpoint,
			Timeout:    cfg.Output.OTLP.Timeout,
			InstanceID: cfg.Federation.Spoke.SpokeID, // empty in non-spoke roles → falls back to hostname
			Protocol:   otelx.Protocol(cfg.Output.OTLP.Protocol),
		})
		if err != nil {
			logger.Error("building OTLP exporter", "error", err)
			return 1
		}
		pub = NewOTLPPublisher(otlpExp, MaxOTLPPushConcurrency, logger, m)
	}

	// Issue #73: admin out-of-cycle re-discovery. cycleMu serialises forced
	// walks against the regular cycle (shared with LoopConfig.CycleMu).
	// The Rediscoverer shares ONE credential resolver with the discovery
	// loop (#169): a sticky-credential win recorded by either path is
	// reused by the other, and the snapshot hydration the loop performs
	// benefits admin walks too. Access is serialised by cycleMu, so the
	// two paths never hit a device concurrently. The endpoint is privileged:
	// it only runs when listen.web_config_file actually authenticates the
	// caller (basic_auth_users, or a client-cert-requiring client_auth_type).
	// A TLS-only web-config encrypts but does NOT authenticate the client, so
	// it does not enable the endpoint — the handler returns 403.
	var cycleMu sync.Mutex
	resolver, err := credentials.New(cfg.Credentials)
	if err != nil {
		logger.Error("building credential resolver", "error", err)
		return 1
	}
	allowedNets := snmpwalk.ParseCIDRs(cfg.Discovery.Scope.CIDRAllowList)
	rediscoverer := NewRediscoverer(cfg, m, logger, resolver, allowedNets, &cycleMu, WebConfigHasClientAuth(cfg.Listen.WebConfigFile))
	mux.HandleFunc("/admin/rediscover", httpx.NewRediscoverHandler(rediscoverer))

	// Graph-stale watchdog (ops hardening): re-assert GraphStale=1 when this
	// running loop wedges. Only started when a local discovery loop runs (the
	// default branch already excludes hub) AND the liveness gate is enabled
	// (livenessMaxStale > 0). Joined via workerDone so graceful shutdown waits
	// for it; it returns on ctx cancellation (no goroutine leak — goleak).
	if livenessMaxStale > 0 {
		workerDone.Add(1)
		go func() {
			defer workerDone.Done()
			// Recover a panic in the watchdog so a bug in the staleness
			// gate cannot crash the process. One-shot: on recovery the
			// watchdog exits, leaving the /healthz gate to fall back to
			// the cycle-timestamp check the handler already performs.
			defer recoverGoroutine("stale_watchdog", logger, m)
			RunStaleWatchdog(ctx, &status, m, cfg.Discovery.Interval, livenessMaxStale, time.Now)
		}()
	}

	workerDone.Add(1)
	go func() {
		defer workerDone.Done()
		// Recover a panic in the discovery scheduler so one wedged
		// cycle cannot crash the process. Per-cycle work is already
		// panic-isolated at two finer layers — the per-device probe
		// recover in cycle.go and the per-cycle recover added inside
		// RunDiscoveryLoop — so reaching here means the scheduler shell
		// itself panicked; recover, exit, and let shutdown drain.
		defer recoverGoroutine("discovery_loop", logger, m)
		RunDiscoveryLoop(ctx, LoopConfig{
			Cancel:        cancel,
			Logger:        logger,
			WarnLimiter:   warnLimiter,
			Cfg:           cfg,
			M:             m,
			WalkerMetrics: walkerMetrics,
			Status:        &status,
			Ready:         &ready,
			Spoke:         spoke,
			Otlp:          pub,
			CycleMu:       &cycleMu,
			Pool:          sessionPool,
			Resolver:      resolver,
		})
	}()

	mux.HandleFunc("/readyz", httpx.NewReadyzHandler(isReadyFn))

	// Issue #75: opt-in RFC 8345 YANG-JSON pull endpoint. Off by default; when
	// enabled, GET /topology/yang renders the current reconciled topology. The
	// handler shares the same readiness flag as /readyz, so it returns 503 until
	// the first live cycle/push has populated the graph.
	registerYANG(mux, m.Topology, isReadyFn, cfg.Output.YANG)

	go func() {
		var serveErr error
		switch {
		case cfg.Listen.WebConfigFile != "":
			// Prometheus exporter-toolkit web-config: supports basic_auth, server TLS,
			// and mTLS via the same YAML schema as snmp_exporter / node_exporter /
			// blackbox_exporter. The toolkit handles cert reload-on-change, bcrypt,
			// and TLS-cipher hardening — no need to reimplement.
			logger.Info("metrics server listening (web-config)", "addr", effectiveAddr, "web_config_file", cfg.Listen.WebConfigFile)
			webFlags := &web.FlagConfig{
				WebListenAddresses: &[]string{effectiveAddr},
				WebSystemdSocket:   new(bool),
				WebConfigFile:      &cfg.Listen.WebConfigFile,
			}
			serveErr = web.ListenAndServe(srv, webFlags, logger)
		default:
			logger.Info("metrics server listening", "addr", effectiveAddr)
			serveErr = srv.ListenAndServe()
		}
		if serveErr != nil && !errors.Is(serveErr, http.ErrServerClosed) {
			logger.Error("metrics server error", "error", serveErr)
			cancel()
		}
	}()

	if debugSrv != nil {
		logger.Warn("pprof debug endpoint listening — no auth/TLS; do not expose to the internet",
			"addr", cfg.Listen.DebugListenAddr)
		go func() {
			// A bind failure on the debug listener must NOT take down the
			// exporter: profiling is an optional operability aid. Log it and
			// carry on rather than cancelling the root context.
			if err := debugSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("pprof debug server error", "error", err)
			}
		}()
	}

	<-ctx.Done()
	logger.Info("shutdown signal received, draining")

	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer shutdownCancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("http shutdown error", "error", err)
	}
	if debugSrv != nil {
		if err := debugSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("pprof debug server shutdown error", "error", err)
		}
	}
	drainTimeout := time.Duration(float64(cfg.Discovery.Interval)*cfg.Discovery.CycleBudgetFraction) + cfg.Discovery.TimeoutPerDevice
	drainDone := make(chan struct{})
	go func() { workerDone.Wait(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(drainTimeout):
		logger.Warn("discovery drain timed out, forcing exit")
	}
	// Bound the OTLP drain on the same timeout as the discovery drain so a
	// stuck OTLP push cannot hang shutdown indefinitely (the discovery drain is
	// bounded but this Wait previously was not).
	otlpDone := make(chan struct{})
	go func() { pub.Drain(); close(otlpDone) }()
	select {
	case <-otlpDone:
		logger.Info("otlp push goroutines drained")
	case <-time.After(drainTimeout):
		logger.Warn("otlp drain timed out, forcing exit")
	}
	// Issue #83: close the SNMP session pool after discovery has drained so no
	// in-flight walk loses its session mid-walk. Closing every pooled session
	// also clears the credentials they held. No-op when the pool is disabled.
	if sessionPool != nil {
		sessionPool.Close()
		logger.Info("snmp session pool closed")
	}
	if traceProvider != nil {
		if err := traceProvider.Shutdown(shutdownCtx); err != nil {
			logger.Error("tracer provider shutdown error", "error", err)
		} else {
			logger.Info("tracer provider flushed and shut down")
		}
	}
	logger.Info("clean shutdown complete")
	return 0
}

// livenessMaxStale returns the /healthz liveness staleness threshold for this
// config: discovery.interval × discovery.liveness_max_stale_cycles. It returns
// 0 (gate disabled) in two cases:
//
//   - liveness_max_stale_cycles == 0 — the operator disabled the gate; or
//   - federation.role == "hub" — a pure hub runs NO local discovery loop, so it
//     never updates the cycle timestamp and would otherwise falsely trip the
//     gate. This is the single hub-exclusion point shared by the /healthz gate
//     and the graph-stale watchdog: a 0 result means neither is engaged.
//
// A 0 return therefore preserves today's behaviour (/healthz always 200 once up
// and no watchdog goroutine), which is exactly what hub mode and an explicit
// opt-out both want.
func livenessMaxStale(cfg *config.Config) time.Duration {
	if cfg.Federation.Role == config.RoleHub {
		return 0
	}
	cycles := cfg.Discovery.LivenessMaxStaleCyclesValue()
	if cycles <= 0 {
		return 0
	}
	return cfg.Discovery.Interval * time.Duration(cycles)
}

// registerYANG wires the issue-#75 GET /topology/yang endpoint onto mux when
// output.yang.enabled is true; when disabled it registers nothing, so the route
// 404s. src is the live graph source (the *metrics.TopologyCollector), ready is
// the same readiness func feeding /readyz (isReadyFn), so the YANG endpoint
// reports ready in hub mode too. Extracted so the wiring contract is
// unit-testable without standing up the full Run server.
func registerYANG(mux *http.ServeMux, src yangout.GraphSource, ready func() bool, cfg config.YANGOutputConfig) {
	if !cfg.Enabled {
		return
	}
	mux.HandleFunc("/topology/yang", yangout.Handler(src, ready, yangout.Config{NetworkID: cfg.NetworkID}))
}

// NewLogger returns a slog.Logger configured to emit JSON to stderr at the
// requested level. Unknown level strings default to Info.
func NewLogger(level string) *slog.Logger {
	var lvl slog.Level
	switch level {
	case "debug":
		lvl = slog.LevelDebug
	case "warn":
		lvl = slog.LevelWarn
	case "error":
		lvl = slog.LevelError
	default:
		lvl = slog.LevelInfo
	}
	return slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: lvl}))
}
