package cmd

import (
	"fmt"
	"log/slog"
	"os"

	"huawei.com/devbridge/internal/i18n"
	"huawei.com/devbridge/internal/updater"

	"github.com/spf13/cobra"
)

var verbose bool

var version = "dev"

var RootCmd = &cobra.Command{
	Use:   "devbridge",
	Short: i18n.T(i18n.Msg.Common.VersionInfo),
	Long:  i18n.T(i18n.Msg.Common.VersionInfo),
	PersistentPreRun: func(cmd *cobra.Command, args []string) {
		level := slog.LevelInfo
		if verbose {
			level = slog.LevelDebug
		}
		slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: level})))

		// Version check: print update notice to stderr if a newer version is
		// available. Normal commands notify at most once per day (CheckAsync);
		// the version command does its own synchronous check in its Run.
		if cmd.Name() != "version" {
			updater.CheckAsync(version)
		}
	},
}

var versionCmd = &cobra.Command{
	Use:   "version",
	Short: i18n.T(i18n.Msg.Common.VersionInfo),
	Args:  cobra.NoArgs,
	Run: func(cmd *cobra.Command, args []string) {
		fmt.Println(version)
		// Update notice goes to stderr so stdout contains only the version
		// number, keeping CI version checks reliable. Synchronous: prints every time.
		updater.CheckSync(version)
	},
}

func runError(fn func(cmd *cobra.Command, args []string) error) func(cmd *cobra.Command, args []string) error {
	return func(cmd *cobra.Command, args []string) error {
		err := fn(cmd, args)
		if err != nil {
			cmd.SilenceUsage = true
			cmd.SilenceErrors = true
			cmd.PrintErrln(err)
		}
		return err
	}
}

func init() {
	RootCmd.PersistentFlags().BoolVarP(&verbose, "verbose", "v", false, i18n.T(i18n.Msg.Common.FlagVerbose))
	RootCmd.AddCommand(versionCmd)
}
