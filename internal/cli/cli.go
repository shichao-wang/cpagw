package cli

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
	"github.com/shichao-wang/cpagw/internal/upgrade"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Options 为主程序集成运行时热重载和 agent 引用检查提供回调。
type Options struct {
	OnChange          func() error
	ProfileReferenced func(profileID string) (bool, error)
	Login             func(context.Context, bool) (config.OAuthCredential, error)
}

// VersionInfo 保存构建时注入的版本信息，供 version 命令输出。
type VersionInfo struct {
	Version string
	Commit  string
	Date    string
}

// version 由发布流程通过 ldflags -X 注入；从源码直接构建时保持为空。
var version string

// commit 由发布流程通过 ldflags -X 注入 commit 短 SHA。
var commit string

// date 由发布流程通过 ldflags -X 注入构建时间（RFC3339）。
var date string

// Version 返回构建时注入的版本信息；未注入时 Version 为空字符串。
func Version() VersionInfo {
	return VersionInfo{Version: version, Commit: commit, Date: date}
}

// formatVersion 拼接 version 命令的输出；各字段为空时省略对应行，均未注入时返回 unknown。
func formatVersion(info VersionInfo) string {
	var lines []string
	if info.Version != "" {
		lines = append(lines, "cpagw "+info.Version)
	}
	if info.Commit != "" {
		lines = append(lines, "commit: "+info.Commit)
	}
	if info.Date != "" {
		lines = append(lines, "构建时间: "+info.Date)
	}
	if len(lines) == 0 {
		return "cpagw unknown（开发构建，版本信息未注入）"
	}
	return strings.Join(lines, "\n")
}

// newVersionCommand 展示构建时注入的版本信息。
func newVersionCommand() *cobra.Command {
	return &cobra.Command{
		Use:   "version",
		Short: "显示版本信息",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			_, err := fmt.Fprintln(cmd.OutOrStdout(), formatVersion(Version()))
			return err
		},
	}
}

// NewCommand 构建 cpagw 命令树；服务端和 agent 命令可由主程序继续挂载。
func NewCommand() *cobra.Command {
	return NewCommandWithOptions(Options{})
}

// NewCommandWithOptions 构建带运行时集成回调的 cpagw 命令树。
func NewCommandWithOptions(opts Options) *cobra.Command {
	var stateDir string
	root := &cobra.Command{
		Use:           "cpagw",
		Short:         "管理 CLIProxyAPI 网关配置",
		SilenceUsage:  true,
		SilenceErrors: true,
	}
	// 不设置 root.Version：cobra 仅在 Version 非空时注册 --version/-v，
	// 本工具只用 version 子命令，避免两套入口输出不一致。
	root.PersistentFlags().StringVar(&stateDir, "state-dir", "", "状态目录（默认：$XDG_CONFIG_HOME/cpagw 或 ~/.config/cpagw）")
	root.AddCommand(newVersionCommand())
	root.AddCommand(newUpgradeCommand(upgrade.Run))
	root.AddCommand(newProviderCommand(func() (*store.Store, error) { return store.New(stateDir) }, opts))
	root.AddCommand(newProfileCommand(func() (*store.Store, error) { return store.New(stateDir) }, opts))
	root.AddCommand(stateCommand(func() (*store.Store, error) { return store.New(stateDir) }))
	return root
}

func stateCommand(open storeFactory) *cobra.Command {
	parent := &cobra.Command{Use: "state", Short: "管理本地状态格式"}
	parent.AddCommand(&cobra.Command{Use: "migrate", Short: "将本地状态逐版本迁移到当前格式", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		lock, err := s.AcquireRunLock()
		if err != nil {
			return fmt.Errorf("迁移前必须停止网关：%w", err)
		}
		defer lock.Close()
		if err := s.Migrate(); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "状态迁移检查完成")
		return err
	}})
	return parent
}

func notifyChanged(cmd *cobra.Command, opts Options) error {
	if opts.OnChange == nil {
		return nil
	}
	if err := opts.OnChange(); err != nil {
		return fmt.Errorf("配置已保存，但运行时重载失败")
	}
	return nil
}

func apiKey(cmd *cobra.Command, useStdin bool) (string, error) {
	if useStdin {
		data, err := ioReadAll(cmd.InOrStdin())
		if err != nil {
			return "", fmt.Errorf("无法从标准输入读取 API key")
		}
		key := strings.TrimSpace(string(data))
		if err := config.ValidateKey(key); err != nil {
			return "", err
		}
		return key, nil
	}
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return "", fmt.Errorf("非交互环境请使用 --api-key-stdin 从标准输入提供 API key")
	}
	if _, err := fmt.Fprint(cmd.ErrOrStderr(), "API key: "); err != nil {
		return "", fmt.Errorf("无法读取 API key")
	}
	secret, err := term.ReadPassword(int(file.Fd()))
	_, _ = fmt.Fprintln(cmd.ErrOrStderr())
	if err != nil {
		return "", fmt.Errorf("无法读取 API key")
	}
	key := strings.TrimSpace(string(secret))
	if err := config.ValidateKey(key); err != nil {
		return "", err
	}
	return key, nil
}

func confirmRemoval(cmd *cobra.Command, yes bool, subject string) error {
	if yes {
		return nil
	}
	file, ok := cmd.InOrStdin().(*os.File)
	if !ok || !term.IsTerminal(int(file.Fd())) {
		return fmt.Errorf("非交互环境删除操作必须提供 --yes")
	}
	if _, err := fmt.Fprintf(cmd.ErrOrStderr(), "确认删除 %s？[y/N] ", subject); err != nil {
		return fmt.Errorf("无法确认删除")
	}
	answer, err := bufio.NewReader(cmd.InOrStdin()).ReadString('\n')
	if err != nil && len(answer) == 0 {
		return fmt.Errorf("已取消删除")
	}
	if !strings.EqualFold(strings.TrimSpace(answer), "y") && !strings.EqualFold(strings.TrimSpace(answer), "yes") {
		return fmt.Errorf("已取消删除")
	}
	return nil
}

func ioReadAll(r io.Reader) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(r, (64<<10)+1))
	if err != nil || len(data) > 64<<10 {
		return nil, fmt.Errorf("API key 输入过长或读取失败")
	}
	return data, nil
}
