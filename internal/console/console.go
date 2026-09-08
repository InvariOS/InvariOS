// Package console duplicates the process's stdout/stderr onto a second
// display when one is present, so a graphical or VNC console shows the
// same boot and appliance log output as a serial connection.
package console

import (
	"fmt"
	"io"
	"os"
	"time"

	"golang.org/x/sys/unix"
)

// ttyPath is the graphical framebuffer console. When present (a VGA/GOP
// or virtio-gpu display is attached), it's a second destination for
// everything written to stdout/stderr, in addition to whatever
// /dev/console already resolves to (the kernel's "console=" cmdline
// target, typically the serial port).
const ttyPath = "/dev/tty0"

// realConsoles holds the same two underlying files Setup fans stdout/
// stderr out to (nil until/unless Setup populates it). Fatal writes to
// these directly rather than through the stdout/stderr fd Setup
// repoints at a pipe: that fd is drained by a background goroutine with
// no guarantee of having caught up by the time the calling goroutine's
// next line of code runs, which for Fatal is a halt that never resumes
// this goroutine to find out.
var realConsoles []io.Writer

// Setup fans stdout and stderr out to both the console already in place
// (bound by the kernel before this process was exec'd) and /dev/tty0,
// when the latter exists. It's a no-op when /dev/tty0 doesn't exist --
// stdout/stderr are left exactly as the kernel set them up.
//
// Because this works by repointing the underlying file descriptors
// (not just this process's os.Stdout/os.Stderr), any child process that
// inherits fd 1/2 -- e.g. the supervised bao server -- is duplicated to
// both consoles too, without that child needing to know anything about
// it.
//
// Errors are only ever printed, never returned: losing the secondary
// console output isn't worth failing boot over.
func Setup() {
	if _, err := os.Stat(ttyPath); err != nil {
		return
	}

	// Preserve the console the kernel already bound to fd 1, before
	// that fd gets repointed at the pipe below.
	originalFd, err := unix.Dup(int(os.Stdout.Fd()))
	if err != nil {
		fmt.Println("[console] duplicating original console fd:", err)
		return
	}
	original := os.NewFile(uintptr(originalFd), "console")

	tty, err := os.OpenFile(ttyPath, os.O_WRONLY, 0)
	if err != nil {
		fmt.Println("[console] opening", ttyPath+":", err)
		_ = original.Close()
		return
	}

	r, w, err := os.Pipe()
	if err != nil {
		fmt.Println("[console] creating pipe:", err)
		_ = original.Close()
		_ = tty.Close()
		return
	}

	if err := unix.Dup2(int(w.Fd()), int(os.Stdout.Fd())); err != nil {
		fmt.Println("[console] duplicating pipe onto stdout:", err)
		_ = original.Close()
		_ = tty.Close()
		_ = r.Close()
		_ = w.Close()
		return
	}

	if err := unix.Dup2(int(w.Fd()), int(os.Stderr.Fd())); err != nil {
		// stdout is already repointed at the pipe at this point, so
		// there's nothing left to safely roll back -- proceed without
		// stderr duplicated rather than leaving stdout half-migrated.
		fmt.Println("[console] duplicating pipe onto stderr:", err)
	}

	// fd 1 (and, usually, fd 2) now reference the same pipe write end,
	// so closing this copy of it doesn't close the pipe.
	_ = w.Close()

	// Only now, with every step that could still fail behind it, is it
	// safe to publish original/tty for Fatal to write to directly --
	// any earlier failure return above leaves realConsoles at its nil
	// zero value, so Fatal correctly falls back to its plain path
	// instead of writing into files this function already closed.
	realConsoles = []io.Writer{original, tty}

	go func() {
		_, _ = io.Copy(io.MultiWriter(original, tty), r)
	}()
}

// Fatal prints args exactly like fmt.Println, straight to the real
// console(s) -- bypassing the stdout pipe Setup installs, if it ran at
// all -- and then hangs forever without returning.
//
// This process is PID 1: the kernel panics with "Attempted to kill
// init!" and dumps a stack trace the instant PID 1 exits for any
// reason, os.Exit(1) included, discarding whatever this printed in
// favor of that generic panic. Writing directly to realConsoles (when
// Setup has populated it) rather than through stdout's pipe matters
// for the same reason: that pipe is drained by a background goroutine
// with no guaranteed-caught-up point to wait for, and Fatal's caller
// never runs another line of code to find out if it lost the race.
// Hanging in place, rather than exiting or rebooting, keeps the actual
// reason on screen for whoever's watching the console instead of
// replacing it with either of those.
func Fatal(args ...any) {
	msg := fmt.Sprintln(args...)

	if len(realConsoles) == 0 {
		fmt.Print(msg)
	} else {
		_, _ = io.MultiWriter(realConsoles...).Write([]byte(msg))
	}

	for {
		time.Sleep(time.Hour)
	}
}
