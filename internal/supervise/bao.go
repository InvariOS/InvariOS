// Package supervise starts OpenBao as a supervised child process.
//
// bao is never PID 1: the Go binary that calls StartOpenBao is PID 1
// itself, so bao is started with os/exec rather than syscall.Exec, and its
// exit is observed via Wait in a goroutine so it doesn't linger as a
// zombie.
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
// bao's lifetime is not tied to ctx, since this slice has no shutdown
// path yet that would cancel it. ctx is accepted for signature parity
// with network.Up and future use (e.g. a supervised restart loop).
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
