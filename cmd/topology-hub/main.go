// Command topology-hub runs network-topology-exporter's optional federation
// hub: the pure aggregator that receives spoke pushes over mTLS
// (federation.role: hub) and combines them into a single reconciled graph.
//
// It is split out of cmd/topology-exporter (docs/proposals/core-hub-split.md
// §3) so that only THIS binary links k8s.io/client-go — needed only for the
// opt-in native-HA leader election (federation.hub.ha.enabled) — and every
// other deployment shape (standalone, uncoordinated, spoke) never pays for a
// Kubernetes API client it never uses. cmd/topology-exporter now rejects
// federation.role: hub at startup with a message pointing back here.
//
// Everything below (config load, HTTP server wiring, LD-13 snapshot load,
// the optional HA elector, hub.Serve) is relocated, not rewritten, from
// internal/app/app.go's former "case config.RoleHub" branch — a pure
// repackaging with no behavior change for role: hub, per §3.2/§8 of the
// proposal above.
package main

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
	"github.com/grafana/network-topology-exporter/internal/discovery"
	"github.com/grafana/network-topology-exporter/internal/federationhub"
	"github.com/grafana/network-topology-exporter/internal/hubcli"
	"github.com/grafana/network-topology-exporter/internal/metrics"
	"github.com/grafana/network-topology-exporter/internal/otelx"
	yangout "github.com/grafana/network-topology-exporter/internal/output/yang"
	"github.com/grafana/network-topology-exporter/internal/snapshot"
	"github.com/grafana/network-topology-exporter/internal/tracing"
	"github.com/grafana/network-topology-exporter/internal/version"
)

func main() {
	os.Exit(run(context.Background(), os.Args[1:]))
}

