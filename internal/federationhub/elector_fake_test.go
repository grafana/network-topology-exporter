package federationhub

import "context"

// fakeElector is a deterministic, test-only LeaderElector driven by
// lead()/step(). It lives in a _test.go file so production stays free of an
// unused symbol until the hub wires election in a later task.
type fakeElector struct {
	cb LeaderCallbacks
}

func newFakeElector() *fakeElector { return &fakeElector{} }

func (f *fakeElector) Run(ctx context.Context, cb LeaderCallbacks) error {
	return f.run(ctx, cb, nil)
}

// run is the Run body with an optional ready channel that is closed once the
// callbacks are recorded, letting a test drive lead()/step() without racing
// the goroutine that records cb.
func (f *fakeElector) run(ctx context.Context, cb LeaderCallbacks, ready chan<- struct{}) error {
	f.cb = cb
	if ready != nil {
		close(ready)
	}
	<-ctx.Done()
	return ctx.Err()
}

func (f *fakeElector) lead() {
	if f.cb.OnStartedLeading != nil {
		f.cb.OnStartedLeading(context.Background())
	}
}

func (f *fakeElector) step() {
	if f.cb.OnStoppedLeading != nil {
		f.cb.OnStoppedLeading()
	}
}
