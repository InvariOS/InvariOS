package supervise

import (
	"context"
	"errors"
	"os/exec"
	"testing"
)

// newRunningSupervisor returns a Supervisor whose Run loop is already
// executing in its own goroutine, stopped automatically at the end of
// the test.
func newRunningSupervisor(t *testing.T, bootstrapFn BootstrapFunc) *Supervisor {
	t.Helper()

	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)

	m := NewSupervisor(bootstrapFn)
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
