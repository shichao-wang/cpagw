package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/connection"
	"github.com/spf13/cobra"
)

func newConnectionCommand(open storeFactory, opts Options) *cobra.Command {
	return newConnectionCommandWithPrompter(open, opts, nil)
}

func newConnectionCommandWithPrompter(open storeFactory, opts Options, prompter connectionPrompter) *cobra.Command {
	parent := &cobra.Command{Use: "connection", Short: "管理独立上游连接"}
	var protocol, baseURL, modelsPath, authType string
	var apiKeyStdin bool
	var modelsValue []config.Model
	add := &cobra.Command{
		Use:   "add [name]",
		Short: "添加上游连接",
		Args:  cobra.MaximumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			p := prompter
			if p == nil {
				p = newTerminalPrompter(cmd.InOrStdin(), cmd.ErrOrStderr())
			}
			name := ""
			if len(args) == 1 {
				name = args[0]
			}
			authExplicit := cmd.Flags().Changed("auth-type")
			if apiKeyStdin && authType != config.AuthAPIKey {
				return fmt.Errorf("Codex OAuth 连接不能读取 API key")
			}
			if apiKeyStdin && (name == "" || protocol == "" || baseURL == "" || modelsPath == "") {
				return fmt.Errorf("使用 --api-key-stdin 时必须同时提供 name、--protocol、--base-url 和 --models")
			}
			if !p.IsTerminal() {
				if name == "" || protocol == "" || modelsPath == "" {
					return fmt.Errorf("非交互模式必须提供 name、--protocol 和 --models；API-key 还必须提供 --api-key-stdin")
				}
				if authType == config.AuthAPIKey && (!apiKeyStdin || baseURL == "") {
					return fmt.Errorf("API-key 非交互模式必须提供 --base-url 和 --api-key-stdin")
				}
				if authType == config.AuthCodexOAuth && !authExplicit {
					return fmt.Errorf("OAuth 非交互模式必须显式提供 --auth-type codex-oauth")
				}
			}
			var err error
			if name == "" {
				name, err = promptLine(cmd.Context(), p, "连接名称：", "连接名称")
				if err != nil {
					return err
				}
			}
			if p.IsTerminal() && !apiKeyStdin && !authExplicit {
				authType, err = p.Select(cmd.Context(), "选择认证方式", []promptOption{
					{label: "API key（默认）", value: config.AuthAPIKey},
					{label: "Codex OAuth", value: config.AuthCodexOAuth},
				})
				if err != nil {
					return err
				}
			}
			if authType != config.AuthAPIKey && authType != config.AuthCodexOAuth {
				return fmt.Errorf("不支持的认证方式：%s", authType)
			}
			if err := config.ValidateName(name); err != nil {
				return err
			}
			st, err := open()
			if err != nil {
				return err
			}
			state, err := st.Read()
			if err != nil {
				return err
			}
			if _, exists := state.Connections[name]; exists {
				return fmt.Errorf("连接已存在：%s", name)
			}
			if authType == config.AuthCodexOAuth {
				if protocol == "" {
					protocol = config.Responses
				}
				if protocol != config.Responses {
					return fmt.Errorf("Codex OAuth 仅支持 responses 协议")
				}
				if baseURL == "" {
					baseURL = config.CodexBaseURL
				} else if baseURL != config.CodexBaseURL {
					return fmt.Errorf("Codex OAuth 必须使用官方 API 地址：%s", config.CodexBaseURL)
				}
			}
			if !apiKeyStdin && p.IsTerminal() {
				if authType == config.AuthAPIKey && protocol == "" {
					protocol, err = promptProtocol(cmd.Context(), p)
					if err != nil {
						return err
					}
				}
				if authType == config.AuthAPIKey && baseURL == "" {
					baseURL, err = promptLine(cmd.Context(), p, "API 根地址：", "API 根地址")
					if err != nil {
						return err
					}
				}
				if modelsPath == "" {
					models, path, err := promptModels(cmd.Context(), p)
					if err != nil {
						return err
					}
					if path != "" {
						modelsPath = path
					} else {
						modelsValue = models
					}
				}
			}
			models := modelsValue
			if models == nil {
				models, err = readConnectionModels(modelsPath)
				if err != nil {
					return err
				}
			}
			c := config.Connection{Name: name, AuthType: authType, Protocol: protocol, BaseURL: baseURL, Models: models}
			if err := connection.ValidateConnection(c); err != nil {
				return err
			}
			key := ""
			if authType == config.AuthAPIKey {
				if apiKeyStdin {
					key, err = apiKey(cmd, true)
				} else {
					key, err = p.ReadSecret(cmd.Context(), "API key")
					if err == nil {
						key = strings.TrimSpace(key)
						if err = config.ValidateKey(key); err != nil {
							return err
						}
					}
				}
			}
			if err != nil {
				return sanitizeCredentialError(err)
			}
			if p.IsTerminal() && !apiKeyStdin {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "\n连接摘要\n名称：%s\n认证方式：%s\n协议：%s\nAPI 根地址：%s\n模型数：%d\n", c.Name, c.AuthType, c.Protocol, c.BaseURL, len(c.Models)); err != nil {
					return err
				}
				confirmed, err := p.Confirm(cmd.Context(), "确认保存此连接？")
				if err != nil {
					return err
				}
				if !confirmed {
					return context.Canceled
				}
			}
			if err = cmd.Context().Err(); err != nil {
				return err
			}
			if err = connection.Create(cmd.Context(), st, c, key); err != nil {
				return err
			}
			if err = notifyChanged(cmd, opts); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "已创建连接 %s\n", name)
			return err
		},
	}
	add.Flags().StringVar(&authType, "auth-type", config.AuthAPIKey, "认证方式：api-key 或 codex-oauth")
	add.Flags().StringVar(&protocol, "protocol", "", "chat-completions、anthropic-messages 或 responses")
	add.Flags().StringVar(&baseURL, "base-url", "", "该协议的 API 根地址")
	add.Flags().StringVar(&modelsPath, "models", "", "模型清单 YAML 文件")
	add.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "从标准输入读取 API key")

	parent.AddCommand(add, connectionUpdateCommand(open, opts))
	parent.AddCommand(&cobra.Command{Use: "list", Short: "列出连接", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		st, err := open()
		if err != nil {
			return err
		}
		state, err := st.Read()
		if err != nil {
			return err
		}
		names := make([]string, 0, len(state.Connections))
		for name := range state.Connections {
			names = append(names, name)
		}
		sort.Strings(names)
		views := make([]map[string]any, 0, len(names))
		for _, name := range names {
			views = append(views, connectionView(state, state.Connections[name]))
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(views)
	}})
	parent.AddCommand(&cobra.Command{Use: "show <name>", Short: "查看连接（不显示 key 或凭证引用）", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		st, err := open()
		if err != nil {
			return err
		}
		state, err := st.Read()
		if err != nil {
			return err
		}
		c, ok := state.Connections[args[0]]
		if !ok {
			return fmt.Errorf("连接不存在：%s", args[0])
		}
		return json.NewEncoder(cmd.OutOrStdout()).Encode(connectionView(state, c))
	}})
	var yes bool
	remove := &cobra.Command{Use: "remove <name>", Short: "删除未被 profile 引用的连接", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := confirmRemoval(cmd, yes, "连接 "+args[0]); err != nil {
			return err
		}
		st, err := open()
		if err != nil {
			return err
		}
		if err = connection.Remove(st, args[0]); err != nil {
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
	parent.AddCommand(connectionLoginCommand(open, opts), connectionLogoutCommand(open))
	parent.AddCommand(connectionDirectoryCommand(open, false), connectionDirectoryCommand(open, true))
	return parent
}

func connectionView(state *config.State, c config.Connection) map[string]any {
	view := map[string]any{"name": c.Name, "authType": c.AuthType, "protocol": c.Protocol, "baseURL": c.BaseURL, "models": c.Models}
	if c.AuthType == config.AuthCodexOAuth {
		status := "not-logged-in"
		if _, err := state.Credential(c); err == nil {
			status = "ready"
		}
		view["credentialStatus"] = status
	} else {
		_, err := state.Key(c)
		view["credentialConfigured"] = err == nil
	}
	return view
}

func connectionUpdateCommand(open storeFactory, opts Options) *cobra.Command {
	var baseURL, modelsPath, authType string
	var apiKeyStdin bool
	cmd := &cobra.Command{Use: "update <name>", Short: "原子更新地址、模型清单、认证方式和 key", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var base *string
		if cmd.Flags().Changed("base-url") {
			base = &baseURL
		}
		var auth *string
		if cmd.Flags().Changed("auth-type") {
			auth = &authType
			if authType == config.AuthCodexOAuth && base == nil {
				officialURL := config.CodexBaseURL
				base = &officialURL
			}
			if authType == config.AuthCodexOAuth && apiKeyStdin {
				return fmt.Errorf("Codex OAuth 不能与 --api-key-stdin 同时使用")
			}
		}
		var models *[]config.Model
		if cmd.Flags().Changed("models") {
			m, err := readConnectionModels(modelsPath)
			if err != nil {
				return err
			}
			models = &m
		}
		if base == nil && models == nil && !apiKeyStdin && auth == nil {
			return fmt.Errorf("请指定至少一个更新字段")
		}
		if err := config.ValidateName(args[0]); err != nil {
			return err
		}
		st, err := open()
		if err != nil {
			return err
		}
		state, err := st.Read()
		if err != nil {
			return err
		}
		candidate, ok := state.Connections[args[0]]
		if !ok {
			return fmt.Errorf("连接不存在：%s", args[0])
		}
		if base != nil {
			candidate.BaseURL = *base
		}
		if auth != nil {
			candidate.AuthType = *auth
		}
		if models != nil {
			candidate.Models = *models
		}
		if err := connection.ValidateConnection(candidate); err != nil {
			return err
		}
		var key *string
		if apiKeyStdin {
			k, err := apiKey(cmd, true)
			if err != nil {
				return sanitizeCredentialError(err)
			}
			key = &k
		}
		if err = connection.Patch(st, args[0], base, models, key, auth); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "已更新连接 %s\n", args[0])
		return err
	}}
	cmd.Flags().StringVar(&authType, "auth-type", "", "显式切换认证方式：api-key 或 codex-oauth")
	cmd.Flags().StringVar(&baseURL, "base-url", "", "新的 API 根地址")
	cmd.Flags().StringVar(&modelsPath, "models", "", "新的模型清单 YAML 文件")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "从标准输入读取新 API key")
	return cmd
}

