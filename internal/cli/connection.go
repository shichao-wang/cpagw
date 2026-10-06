package cli

import (
	"encoding/json"
	"fmt"
	"os"
	"sort"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/connection"
	"github.com/spf13/cobra"
)

func newConnectionCommand(open storeFactory, opts Options) *cobra.Command {
	return newConnectionCommandWithPrompter(open, opts, nil)
}

func newConnectionCommandWithPrompter(open storeFactory, opts Options, prompter connectionPrompter) *cobra.Command {
	parent := &cobra.Command{Use: "connection", Short: "管理独立上游连接"}
	var protocol, baseURL, modelsPath string
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
			if apiKeyStdin && (name == "" || protocol == "" || baseURL == "" || modelsPath == "") {
				return fmt.Errorf("使用 --api-key-stdin 时必须同时提供 name、--protocol、--base-url 和 --models")
			}
			if !p.IsTerminal() && (name == "" || protocol == "" || baseURL == "" || modelsPath == "" || !apiKeyStdin) {
				return fmt.Errorf("非交互模式必须提供 name、--protocol、--base-url、--models 和 --api-key-stdin")
			}
			var err error
			if name == "" {
				name, err = promptLine(p, "连接名称：", "连接名称")
				if err != nil {
					return err
				}
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
			if !apiKeyStdin && p.IsTerminal() {
				if protocol == "" {
					protocol, err = promptProtocol(p)
					if err != nil {
						return err
					}
				}
				if baseURL == "" {
					baseURL, err = promptLine(p, "API 根地址：", "API 根地址")
					if err != nil {
						return err
					}
				}
				if modelsPath == "" {
					models, path, err := promptModels(p)
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
			c := config.Connection{Name: name, Protocol: protocol, BaseURL: baseURL, Models: models}
			if err := connection.ValidateConnection(c); err != nil {
				return err
			}
			key := ""
			if apiKeyStdin {
				key, err = apiKey(cmd, true)
			} else {
				key, err = p.ReadSecret("API key（隐藏输入）：")
				if err == nil {
					key = strings.TrimSpace(key)
					if err = config.ValidateKey(key); err != nil {
						return err
					}
				}
			}
			if err != nil {
				return sanitizeCredentialError(err)
			}
			if p.IsTerminal() && !apiKeyStdin {
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "\n连接摘要\n名称：%s\n协议：%s\nAPI 根地址：%s\n模型数：%d\n", c.Name, c.Protocol, c.BaseURL, len(c.Models)); err != nil {
					return err
				}
				confirmed, err := p.Confirm("确认保存此连接？")
				if err != nil {
					return fmt.Errorf("向导已取消")
				}
				if !confirmed {
					return fmt.Errorf("向导已取消")
				}
			}
			if err = connection.Create(st, c, key); err != nil {
				return err
			}
			if err = notifyChanged(cmd, opts); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "已创建连接 %s\n", name)
			return err
		},
	}
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
			views = append(views, connectionView(state.Connections[name]))
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
		return json.NewEncoder(cmd.OutOrStdout()).Encode(connectionView(c))
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
	parent.AddCommand(connectionDirectoryCommand(open, false), connectionDirectoryCommand(open, true))
	return parent
}

func connectionView(c config.Connection) map[string]any {
	return map[string]any{"name": c.Name, "protocol": c.Protocol, "baseURL": c.BaseURL, "models": c.Models, "credentialConfigured": c.CredentialRef != ""}
}

func connectionUpdateCommand(open storeFactory, opts Options) *cobra.Command {
	var baseURL, modelsPath string
	var apiKeyStdin bool
	cmd := &cobra.Command{Use: "update <name>", Short: "原子更新地址、模型清单和 key", Args: cobra.ExactArgs(1), RunE: func(cmd *cobra.Command, args []string) error {
		var base *string
		if cmd.Flags().Changed("base-url") {
			base = &baseURL
		}
		var models *[]config.Model
		if cmd.Flags().Changed("models") {
			m, err := readConnectionModels(modelsPath)
			if err != nil {
				return err
			}
			models = &m
		}
		if base == nil && models == nil && !apiKeyStdin {
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
		if err = connection.Patch(st, args[0], base, models, key); err != nil {
			return err
		}
		if err = notifyChanged(cmd, opts); err != nil {
			return err
		}
		_, err = fmt.Fprintf(cmd.OutOrStdout(), "已更新连接 %s\n", args[0])
		return err
	}}
	cmd.Flags().StringVar(&baseURL, "base-url", "", "新的 API 根地址")
	cmd.Flags().StringVar(&modelsPath, "models", "", "新的模型清单 YAML 文件")
	cmd.Flags().BoolVar(&apiKeyStdin, "api-key-stdin", false, "从标准输入读取新 API key")
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

func promptLine(p connectionPrompter, label, field string) (string, error) {
	value, err := p.ReadLine(label)
	if err != nil {
		return "", fmt.Errorf("向导已取消")
	}
	return validatePromptText(value, field)
}

func promptProtocol(p connectionPrompter) (string, error) {
	value, err := p.ReadLine("协议（1 chat-completions / 2 anthropic-messages / 3 responses）：")
	if err != nil {
		return "", fmt.Errorf("向导已取消")
	}
	protocols := map[string]string{"1": config.Chat, "2": config.Anthropic, "3": config.Responses, config.Chat: config.Chat, config.Anthropic: config.Anthropic, config.Responses: config.Responses}
	protocol, ok := protocols[strings.TrimSpace(value)]
	if !ok {
		return "", fmt.Errorf("协议必须明确选择 chat-completions、anthropic-messages 或 responses")
	}
	return protocol, nil
}

func promptModels(p connectionPrompter) ([]config.Model, string, error) {
	choice, err := p.ReadLine("模型来源（1 直接录入 / 2 YAML 文件）：")
	if err != nil {
		return nil, "", fmt.Errorf("向导已取消")
	}
	switch strings.TrimSpace(choice) {
	case "1":
		models := make([]config.Model, 0, 4)
		for len(models) < 100 {
			line, err := p.ReadLine("模型 ID[=展示名]（空行结束）：")
			if err != nil {
				return nil, "", fmt.Errorf("向导已取消")
			}
			line = strings.TrimSpace(line)
			if line == "" {
				break
			}
			clean, err := validatePromptText(line, "模型")
			if err != nil {
				return nil, "", err
			}
			model := config.Model{}
			id, display, hasDisplay := strings.Cut(clean, "=")
			model.ID = strings.TrimSpace(id)
			if hasDisplay {
				model.Name = strings.TrimSpace(display)
			}
			models = append(models, model)
		}
		if err := config.ValidateModels(models); err != nil {
			return nil, "", err
		}
		return models, "", nil
	case "2":
		path, err := promptLine(p, "模型 YAML 文件路径：", "模型 YAML 文件路径")
		return nil, path, err
	default:
		return nil, "", fmt.Errorf("请选择直接录入或 YAML 文件")
	}
}

func sanitizeCredentialError(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("API key 输入无效或读取失败")
}
