package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os/exec"
	"strings"
	"testing"
	"time"

	"github.com/invarios/invarios/internal/power"
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

	s := newBootstrapServer(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
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
	s := newBootstrapServer(func(_ context.Context, mode supervise.Mode) (*exec.Cmd, error) {
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
	s := newBootstrapServer(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		return nil, supervise.ErrAlreadyBootstrapped
	})

	rec := postBootstrap(t, s, `{"mode":"single-dev"}`)

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestHandleBootstrap_MissingMode(t *testing.T) {
	s := newBootstrapServer(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		t.Fatal("bootstrapFn should not be called when mode is missing")

		return nil, nil
	})

	rec := postBootstrap(t, s, `{}`)

	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusBadRequest, rec.Body.String())
	}
}

func TestHandleBootstrap_MalformedJSON(t *testing.T) {
	s := newBootstrapServer(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
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
	s := newBootstrapServer(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
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

// rejectPower is the powerFn for tests that only exercise bootstrap:
// a power request reaching it is a test bug, not a scenario.
func rejectPower(_ context.Context, action power.Action) error {
	return fmt.Errorf("unexpected power request: %s", action)
}

// rejectBootstrap is the bootstrapFn for tests that only exercise the
// power endpoints, mirroring rejectPower.
func rejectBootstrap(context.Context, supervise.Mode) (*exec.Cmd, error) {
	return nil, errors.New("unexpected bootstrap request")
}

// newBootstrapServer is New for tests that only care about bootstrapFn.
func newBootstrapServer(bootstrapFn supervise.BootstrapFunc) *Server {
	return New(bootstrapFn, rejectPower)
}

// newPowerServer is New for tests that only care about powerFn.
func newPowerServer(powerFn power.RequestFunc) *Server {
	return New(rejectBootstrap, powerFn)
}

// post sends a bodiless POST to path through s's route table and
// returns the recorded response.
func post(t *testing.T, s *Server, path string) *httptest.ResponseRecorder {
	t.Helper()

	req := httptest.NewRequest(http.MethodPost, path, nil)
	rec := httptest.NewRecorder()

	s.mux().ServeHTTP(rec, req)

	return rec
}

func decodePowerStatus(t *testing.T, rec *httptest.ResponseRecorder) string {
	t.Helper()

	var resp powerResponse
	if err := json.NewDecoder(rec.Body).Decode(&resp); err != nil {
		t.Fatalf("decoding response: %v", err)
	}

	return resp.Status
}

// TestHandleBootstrap_Stopping verifies handleBootstrap maps
// supervise.ErrStopping to 503: the node is going down, so the request
// can't be served now and won't be until it's back up.
func TestHandleBootstrap_Stopping(t *testing.T) {
	s := newBootstrapServer(func(context.Context, supervise.Mode) (*exec.Cmd, error) {
		return nil, supervise.ErrStopping
	})

	rec := postBootstrap(t, s, `{"mode":"single-dev"}`)

	if rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusServiceUnavailable, rec.Body.String())
	}
}

func TestHandlePower_Reboot(t *testing.T) {
	var requested []power.Action

	s := newPowerServer(func(_ context.Context, action power.Action) error {
		requested = append(requested, action)

		return nil
	})

	rec := post(t, s, "/reboot")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	if len(requested) != 1 || requested[0] != power.Reboot {
		t.Fatalf("powerFn requests = %v, want exactly [%q]", requested, power.Reboot)
	}

	if status := decodePowerStatus(t, rec); status != "rebooting" {
		t.Fatalf("status field = %q, want %q", status, "rebooting")
	}
}

func TestHandlePower_Shutdown(t *testing.T) {
	var requested []power.Action

	s := newPowerServer(func(_ context.Context, action power.Action) error {
		requested = append(requested, action)

		return nil
	})

	rec := post(t, s, "/shutdown")

	if rec.Code != http.StatusAccepted {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusAccepted, rec.Body.String())
	}

	if len(requested) != 1 || requested[0] != power.Shutdown {
		t.Fatalf("powerFn requests = %v, want exactly [%q]", requested, power.Shutdown)
	}

	if status := decodePowerStatus(t, rec); status != "shutting-down" {
		t.Fatalf("status field = %q, want %q", status, "shutting-down")
	}
}

