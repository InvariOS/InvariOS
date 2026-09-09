package cmd

import (
	"context"
	"fmt"
	"os"

	"github.com/spf13/cobra"

	"github.com/invarios/invarios/internal/console"
	"github.com/invarios/invarios/internal/install"
	"github.com/invarios/invarios/internal/mgmtapi"
	"github.com/invarios/invarios/internal/mount"
	"github.com/invarios/invarios/internal/network"
	"github.com/invarios/invarios/internal/supervise"
	"github.com/invarios/invarios/internal/version"
)

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
// unix.Reboot, which hands control back to the firmware. Only the
// failure path returns here, and that's fatal -- there's no disk to
// boot from yet to fall back to.
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

	srv := mgmtapi.New(supervisor.Bootstrap)
	if err := srv.ListenAndServe(ctx); err != nil {
		console.Fatal("[mgmtapi] fatal:", err)
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
