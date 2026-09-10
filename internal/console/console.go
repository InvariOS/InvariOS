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

// pipeReader is the read end of the stdout/stderr pipe Setup installs
// (nil until/unless it does), drained by the goroutine Setup starts.
// Flush inspects it to find out whether that goroutine has caught up.
var pipeReader *os.File

// flushTimeout bounds how long Flush waits for the pipe to drain. It's
// generous relative to how quickly the drain goroutine normally keeps
// up (microseconds), so reaching it means the console is wedged -- in
// which case waiting longer won't help, and holding up a reboot for it
// is worse than losing the tail of the log.
const flushTimeout = 2 * time.Second

// flushGrace is how long Flush lingers after the pipe reads empty. An
// empty pipe means the drain goroutine has read everything, not that
// its final Write to the tty/serial device has completed, and there's
// no way to observe that from here. The same pause also gives the
// kernel a moment to put any in-flight network responses (mgmtapi's
// 202 to a reboot request) on the wire, which reboot(2) otherwise
// doesn't wait for -- so Flush pauses for it even when Setup installed
// no pipe.
const flushGrace = 100 * time.Millisecond

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
	pipeReader = r

	go drain(r, original, tty)
}

// drainBufSize is how much drain reads from the pipe per iteration.
// Half the pipe's default 64 KiB capacity: large enough that a burst
// of bao log output moves in a couple of reads, small enough that a
// single write to a 115200-baud serial console doesn't monopolize the
// loop for very long.
const drainBufSize = 32 * 1024

// drain copies everything written to r onto each of dsts, forever. It
// is Setup's background goroutine, and its one hard rule is that it
// never stops reading r while the pipe's write end is open.
//
// That rule is why this isn't io.Copy(io.MultiWriter(dsts...), r):
// io.Copy returns on the first write error to *either* destination
// (EIO from /dev/tty0 during a VT switch, a hung-up serial line, a
// short write), and once nothing reads the pipe its 64 KiB buffer
// fills and every subsequent write(2) to fd 1/2 -- every fmt.Println
// in PID 1, every log line from bao, which inherits those fds --
// blocks forever. runPower prints before it stops the workload, so a
// reboot request would be answered 202 and then never happen. Here
// each destination is written independently and a failure is simply
// dropped: losing output on one dead console is fine, wedging the
// whole node is not.
//
// Read only fails once the pipe's write end is closed everywhere,
// which never happens during Setup's lifetime (fd 1 and 2 hold it),
// so returning on a read error is just tidy shutdown, not a way the
// rule above gets broken.
func drain(r io.Reader, dsts ...io.Writer) {
	buf := make([]byte, drainBufSize)

	for {
		n, err := r.Read(buf)
		if n > 0 {
			for _, dst := range dsts {
				_, _ = dst.Write(buf[:n])
			}
		}

		if err != nil {
			return
		}
	}
}

// Flush waits, briefly and best-effort, for everything written to
// stdout/stderr so far to have been read out of the pipe Setup
// installed and handed to the real console(s). It exists for the one
// moment that matters: right before reboot(2), which discards whatever
// the drain goroutine hasn't copied yet -- the "[install] rebooting"
// line, bao's own shutdown messages -- with no chance to catch up.
//
// It observes the pipe's unread byte count (TIOCINQ, Linux's name for
// FIONREAD; on a pipe it reports bytes not yet read) rather than
// coordinating with the drain goroutine because that goroutine copies
// blindly and the writers include child processes (bao inherits fd
// 1/2) this package never sees; the pipe itself is the only place every
// writer's bytes pass through. When Setup installed no pipe, writes
// already go straight to the kernel's console and only the trailing
// flushGrace pause applies (see its comment for why it still matters).
func Flush() {
	if pipeReader != nil {
		deadline := time.Now().Add(flushTimeout)

		for time.Now().Before(deadline) {
			unread, err := unix.IoctlGetInt(int(pipeReader.Fd()), unix.TIOCINQ)
			if err != nil || unread == 0 {
				break
			}

			time.Sleep(10 * time.Millisecond)
		}
	}

	time.Sleep(flushGrace)
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
