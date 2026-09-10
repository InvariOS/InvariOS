package power

import (
	"context"
	"errors"
	"testing"
)

func TestPending_FirstRequestWins(t *testing.T) {
	p := NewPending()

	select {
	case <-p.Done():
		t.Fatal("Done closed before any Request")
	default:
	}

	if got := p.Action(); got != "" {
		t.Fatalf("Action() = %q before any Request, want empty", got)
	}

	if err := p.Request(context.Background(), Reboot); err != nil {
		t.Fatalf("Request(Reboot): %v", err)
	}

	select {
	case <-p.Done():
	default:
		t.Fatal("Done not closed after a successful Request")
	}

	if got := p.Action(); got != Reboot {
		t.Fatalf("Action() = %q, want %q", got, Reboot)
	}
}

// TestPending_SecondRequestRejected verifies a later request, even for
// a different Action, neither overwrites the first nor is reported as
// accepted: the machine is already going down for the first one.
func TestPending_SecondRequestRejected(t *testing.T) {
	p := NewPending()

	if err := p.Request(context.Background(), Reboot); err != nil {
		t.Fatalf("first Request: %v", err)
	}

	err := p.Request(context.Background(), Shutdown)
	if !errors.Is(err, ErrAlreadyRequested) {
		t.Fatalf("second Request err = %v, want it to satisfy errors.Is(err, ErrAlreadyRequested)", err)
	}

	if got := p.Action(); got != Reboot {
		t.Fatalf("Action() = %q after rejected second Request, want the first, %q", got, Reboot)
	}
}

func TestPending_UnknownActionRejected(t *testing.T) {
	p := NewPending()

	if err := p.Request(context.Background(), Action("halt")); err == nil {
		t.Fatal("Request with an unknown Action: want an error, got nil")
	}

	select {
	case <-p.Done():
		t.Fatal("Done closed after a rejected Request")
	default:
	}

	// The slot is still free for a valid request afterwards.
	if err := p.Request(context.Background(), Shutdown); err != nil {
		t.Fatalf("Request(Shutdown) after a rejected one: %v", err)
	}
}
