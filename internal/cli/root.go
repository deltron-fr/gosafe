package cli

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

var rootCmd = &cobra.Command{
	Use:   "gosafe",
	Short: "A tool for inspecting and running untrusted projects",
	Long: `gosafe is a local tool that helps you run and
inspect untrusted projects inside isolated and disposable VMs.

Examples:
	gosafe run ./project -- npm test // runs command inside VM
	gosafe ./project // opens shell for VM`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}

func Execute() {
	rootCmd.AddCommand(runCmd)
	rootCmd.SilenceErrors = true
	rootCmd.SilenceUsage = true

	if err := rootCmd.Execute(); err != nil {
		fmt.Fprintln(rootCmd.ErrOrStderr(), err)
		os.Exit(1)
	}
}
