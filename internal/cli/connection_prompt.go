package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"sync"
	"unicode"

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
	Close() error
}

type promptOption struct {
	label string
	value string
}

type terminalPrompter struct {
	in  io.Reader
	out io.Writer

	mu      sync.Mutex
	session *promptSession
	closed  bool
}

func newTerminalPrompter(in io.Reader, out io.Writer) connectionPrompter {
	return &terminalPrompter{in: in, out: out}
}

func (p *terminalPrompter) IsTerminal() bool {
	f, ok := p.in.(*os.File)
	return ok && term.IsTerminal(int(f.Fd()))
}

func (p *terminalPrompter) ReadLine(ctx context.Context, label string) (string, error) {
	var value string
	field := huh.NewInput().Title(label + "（Enter 提交；Ctrl+C 取消）").CharLimit(4096).Value(&value)
	result, err := p.prompt(ctx, p.form(field))
	if err != nil {
		return "", promptError(ctx, err)
	}
	return result.(string), nil
}

func (p *terminalPrompter) ReadSecret(ctx context.Context, label string) (string, error) {
	var value string
	field := huh.NewInput().Title(label + "（Enter 提交；Ctrl+C 取消）").EchoMode(huh.EchoModeNone).CharLimit(4096).Value(&value)
	result, err := p.prompt(ctx, p.form(field))
	if err != nil {
		return "", promptError(ctx, err)
	}
	return result.(string), nil
}

func (p *terminalPrompter) Select(ctx context.Context, label string, options []promptOption) (string, error) {
	var value string
	huhOptions := make([]huh.Option[string], 0, len(options))
	for _, option := range options {
		huhOptions = append(huhOptions, huh.NewOption(option.label, option.value))
	}
	field := huh.NewSelect[string]().Title(label + "（↑/↓选择，Enter 确认；Ctrl+C 取消）").Options(huhOptions...).Value(&value)
	result, err := p.prompt(ctx, p.form(field))
	if err != nil {
		return "", promptError(ctx, err)
	}
	value, ok := result.(string)
	if !ok {
		return "", fmt.Errorf("请选择列表中的选项")
	}
	for _, option := range options {
		if option.value == value {
			return value, nil
		}
	}
	return "", fmt.Errorf("请选择列表中的选项")
}

func (p *terminalPrompter) Confirm(ctx context.Context, label string) (bool, error) {
	value := false
	field := huh.NewSelect[bool]().Title(label+"（↑/↓选择，Enter 确认；Ctrl+C 取消）").Options(
		huh.NewOption("取消", false),
		huh.NewOption("确认", true),
	).Value(&value)
	result, err := p.prompt(ctx, p.form(field))
	if err != nil {
		return false, promptError(ctx, err)
	}
	confirmed, ok := result.(bool)
	if !ok {
		return false, fmt.Errorf("请选择确认或取消")
	}
	return confirmed, nil
}

func (p *terminalPrompter) prompt(ctx context.Context, form *huh.Form) (any, error) {
	p.mu.Lock()
	if p.closed {
		p.mu.Unlock()
		return nil, context.Canceled
	}
	if p.session == nil {
		p.session = newPromptSession(ctx, p.in, p.out)
	}
	session := p.session
	p.mu.Unlock()
	return session.prompt(ctx, form)
}

func (p *terminalPrompter) Close() error {
	p.mu.Lock()
	p.closed = true
	session := p.session
	p.mu.Unlock()
	if session == nil {
		return nil
	}
	return session.close()
}

func (p *terminalPrompter) form(field huh.Field) *huh.Form {
	keymap := huh.NewDefaultKeyMap()
	keymap.Input.Next.SetHelp("Enter", "下一项")
	keymap.Input.Submit.SetHelp("Enter", "提交")
	keymap.Select.Next.SetHelp("Enter", "选择")
	keymap.Select.Submit.SetHelp("Enter", "确认")
	keymap.Select.Up.SetHelp("↑", "上移")
	keymap.Select.Down.SetHelp("↓", "下移")
	return huh.NewForm(huh.NewGroup(field)).WithAccessible(false).WithKeyMap(keymap)
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
