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

		// Skip version check for the version command itself (it does a sync check)
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
		if result := updater.CheckSync(version); result != nil {
			if updater.IsNewer(version, result.LatestVersion) {
				// 更新通知属于信息性输出，输出到 stderr，
				// 保持 stdout 只含版本号，与 CheckAsync 行为一致，
				// 避免 CI 版本注入校验等多行 stdout 比较失败。
				fmt.Fprintf(os.Stderr, "\nA new version is available: %s (current: %s)\nUpdate:\n%s\n",
					result.LatestVersion, version, updater.InstallCommand())
			}
		}
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
