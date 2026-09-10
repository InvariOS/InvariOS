// Package mgmtapi implements the appliance's machine-plane management
// API: an HTTP server, run in PID 1, that an operator drives node
// lifecycle actions through. It is separate from any API the appliance's
// supervised workload exposes of its own -- operators talk to this one
// to control the node itself, and to the workload directly for whatever
// that workload offers.
//
// There is no authentication yet: every request is trusted as-is.
// Binding all interfaces without auth is a deliberate, temporary gap --
// mTLS is a later addition, not a redesign of this package's shape.
package mgmtapi

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"time"

	"github.com/invarios/invarios/internal/power"
	"github.com/invarios/invarios/internal/supervise"
)

// Addr is the address and port the management API binds to. 8420 is
// below Linux's default ephemeral port range (32768-60999), so it
// can't collide with a source port the appliance's own outbound
// connections (e.g. an OCI registry pull) get auto-assigned.
const Addr = "0.0.0.0:8420"

// maxBodyBytes caps how much of a request body a handler will read.
// The only body this API accepts is /bootstrap's few-field JSON object,
// so 4 KiB is generous. Without a cap, json.Decoder reads whatever the
// client sends into PID 1's heap: one unauthenticated multi-gigabyte
// POST is enough to have the OOM killer take out bao (it won't kill
// init) or the Go runtime abort, which for PID 1 is a kernel panic.
const maxBodyBytes = 4096

// Server timeouts. All three are vars rather than consts only so tests
// can shorten them; production never changes them.
//
// This API is reachable by anyone on the network and has no auth yet,
// so a client that opens a connection and then sends nothing (or
// trickles a header one byte at a time) must not be able to hold a
// goroutine and a file descriptor open indefinitely. readHeaderTimeout
// bounds the wait for a complete request line and headers; readTimeout
// bounds the whole request including the (already capped) body;
// idleTimeout bounds how long a keep-alive connection sits between
// requests. There is deliberately no WriteTimeout: /bootstrap's
// response waits on the workload actually starting, and cutting that
// off at an arbitrary point would report failure for a bootstrap that
// then succeeds.
var (
	readHeaderTimeout = 5 * time.Second
	readTimeout       = 10 * time.Second
	idleTimeout       = 60 * time.Second
)

// drainTimeout bounds how long ListenAndServe waits for in-flight
// requests to finish once its ctx is canceled. The only caller that
// cancels it is the power sequence in cmd/root.go, right after a
// reboot/shutdown handler has accepted the request -- so what's
// in flight is that handler's own 202, plus at most a few stragglers.
// Anything still open after this long is cut off: the machine is going
// down and a slow client shouldn't hold that up.
const drainTimeout = 5 * time.Second

// Server is the management API's HTTP handler. It holds no process,
// bootstrap, or power state of its own -- bootstrapFn (in production, a
// running supervise.Supervisor's Bootstrap method) and powerFn (a
// power.Pending's Request method) own that, on their own terms.
// Server's only job is decoding a request, forwarding it to the right
// function, and translating the result into an HTTP response.
type Server struct {
	bootstrapFn supervise.BootstrapFunc
	powerFn     power.RequestFunc
}

// New returns a Server that calls bootstrapFn when a bootstrap request
// is accepted and powerFn when a reboot or shutdown request is.
// Production code passes a supervise.Supervisor's Bootstrap method and a
// power.Pending's Request method; tests pass fakes so a request doesn't
// spawn a real child process or take the test machine down.
func New(bootstrapFn supervise.BootstrapFunc, powerFn power.RequestFunc) *Server {
	return &Server{bootstrapFn: bootstrapFn, powerFn: powerFn}
}

// ListenAndServe starts the management API on Addr and blocks until it
// exits: with an error if the listener or server fails on its own, or
// with nil once ctx is canceled and in-flight requests have been
// drained (see serve).
func (s *Server) ListenAndServe(ctx context.Context) error {
	l, err := net.Listen("tcp", Addr)
	if err != nil {
		return fmt.Errorf("listening on %s: %w", Addr, err)
	}

	return s.serve(ctx, l)
}

