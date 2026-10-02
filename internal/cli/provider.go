package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"

	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/provider"
	"github.com/shichao-wang/cpa-tui/internal/store"
	"github.com/spf13/cobra"
)

type storeFactory func() (*store.Store, error)

func newProviderCommand(open storeFactory, opts Options) *cobra.Command {
	parent := &cobra.Command{Use: "provider", Short: "管理服务提供商和协议连接"}
	var stdin, noDefault bool
	add := &cobra.Command{Use: "add <name>", Short: "创建提供商容器，不绑定协议或地址", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if stdin && noDefault {
			return fmt.Errorf("不能同时指定默认 key 和 --no-default-key")
		}
		key := ""
		var err error
		if !noDefault {
			key, err = apiKey(cmd, stdin)
			if err != nil {
				return err
			}
		}
		s, err := open()
		if err != nil {
			return err
		}
		if err = provider.CreateProvider(s, args[0], key); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "已创建提供商 %s\n", args[0])
		return err
	}}
	add.Flags().BoolVar(&stdin, "api-key-stdin", false, "从标准输入读取默认 API key")
	add.Flags().BoolVar(&noDefault, "no-default-key", false, "不设置默认 key，连接需独立配置 key")
	var updateStdin bool
	update := &cobra.Command{Use: "update <name>", Short: "轮换默认 key，保留连接级覆盖", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		key, err := apiKey(cmd, updateStdin)
		if err != nil {
			return err
		}
		s, err := open()
		if err != nil {
			return err
		}
		if err = provider.RotateDefaultKey(s, args[0], key); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "默认 key 已更新，连接级覆盖保持不变")
		return err
	}}
	update.Flags().BoolVar(&updateStdin, "api-key-stdin", false, "从标准输入读取新默认 API key")
	parent.AddCommand(add, update)
	parent.AddCommand(&cobra.Command{Use: "list", Short: "列出提供商", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		state, err := s.Read()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(state.Providers))
		for name := range state.Providers {
			names = append(names, name)
		}
		sort.Strings(names)
		views := make([]map[string]any, 0, len(names))
		for _, name := range names {
			p := state.Providers[name]
			views = append(views, map[string]any{"name": name, "connections": len(p.Connections), "defaultKeyConfigured": state.Secrets[p.DefaultCredentialRef] != ""})
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(views)
	}})
	parent.AddCommand(&cobra.Command{Use: "show <name>", Short: "查看提供商，不显示 key 或凭证引用", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		state, err := s.Read()
		if err != nil {
			return err
		}
		p, ok := state.Providers[args[0]]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", args[0])
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(map[string]any{"name": p.Name, "defaultKeyConfigured": state.Secrets[p.DefaultCredentialRef] != "", "connections": connectionViews(p)})
	}})
	var yes bool
	remove := &cobra.Command{Use: "remove <name>", Short: "删除未被 profile 引用的提供商", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := confirmRemoval(cmd, yes, args[0]); err != nil {
			return err
		}
		s, err := open()
		if err != nil {
			return err
		}
		if err = provider.Remove(s, args[0]); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "提供商已删除")
		return err
	}}
	remove.Flags().BoolVar(&yes, "yes", false, "确认删除")
	parent.AddCommand(remove)
	parent.AddCommand(connectionCommand(open, opts), providerDirectoryCommand(open, false), providerDirectoryCommand(open, true))
	return parent
}
func connectionView(c config.Connection) map[string]any {
	source := "provider-default"
	if c.CredentialRef != "" {
		source = "connection-override"
	}
	return map[string]any{"name": c.Name, "protocol": c.Protocol, "baseURL": c.BaseURL, "credentialSource": source, "models": c.Models}
}
func connectionViews(p config.Provider) []map[string]any {
	names := make([]string, 0, len(p.Connections))
	for name := range p.Connections {
		names = append(names, name)
	}
	sort.Strings(names)
	result := make([]map[string]any, 0, len(names))
	for _, name := range names {
		result = append(result, connectionView(p.Connections[name]))
	}
	return result
}
func connectionCommand(open storeFactory, opts Options) *cobra.Command {
	parent := &cobra.Command{Use: "connection", Short: "管理提供商下的协议连接"}
	var protocol, base, modelsPath string
	var stdin bool
	add := &cobra.Command{Use: "add <provider> <connection>", Short: "添加协议连接，默认继承提供商 key", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		models, err := readModels(modelsPath)
		if err != nil {
			return err
		}
		key := ""
		if stdin {
			key, err = apiKey(cmd, true)
			if err != nil {
				return err
			}
		}
		s, err := open()
		if err != nil {
			return err
		}
		if err = provider.AddConnection(s, args[0], config.Connection{Name: args[1], Protocol: protocol, BaseURL: base, Models: models}, key); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "连接已添加")
		return err
	}}
	add.Flags().StringVar(&protocol, "protocol", "", "chat-completions、anthropic-messages 或 responses")
	add.Flags().StringVar(&base, "base-url", "", "该协议的 API 根地址")
	add.Flags().StringVar(&modelsPath, "models", "", "模型清单 YAML 文件")
	add.Flags().BoolVar(&stdin, "api-key-stdin", false, "从标准输入读取连接专属 key，不提供则继承默认 key")
	for _, flag := range []string{"protocol", "base-url", "models"} {
		_ = add.MarkFlagRequired(flag)
	}
	parent.AddCommand(add, connectionUpdateCommand(open, opts))
	parent.AddCommand(&cobra.Command{Use: "list <provider>", Short: "列出提供商的连接", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		state, err := s.Read()
		if err != nil {
			return err
		}
		p, ok := state.Providers[args[0]]
		if !ok {
			return fmt.Errorf("提供商不存在")
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(connectionViews(p))
	}})
	parent.AddCommand(&cobra.Command{Use: "show <provider> <connection>", Short: "查看连接，不显示凭证", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		_, c, err := getConnection(s, args[0], args[1])
		if err != nil {
			return err
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(connectionView(c))
	}})
	var yes bool
	remove := &cobra.Command{Use: "remove <provider> <connection>", Short: "删除未被引用的连接", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if err := confirmRemoval(cmd, yes, args[0]+"/"+args[1]); err != nil {
			return err
		}
		s, err := open()
		if err != nil {
			return err
		}
		if err = provider.RemoveConnection(s, args[0], args[1]); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "连接已删除")
		return err
	}}
	remove.Flags().BoolVar(&yes, "yes", false, "确认删除")
	parent.AddCommand(remove)
	return parent
}
func connectionUpdateCommand(open storeFactory, opts Options) *cobra.Command {
	var base, modelsPath string
	var stdin, inherit bool
	cmd := &cobra.Command{Use: "update <provider> <connection>", Short: "原子更新地址、模型清单和 key 配置", Args: cobra.ExactArgs(2), RunE: func(cmd *cobra.Command, args []string) error {
		if stdin && inherit {
			return fmt.Errorf("--api-key-stdin 和 --inherit-api-key 不能同时使用")
		}
		var url *string
		if cmd.Flags().Changed("base-url") {
			url = &base
		}
		var models *[]config.Model
		if cmd.Flags().Changed("models") {
			m, err := readModels(modelsPath)
			if err != nil {
				return err
			}
			models = &m
		}
		var key *string
		if stdin {
			k, err := apiKey(cmd, true)
			if err != nil {
				return err
			}
			key = &k
		}
		if url == nil && models == nil && key == nil && !inherit {
			return fmt.Errorf("请指定至少一个更新字段")
		}
		s, err := open()
		if err != nil {
			return err
		}
		if err = provider.PatchConnection(s, args[0], args[1], url, models, key, inherit); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "连接已更新")
		return err
	}}
	cmd.Flags().StringVar(&base, "base-url", "", "新的 API 根地址")
	cmd.Flags().StringVar(&modelsPath, "models", "", "新的模型清单 YAML 文件")
	cmd.Flags().BoolVar(&stdin, "api-key-stdin", false, "从标准输入读取连接专属 key")
	cmd.Flags().BoolVar(&inherit, "inherit-api-key", false, "移除专属 key，恢复继承提供商默认 key")
	return cmd
}
func providerDirectoryCommand(open storeFactory, check bool) *cobra.Command {
	var connection string
	name, description := "models", "列出连接的本地模型清单，不联网"
	if check {
		name, description = "check", "显式检查远端模型目录，不发送推理"
	}
	cmd := &cobra.Command{Use: name + " <provider>", Short: description, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		s, err := open()
		if err != nil {
			return err
		}
		state, c, err := getConnection(s, args[0], connection)
		if err != nil {
			return err
		}
		if !check {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(c.Models)
		}
		key, err := state.Key(args[0], c)
		if err != nil {
			return err
		}
		if err = provider.Check(cmd.Context(), c, key); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "模型目录检查通过；这不代表推理能力验收通过")
		return err
	}}
	cmd.Flags().StringVar(&connection, "connection", "", "明确选择连接")
	_ = cmd.MarkFlagRequired("connection")
	return cmd
}
func getConnection(s *store.Store, pname, cname string) (*config.State, config.Connection, error) {
	state, err := s.Read()
	if err != nil {
		return nil, config.Connection{}, err
	}
	p, ok := state.Providers[pname]
	if !ok {
		return nil, config.Connection{}, fmt.Errorf("提供商不存在：%s", pname)
	}
	c, ok := p.Connections[cname]
	if !ok {
		return nil, config.Connection{}, fmt.Errorf("连接不存在：%s/%s", pname, cname)
	}
	return state, c, nil
}
func readModels(path string) ([]config.Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取模型清单文件")
	}
	defer f.Close()
	return provider.ParseModels(f)
}