func connectionLoginCommand(open storeFactory, opts Options) *cobra.Command {
	var noBrowser bool
	cmd := &cobra.Command{Use: "login <name>", Short: "登录 Codex OAuth 连接", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if opts.Login == nil {
			return fmt.Errorf("Codex OAuth 登录器未配置")
		}
		st, err := open()
		if err != nil {
			return err
		}
		session, err := connection.BeginOAuth(st, args[0])
		if err != nil {
			return err
		}
		defer session.Close()
		ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Minute)
		defer cancel()
		credential, err := opts.Login(ctx, noBrowser)
		if err != nil {
			return err
		}
		if err = session.Commit(credential); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "Codex OAuth 登录完成；凭证仅保存在本机")
		return err
	}}
	cmd.Flags().BoolVar(&noBrowser, "no-browser", false, "不自动打开官方设备授权页面")
	return cmd
}

func connectionLogoutCommand(open storeFactory) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{Use: "logout <name>", Short: "清除本地 Codex OAuth 凭证", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		if err := confirmRemoval(cmd, yes, args[0]+" 的本地 OAuth 凭证"); err != nil {
			return err
		}
		st, err := open()
		if err != nil {
			return err
		}
		if err = connection.LogoutOAuth(st, args[0]); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "本地 OAuth 凭证已清除；未向服务端撤销授权")
		return err
	}}
	cmd.Flags().BoolVar(&yes, "yes", false, "确认清除本地 OAuth 凭证")
	return cmd
}

