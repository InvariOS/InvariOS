package supervise

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"syscall"
)

// ErrStopping indicates a Supervisor was asked to bootstrap after Stop
// had already begun taking the node down. Callers that need to tell
// this apart from other failures (e.g. to pick an HTTP status code) can
// check for it with errors.Is.
var ErrStopping = errors.New("supervisor is stopping")

// stopRequest is sent to a running Supervisor's Run loop by Stop,
// carrying the caller's ctx (whose deadline is the grace period) and
// the reply channel Run sends the result back on.
type stopRequest struct {
	ctx   context.Context
	reply chan error
}

// Stop asks m's running Run loop to stop the workload, if one was
// started, and waits until it's gone. It is the first half of taking
// the node down (cmd/root.go's runPower): once it returns, nothing the
// supervisor started is still writing to DATA or STATE, so they can be
// unmounted.
//
// ctx's deadline is the grace period: the workload gets SIGTERM and
// until then to exit on its own, after which it's killed. The deadline
// belongs to the caller rather than this package because it's a
// property of how long the machine is willing to wait to go down, not
// of the workload.
//
// The request send is selected against ctx.Done(), like Bootstrap's,
// so a caller doesn't block forever on a Run loop that has already
// exited. The reply wait deliberately is not: once Run has accepted
// the request it always replies, and the deadline is enforced inside
// stop (which returns promptly after it passes, having killed the
// workload). Returning early on ctx here instead would hand control
// back to a caller about to unmount volumes while the workload is
// still being killed underneath them.
func (m *Supervisor) Stop(ctx context.Context) error {
	reply := make(chan error, 1)

	select {
	case m.stops <- stopRequest{ctx: ctx, reply: reply}:
	case <-ctx.Done():
		return ctx.Err()
	}

	return <-reply
}

// stop is Run's own stop step. It marks m as stopping before anything
// else, so a bootstrap request that arrives from here on is refused
// (ErrStopping) instead of starting a fresh workload on a node that's
// about to unmount its volumes -- the caller drains its API server
// before calling Stop, but a request already inside a slow handler can
// outlive that drain, and this closes that window regardless of
// timing.
//
// If a workload is running it gets SIGTERM (bao's shutdown signal,
// alongside SIGINT), then until ctx's deadline to exit; after that it's
// SIGKILLed. Wait is called exactly once, on its own goroutine, and
// both the graceful and the killed path collect that same result --
// os/exec rejects a second Wait, and SIGKILL guarantees the first one
// returns. This is also the first time anything Waits on the workload
// at all (see the package doc), so a workload that already exited on
// its own is simply reaped here.
//
// A non-zero or signal exit status is not a failure from this step's
// point of view: the process is gone, which is all a stop needs. Only
// failing to deliver a signal, or having to fall back to SIGKILL, is
// reported, so the console shows when a shutdown wasn't clean.
func (m *Supervisor) stop(ctx context.Context) error {
	m.stopping = true

	cmd := m.cmd
	m.cmd = nil

	if cmd == nil || cmd.Process == nil {
		return nil
	}

	if err := cmd.Process.Signal(syscall.SIGTERM); err != nil {
		return fmt.Errorf("signaling workload: %w", err)
	}

	waited := make(chan error, 1)
	go func() { waited <- cmd.Wait() }()

	select {
	case err := <-waited:
		return ignoreExitStatus(err)
	case <-ctx.Done():
	}

	// os.ErrProcessDone here means the workload exited between the
	// deadline passing and Kill: waited then already holds its result,
	// and the outcome is a clean stop after all, just a late one.
	if err := cmd.Process.Kill(); err != nil && !errors.Is(err, os.ErrProcessDone) {
		return fmt.Errorf("killing workload after grace period: %w", err)
	}

	if err := ignoreExitStatus(<-waited); err != nil {
		return fmt.Errorf("waiting for killed workload: %w", err)
	}

	return errors.New("workload did not exit within grace period, killed")
}

// ignoreExitStatus turns Wait's *exec.ExitError -- the process ran and
// exited non-zero or on a signal, exactly what stopping it produces --
// into nil, leaving only failures to actually wait on the process.
func ignoreExitStatus(err error) error {
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}

	return err
}
