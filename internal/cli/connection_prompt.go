package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	tea "charm.land/bubbletea/v2"
	"charm.land/huh/v2"
	"golang.org/x/term"
)

// connectionPrompter 隔离向导输入，测试可注入实现而不扩展公开 Options。
type connectionPrompter interface {
	IsTerminal() bool
	ReadLine(ctx context.Context, label string) (string, error)
	ReadSecret(ctx context.Context, label string) (string, error)
	Select(ctx context.Context, label string, options []promptOption) (string, error)
	Confirm(ctx context.Context, label string) (bool, error)
}

type promptOption struct {
	label string
	value string
}

type terminalPrompter struct {
	in  io.Reader
	out io.Writer
}

func newTerminalPrompter(in io.Reader, out io.Writer) connectionPrompter {
	return terminalPrompter{in: in, out: out}
}

func (p terminalPrompter) IsTerminal() bool {
	f, ok := p.in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func (p terminalPrompter) ReadLine(ctx context.Context, label string) (string, error) {
	var value string
	field := huh.NewInput().Title(label + "（Enter 提交；Ctrl+C 取消）").CharLimit(4096).Value(&value)
	if err := p.form(field).RunWithContext(ctx); err != nil {
		return "", promptError(ctx, err)
	}
	return value, nil
}

func (p terminalPrompter) ReadSecret(ctx context.Context, label string) (string, error) {
	var value string
	field := huh.NewInput().Title(label + "（Enter 提交；Ctrl+C 取消）").EchoMode(huh.EchoModeNone).CharLimit(4096).Value(&value)
	if err := p.form(field).RunWithContext(ctx); err != nil {
		return "", promptError(ctx, err)
	}
	return value, nil
}

func (p terminalPrompter) Select(ctx context.Context, label string, options []promptOption) (string, error) {
	var value string
	huhOptions := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		huhOptions = append(huhOptions, huh.NewOption(option.label, option.value))
	}
	field := huh.NewSelect[string]().Title(label + "（↑/↓选择，Enter 确认；Ctrl+C 取消）").Options(huhOptions...).Value(&value)
	if err := p.form(field).RunWithContext(ctx); err != nil {
		return "", promptError(ctx, err)
	}
	for _, option := range options {
		if option.value == value {
			return value, nil
		}
	}
	return "", fmt.Errorf("请选择列表中的选项")
}

func (p terminalPrompter) Confirm(ctx context.Context, label string) (bool, error) {
	value := false
	field := huh.NewSelect[bool]().Title(label+"（↑/↓选择，Enter 确认；Ctrl+C 取消）").Options(
		huh.NewOption("取消", false),
		huh.NewOption("确认", true),
	).Value(&value)
	if err := p.form(field).RunWithContext(ctx); err != nil {
		return false, promptError(ctx, err)
	}
	return value, nil
}

func (p terminalPrompter) form(field huh.Field) *huh.Form {
	keymap := huh.NewDefaultKeyMap()
	keymap.Input.Next.SetHelp("Enter", "下一项")
	keymap.Input.Submit.SetHelp("Enter", "提交")
	keymap.Select.Next.SetHelp("Enter", "选择")
	keymap.Select.Submit.SetHelp("Enter", "确认")
	keymap.Select.Up.SetHelp("↑", "上移")
	keymap.Select.Down.SetHelp("↓", "下移")
	// OS 信号统一由主程序的 NotifyContext 管理，表单仅通过 context 取消。
	return huh.NewForm(huh.NewGroup(field)).WithProgramOptions(tea.WithoutSignalHandler()).WithInput(p.in).WithOutput(p.out).WithAccessible(false).WithKeyMap(keymap)
}

func promptError(ctx context.Context, err error) error {
	if ctx.Err() != nil {
		return ctx.Err()
	}
	if errors.Is(err, huh.ErrUserAborted) || errors.Is(err, io.EOF) {
		return context.Canceled
	}
	return err
}

func validatePromptText(value, field string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("%s 不能为空", field)
	}
	if len(value) > 4096 {
		return "", fmt.Errorf("%s 输入过长", field)
	}
	for _, r := range value {
		if unicode.IsControl(r) {
			return "", fmt.Errorf("%s 不能包含控制字符", field)
		}
	}
	return value, nil
}
