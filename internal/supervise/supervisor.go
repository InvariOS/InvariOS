package supervise

import (
	"context"
	"errors"
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
	requests    chan bootstrapRequest

	// bootstrapped and cmd are only ever read or written from Run's
	// goroutine.
	bootstrapped bool
	cmd          *exec.Cmd
}

// NewSupervisor returns a Supervisor that calls bootstrapFn to actually
// start a workload once Run is bootstrapped. Production code passes
// Bootstrap; tests pass a fake so a bootstrap request doesn't spawn a
// real child process.
func NewSupervisor(bootstrapFn BootstrapFunc) *Supervisor {
	return &Supervisor{
		bootstrapFn: bootstrapFn,
		requests:    make(chan bootstrapRequest),
	}
}

// Run is the supervisor's loop: the only goroutine that ever reads or
// writes m's bootstrapped/cmd state. It returns when ctx is canceled --
// callers run it in its own goroutine and rely on that cancellation for
// shutdown, since it otherwise never returns on its own.
//
// Reaping the started process if it exits, restarting it, and reacting
// to reboot/shutdown requests are not implemented here yet -- this loop
// only ever handles bootstrap requests today.
func (m *Supervisor) Run(ctx context.Context) {
	for {
		select {
		case <-ctx.Done():
			return
		case req := <-m.requests:
			req.reply <- m.bootstrap(ctx, req.mode)
		}
	}
}

// bootstrap is Run's own bootstrap step: reject the request if
// bootstrapped is already set, otherwise start the workload and record
// that this node has bootstrapped. It lives here, rather than in a
// caller like mgmtapi, because bootstrapped and cmd are only safe to
// read or write from Run's goroutine.
func (m *Supervisor) bootstrap(ctx context.Context, mode Mode) bootstrapResult {
	if m.bootstrapped {
		return bootstrapResult{err: ErrAlreadyBootstrapped}
	}

	cmd, err := m.bootstrapFn(ctx, mode)
	if err != nil {
		// Not marking bootstrapped lets the caller retry: nothing
		// about this node's state has actually changed yet.
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