// run is topology-hub's top-level entry point: parse argv, load config,
// reject anything that isn't federation.role: hub, wire /metrics /healthz
// /readyz, construct the federationhub.Hub, load its LD-13 snapshot, wire
// the optional HA elector, and call hub.Serve — then block until
// SIGINT/SIGTERM and drain cleanly. Returns the process exit code.
func run(ctx context.Context, args []string) int {
	fs := flag.NewFlagSet("topology-hub", flag.ContinueOnError)
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
		fmt.Printf("topology-hub %s (%s, built %s)\n", version.Version, version.Commit, version.BuildDate)
		return 0
	}

	logger := newLogger(*logLevel)
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

	// The mirror image of cmd/topology-exporter's rejection: this binary only
	// runs the hub. Any other role belongs in cmd/topology-exporter. The
	// check itself lives in internal/hubcli so it (and the flag-override rule
	// below) can be unit tested without spinning up a real server — see
	// internal/hubcli/hubcli_test.go.
	if err := hubcli.CheckRole(cfg.Federation.Role); err != nil {
		logger.Error(err.Error(), "role", cfg.Federation.Role, "config", *configPath)
		return 1
	}
	logger.Info("config loaded", "config", *configPath)

	m := metrics.New(false) // the uncoordinated-mode boundary-observation metric never applies to a hub
	m.SnapshotLastWrittenUnix.SetToCurrentTime()

	var status atomic.Pointer[httpx.CycleStatus] // never populated: a pure hub runs no local discovery cycle

	mux := http.NewServeMux()
	mux.Handle("/metrics", httpx.InstrumentMetricsHandler(
		promhttp.HandlerFor(m.Registry(), promhttp.HandlerOpts{Registry: m.Registry()}),
		m.MetricsRenderDuration,
		m.MetricsPayloadBytes,
	))
	// maxStale is always 0 here: a pure hub never advances `status`, so the
	// liveness-staleness gate would trip permanently if enabled. Mirrors
	// internal/app.livenessMaxStale's hub-exclusion rule from before the split.
	mux.HandleFunc("/healthz", httpx.NewHealthzHandler(&status, 0, time.Now))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/" {
			http.NotFound(w, r)
			return
		}
		_, _ = fmt.Fprintf(w, "topology-hub %s\nendpoints: /metrics /healthz /readyz\n", version.Version)
	})

	var listenAddrExplicitlySet bool
	fs.Visit(func(f *flag.Flag) {
		if f.Name == "web.listen-address" {
			listenAddrExplicitlySet = true
		}
	})
	effectiveAddr := hubcli.EffectiveListenAddr(listenAddrExplicitlySet, *listenAddr, cfg.Listen.Addr)

	srv := &http.Server{
		Addr:              effectiveAddr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		WriteTimeout:      60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}

	// Issue #69: opt-in pprof debug endpoint on a separate listener, same as
	// cmd/topology-exporter. Off by default.
	var debugSrv *http.Server
	if cfg.Listen.DebugListenAddr != "" {
		// Mutex and block profiles are empty unless sampling is enabled at
		// runtime. Enable conservative sampling ONLY when the debug endpoint is
		// on, so there is zero overhead when it is off. Matches
		// internal/app.Run's equivalent code path (internal/app/app.go) — this
		// binary duplicates the debug-mux wiring rather than importing
		// internal/app (see debug.go), so the sampling calls must be
		// duplicated too, not just the mux.
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

	// Issue #68: opt-in OpenTelemetry tracing so hub.handlePush's span links
	// to the spoke.push span that produced the request (see
	// tests/integration/tracing_propagation_test.go for the acceptance test).
	var traceProvider *tracing.Provider
	if cfg.Output.OTLP.Traces.Enabled {
		sampleRate := 0.1
		if cfg.Output.OTLP.Traces.SampleRate != nil {
			sampleRate = *cfg.Output.OTLP.Traces.SampleRate
		}
		var terr error
		traceProvider, terr = tracing.New(ctx, tracing.Config{
			Endpoint:   cfg.Output.OTLP.Endpoint,
			Timeout:    cfg.Output.OTLP.Timeout,
			Protocol:   otelx.Protocol(cfg.Output.OTLP.Protocol),
			InstanceID: cfg.Federation.Spoke.SpokeID, // always empty for a hub → falls back to hostname
			SampleRate: sampleRate,
		})
		if terr != nil {
			logger.Error("building tracer provider", "error", terr)
			return 1
		}
		logger.Info("tracing enabled", "sample_rate", sampleRate, "protocol", cfg.Output.OTLP.Protocol)
	}

	// Hub mode: pure aggregator — no local SNMP discovery. The hub server
	// exposes /spoke/push on a separate mTLS listener (LD-20).
	hub := federationhub.NewHub(cfg.Federation, m, logger, cfg.Snapshot.Path)

	// LD-13: load snapshot so the hub can serve stale-but-valid metrics
	// (GraphStale=1) until the first live spoke push arrives.
	m.GraphStale.Set(1)
	hubSnap, err := snapshot.Load(cfg.Snapshot.Path)
	if err != nil {
		if errors.Is(err, snapshot.ErrVersionMismatch) {
			logger.Warn("hub snapshot version mismatch, cold start", "path", cfg.Snapshot.Path, "error", err)
		} else {
			logger.Warn("hub snapshot load failed, cold start", "path", cfg.Snapshot.Path, "error", err)
		}
	}
	if hubSnap != nil {
		hub.RestoreGraph(discovery.Graph{
			Devices:    hubSnap.Devices,
			Edges:      hubSnap.Edges,
			OutOfScope: hubSnap.OutOfScope,
		})
		logger.Info("hub snapshot loaded", "devices", len(hubSnap.Devices), "edges", len(hubSnap.Edges))
		m.SnapshotLoadedDevicesTotal.Set(float64(len(hubSnap.Devices)))
	}

	// Readiness is driven by the first live spoke push.
	isReadyFn := hub.IsReady

	// Issue #71 native HA. ONLY when ha.enabled: construct the k8s lease
	// elector and drive hub leadership. When disabled this whole block is
	// skipped — no elector, no in-cluster config attempt, zero k8s API
	// calls — and the hub stays leader (isLeader defaults true in NewHub),
	// byte-identical to single-hub mode (the regression gate).
	if cfg.Federation.Hub.HA.Enabled {
		ha := cfg.Federation.Hub.HA
		identity := os.Getenv("POD_NAME")
		if identity == "" {
			if host, herr := os.Hostname(); herr == nil {
				identity = host
			}
		}
		namespace := ha.LeaseNamespace
		if namespace == "" {
			namespace = os.Getenv("POD_NAMESPACE")
		}
		elector, eerr := federationhub.NewK8sLeaseElector(federationhub.K8sElectorConfig{
			LeaseName:      ha.LeaseName,
			LeaseNamespace: namespace,
			Identity:       identity,
			LeaseDuration:  ha.LeaseDuration,
			RenewDeadline:  ha.RenewDeadline,
			RetryPeriod:    ha.RetryPeriod,
		})
		if eerr != nil {
			// Off-cluster (no serviceaccount/kubeconfig) returns a clear
			// error here, not a panic. Fatal at startup like the other
			// build-the-client paths in this function.
			logger.Error("building federation hub HA elector", "error", eerr)
			return 1
		}

		// Until elected, the hub is NOT leader: it 503s pushes (with
		// Connection: close) so spokes route to the actual leader. The
		// callbacks below flip this.
		hub.SetLeader(false)

		// Fence-token epoch source (design §4.4). Prefer the Lease's
		// server-assigned LeaderTransitions, which orders writes across pods.
		// When no EpochReader is available (e.g. the fake elector in tests),
		// fall back to a process-local monotonic counter. When an EpochReader
		// IS present but a transient CurrentEpoch read errors, retain the
		// hub's current epoch rather than overwriting it with the local
		// counter — see internal/app's former hub branch for the full
		// rationale (unchanged by this move).
		epochReader, _ := elector.(federationhub.EpochReader)
		var localEpoch atomic.Uint64

		workerDone.Add(1)
		go func() {
			defer workerDone.Done()
			defer cancel()
			defer recoverGoroutine("hub_elector", logger, m)
			rerr := elector.Run(ctx, federationhub.LeaderCallbacks{
				OnStartedLeading: func(c context.Context) {
					hub.SetLeader(true)
					if epochReader == nil {
						epoch := localEpoch.Add(1)
						hub.SetLeaseEpoch(epoch)
						logger.Info("hub HA: became leader", "identity", identity, "lease_epoch", epoch)
						return
					}
					e, ferr := epochReader.CurrentEpoch(c)
					if ferr != nil {
						logger.Warn("hub HA: lease epoch read failed; retaining current epoch",
							"error", ferr, "current_epoch", hub.LeaseEpoch())
						logger.Info("hub HA: became leader", "identity", identity, "lease_epoch", hub.LeaseEpoch())
						return
					}
					hub.SetLeaseEpoch(e)
					logger.Info("hub HA: became leader", "identity", identity, "lease_epoch", e)
				},
				OnStoppedLeading: func() {
					// Step down NOW (design §4.3). The T3 Connection:close
					// header on the push handler's 503 is the primary
					// step-down mechanism.
					hub.SetLeader(false)
					logger.Warn("hub HA: lost leadership; stepping down (503 + Connection:close on pushes)")
				},
				OnNewLeader: func(id string) {
					logger.Info("hub HA: leadership", "leader", id)
				},
			})
			if rerr != nil && ctx.Err() == nil {
				logger.Error("hub HA elector error", "error", rerr)
			}
		}()
	}

	workerDone.Add(1)
	go func() {
		defer workerDone.Done()
		// cancel() is registered before recoverGoroutine so it runs AFTER
		// the recover (defers are LIFO): a recovered hub-serve panic must
		// still trigger shutdown/restart, not leave the process half-alive.
		defer cancel()
		defer recoverGoroutine("hub_serve", logger, m)
		if err := hub.Serve(ctx); err != nil && ctx.Err() == nil {
			logger.Error("hub federation server error", "error", err)
			cancel()
		}
	}()

	mux.HandleFunc("/readyz", httpx.NewReadyzHandler(isReadyFn))

	// Issue #75: opt-in RFC 8345 YANG-JSON pull endpoint, same as
	// cmd/topology-exporter — renders the hub's combined graph once ready.
	if cfg.Output.YANG.Enabled {
		mux.HandleFunc("/topology/yang", yangout.Handler(m.Topology, isReadyFn, yangout.Config{NetworkID: cfg.Output.YANG.NetworkID}))
	}

	go func() {
		var serveErr error
		switch {
		case cfg.Listen.WebConfigFile != "":
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
	// hub_elector and hub_serve are the only long-lived goroutines this
	// binary starts; unlike cmd/topology-exporter's discovery drain, there is
	// no per-cycle budget to derive a timeout from, so this uses a fixed
	// bound instead.
	drainDone := make(chan struct{})
	go func() { workerDone.Wait(); close(drainDone) }()
	select {
	case <-drainDone:
	case <-time.After(30 * time.Second):
		logger.Warn("hub drain timed out, forcing exit")
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
