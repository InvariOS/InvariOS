package supervise

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// ErrAlreadyBootstrapped indicates a Supervisor was asked to bootstrap
// when it already had (whether that succeeded or is still running).
// Callers that need to tell this apart from other failures (e.g. to
// pick an HTTP status code) can check for it with errors.Is.
var ErrAlreadyBootstrapped = errors.New("already bootstrapped")

// bootstrapRequest is sent to a running Supervisor's Run loop by
// Bootstrap, carrying the reply channel Run sends the result back on.
type bootstrapRequest struct {
	mode  Mode
	reply chan bootstrapResult
}

// bootstrapResult is Run's reply to a bootstrapRequest.
type bootstrapResult struct {
	cmd *exec.Cmd
	err error
}

// Supervisor owns whatever workload process gets bootstrapped on this
// node and serializes all access to that state through a single
// goroutine (Run), rather than a mutex -- callers like mgmtapi send a
// request and wait for a reply instead of touching bootstrapped/cmd
// directly, so the goroutine that ends up calling exec.Command(...)
// .Start() is Run's, never an HTTP handler's.
type Supervisor struct {
	bootstrapFn BootstrapFunc
	persistFn   PersistFunc
	requests    chan bootstrapRequest
	stops       chan stopRequest

	// bootstrapped, stopping, and cmd are only ever read or written
	// from Run's goroutine.
	bootstrapped bool
	stopping     bool
	cmd          *exec.Cmd
}

// NewSupervisor returns a Supervisor that calls bootstrapFn to actually
// start a workload once Run is bootstrapped, and persistFn to record
// that it did so a later boot can recover the same Mode via
// PersistedMode. Production code passes Bootstrap and PersistMode;
// tests pass fakes so a bootstrap request doesn't spawn a real child
// process or touch the real STATE partition.
func NewSupervisor(bootstrapFn BootstrapFunc, persistFn PersistFunc) *Supervisor {
	return &Supervisor{
		bootstrapFn: bootstrapFn,
		persistFn:   persistFn,
		requests:    make(chan bootstrapRequest),
		stops:       make(chan stopRequest),
	}
}

// Run is the supervisor's loop: the only goroutine that ever reads or
// writes m's bootstrapped/stopping/cmd state. It returns when ctx is
// canceled -- callers run it in its own goroutine, and in production
// that ctx is never canceled: taking the node down goes through Stop
// (which leaves Run running, refusing further bootstraps) and then
// reboot(2), not through this loop exiting.
//
// It handles bootstrap requests (Bootstrap) and stop requests (Stop).
// Reaping the started process if it exits on its own, and restarting
// it, are not implemented here yet.
func (m *Supervisor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-m.requests:
			req.reply <- m.bootstrap(ctx, req.mode)
		case req := <-m.stops:
			req.reply <- m.stop(req.ctx)
		}
	}
}

// bootstrap is Run's own bootstrap step: reject the request if the
// node is stopping or bootstrapped is already set, otherwise persist
// mode and only then start the workload. It lives here, rather than in
// a caller like mgmtapi, because bootstrapped and cmd are only safe to
// read or write from Run's goroutine.
//
// The stopping check comes before the bootstrapped one so a caller
// racing a shutdown gets the more useful answer -- the node is going
// down, rather than merely already bootstrapped.
//
// Persisting comes first, before bootstrapFn ever runs: a workload this
// node can't durably remember choosing isn't safe to start in the first
// place, since a reboot would forget it ever ran and this call would
// have no way to undo whatever bootstrapFn just started. Persisting
// mode again on a later retry (e.g. after a transient bootstrapFn
// failure) is harmless -- it's the same value PersistMode already
// wrote.
func (m *Supervisor) bootstrap(ctx context.Context, mode Mode) bootstrapResult {
	if m.stopping {
		return bootstrapResult{err: ErrStopping}
	}

	if m.bootstrapped {
		return bootstrapResult{err: ErrAlreadyBootstrapped}
	}

	if err := m.persistFn(mode); err != nil {
		return bootstrapResult{err: fmt.Errorf("persisting bootstrap state: %w", err)}
	}

	cmd, err := m.bootstrapFn(ctx, mode)
	if err != nil {
		// Not marking bootstrapped lets the caller retry: nothing
		// about this node's running state has actually changed yet.
		return bootstrapResult{err: err}
	}

	m.bootstrapped = true
	m.cmd = cmd

	return bootstrapResult{cmd: cmd}
}

// Bootstrap sends a bootstrap request to m's running Run loop and waits
// for the result. It satisfies BootstrapFunc itself, so it's a drop-in
// replacement for the bare Bootstrap function at whatever call site
// constructs mgmtapi.New -- the only difference from that caller's
// point of view is that the actual work now happens on Run's goroutine,
// not the caller's.
//
// Both the request send and the reply wait are selected against
// ctx.Done() so a caller whose context is canceled doesn't block
// forever waiting on a Run loop that may have already exited.
func (m *Supervisor) Bootstrap(ctx context.Context, mode Mode) (*exec.Cmd, error) {
	reply := make(chan bootstrapResult, 1)

	select {
	case m.requests <- bootstrapRequest{mode: mode, reply: reply}:
	case <-ctx.Done():
		return nil, ctx.Err()
	}

	select {
	case res := <-reply:
		return res.cmd, res.err
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}
