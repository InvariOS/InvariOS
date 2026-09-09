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
	"net/http"

	"github.com/invarios/invarios/internal/supervise"
)

// Addr is the address and port the management API binds to. 8420 is
// below Linux's default ephemeral port range (32768-60999), so it
// can't collide with a source port the appliance's own outbound
// connections (e.g. an OCI registry pull) get auto-assigned.
const Addr = "0.0.0.0:8420"

// Server is the management API's HTTP handler. It holds no process or
// bootstrap state of its own -- bootstrapFn (in production, a running
// supervise.Supervisor's Bootstrap method) owns that, on its own
// goroutine. Server's only job is decoding a request, forwarding it to
// bootstrapFn, and translating the result into an HTTP response.
type Server struct {
	bootstrapFn supervise.BootstrapFunc
}

// New returns a Server that calls bootstrapFn when a bootstrap request
// is accepted. Production code passes a supervise.Supervisor's
// Bootstrap method; tests pass a fake so a bootstrap request doesn't
// spawn a real child process.
func New(bootstrapFn supervise.BootstrapFunc) *Server {
	return &Server{bootstrapFn: bootstrapFn}
}

// ListenAndServe starts the management API and blocks until it exits.
//
// ctx is accepted for signature parity with network.Up and bootstrapFn
// (a future graceful-shutdown path would drain in-flight requests using
// it before returning), not wired to cancellation yet: nothing today
// calls ListenAndServe with a ctx that is ever canceled.
func (s *Server) ListenAndServe(_ context.Context) error {
	srv := &http.Server{
		Addr:    Addr,
		Handler: s.mux(),
	}

	return srv.ListenAndServe()
}

// mux builds the management API's route table.
func (s *Server) mux() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(http.MethodPost+" /bootstrap", s.handleBootstrap)

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
	var req bootstrapRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
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
		default:
			writeError(w, http.StatusInternalServerError, fmt.Sprintf("starting workload: %v", err))
		}

		return
	}

	writeJSON(w, http.StatusOK, bootstrapResponse{Status: "bootstrapped"})
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
