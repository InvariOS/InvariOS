package supervise

import (
	"os"
	"path/filepath"
	"testing"
)

// withTempStateFile points bootstrapStateFile at a fresh temp file for
// the duration of the test, so PersistMode/PersistedMode never touch
// the real STATE partition.
func withTempStateFile(t *testing.T) {
	t.Helper()

	orig := bootstrapStateFile
	bootstrapStateFile = filepath.Join(t.TempDir(), "bootstrap.json")

	t.Cleanup(func() { bootstrapStateFile = orig })
}

func TestPersistMode_RoundTrip(t *testing.T) {
	withTempStateFile(t)

	if err := PersistMode(ModeSingleDev); err != nil {
		t.Fatalf("PersistMode: %v", err)
	}

	mode, ok, err := PersistedMode()
	if err != nil {
		t.Fatalf("PersistedMode: %v", err)
	}

	if !ok {
		t.Fatal("PersistedMode: ok = false, want true after PersistMode")
	}

	if mode != ModeSingleDev {
		t.Fatalf("mode = %q, want %q", mode, ModeSingleDev)
	}
}

func TestPersistedMode_NoFile(t *testing.T) {
	withTempStateFile(t)

	mode, ok, err := PersistedMode()
	if err != nil {
		t.Fatalf("PersistedMode: %v, want nil error when nothing has been persisted", err)
	}

	if ok {
		t.Fatalf("ok = true, mode = %q, want ok = false when nothing has been persisted", mode)
	}
}

func TestPersistedMode_CorruptFile(t *testing.T) {
	withTempStateFile(t)

	if err := os.WriteFile(bootstrapStateFile, []byte("not json"), 0o600); err != nil {
		t.Fatalf("writing corrupt state file: %v", err)
	}

	if _, _, err := PersistedMode(); err == nil {
		t.Fatal("PersistedMode: want a non-nil error for a corrupt state file, got nil")
	}
}