// TestHandlePower_AlreadyRequested verifies handlePower maps
// power.ErrAlreadyRequested to 409. Whether an action is already
// pending is powerFn's decision (in production, a power.Pending's):
// mgmtapi holds no power state to decide it from.
func TestHandlePower_AlreadyRequested(t *testing.T) {
	s := newPowerServer(func(context.Context, power.Action) error {
		return fmt.Errorf("%w: reboot", power.ErrAlreadyRequested)
	})

	rec := post(t, s, "/shutdown")

	if rec.Code != http.StatusConflict {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusConflict, rec.Body.String())
	}
}

func TestHandlePower_Failure(t *testing.T) {
	s := newPowerServer(func(context.Context, power.Action) error {
		return errors.New("power: boom")
	})

	rec := post(t, s, "/reboot")

	if rec.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d (body: %s)", rec.Code, http.StatusInternalServerError, rec.Body.String())
	}

	if msg := decodeError(t, rec); !strings.Contains(msg, "boom") {
		t.Fatalf("error = %q, want it to mention the underlying failure", msg)
	}
}

// TestHandlePower_MethodNotAllowed verifies the route table only
// accepts POST: a GET must not take the machine down (or even reach
// powerFn).
func TestHandlePower_MethodNotAllowed(t *testing.T) {
	s := newPowerServer(func(_ context.Context, action power.Action) error {
		t.Fatalf("powerFn called for a GET (%s)", action)

		return nil
	})

	for _, path := range []string{"/reboot", "/shutdown"} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		rec := httptest.NewRecorder()

		s.mux().ServeHTTP(rec, req)

		if rec.Code != http.StatusMethodNotAllowed {
			t.Fatalf("GET %s status = %d, want %d", path, rec.Code, http.StatusMethodNotAllowed)
		}
	}
}

// TestServe_DrainsInFlightResponseOnCancel is the property the power
// sequence depends on: when ctx is canceled from inside a handler (as
// power.Pending's Request effectively does, by unblocking runBoot),
// the client of that very request still receives its full response,
// and serve returns nil only afterwards.
func TestServe_DrainsInFlightResponseOnCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	s := newPowerServer(func(context.Context, power.Action) error {
		cancel()

		return nil
	})

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	served := make(chan error, 1)
	go func() { served <- s.serve(ctx, l) }()

	client := &http.Client{
		Transport: &http.Transport{DisableKeepAlives: true},
		Timeout:   5 * time.Second,
	}

	resp, err := client.Post("http://"+l.Addr().String()+"/reboot", "application/json", nil)
	if err != nil {
		t.Fatalf("POST /reboot: %v", err)
	}
	defer resp.Body.Close() //nolint:errcheck

	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusAccepted)
	}

	var body powerResponse
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding response body (was it cut off by the shutdown?): %v", err)
	}

	if body.Status != "rebooting" {
		t.Fatalf("status field = %q, want %q", body.Status, "rebooting")
	}

	select {
	case err := <-served:
		if err != nil {
			t.Fatalf("serve returned %v after a clean drain, want nil", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("serve did not return after ctx was canceled")
	}
}

// TestServe_ReturnsServerFailure verifies a server that dies on its own
// (here: its listener closed underneath it) surfaces that as an error,
// which cmd/root.go treats as fatal -- as opposed to the nil a
// deliberate cancellation produces.
func TestServe_ReturnsServerFailure(t *testing.T) {
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listening: %v", err)
	}

	if err := l.Close(); err != nil {
		t.Fatalf("closing listener: %v", err)
	}

	if err := newPowerServer(rejectPower).serve(context.Background(), l); err == nil {
		t.Fatal("serve on a closed listener returned nil, want an error")
	}
}
