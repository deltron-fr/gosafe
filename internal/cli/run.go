package cli

import "github.com/spf13/cobra"

var runCmd = &cobra.Command{
	Use:   "gosafe run ./myproject",
	Short: "Run a specific project inside the sandbox",
	Long: `gosafe runs a specific project inside the sandbox.

Examples:
	gosafe run ./project -- npm test // runs command inside VM`,
	RunE: func(cmd *cobra.Command, args []string) error {
		return nil
	},
}
