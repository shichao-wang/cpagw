package cli

import (
	"fmt"
	"sort"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/profile"
	"github.com/spf13/cobra"
)

func newProfileCommand(openStore storeFactory, opts Options) *cobra.Command {
	cmd := &cobra.Command{Use: "profile", Short: "管理 Claude Code profile"}
	cmd.AddCommand(profileListCommand(openStore))
	cmd.AddCommand(profileShowCommand(openStore))
	cmd.AddCommand(profileCreateCommand(openStore, opts))
	cmd.AddCommand(profileRemoveCommand(openStore, opts))
	return cmd
}

func profileListCommand(openStore storeFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "list",
		Short: "列出 profile（不显示 key）",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			s, err := st.Read()
			if err != nil {
				return err
			}
			names := make([]string, 0, len(s.Profiles))
			for name := range s.Profiles {
				names = append(names, name)
			}
			sort.Strings(names)
			for _, name := range names {
				p := s.Profiles[name]
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s\t%s\t%s\n", name, p.Agent, p.ID); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func profileShowCommand(openStore storeFactory) *cobra.Command {
	return &cobra.Command{
		Use:   "show <name>",
		Short: "显示 profile 映射（不显示 key 或凭证引用）",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			st, err := openStore()
			if err != nil {
				return err
			}
			s, err := st.Read()
			if err != nil {
				return err
			}
			p, ok := s.Profiles[args[0]]
			if !ok {
				return fmt.Errorf("profile 不存在：%s", args[0])
			}
			if _, err := fmt.Fprintf(cmd.OutOrStdout(), "名称：%s\nID：%s\nAgent：%s\n模型映射：\n", p.Name, p.ID, p.Agent); err != nil {
				return err
			}
			for _, slot := range config.Slots {
				b, ok := p.Models[slot]
				if !ok {
					continue
				}
				if _, err := fmt.Fprintf(cmd.OutOrStdout(), "  %s: %s -> %s/%s/%s", slot, b.PublicModel, b.Provider, b.Connection, b.TargetModel); err != nil {
					return err
				}
				if b.Label != "" {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), " [%s]", b.Label); err != nil {
						return err
					}
				}
				if b.Description != "" {
					if _, err := fmt.Fprintf(cmd.OutOrStdout(), " — %s", b.Description); err != nil {
						return err
					}
				}
				if _, err := fmt.Fprintln(cmd.OutOrStdout()); err != nil {
					return err
				}
			}
			return nil
		},
	}
}

func profileCreateCommand(openStore storeFactory, opts Options) *cobra.Command {
	var filePath, agent string
	cmd := &cobra.Command{
		Use: "create <name>", Short: "创建下游 profile 并私密保存专属 key", Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := profile.ReadFile(filePath)
			if err != nil {
				return err
			}
			if file.Name != nil && strings.TrimSpace(*file.Name) != args[0] {
				return fmt.Errorf("文件中的 name 与命令名称不一致")
			}
			file.Name = &args[0]
			if cmd.Flags().Changed("agent") || file.Agent == nil {
				file.Agent = &agent
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			if _, err = profile.Create(st, file); err != nil {
				return err
			}
			if err = notifyChanged(cmd, opts); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "已创建 profile %s；API key 已私密保存，可通过 profile apply 应用\n", args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&filePath, "file", "", "模型绑定 YAML 文件")
	cmd.Flags().StringVar(&agent, "agent", "claude-code", "下游 Agent（首期仅 claude-code）")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func profileUpdateCommand(openStore storeFactory, opts Options) *cobra.Command {
	var filePath string
	cmd := &cobra.Command{
		Use:   "update <name>",
		Short: "合并更新 profile；未指定的档位和字段保持不变",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			file, err := profile.ReadFile(filePath)
			if err != nil {
				return err
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			if err := profile.Update(st, args[0], file); err != nil {
				return err
			}
			if err := notifyChanged(cmd, opts); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "已更新 profile %s\n", args[0])
			return err
		},
	}
	cmd.Flags().StringVar(&filePath, "file", "", "profile YAML 文件（只合并其中存在的字段）")
	_ = cmd.MarkFlagRequired("file")
	return cmd
}

func profileRemoveCommand(openStore storeFactory, opts Options) *cobra.Command {
	var yes bool
	cmd := &cobra.Command{
		Use:   "delete <name>",
		Short: "删除未被 agent 配置引用的 profile",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			if err := confirmRemoval(cmd, yes, "profile "+args[0]); err != nil {
				return err
			}
			if opts.ProfileReferenced == nil {
				return fmt.Errorf("当前未连接 agent 管理器，无法安全确认 profile 引用关系")
			}
			st, err := openStore()
			if err != nil {
				return err
			}
			if err := profile.Remove(st, args[0], opts.ProfileReferenced); err != nil {
				return err
			}
			if err := notifyChanged(cmd, opts); err != nil {
				return err
			}
			_, err = fmt.Fprintf(cmd.OutOrStdout(), "已删除 profile %s\n", args[0])
			return err
		},
	}
	cmd.Flags().BoolVar(&yes, "yes", false, "确认删除")
	return cmd
}
