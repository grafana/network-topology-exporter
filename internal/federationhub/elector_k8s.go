package federationhub

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/leaderelection"
	"k8s.io/client-go/tools/leaderelection/resourcelock"
)

// inClusterNamespaceFile is the path the kubelet mounts the pod's namespace at
// when a service-account token is automounted. Used as the namespace fallback
// when neither config nor the downward-API env var supplies one.
const inClusterNamespaceFile = "/var/run/secrets/kubernetes.io/serviceaccount/namespace"

// K8sElectorConfig parameterises the default k8s Lease-based LeaderElector.
// Durations map directly onto leaderelection.LeaderElectionConfig; the
// config package has already validated renew_deadline < lease_duration.
type K8sElectorConfig struct {
	LeaseName      string
	LeaseNamespace string
	Identity       string
	LeaseDuration  time.Duration
	RenewDeadline  time.Duration
	RetryPeriod    time.Duration
}

// k8sLeaseElector is the production LeaderElector: it elects over a
// coordination.k8s.io/Lease via client-go/tools/leaderelection. It is
// constructed ONLY when federation.hub.ha.enabled is true (see internal/app):
// in single-hub mode nothing here runs and no k8s API call is made.
type k8sLeaseElector struct {
	clientset kubernetes.Interface
	lock      *resourcelock.LeaseLock
	cfg       K8sElectorConfig
}

// NewK8sLeaseElector builds the default elector. It REQUIRES running in-cluster
// (a mounted service-account token); off-cluster it returns a clear error
// rather than panicking, so `ha.enabled: true` on a laptop fails loudly at
// startup (design §9). Identity falls back to the hostname; namespace falls
// back to the in-cluster namespace file, then "default".
func NewK8sLeaseElector(cfg K8sElectorConfig) (LeaderElector, error) {
	restCfg, err := rest.InClusterConfig()
	if err != nil {
		return nil, fmt.Errorf("federation.hub.ha requires running in-cluster (no kubeconfig/serviceaccount): %w", err)
	}
	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return nil, fmt.Errorf("federation.hub.ha: build kubernetes clientset: %w", err)
	}

	identity := cfg.Identity
	if identity == "" {
		host, herr := os.Hostname()
		if herr != nil {
			return nil, fmt.Errorf("federation.hub.ha: resolve identity (no POD_NAME and os.Hostname failed): %w", herr)
		}
		identity = host
	}

	namespace := cfg.LeaseNamespace
	if namespace == "" {
		namespace = inClusterNamespace()
	}

	lock := &resourcelock.LeaseLock{
		LeaseMeta: metav1.ObjectMeta{
			Name:      cfg.LeaseName,
			Namespace: namespace,
		},
		Client: clientset.CoordinationV1(),
		LockConfig: resourcelock.ResourceLockConfig{
			Identity: identity,
		},
	}

	cfg.Identity = identity
	cfg.LeaseNamespace = namespace
	return &k8sLeaseElector{clientset: clientset, lock: lock, cfg: cfg}, nil
}

// inClusterNamespace reads the pod's namespace from the service-account mount,
// falling back to "default" if the file is absent or empty.
func inClusterNamespace() string {
	data, err := os.ReadFile(inClusterNamespaceFile)
	if err != nil {
		return "default"
	}
	ns := strings.TrimSpace(string(data))
	if ns == "" {
		return "default"
	}
	return ns
}

// Run blocks until ctx is cancelled, driving cb as leadership changes. It uses
// NewLeaderElector + le.Run (not RunOrDie) so a lock-config error returns
// instead of calling klog.Fatal. ReleaseOnCancel:true makes a clean shutdown
// (SIGTERM → ctx cancel) yield the Lease promptly so a follower is elected
// without waiting out the full lease duration.
//
// The fence epoch (design §4.4) is read separately by app.go via CurrentEpoch
// inside its OnStartedLeading callback, after this pod has acquired the Lease.
func (e *k8sLeaseElector) Run(ctx context.Context, cb LeaderCallbacks) error {
	le, err := leaderelection.NewLeaderElector(leaderelection.LeaderElectionConfig{
		Lock:            e.lock,
		ReleaseOnCancel: true,
		LeaseDuration:   e.cfg.LeaseDuration,
		RenewDeadline:   e.cfg.RenewDeadline,
		RetryPeriod:     e.cfg.RetryPeriod,
		Callbacks: leaderelection.LeaderCallbacks{
			OnStartedLeading: func(c context.Context) {
				if cb.OnStartedLeading != nil {
					cb.OnStartedLeading(c)
				}
			},
			OnStoppedLeading: func() {
				if cb.OnStoppedLeading != nil {
					cb.OnStoppedLeading()
				}
			},
			OnNewLeader: func(id string) {
				if cb.OnNewLeader != nil {
					cb.OnNewLeader(id)
				}
			},
		},
	})
	if err != nil {
		return fmt.Errorf("federation.hub.ha: build leader elector: %w", err)
	}

	le.Run(ctx) // blocks until ctx is cancelled
	return ctx.Err()
}

// CurrentEpoch returns the Lease's monotonic LeaderTransitions count as the
// fence token (design §4.4). It is server-assigned and increments on every
// leadership handover, so it orders writes across pods (a resumed stale leader
// carries a lower value than the current holder). Called from app.go's
// OnStartedLeading callback, after this pod has acquired the Lease, so the read
// reflects the transition that just promoted us. On any read error it returns
// 0 (unfenced) and the error, so the caller can log rather than crash — a
// missing fence degrades to last-writer-wins (corruption-free via atomic
// rename), never a panic.
func (e *k8sLeaseElector) CurrentEpoch(ctx context.Context) (uint64, error) {
	record, _, err := e.lock.Get(ctx)
	if err != nil {
		return 0, fmt.Errorf("federation.hub.ha: read lease for fence epoch: %w", err)
	}
	if record.LeaderTransitions < 0 {
		return 0, nil
	}
	return uint64(record.LeaderTransitions), nil
}

// EpochReader is the optional capability app.go uses to wire the fence token.
// k8sLeaseElector implements it; a fake elector may or may not. app.go type-
// asserts and falls back to a monotonic local counter if absent.
type EpochReader interface {
	CurrentEpoch(ctx context.Context) (uint64, error)
}
