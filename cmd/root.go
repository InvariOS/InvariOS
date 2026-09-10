package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/invarios/invarios/internal/console"
	"github.com/invarios/invarios/internal/install"
	"github.com/invarios/invarios/internal/mgmtapi"
	"github.com/invarios/invarios/internal/mount"
	"github.com/invarios/invarios/internal/network"
	"github.com/invarios/invarios/internal/power"
	"github.com/invarios/invarios/internal/supervise"
	"github.com/invarios/invarios/internal/version"
)

// stopGrace is how long runPower gives the workload to exit after
// SIGTERM before it's killed. bao's graceful stop (seal, close
// listeners) normally takes well under a second; this leaves room for a
// future raft-backed mode flushing storage without letting a hung
// process hold up a reboot indefinitely.
const stopGrace = 15 * time.Second

// rootCmd represents the base command when called without any subcommands
var rootCmd = &cobra.Command{
	Use:   "invarios",
	Short: "An immutable operating system for OpenBao.",
	Run: func(_ *cobra.Command, _ []string) {
		runInitialize()

		ctx := context.Background()

		installed, diskPath, err := install.Detect()
		if err != nil {
			console.Fatal("[install] detecting install state:", err)
		}

		if !installed {
			runInstall(ctx, diskPath)

			return // Unreachable: runInstall reboots or hangs in console.Fatal.
		}

		runBoot(ctx, diskPath)
	},
}

// runInitialize brings up the pseudo-filesystems (including efivarfs,
// needed by both Install and Boot below) and the console, mounts the
// ephemeral filesystems and remounts / read-only (see mount.Ephemeral),
// and prints the startup banner. This step runs unconditionally, before
// it's known whether the machine still needs installing.
func runInitialize() {
	if err := mount.VirtualFilesystems(); err != nil {
		// console.Setup hasn't run yet at this point, so console.Fatal
		// falls back to its plain os.Stdout path -- still correct here,
		// since stdout is still exactly what the kernel bound it to.
		console.Fatal("[init] fatal:", err)
	}

	console.Setup()

	if err := mount.Ephemeral(); err != nil {
		console.Fatal("[init] fatal:", err)
	}

	fmt.Println()
	fmt.Println("========================================")
	fmt.Println("                InvariOS                ")
	fmt.Println("========================================")
	fmt.Println(version.Short())
	fmt.Println()

	if kernelVersion, err := os.ReadFile("/proc/version"); err == nil {
		fmt.Print(string(kernelVersion))
	}
}

// runInstall installs invarios onto diskPath and reboots. It does not
// return on success: install.Run's own success path ends in
// power.Do (reboot(2)), which hands control back to the firmware. Only the
// failure path returns here, and that's fatal -- there's no disk to
// boot from yet to fall back to. It is not, however, a dead end: META
// is only initialized as Install's final step, so a power-cycle after
// any failure lands back in Install (install.Detect still reports not
// installed) rather than in runBoot against a half-written disk.
func runInstall(ctx context.Context, diskPath string) {
	if err := install.Run(ctx, diskPath); err != nil {
		console.Fatal("[install] fatal:", err)
	}
}

