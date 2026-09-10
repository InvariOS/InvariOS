// Package supervise starts OpenBao as a supervised child process.
//
// bao is never PID 1: the Go binary that calls StartOpenBao is PID 1
// itself, so bao is started with os/exec rather than syscall.Exec.
// Nothing Waits on it while it runs -- the only Wait today is in
// Supervisor.stop, when the node is going down -- so a bao that exits
// on its own lingers as a zombie until then. Observing and reacting to
// that exit (reaping, restarting) is not implemented yet.
package supervise

import (
	"context"
	"fmt"
	"os"
	"os/exec"
)

// baoPath is the location build_bao_uki's sibling step (fetch_openbao)
// installs the bao binary to in the image.
const baoPath = "/usr/bin/bao"

// StartOpenBao starts OpenBao's dev-mode server as a child process and
// returns immediately once it has started; it does not wait for bao to
// become ready to serve.
//
// -dev-no-store-token skips dev mode's usual write of the generated
// root token to a local convenience file (normally ~/.vault-token, for
// an interactive CLI on the same machine): mount.Ephemeral has already
// remounted / read-only by the time this runs, that write has nowhere
// writable to land, and the fixed -dev-root-token-id below already
// makes the token predictable without it.
//
// The child's stdout/stderr are inherited, so its logs land on whatever
// console(s) console.Setup already redirected this process's own output
// to.
// It is deliberately started with exec.Command, not exec.CommandContext:
// bao's lifetime is not tied to ctx. Stopping it is Supervisor.stop's
// job, which signals the returned process explicitly (SIGTERM, then
// SIGKILL after a grace period) so the shutdown sequence controls the
// timing, rather than a context cancellation that could only ever
// SIGKILL. ctx is accepted for signature parity with network.Up and
// future use (e.g. a supervised restart loop).
func StartOpenBao(_ context.Context) (*exec.Cmd, error) {
	cmd := exec.Command(
		baoPath,
		"server",
		"-dev",
		"-dev-listen-address=0.0.0.0:8200",
		"-dev-root-token-id=root",
		"-dev-no-store-token",
	)

	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("starting bao: %w", err)
	}

	return cmd, nil
}
