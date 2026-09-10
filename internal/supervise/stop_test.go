package supervise

import (
	"context"
	"errors"
	"os/exec"
	"testing"
	"time"
)

// startChild is a BootstrapFunc that starts name with args as a real
// child process, for tests that need Stop to signal something that
// actually reacts. It skips the test when name isn't installed, since
// these are OS tools (sleep, sh) rather than anything this repo builds.
func startChild(t *testing.T, name string, args ...string) BootstrapFunc {
	t.Helper()

	if _, err := exec.LookPath(name); err != nil {
		t.Skipf("%s not available: %v", name, err)
	}

	return func(context.Context, Mode) (*exec.Cmd, error) {
		cmd := exec.Command(name, args...)
		if err := cmd.Start(); err != nil {
			return nil, err
		}

		// If the test fails before Stop gets to it, don't leave the
		// child around. Kill on an already-reaped process is a harmless
		// os.ErrProcessDone.
		t.Cleanup(func() { _ = cmd.Process.Kill() })

		return cmd, nil
	}
}

func TestSupervisor_StopBeforeBootstrap(t *testing.T) {
	m := newRunningSupervisor(t, func(context.Context, Mode) (*exec.Cmd, error) {
		t.Fatal("bootstrapFn called without a Bootstrap request")

		return nil, nil
	})

	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop with nothing running: %v", err)
	}
}

// TestSupervisor_StopWithoutProcess covers a bootstrapFn that returned a
// Cmd it never started (as the fakes in supervisor_test.go do): there
// is no process to signal, and Stop must not try to.
func TestSupervisor_StopWithoutProcess(t *testing.T) {
	m := newRunningSupervisor(t, func(context.Context, Mode) (*exec.Cmd, error) {
		return &exec.Cmd{}, nil
	})

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop with an unstarted Cmd: %v", err)
	}
}

// TestSupervisor_StopTerminatesChild verifies the graceful path: a
// child that honors SIGTERM exits well within the grace period, and its
// signal exit status is not reported as a failure.
func TestSupervisor_StopTerminatesChild(t *testing.T) {
	m := newRunningSupervisor(t, startChild(t, "sleep", "60"))

	cmd, err := m.Bootstrap(context.Background(), ModeSingleDev)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	start := time.Now()

	if err := m.Stop(ctx); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("Stop took %v for a child that honors SIGTERM, want well under the grace period", elapsed)
	}

	if cmd.ProcessState == nil {
		t.Fatal("child not reaped after Stop")
	}
}

// TestSupervisor_StopKillsStubbornChild verifies the fallback: a child
// that ignores SIGTERM is SIGKILLed once the grace period passes, and
// Stop says so. `trap "" TERM` sets SIGTERM to SIG_IGN, which survives
// the exec into sleep.
func TestSupervisor_StopKillsStubbornChild(t *testing.T) {
	m := newRunningSupervisor(t, startChild(t, "sh", "-c", `trap "" TERM; exec sleep 60`))

	cmd, err := m.Bootstrap(context.Background(), ModeSingleDev)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	// Give sh a moment to install the trap before SIGTERM arrives;
	// otherwise the signal lands on a shell that still has the default
	// disposition and the test exercises the wrong path.
	time.Sleep(200 * time.Millisecond)

	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()

	err = m.Stop(ctx)
	if err == nil {
		t.Fatal("Stop of a child ignoring SIGTERM returned nil, want it to report the kill")
	}

	if errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Stop returned the bare deadline error %v, want it to have waited for the kill", err)
	}

	if cmd.ProcessState == nil {
		t.Fatal("child not reaped after Stop")
	}
}

// TestSupervisor_BootstrapAfterStopRefused verifies the window Stop
// closes: once stopping, a bootstrap request must not start a workload
// on a node that is about to unmount its volumes.
func TestSupervisor_BootstrapAfterStopRefused(t *testing.T) {
	var calls int

	m := newRunningSupervisor(t, func(context.Context, Mode) (*exec.Cmd, error) {
		calls++

		return &exec.Cmd{}, nil
	})

	if err := m.Stop(context.Background()); err != nil {
		t.Fatalf("Stop: %v", err)
	}

	_, err := m.Bootstrap(context.Background(), ModeSingleDev)
	if !errors.Is(err, ErrStopping) {
		t.Fatalf("Bootstrap after Stop err = %v, want it to satisfy errors.Is(err, ErrStopping)", err)
	}

	if calls != 0 {
		t.Fatalf("bootstrapFn called %d times after Stop, want 0", calls)
	}
}
