// Package power is the appliance's single hand-off point to the kernel
// for going down. It records an operator's reboot/shutdown request
// (Pending) so the boot path can wind the node down in order before
// acting on it, and performs the final flush-sync-reboot(2) step (Do)
// that both that path and Install's end-of-install reboot share.
package power

import (
	"context"
	"errors"
	"fmt"
	"sync"

	"golang.org/x/sys/unix"

	"github.com/invarios/invarios/internal/console"
)

// Action is what an operator asked the machine to do once it has
// finished winding down. It's a distinct type so the request path
// (mgmtapi -> Pending -> cmd/root.go) passes the operator's choice
// around by name rather than as a bool or a raw reboot(2) magic value.
type Action string

const (
	// Reboot restarts the machine. On an installed node it comes back
	// up into the Boot path with whatever is persisted on STATE.
	Reboot Action = "reboot"
	// Shutdown powers the machine off.
	Shutdown Action = "shutdown"
)

// ErrAlreadyRequested indicates a Pending already holds an Action: the
// machine is already on its way down and a second request can neither
// change nor hurry that. Callers that need to tell this apart from
// other failures (e.g. to pick an HTTP status code) can check for it
// with errors.Is.
var ErrAlreadyRequested = errors.New("power action already requested")

// RequestFunc is Pending.Request's shape. It exists so callers that
// take it as a dependency (mgmtapi, so its tests can inject a fake) can
// name the type without depending on Pending itself.
type RequestFunc func(context.Context, Action) error

// Pending records a single requested Action for the boot path to act
// on.
//
// Request deliberately only records. Its caller is an HTTP handler
// that still has to answer the request it's handling, and the machine
// has to drain that server, stop the workload, and unmount its volumes
// before reboot(2) is safe -- none of which belongs on a request-scoped
// context or goroutine. So the handler records, and the goroutine that
// owns the boot sequence (cmd/root.go's runBoot) waits on Done and
// drives the rest.
//
// The first request wins and later ones are rejected rather than
// overwriting it: once the machine has started going down for a
// reboot, letting a shutdown request flip the outcome mid-sequence
// would make what happens next depend on timing the operator can't
// see.
type Pending struct {
	mu     sync.Mutex
	action Action
	done   chan struct{}
}

// NewPending returns a Pending with no Action recorded yet.
func NewPending() *Pending {
	return &Pending{done: make(chan struct{})}
}

// Request records action as the one the machine will perform and
// returns immediately; the actual work happens on whoever is waiting
// on Done. It satisfies RequestFunc. A second call, with any Action,
// returns ErrAlreadyRequested.
//
// ctx is accepted for RequestFunc's shape (matching
// supervise.BootstrapFunc); recording is instantaneous and never
// blocks on it.
func (p *Pending) Request(_ context.Context, action Action) error {
	switch action {
	case Reboot, Shutdown:
	default:
		return fmt.Errorf("unknown power action %q", action)
	}

	p.mu.Lock()
	defer p.mu.Unlock()

	if p.action != "" {
		return fmt.Errorf("%w: %s", ErrAlreadyRequested, p.action)
	}

	p.action = action
	close(p.done)

	return nil
}

// Done returns a channel that is closed once Request has recorded an
// Action, at which point Action returns it.
func (p *Pending) Done() <-chan struct{} {
	return p.done
}

// Action returns the recorded Action, or "" if Request hasn't been
// called successfully yet.
func (p *Pending) Action() Action {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.action
}

// Do hands the machine to the kernel for action and does not return on
// success: reboot(2) with RESTART or POWER_OFF never does. It is the
// last step of both the Boot path's operator-requested sequence
// (cmd/root.go's runPower) and Install's end-of-install reboot, so
// everything that must happen for the hand-off itself to be clean
// lives here, where neither caller can forget it. In order:
//
//  1. console.Flush, so the last lines written to the console -- the
//     caller's "rebooting" message, a stopped workload's own shutdown
//     output -- reach the real console(s) instead of dying unread in
//     the stdout pipe console.Setup installed.
//  2. unix.Sync, because reboot(2) itself does not sync: RESTART and
//     POWER_OFF discard dirty pages that haven't been written back.
//     Callers unmount what they can before getting here, but Sync is
//     what covers anything still mounted (and is harmless when nothing
//     is).
//  3. unix.Reboot. POWER_OFF rather than HALT for Shutdown: the kernel
//     is built with ACPI, so POWER_OFF actually cuts power (and exits
//     QEMU under `make boot`), whereas HALT prints "System halted" and
//     spins forever with the machine still on.
//
// Stopping the workload and unmounting volumes are deliberately not
// here: they depend on what the caller has started and mounted, which
// differs between Install (nothing persistent) and Boot (bao, STATE,
// DATA).
func Do(action Action) error {
	var cmd int

	switch action {
	case Reboot:
		cmd = unix.LINUX_REBOOT_CMD_RESTART
	case Shutdown:
		cmd = unix.LINUX_REBOOT_CMD_POWER_OFF
	default:
		return fmt.Errorf("unknown power action %q", action)
	}

	console.Flush()
	unix.Sync()

	if err := unix.Reboot(cmd); err != nil {
		return fmt.Errorf("reboot(2) for %s: %w", action, err)
	}

	return nil
}
