package cli

import (
	"fmt"
	"io"
	"os"
	"strings"
	"unicode"

	"golang.org/x/term"
)

// connectionPrompter 隔离向导输入，测试可注入实现而不扩展公开 Options。
type connectionPrompter interface {
	IsTerminal() bool
	ReadLine(label string) (string, error)
	ReadSecret(label string) (string, error)
	Confirm(label string) (bool, error)
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

// ReadLine 按字节读取单行，避免 bufio 预读后续隐藏密码输入。
func (p terminalPrompter) ReadLine(label string) (string, error) {
	if _, err := fmt.Fprintf(p.out, "%s", label); err != nil {
		return "", err
	}
	f, ok := p.in.(*os.File)
	if !ok {
		return "", fmt.Errorf("交互输入不可用")
	}
	var result []byte
	for len(result) <= 4096 {
		var b [1]byte
		n, err := f.Read(b[:])
		if err != nil {
			return "", err
		}
		if n == 0 {
			return "", io.EOF
		}
		if b[0] == '\n' {
			return strings.TrimSuffix(string(result), "\r"), nil
		}
		result = append(result, b[0])
	}
	return "", fmt.Errorf("输入过长")
}

func (p terminalPrompter) ReadSecret(label string) (string, error) {
	f, ok := p.in.(*os.File)
	if !ok || !term.IsTerminal(int(f.Fd())) {
		return "", fmt.Errorf("隐藏输入不可用")
	}
	if _, err := fmt.Fprint(p.out, label); err != nil {
		return "", err
	}
	secret, err := term.ReadPassword(int(f.Fd()))
	_, _ = fmt.Fprintln(p.out)
	if err != nil {
		return "", err
	}
	return string(secret), nil
}

func (p terminalPrompter) Confirm(label string) (bool, error) {
	answer, err := p.ReadLine(label + " [y/N] ")
	if err != nil {
		return false, err
	}
	return strings.EqualFold(strings.TrimSpace(answer), "y") || strings.EqualFold(strings.TrimSpace(answer), "yes"), nil
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
