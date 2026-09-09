package mount

import (
	"fmt"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"
)

// NoExec is the mount(2) flag combination for a writable mount that
// must never be executable, so a bug that plants a file there can't
// turn into code execution. Applied to every mount in this package,
// and to any other writable mount elsewhere in invarios (e.g.
// internal/install's temporary ESP mount).
const NoExec = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC

// tmpfsTargets must be writable but must not survive reboot: /run
// (sockets, pid files, DHCP scratch), /tmp (Install's boot-artifact
// extraction and ESP staging), and /var (until a LOG volume exists,
// this is also where logs land).
var tmpfsTargets = []string{"/run", "/tmp", "/var"}

// etcOverlayDir holds the /etc overlay's upper and work directories.
// They live on the /run tmpfs mounted by Ephemeral, above, so they're
// on a different filesystem than /etc's lower layer -- an overlayfs
// requirement -- and don't survive reboot themselves.
const etcOverlayDir = "/run/overlays/etc"

// Ephemeral mounts tmpfs over /run, /tmp, and /var, overlays /etc so
// runtime writes (network.Up's /etc/resolv.conf) keep landing at
// their usual path, and remounts / read-only. It must run after
// VirtualFilesystems (devtmpfs provides /dev) and before anything
// writes to /etc or relies on /tmp, and before Volumes mounts STATE
// and DATA (those are separate mount points, unaffected by /'s own
// read-only remount, but Ephemeral establishes the ordering the rest
// of boot assumes).
func Ephemeral() error {
	for _, target := range tmpfsTargets {
		if err := unix.Mount("tmpfs", target, "tmpfs", NoExec, ""); err != nil {
			return fmt.Errorf("mounting tmpfs at %s: %w", target, err)
		}
	}

	if err := overlayEtc(); err != nil {
		return err
	}

	if err := unix.Mount("", "/", "", unix.MS_REMOUNT|unix.MS_RDONLY, ""); err != nil {
		return fmt.Errorf("remounting / read-only: %w", err)
	}

	return nil
}

// overlayEtc gives /etc a writable upper layer backed by the /run
// tmpfs, while its lower layer stays the /etc the UKI shipped (the CA
// bundle, os-release). The lower layer is a bind mount of /etc rather
// than /etc itself: overlayfs rejects a lowerdir that is also the
// mount's own target.
func overlayEtc() error {
	lower := filepath.Join(etcOverlayDir, "lower")
	upper := filepath.Join(etcOverlayDir, "upper")
	work := filepath.Join(etcOverlayDir, "work")

	for _, dir := range []string{lower, upper, work} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return fmt.Errorf("creating %s: %w", dir, err)
		}
	}

	if err := unix.Mount("/etc", lower, "", unix.MS_BIND, ""); err != nil {
		return fmt.Errorf("bind-mounting /etc at %s: %w", lower, err)
	}

	options := fmt.Sprintf("lowerdir=%s,upperdir=%s,workdir=%s", lower, upper, work)
	if err := unix.Mount("overlay", "/etc", "overlay", NoExec, options); err != nil {
		return fmt.Errorf("overlaying /etc: %w", err)
	}

	return nil
}