// serve runs the HTTP server on l until it fails or ctx is canceled.
//
// Cancellation is a graceful stop: the listener closes so no new
// requests are accepted, and http.Server.Shutdown waits (up to
// drainTimeout) for handlers already running to finish and their
// responses to be written to the socket before serve returns nil.
// That ordering is what lets a reboot/shutdown handler's 202 reach the
// operator: cmd/root.go cancels ctx as soon as the power request is
// recorded, then waits for this to return before it stops the
// workload and hands the machine to the kernel. Shutdown is given a
// fresh context rather than the already-canceled ctx, since it treats
// its own context expiring as "stop waiting, cut everything off" --
// passing ctx would skip the drain entirely.
//
// serve is separate from ListenAndServe so tests can bind an ephemeral
// port instead of the fixed Addr.
func (s *Server) serve(ctx context.Context, l net.Listener) error {
	srv := &http.Server{
		Handler:           s.mux(),
		ReadHeaderTimeout: readHeaderTimeout,
		ReadTimeout:       readTimeout,
		IdleTimeout:       idleTimeout,
	}

	served := make(chan error, 1)
	go func() { served <- srv.Serve(l) }()

	select {
	case err := <-served:
		return err
	case <-ctx.Done():
	}

	drainCtx, cancel := context.WithTimeout(context.Background(), drainTimeout)
	defer cancel()

	if err := srv.Shutdown(drainCtx); err != nil {
		// Shutdown gave up waiting; Close drops whatever is left so
		// Serve returns and this doesn't leak into the power sequence.
		_ = srv.Close()
		<-served

		return fmt.Errorf("draining in-flight requests: %w", err)
	}

	// Serve returns http.ErrServerClosed after a successful Shutdown;
	// that's the expected outcome here, not a failure.
	<-served

	return nil
}

// mux builds the management API's route table.
func (s *Server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodPost+" /bootstrap", s.handleBootstrap)
	mux.HandleFunc(http.MethodPost+" /reboot", s.handlePower(power.Reboot, "rebooting"))
	mux.HandleFunc(http.MethodPost+" /shutdown", s.handlePower(power.Shutdown, "shutting-down"))

	return mux
}

// bootstrapRequest is the JSON body a POST /bootstrap request carries.
type bootstrapRequest struct {
	Mode supervise.Mode `json:"mode"`
}

// bootstrapResponse is the JSON body a successful POST /bootstrap
// response carries.
type bootstrapResponse struct {
	Status string `json:"status"`
}

// powerResponse is the JSON body a successful POST /reboot or
// POST /shutdown response carries.
type powerResponse struct {
	Status string `json:"status"`
}

// errorResponse is the JSON body an unsuccessful response carries.
type errorResponse struct {
	Error string `json:"error"`
}

// handleBootstrap forwards the requested Mode to bootstrapFn and
// translates its result into an HTTP response. It never touches process
// or bootstrap state itself: whether this is the first bootstrap call
// or a repeat is bootstrapFn's decision (ErrAlreadyBootstrapped below),
// not something this handler tracks.
func (s *Server) handleBootstrap(w http.ResponseWriter, r *http.Request) {
	// MaxBytesReader also tells the server to close the connection
	// once the limit is hit, so an oversized body isn't drained either.
	r.Body = http.MaxBytesReader(w, r.Body, maxBodyBytes)

	var req bootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		var tooLarge *http.MaxBytesError
		if errors.As(err, &tooLarge) {
			writeError(w, http.StatusRequestEntityTooLarge, fmt.Sprintf("request body exceeds %d bytes", maxBodyBytes))

			return
		}

		writeError(w, http.StatusBadRequest, fmt.Sprintf("decoding request body: %v", err))

		return
	}

	if req.Mode == "" {
		writeError(w, http.StatusBadRequest, `"mode" is required`)

		return
	}

	if _, err := s.bootstrapFn(r.Context(), req.Mode); err != nil {
		switch {
		case errors.Is(err, supervise.ErrModeNotImplemented):
			writeError(w, http.StatusBadRequest, err.Error())
		case errors.Is(err, supervise.ErrAlreadyBootstrapped):
			writeError(w, http.StatusConflict, err.Error())
		case errors.Is(err, supervise.ErrStopping):
			writeError(w, http.StatusServiceUnavailable, err.Error())
		default:
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("starting workload: %v", err))
		}

		return
	}

	writeJSON(w, http.StatusOK, bootstrapResponse{Status: "bootstrapped"})
}

// handlePower returns the handler for a power endpoint: it asks powerFn
// to record action and, if accepted, answers 202 with status as the
// response's status field.
//
// 202 Accepted rather than 200 because the action has only been
// recorded at that point, not performed: the machine goes down after
// this response is written, once cmd/root.go has drained this server
// and stopped the workload. The request body is ignored -- there's
// nothing to parameterize yet, and the endpoint's path already says
// which action is wanted.
//
// Like handleBootstrap, this tracks no state of its own: whether an
// action is already pending is powerFn's decision (ErrAlreadyRequested),
// translated here to 409.
func (s *Server) handlePower(action power.Action, status string) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if err := s.powerFn(r.Context(), action); err != nil {
			switch {
			case errors.Is(err, power.ErrAlreadyRequested):
				writeError(w, http.StatusConflict, err.Error())
			default:
				writeError(w, http.StatusInternalServerError, fmt.Sprintf("requesting %s: %v", action, err))
			}

			return
		}

		writeJSON(w, http.StatusAccepted, powerResponse{Status: status})
	}
}

// writeJSON encodes v as the JSON response body with the given status
// code.
func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// writeError writes msg as a JSON error response with the given status
// code.
func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, errorResponse{Error: msg})
}
