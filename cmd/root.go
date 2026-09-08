package cmd

import (
	"context"
	"fmt"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/invarios/invarios/internal/console"
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
		if err := mount.VirtualFilesystems(); err != nil {
			fmt.Println("[init] fatal:", err)
			os.Exit(1)
		}

		console.Setup()

		fmt.Println()
		fmt.Println("========================================")
		fmt.Println("                InvariOS                ")
		fmt.Println("========================================")
		fmt.Println(version.Short())
		fmt.Println()

		if kernelVersion, err := os.ReadFile("/proc/version"); err == nil {
			fmt.Print(string(kernelVersion))
		}

		ctx := context.Background()

		if err := network.Up(ctx, "eth0"); err != nil {
			fmt.Println("[net] error:", err)
		}

		if child, err := supervise.StartOpenBao(ctx); err != nil {
			fmt.Println("[openbao] failed to start:", err)
		} else {
			go func() {
				fmt.Println("[openbao] exited:", child.Wait())
			}()
		}

		fmt.Println("Go supervisor successfully started...")
		for {
			time.Sleep(1 * time.Second)
		}
	},
}

// Execute adds all child commands to the root command and sets flags appropriately.
// This is called by main.main(). It only needs to happen once to the rootCmd.
func Execute() {
	err := rootCmd.Execute()
	if err != nil {
		os.Exit(1)
	}
}

func init() {

}
