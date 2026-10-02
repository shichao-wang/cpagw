package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"strconv"

	"github.com/shichao-wang/cpa-tui/internal/agent/claudecode"
	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/server"
	"github.com/shichao-wang/cpa-tui/internal/store"
	"github.com/spf13/cobra"
)

// NewAppCommand 装配实际 binary 的服务管理和客户端配置功能。
func NewAppCommand() *cobra.Command {
	var root *cobra.Command
	openStore := func() (*store.Store, error) {
		dir, err := root.PersistentFlags().GetString("state-dir")
		if err != nil {
			return nil, err
		}
		return store.New(dir)
	}
	root = NewCommandWithOptions(Options{
		OnChange: func() error {
			s, err := openStore()
			if err != nil {
				return err
			}
			return server.WaitForRevision(root.Context(), s)
		},
		ProfileReferenced: func(id string) (bool, error) {
			s, err := openStore()
			if err != nil {
				return false, err
			}
			state, err := s.Read()
			if err != nil {
				return false, err
			}
			for name, p := range state.Profiles {
				if p.ID == id {
					return claudecode.IsManagedProfile(s, name), nil
				}
			}
			return false, nil
		},
	})
	root.AddCommand(serverCommand(openStore))
	for _, cmd := range root.Commands() {
		if cmd.Name() == "profile" {
			cmd.AddCommand(applyCommand(openStore), restoreCommand(openStore))
			break
		}
	}
	return root
}
func serverCommand(openStore storeFactory) *cobra.Command {
	parent := &cobra.Command{Use: "server", Short: "启动、查询和停止本地网关"}
	var foreground bool
	var listen string
	start := &cobra.Command{Use: "start", Short: "启动网关（默认后台运行）", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := openStore()
		if err != nil {
			return err
		}
		if cmd.Flags().Changed("listen") {
			host, port, err := net.SplitHostPort(listen)
			if err != nil {
				return fmt.Errorf("监听地址必须是 host:port")
			}
			ip := net.ParseIP(host)
			if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
				return fmt.Errorf("只允许 loopback 监听地址")
			}
			n, err := strconv.Atoi(port)
			if err != nil || n < 1 || n > 65535 {
				return fmt.Errorf("监听端口必须在 1–65535")
			}
			info, err := server.Status(cmd.Context(), s)
			if err != nil {
				return err
			}
			if info.State != "stopped" {
				return fmt.Errorf("修改监听地址前必须先停止网关")
			}
			if err = s.Update(func(state *config.State) error { state.Listen = net.JoinHostPort(host, port); return nil }); err != nil {
				return err
			}
		}
		if foreground {
			return server.Foreground(cmd.Context(), s)
		}
		if err = server.Start(cmd.Context(), s); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "网关已启动")
		return err
	}}
	start.Flags().BoolVar(&foreground, "foreground", false, "以前台方式运行")
	start.Flags().StringVar(&listen, "listen", "", "loopback 监听地址，例如 127.0.0.1:8317")
	parent.AddCommand(start)
	parent.AddCommand(&cobra.Command{Use: "status", Short: "查询实际进程和就绪状态", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := openStore()
		if err != nil {
			return err
		}
		info, err := server.Status(cmd.Context(), s)
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(info)
	}})
	parent.AddCommand(&cobra.Command{Use: "stop", Short: "停止本实例，不强制杀死其他进程", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := openStore()
		if err != nil {
			return err
		}
		if err = server.Stop(cmd.Context(), s); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "网关已停止")
		return err
	}})
	return parent
}
func applyCommand(openStore storeFactory) *cobra.Command {
	var settings string
	cmd := &cobra.Command{Use: "apply <name>", Short: "将 profile 持久应用到 Claude Code 用户配置", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := openStore()
		if err != nil {
			return err
		}
		state, err := s.Read()
		if err != nil {
			return err
		}
		p, ok := state.Profiles[args[0]]
		if !ok {
			return fmt.Errorf("profile 不存在：%s", args[0])
		}
		if err = claudecode.Apply(s, p, settings); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "profile 已应用；请启动新的 Claude Code 会话。当前 shell 或项目配置可能覆盖用户设置。")
		return err
	}}
	cmd.Flags().StringVar(&settings, "settings", "", "Claude Code settings 路径（默认 ~/.claude/settings.json）")
	return cmd
}
func restoreCommand(openStore storeFactory) *cobra.Command {
	var settings string
	cmd := &cobra.Command{Use: "restore", Short: "仅恢复由 cpagw 接管的配置", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := openStore()
		if err != nil {
			return err
		}
		if err = claudecode.Restore(s, settings); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "已恢复受管配置并解除 cpagw 接管")
		return err
	}}
	cmd.Flags().StringVar(&settings, "settings", "", "之前应用的 Claude Code settings 路径")
	return cmd
}

// Execute 使用调用者的 context，避免测试或取消操作留下隐式后台工作。
func Execute(ctx context.Context, args []string) error {
	root := NewAppCommand()
	root.SetArgs(args)
	return root.ExecuteContext(ctx)
}