func connectionDirectoryCommand(open storeFactory, check bool) *cobra.Command {
	name, short := "models", "列出连接的本地模型清单，不联网"
	if check {
		name, short = "check", "显式检查远端模型目录，不发送推理"
	}
	return &cobra.Command{Use: name + " <name>", Short: short, Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		st, err := open()
		if err != nil {
			return err
		}
		state, err := st.Read()
		if err != nil {
			return err
		}
		c, ok := state.Connections[args[0]]
		if !ok {
			return fmt.Errorf("连接不存在：%s", args[0])
		}
		if !check {
			return json.NewEncoder(cmd.OutOrStdout()).Encode(c.Models)
		}
		if c.AuthType == config.AuthCodexOAuth {
			if _, err := state.Credential(c); err != nil {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "Codex OAuth 尚未登录；未发送网络请求")
				return err
			}
			_, err := fmt.Fprintln(cmd.OutOrStdout(), "Codex OAuth 凭证已保存；未请求通用 /models endpoint")
			return err
		}
		key, err := state.Key(c)
		if err != nil {
			return err
		}
		if err = connection.Check(cmd.Context(), c, key); err != nil {
			return err
		}
		_, err = fmt.Fprintln(cmd.OutOrStdout(), "模型目录检查通过；这不代表推理能力验收通过")
		return err
	}}
}

