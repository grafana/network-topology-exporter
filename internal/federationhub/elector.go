package federationhub

import "context"

// LeaderElector runs a leader election and drives the callbacks. The default
// implementation (elector_k8s.go, added later) uses a Kubernetes Lease; the
// interface keeps the hub decoupled so a fake (tests) or a future backend can
// substitute. See docs/superpowers/specs/2026-06-09-hub-ha-design.md §4.1.
type LeaderElector interface {
	// Run blocks until ctx is cancelled, invoking the callbacks as leadership changes.
	Run(ctx context.Context, cb LeaderCallbacks) error
}

// LeaderCallbacks are invoked by a LeaderElector. OnStoppedLeading MUST be
// treated as "step down NOW" — a frozen-then-resumed leader may still believe
// it leads (design §4.3/§8).
type LeaderCallbacks struct {
	OnStartedLeading func(ctx context.Context)
	OnStoppedLeading func()
	OnNewLeader      func(identity string)
}
