package supervise

import (
	"context"
	"errors"
	"testing"
)

func TestBootstrap_UnknownMode(t *testing.T) {
	_, err := Bootstrap(context.Background(), Mode("bogus"))
	if !errors.Is(err, ErrModeNotImplemented) {
		t.Fatalf("err = %v, want it to satisfy errors.Is(err, ErrModeNotImplemented)", err)
	}
}
