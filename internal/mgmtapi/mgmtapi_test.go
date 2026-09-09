package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"

	"github.com/invarios/invarios/internal/supervise"
)

// postBootstrap sends a POST /bootstrap request with the given raw JSON
// body (or, if body is "", no body at all) through s's route table and
// returns the recorded response.
func postBootstrap(t *testing.T, s *Server, body string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, "/bootstrap", strings.NewReader(body))
	rec := httptest.NewRecorder()

	s.mux().ServeHTTP(rec, req)

	return rec
}

func decodeError(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var resp errorResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding error response: %v", err)
	}

	return resp.Error
}

func TestHandleBootstrap_Success(t *testing.T) {
	var started bool

	s := New(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		started = true

		return &exec.Cmd{}, nil
	})

	rec := postBootstrap(t, s, `{"mode":"single-dev"}`)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusOK, rec.Body.String())
	}

	if !started {
		t.Fatal("bootstrapFn was never called")
	}

	var resp bootstrapResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	if resp.Status != "bootstrapped" {
		t.Fatalf("status field = %q, want %q", resp.Status, "bootstrapped")
	}
}

// TestHandleBootstrap_UnsupportedMode verifies handleBootstrap's error
// translation, not mode knowledge (mgmtapi has none): whatever
// bootstrapFn decides is unsupported, wrapping
// supervise.ErrModeNotImplemented, must come back as 400, regardless of
// which package made that decision.
func TestHandleBootstrap_UnsupportedMode(t *testing.T) {
	s := New(func(_ context.Context, mode supervise.Mode) (*exec.Cmd, error) {
		return nil, fmt.Errorf("%w: %q", supervise.ErrModeNotImplemented, mode)
	})

	rec := postBootstrap(t, s, `{"mode":"cluster"}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}

	if msg := decodeError(t, rec); !strings.Contains(msg, "cluster") || !strings.Contains(msg, "not implemented") {
		t.Fatalf("error = %q, want it to mention the rejected mode and \"not implemented\"", msg)
	}
}

// TestHandleBootstrap_AlreadyBootstrapped verifies handleBootstrap maps
// supervise.ErrAlreadyBootstrapped to 409. Whether a given call is
// actually the first or a repeat is entirely bootstrapFn's decision (in
// production, a running Supervisor's): this test only checks that
// mgmtapi translates that decision correctly, since mgmtapi holds no
// bootstrap state of its own to make the decision from.
func TestHandleBootstrap_AlreadyBootstrapped(t *testing.T) {
	s := New(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		return nil, supervise.ErrAlreadyBootstrapped
	})

	rec := postBootstrap(t, s, `{"mode":"single-dev"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestHandleBootstrap_MissingMode(t *testing.T) {
	s := New(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		t.Fatal("bootstrapFn should not be called when mode is missing")

		return nil, nil
	})

	rec := postBootstrap(t, s, `{}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandleBootstrap_MalformedJSON(t *testing.T) {
	s := New(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		t.Fatal("bootstrapFn should not be called for a malformed request body")

		return nil, nil
	})

	rec := postBootstrap(t, s, `{"mode":`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

// TestHandleBootstrap_StartFailure verifies handleBootstrap maps a
// plain (non-sentinel) bootstrapFn error to 500. Whether that failure
// leaves the node in a retryable state is a supervise.Supervisor
// property, covered by TestSupervisor_StartFailureIsRetryable: mgmtapi
// holds no state a retry could depend on, so there's nothing about
// retryability for this test to check.
func TestHandleBootstrap_StartFailure(t *testing.T) {
	s := New(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		return nil, errors.New("exec: boom")
	})

	rec := postBootstrap(t, s, `{"mode":"single-dev"}`)

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	if msg := decodeError(t, rec); !strings.Contains(msg, "boom") {
		t.Fatalf("error = %q, want it to mention the underlying failure", msg)
	}
}