func readConnectionModels(path string) ([]config.Model, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("无法读取模型清单文件")
	}
	defer f.Close()
	return connection.ParseModels(f)
}

func promptLine(ctx context.Context, p connectionPrompter, label, field string) (string, error) {
	value, err := p.ReadLine(ctx, label)
	if err != nil {
		return "", err
	}
	return validatePromptText(value, field)
}

func promptProtocol(ctx context.Context, p connectionPrompter) (string, error) {
	return p.Select(ctx, "选择上游协议", []promptOption{
		{label: "Chat Completions", value: config.Chat},
		{label: "Anthropic Messages", value: config.Anthropic},
		{label: "Responses", value: config.Responses},
	})
}

func promptModels(ctx context.Context, p connectionPrompter) ([]config.Model, string, error) {
	choice, err := p.Select(ctx, "选择模型来源", []promptOption{
		{label: "直接录入", value: "direct"},
		{label: "YAML 文件", value: "yaml"},
	})
	if err != nil {
		return nil, "", err
	}
	switch choice {
	case "direct":
		models := make([]config.Model, 0, 4)
		for len(models) < 100 {
			line, err := promptLine(ctx, p, "模型录入：模型 ID[=展示名]", "模型")
			if err != nil {
				return nil, "", err
			}
			model := config.Model{}
			id, display, hasDisplay := strings.Cut(line, "=")
			model.ID = strings.TrimSpace(id)
			if hasDisplay {
				model.Name = strings.TrimSpace(display)
			}
			models = append(models, model)
			if len(models) == 100 {
				break
			}
			next, err := p.Select(ctx, "模型录入", []promptOption{
				{label: "完成录入", value: "finish"},
				{label: "继续添加模型", value: "continue"},
			})
			if err != nil {
				return nil, "", err
			}
			if next == "finish" {
				break
			}
			if next != "continue" {
				return nil, "", fmt.Errorf("请选择列表中的模型录入操作")
			}
		}
		if err := config.ValidateModels(models); err != nil {
			return nil, "", err
		}
		return models, "", nil
	case "yaml":
		path, err := promptLine(ctx, p, "模型 YAML 文件路径：", "模型 YAML 文件路径")
		return nil, path, err
	default:
		return nil, "", fmt.Errorf("请选择列表中的模型来源")
	}
}

func sanitizeCredentialError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("API key 输入无效或读取失败")
}
