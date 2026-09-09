package supervise

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
)

// Mode selects which workload Bootstrap starts. It is a distinct type,
// rather than a bare string, so callers building a request around it
// already have a place for cluster and non-dev modes once they exist --
// adding them later only means adding Mode values and a case in
// Bootstrap, not changing any caller's shape.
type Mode string

// ModeSingleDev is the only Mode Bootstrap has a case for today: a
// single-node OpenBao dev server, with no persistent storage. Cluster
// topologies and non-dev (raft-backed) modes depend on DATA partition
// support that doesn't exist yet.
const ModeSingleDev Mode = "single-dev"

// ErrModeNotImplemented indicates Bootstrap was asked for a Mode it
// doesn't have a case for yet. Callers that need to tell "unsupported
// request" apart from "supported request that failed to start" (e.g.
// to pick an HTTP status code) can check for it with errors.Is.
var ErrModeNotImplemented = errors.New("mode not implemented")

// BootstrapFunc is Bootstrap's shape. It exists so callers that take
// Bootstrap as a dependency (e.g. mgmtapi, so its tests can inject a
// fake instead of spawning a real child process) can name the type
// without redeclaring it around a Mode that isn't theirs.
type BootstrapFunc func(context.Context, Mode) (*exec.Cmd, error)

// Bootstrap starts the workload for mode and returns its process handle
// without waiting for it to become ready to serve. It is the single
// place that maps a bootstrap decision to a running process, so the
// same mapping is reusable by anything that has such a decision to
// apply -- today that's only mgmtapi's HTTP handler, but a later boot
// sequence that re-applies a decision read back from persisted state
// would call this same function rather than duplicating the mapping.
func Bootstrap(ctx context.Context, mode Mode) (*exec.Cmd, error) {
	switch mode {
	case ModeSingleDev:
		return StartOpenBao(ctx)
	default:
		return nil, fmt.Errorf("%w: %q", ErrModeNotImplemented, mode)
	}
}
