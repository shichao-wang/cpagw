package cli

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/store"
	"github.com/spf13/cobra"
	"golang.org/x/term"
)

// Options 为主程序集成运行时热重载和 agent 引用检查提供回调。
type Options struct {
	OnChange          func() error
	ProfileReferenced func(profileID string) (bool, error)
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
	root.PersistentFlags().StringVar(&stateDir, "state-dir", "", "状态目录（默认：$XDG_CONFIG_HOME/cpagw 或 ~/.config/cpagw）")
	root.AddCommand(newProviderCommand(func() (*store.Store, error) { return store.New(stateDir) }, opts))
	root.AddCommand(newProfileCommand(func() (*store.Store, error) { return store.New(stateDir) }, opts))
	return root
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
