package cmd

import (
	"fmt"

	"github.com/spf13/cobra"

	"github.com/invarios/invarios/internal/version"
)

// versionCmd prints the build-time version information baked into
// this binary via -ldflags.
var versionCmd = &cobra.Command{
	Use:   "version",
	Short: "Print version information.",
	Run: func(_ *cobra.Command, _ []string) {
		fmt.Println(version.Name)
		fmt.Println("  Tag:", version.Tag)
		fmt.Println("  SHA:", version.SHA)
	},
}

func init() {
	rootCmd.AddCommand(versionCmd)
}
