package supervise

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/invarios/invarios/internal/paths"
)

// PersistFunc persists mode so a later boot can recover it via
// PersistedMode. Production code passes PersistMode; tests pass a fake
// so persisting doesn't touch the real STATE partition.
type PersistFunc func(Mode) error

// bootstrapStateFile is where PersistMode writes and PersistedMode
// reads the bootstrapped Mode. A var, not a const, so tests can point
// it at a temp file instead of the real STATE partition.
var bootstrapStateFile = filepath.Join(paths.StateDir, "bootstrap.json")

// persistedState is bootstrapStateFile's on-disk shape.
type persistedState struct {
	Mode Mode `json:"mode"`
}

// PersistMode durably records mode as this node's bootstrap decision,
// so a later boot's PersistedMode call can recover it without an
// operator repeating the /bootstrap request.
//
// It writes to a temp file next to bootstrapStateFile and renames it
// into place rather than writing bootstrapStateFile directly: a crash
// or power loss mid-write then leaves either the old file or the new
// one intact, never a partial, unparseable one, since rename(2) on the
// same filesystem swaps the directory entry atomically.
func PersistMode(mode Mode) error {
	data, err := json.Marshal(persistedState{Mode: mode})
	if err != nil {
		return fmt.Errorf("supervise: marshaling bootstrap state: %w", err)
	}

	tmp := bootstrapStateFile + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return fmt.Errorf("supervise: writing %s: %w", tmp, err)
	}

	if err := os.Rename(tmp, bootstrapStateFile); err != nil {
		return fmt.Errorf("supervise: renaming %s to %s: %w", tmp, bootstrapStateFile, err)
	}

	return nil
}

// PersistedMode reads back a Mode PersistMode previously wrote. ok is
// false, with a nil error, when nothing has ever been persisted (e.g.
// a freshly installed node that hasn't been bootstrapped yet) --
// callers fall back to awaiting an operator's /bootstrap call in that
// case rather than treating it as a failure.
func PersistedMode() (mode Mode, ok bool, err error) {
	data, err := os.ReadFile(bootstrapStateFile)
	if errors.Is(err, os.ErrNotExist) {
		return "", false, nil
	}

	if err != nil {
		return "", false, fmt.Errorf("supervise: reading %s: %w", bootstrapStateFile, err)
	}

	var state persistedState
	if err := json.Unmarshal(data, &state); err != nil {
		return "", false, fmt.Errorf("supervise: parsing %s: %w", bootstrapStateFile, err)
	}

	return state.Mode, true, nil
}
