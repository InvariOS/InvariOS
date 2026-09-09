package supervise

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// newRunningSupervisor returns a Supervisor whose Run loop is already
// executing in its own goroutine, stopped automatically at the end of
// the test. persistFn is a no-op; tests that care about persistence
// use newRunningSupervisorWithPersist instead.
func newRunningSupervisor(t *testing.T, bootstrapFn BootstrapFunc) *Supervisor {
	t.Helper()

	return newRunningSupervisorWithPersist(t, bootstrapFn, func(Mode) error { return nil })
}

// newRunningSupervisorWithPersist is newRunningSupervisor, but lets the
// caller supply its own persistFn (e.g. one that records calls) rather
// than a no-op.
func newRunningSupervisorWithPersist(t *testing.T, bootstrapFn BootstrapFunc, persistFn PersistFunc) *Supervisor {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m := NewSupervisor(bootstrapFn, persistFn)
	go m.Run(ctx)

	return m
}

func TestSupervisor_BootstrapSuccess(t *testing.T) {
	var calls int

	m := newRunningSupervisor(t, func(context.Context, Mode) (*exec.Cmd, error) {
		calls++

		return &exec.Cmd{}, nil
	})

	cmd, err := m.Bootstrap(context.Background(), ModeSingleDev)
	if err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if cmd == nil {
		t.Fatal("Bootstrap returned a nil *exec.Cmd on success")
	}

	if calls != 1 {
		t.Fatalf("bootstrapFn called %d times, want exactly 1", calls)
	}
}

func TestSupervisor_DoubleBootstrapConflict(t *testing.T) {
	calls := 0

	m := newRunningSupervisor(t, func(context.Context, Mode) (*exec.Cmd, error) {
		calls++

		return &exec.Cmd{}, nil
	})

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}

	_, err := m.Bootstrap(context.Background(), ModeSingleDev)
	if !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("second Bootstrap err = %v, want it to satisfy errors.Is(err, ErrAlreadyBootstrapped)", err)
	}

	if calls != 1 {
		t.Fatalf("bootstrapFn called %d times, want exactly 1", calls)
	}
}

func TestSupervisor_StartFailureIsRetryable(t *testing.T) {
	fail := true

	m := newRunningSupervisor(t, func(context.Context, Mode) (*exec.Cmd, error) {
		if fail {
			return nil, errors.New("exec: boom")
		}

		return &exec.Cmd{}, nil
	})

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err == nil {
		t.Fatal("first Bootstrap: want an error, got nil")
	}

	fail = false

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err != nil {
		t.Fatalf("retry Bootstrap: %v, want it to succeed since the failed attempt shouldn't have marked bootstrapped", err)
	}
}

func TestSupervisor_BootstrapSuccess_PersistsMode(t *testing.T) {
	var persisted []Mode

	m := newRunningSupervisorWithPersist(t,
		func(context.Context, Mode) (*exec.Cmd, error) { return &exec.Cmd{}, nil },
		func(mode Mode) error {
			persisted = append(persisted, mode)

			return nil
		},
	)

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err != nil {
		t.Fatalf("Bootstrap: %v", err)
	}

	if len(persisted) != 1 || persisted[0] != ModeSingleDev {
		t.Fatalf("persisted = %v, want exactly [%q]", persisted, ModeSingleDev)
	}
}

// TestSupervisor_BootstrapFailure_PersistsBeforeStarting verifies mode
// is already persisted by the time bootstrapFn is asked to start the
// workload -- even though this particular start fails, persisting
// isn't conditioned on it succeeding, only on bootstrapFn being called
// at all.
func TestSupervisor_BootstrapFailure_PersistsBeforeStarting(t *testing.T) {
	var persistCalls int

	m := newRunningSupervisorWithPersist(t,
		func(context.Context, Mode) (*exec.Cmd, error) { return nil, errors.New("exec: boom") },
		func(Mode) error {
			persistCalls++

			return nil
		},
	)

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err == nil {
		t.Fatal("Bootstrap: want an error, got nil")
	}

	if persistCalls != 1 {
		t.Fatalf("persistFn called %d times, want exactly 1", persistCalls)
	}
}

// TestSupervisor_PersistFailure_DoesNotStartWorkload verifies bootstrapFn
// is never called when persistFn fails: a mode this node can't durably
// remember choosing isn't safe to start in the first place, since a
// later retry has no way to stop whatever bootstrapFn might have
// already started.
func TestSupervisor_PersistFailure_DoesNotStartWorkload(t *testing.T) {
	var bootstrapCalls int

	m := newRunningSupervisorWithPersist(t,
		func(context.Context, Mode) (*exec.Cmd, error) {
			bootstrapCalls++

			return &exec.Cmd{}, nil
		},
		func(Mode) error { return errors.New("disk: boom") },
	)

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err == nil {
		t.Fatal("Bootstrap: want an error when persisting fails, got nil")
	}

	if bootstrapCalls != 0 {
		t.Fatalf("bootstrapFn called %d times on a persist failure, want 0", bootstrapCalls)
	}
}

func TestSupervisor_PersistFailure_StaysUnbootstrapped(t *testing.T) {
	fail := true

	m := newRunningSupervisorWithPersist(t,
		func(context.Context, Mode) (*exec.Cmd, error) { return &exec.Cmd{}, nil },
		func(Mode) error {
			if fail {
				return errors.New("disk: boom")
			}

			return nil
		},
	)

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err == nil {
		t.Fatal("first Bootstrap: want an error, got nil")
	}

	fail = false

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err != nil {
		t.Fatalf("retry Bootstrap: %v, want it to succeed since a persist failure shouldn't have marked bootstrapped", err)
	}
}

func TestSupervisor_DoubleBootstrapConflict_DoesNotPersistAgain(t *testing.T) {
	var persistCalls int

	m := newRunningSupervisorWithPersist(t,
		func(context.Context, Mode) (*exec.Cmd, error) { return &exec.Cmd{}, nil },
		func(Mode) error {
			persistCalls++

			return nil
		},
	)

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); err != nil {
		t.Fatalf("first Bootstrap: %v", err)
	}

	if _, err := m.Bootstrap(context.Background(), ModeSingleDev); !errors.Is(err, ErrAlreadyBootstrapped) {
		t.Fatalf("second Bootstrap err = %v, want it to satisfy errors.Is(err, ErrAlreadyBootstrapped)", err)
	}

	if persistCalls != 1 {
		t.Fatalf("persistFn called %d times, want exactly 1 (only for the first, successful call)", persistCalls)
	}
}