// runBoot is what an already-installed system falls through to: it
// mounts STATE and DATA, brings up networking, starts the supervisor
// goroutine that owns OpenBao's process lifecycle, recovers a
// previously persisted bootstrap decision if there is one, and then
// serves the management API. Starting OpenBao means choosing how to
// initialize it (e.g. a new single-node store versus joining an
// existing cluster); the choice itself always comes from a Mode, either
// read back from supervise.PersistedMode (a prior boot already decided)
// or from an operator's POST /bootstrap (this is the first boot since
// install). Either way, mgmtapi only decodes the /bootstrap call and
// forwards the chosen Mode to the supervisor; the supervisor's own
// goroutine is what calls supervise.Bootstrap (mapping the Mode to a
// running process), tracks whether this node has already been
// bootstrapped, and persists a successful Mode via supervise.PersistMode
// so the next boot can skip straight to recovery.
//
// The API keeps serving until an operator asks for a reboot or
// shutdown: mgmtapi records that in pending (again without acting on
// it), this goroutine sees it and hands off to runPower, which winds
// the node down in order. The server running in its own goroutine,
// rather than blocking here as it used to, is what lets this goroutine
// be the one that waits for either outcome -- the server failing on
// its own (fatal, as before) or a power request arriving.
//
// Unlike Install's mount.Ephemeral call (which runs unconditionally in
// runInitialize, before it's known whether the machine is installed),
// mounting STATE and DATA only makes sense once diskPath is known to
// already have them -- Install itself only formats them, on its way to
// a reboot that lands back here.
func runBoot(ctx context.Context, diskPath string) {
	if err := mount.Volumes(diskPath); err != nil {
		console.Fatal("[boot] mounting STATE/DATA:", err)
	}

	if err := network.Up(ctx, "eth0"); err != nil {
		fmt.Println("[net] error:", err)
	}

	supervisor := supervise.NewSupervisor(supervise.Bootstrap, supervise.PersistMode)
	go supervisor.Run(ctx)

	if mode, ok, err := supervise.PersistedMode(); err != nil {
		fmt.Println("[supervise] reading persisted bootstrap state:", err)
	} else if ok {
		fmt.Println("[supervise] recovering previous bootstrap:", mode)

		if _, err := supervisor.Bootstrap(ctx, mode); err != nil {
			fmt.Println("[supervise] recovering previous bootstrap:", err)
		}
	} else {
		fmt.Println("[mgmtapi] awaiting bootstrap")
	}

	fmt.Println("[mgmtapi] listening on", mgmtapi.Addr)

	pending := power.NewPending()
	srv := mgmtapi.New(supervisor.Bootstrap, pending.Request)

	srvCtx, stopServing := context.WithCancel(ctx)
	defer stopServing()

	srvDone := make(chan error, 1)
	go func() { srvDone <- srv.ListenAndServe(srvCtx) }()

	select {
	case err := <-srvDone:
		// Only reachable if the server failed on its own: a graceful
		// stop happens below, after pending fires, never before.
		console.Fatal("[mgmtapi] fatal:", err)
	case <-pending.Done():
	}

	runPower(ctx, pending.Action(), stopServing, srvDone, supervisor)
}

// runPower takes an installed, running node down for action. It does
// not return on success: power.Do's success path ends in reboot(2),
// which hands control back to the firmware. The steps, in order:
//
//  1. Stop the management API and wait for it to drain. The operator's
//     request is still being answered when this starts (the handler
//     only recorded action), and the drain is what guarantees its 202
//     reaches them before anything else happens. It also means no
//     further requests can arrive from here on.
//  2. Stop the workload, giving it stopGrace to exit on SIGTERM before
//     it's killed. Nothing the supervisor started may still be writing
//     to DATA or STATE by the time they're unmounted.
//  3. Unmount DATA and STATE, so their XFS logs are clean on the next
//     mount.
//  4. power.Do: flush the console, sync, reboot(2).
//
// Steps 1-3 log their failures and carry on rather than aborting: the
// operator asked for the machine to go down, and a stuck drain, a
// workload that had to be killed, or a volume that couldn't be
// unmounted are all things the next boot recovers from, whereas a node
// that refuses to reboot over them needs someone at the console. Only
// power.Do returning at all is fatal -- there's no further step to
// fall back to, and console.Fatal keeps the reason on screen.
func runPower(ctx context.Context, action power.Action, stopServing context.CancelFunc, srvDone <-chan error, supervisor *supervise.Supervisor) {
	fmt.Println("[power]", action, "requested")

	stopServing()

	if err := <-srvDone; err != nil {
		fmt.Println("[mgmtapi] stopping:", err)
	}

	stopCtx, cancel := context.WithTimeout(ctx, stopGrace)
	defer cancel()

	if err := supervisor.Stop(stopCtx); err != nil {
		fmt.Println("[supervise] stopping workload:", err)
	} else {
		fmt.Println("[supervise] workload stopped")
	}

	if err := mount.UnmountVolumes(); err != nil {
		fmt.Println("[power] unmounting volumes:", err)
	}

	fmt.Println("[power]", action)

	if err := power.Do(action); err != nil {
		console.Fatal("[power] fatal:", err)
	}
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
//
// Errors reaching here are always from a subcommand's RunE (e.g. build):
// the no-args root Run is the PID 1 boot path and never returns an error,
// handling its own fatal cases internally via console.Fatal instead. Cobra
// has already printed the error and usage by this point, so this only
// needs to give the process a non-zero exit code.
func Execute() {
	if err := rootCmd.Execute(); err != nil {
		os.Exit(1)
	}
}
