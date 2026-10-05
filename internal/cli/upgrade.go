package cli

import (
	"context"
	"fmt"

	"github.com/shichao-wang/cpagw/internal/upgrade"
	"github.com/spf13/cobra"
)

type upgradeRunner func(context.Context, string) (upgrade.Result, error)

func newUpgradeCommand(run upgradeRunner) *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "升级当前可执行文件至最新 Release",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			currentVersion := Version().Version
			currentLabel := currentVersion
			if currentLabel == "" {
				currentLabel = "unknown（开发构建）"
			}
			out := cmd.OutOrStdout()
			if _, err := fmt.Fprintf(out, "当前版本：%s\n", currentLabel); err != nil {
				return fmt.Errorf("无法输出当前版本")
			}
			if _, err := fmt.Fprintln(out, "正在检查最新 Release…"); err != nil {
				return fmt.Errorf("无法输出升级状态")
			}

			result, err := run(cmd.Context(), currentVersion)
			if err != nil {
				if result.Updated {
					return fmt.Errorf("升级过程中发生错误；二进制已更新为 %s（%s）：%w", result.Version, result.Path, err)
				}
				return fmt.Errorf("升级失败：%w", err)
			}

			var message string
			if !result.Updated {
				message = fmt.Sprintf("已是最新版本（%s）", result.Version)
			} else {
				message = fmt.Sprintf("已升级至 %s\n可执行文件：%s\n配置未修改；运行中的网关需手动重启。", result.Version, result.Path)
			}
			if result.Warning != "" {
				message += fmt.Sprintf("\n警告：%s", result.Warning)
			}
			if _, err := fmt.Fprintln(out, message); err != nil {
				if result.Updated {
					return fmt.Errorf("二进制已更新为 %s（%s），但无法输出升级结果", result.Version, result.Path)
				}
				return fmt.Errorf("无法输出升级结果")
			}
			return nil
		},
	}
}
