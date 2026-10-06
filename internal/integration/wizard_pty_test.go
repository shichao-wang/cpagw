package integration

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
	"golang.org/x/sys/unix"
	"golang.org/x/term"
)

const wizardTestKey = "fake-pty-wizard-key"

// TestWizardTerminal 使用独立 PTY 驱动实际 binary，不把 fakePrompter 当成终端验收。
func TestWizardTerminal(t *testing.T) {
	if testing.Short() {
		t.Skip("短测试模式跳过真实终端验收")
	}
	binary := filepath.Join(t.TempDir(), "cpagw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cpagw")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建终端测试binary：%v\n%s", err, output)
	}

	steps := []struct {
		title string
		input string
	}{
		{title: "连接名称", input: "terminal-test\r"},
		{title: "选择认证方式", input: "\r"},
		{title: "上游协议", input: "\x1b[B\r"},
		{title: "API 根地址", input: "https://example.invalid\r"},
		{title: "模型来源", input: "\r"},
		{title: "模型 ID", input: "upstream-model=终端测试模型\r"},
		{title: "模型录入（", input: "\r"},
		{title: "API key（Enter", input: wizardTestKey + "\r"},
		{title: "确认保存此连接", input: "\x1b[B\r"},
	}

	newState := func(t *testing.T) (string, []byte) {
		t.Helper()
		dir := filepath.Join(t.TempDir(), "state")
		st, err := store.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.Update(func(_ *config.State) error { return nil }); err != nil {
			t.Fatal(err)
		}
		data, err := os.ReadFile(st.Path("state.json"))
		if err != nil {
			t.Fatal(err)
		}
		return dir, data
	}

	advance := func(t *testing.T, terminal *wizardTerminal, stage int) {
		t.Helper()
		for i := 0; i <= stage; i++ {
			terminal.waitText(t, steps[i].title)
			if i < stage {
				terminal.send(t, steps[i].input)
			}
		}
	}

	for stage := range steps {
		t.Run("CtrlC/"+steps[stage].title, func(t *testing.T) {
			dir, before := newState(t)
			terminal := startWizardTerminal(t, binary, dir)
			advance(t, terminal, stage)
			if stage == 7 {
				terminal.send(t, wizardTestKey)
			}
			terminal.send(t, "\x03")
			terminal.waitExit(t, 130)
			terminal.waitText(t, "已取消")
			terminal.assertRestored(t)
			assertWizardUnchanged(t, dir, before)
		})
	}

	for _, signal := range []syscall.Signal{syscall.SIGINT, syscall.SIGTERM} {
		for _, stage := range []int{0, 1, 2, 7} {
			t.Run(signal.String()+"/"+steps[stage].title, func(t *testing.T) {
				dir, before := newState(t)
				terminal := startWizardTerminal(t, binary, dir)
				advance(t, terminal, stage)
				if stage == 7 {
					terminal.send(t, wizardTestKey)
				}
				if err := terminal.cmd.Process.Signal(signal); err != nil {
					t.Fatal(err)
				}
				terminal.waitExit(t, 130)
				terminal.waitText(t, "已取消")
				terminal.assertRestored(t)
				assertWizardUnchanged(t, dir, before)
			})
		}
	}

	t.Run("箭头菜单保存", func(t *testing.T) {
		dir, before := newState(t)
		terminal := startWizardTerminal(t, binary, dir)
		advance(t, terminal, len(steps)-1)
		terminal.send(t, steps[len(steps)-1].input)
		terminal.waitExit(t, 0)
		terminal.waitText(t, "已创建连接 terminal-test")
		terminal.assertRestored(t)
		st, err := store.New(dir)
		if err != nil {
			t.Fatal(err)
		}
		state, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		c, exists := state.Connections["terminal-test"]
		if !exists || c.Protocol != "anthropic-messages" || len(c.Models) != 1 || c.Models[0].ID != "upstream-model" || c.Models[0].Name != "终端测试模型" {
			t.Fatal("方向键菜单保存了错误的连接或模型")
		}
		if len(state.Secrets) != 1 || state.Secrets[c.CredentialRef] != wizardTestKey || state.Revision != 2 {
			t.Fatal("连接与独立凭证未按单次事务保存")
		}
		if bytes.Contains(before, []byte(wizardTestKey)) || strings.Contains(terminal.trace(), c.CredentialRef) {
			t.Fatal("凭证或引用出现在不应包含秘密的位置")
		}
	})

	t.Run("确认默认取消", func(t *testing.T) {
		dir, before := newState(t)
		terminal := startWizardTerminal(t, binary, dir)
		advance(t, terminal, len(steps)-1)
		terminal.send(t, "\r")
		terminal.waitExit(t, 130)
		terminal.waitText(t, "已取消")
		terminal.assertRestored(t)
		assertWizardUnchanged(t, dir, before)
	})

	t.Run("持锁时取消", func(t *testing.T) {
		dir, before := newState(t)
		lock, err := unix.Open(filepath.Join(dir, "state.lock"), unix.O_RDWR|unix.O_NOFOLLOW|unix.O_CLOEXEC, 0600)
		if err != nil {
			t.Fatal(err)
		}
		defer unix.Close(lock)
		if err := unix.Flock(lock, unix.LOCK_EX|unix.LOCK_NB); err != nil {
			t.Fatal(err)
		}
		defer unix.Flock(lock, unix.LOCK_UN)
		terminal := startWizardTerminal(t, binary, dir)
		advance(t, terminal, len(steps)-1)
		terminal.send(t, steps[len(steps)-1].input)
		// 等待确认表单结束并恢复终端，避免把菜单中的取消误当成锁等待取消。
		terminal.waitTerminalRestored(t)
		// 原进程仍持有锁；取消必须在释放该锁之前完成。
		if err := terminal.cmd.Process.Signal(os.Interrupt); err != nil {
			t.Fatal(err)
		}
		terminal.waitExit(t, 130)
		terminal.waitText(t, "已取消")
		terminal.assertRestored(t)
		assertWizardUnchanged(t, dir, before)
	})
}

type wizardTerminal struct {
	cmd       *exec.Cmd
	master    *os.File
	slave     *os.File
	fd        int
	initial   *term.State
	finished  bool
	done      chan error
	readerEnd chan struct{}
	stop      chan struct{}
	mu        sync.Mutex
	output    bytes.Buffer
}

func startWizardTerminal(t *testing.T, binary, dir string) *wizardTerminal {
	t.Helper()
	master, slave, err := pty.Open()
	if err != nil {
		t.Fatal(err)
	}
	initial, err := term.GetState(int(slave.Fd()))
	if err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	if err := pty.Setsize(master, &pty.Winsize{Rows: 40, Cols: 120}); err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	cmd := exec.Command(binary, "--state-dir", dir, "connection", "add")
	cmd.Stdin, cmd.Stdout, cmd.Stderr = slave, slave, slave
	cmd.Env = append(os.Environ(), "HOME="+t.TempDir(), "TERM=xterm-256color", "NO_COLOR=1")
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true, Setctty: true, Ctty: 0}
	terminal := &wizardTerminal{cmd: cmd, master: master, slave: slave, fd: int(master.Fd()), initial: initial, done: make(chan error, 1), readerEnd: make(chan struct{}), stop: make(chan struct{})}
	if err := unix.SetNonblock(terminal.fd, true); err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		master.Close()
		slave.Close()
		t.Fatal(err)
	}
	go func() { terminal.done <- cmd.Wait() }()
	go terminal.read()
	t.Cleanup(func() {
		if !terminal.finished {
			select {
			case <-terminal.done:
			default:
				_ = cmd.Process.Kill()
				select {
				case <-terminal.done:
				case <-time.After(5 * time.Second):
					t.Error("终端测试进程未能清理")
				}
			}
		}
		close(terminal.stop)
		select {
		case <-terminal.readerEnd:
		case <-time.After(2 * time.Second):
			t.Error("终端输出reader未能清理")
		}
		_ = master.Close()
		_ = slave.Close()
	})
	return terminal
}

func (terminal *wizardTerminal) read() {
	defer close(terminal.readerEnd)
	buffer := make([]byte, 4096)
	for {
		select {
		case <-terminal.stop:
			return
		default:
		}
		fds := []unix.PollFd{{Fd: int32(terminal.fd), Events: unix.POLLIN}}
		if _, err := unix.Poll(fds, 50); err != nil {
			if errors.Is(err, unix.EINTR) {
				continue
			}
			return
		}
		if fds[0].Revents&unix.POLLIN == 0 {
			if fds[0].Revents&(unix.POLLHUP|unix.POLLERR|unix.POLLNVAL) != 0 {
				return
			}
			continue
		}
		n, err := unix.Read(terminal.fd, buffer)
		if n > 0 {
			terminal.mu.Lock()
			_, _ = terminal.output.Write(buffer[:n])
			terminal.mu.Unlock()
		}
		if err != nil && !errors.Is(err, unix.EAGAIN) && !errors.Is(err, unix.EINTR) {
			return
		}
	}
}

func (terminal *wizardTerminal) trace() string {
	terminal.mu.Lock()
	defer terminal.mu.Unlock()
	return terminal.output.String()
}

var wizardCSI = regexp.MustCompile("\\x1b\\[[0-?]*[ -/]*[@-~]")
var wizardOSC = regexp.MustCompile("\\x1b\\][^\\x07\\x1b]*(?:\\x07|\\x1b\\\\)")

func (terminal *wizardTerminal) waitText(t *testing.T, text string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		clean := wizardCSI.ReplaceAllString(wizardOSC.ReplaceAllString(terminal.trace(), ""), "")
		if strings.Contains(clean, text) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	// 仅输出脱敏后的测试界面，便于定位不同平台的输入时序问题。
	clean := wizardCSI.ReplaceAllString(wizardOSC.ReplaceAllString(terminal.trace(), ""), "")
	clean = strings.ReplaceAll(clean, wizardTestKey, "[隐藏测试凭证]")
	if at := strings.Index(clean, "fake-pty"); at >= 0 {
		clean = clean[:at] + "[隐藏测试凭证及后续输出]"
	}
	if len(clean) > 4096 {
		clean = clean[len(clean)-4096:]
	}
	t.Fatalf("终端未在限定时间内显示：%s；脱敏界面：%q", text, clean)
}

func (terminal *wizardTerminal) send(t *testing.T, text string) {
	t.Helper()
	write := func(value string) {
		data := []byte(value)
		for len(data) > 0 {
			n, err := unix.Write(terminal.fd, data)
			if err != nil || n == 0 {
				t.Fatal("无法向测试终端发送输入")
			}
			data = data[n:]
		}
	}
	// 按终端键盘事件驱动；文本按快速打字节奏逐字发送，方向键和提交仍是独立按键。
	// Linux PTY 下更快的逐字写入会让表单事件循环偶发丢字，Enter 也可能先于输入处理。
	time.Sleep(40 * time.Millisecond)
	for _, part := range strings.SplitAfter(text, "\r") {
		enter := strings.HasSuffix(part, "\r")
		part = strings.TrimSuffix(part, "\r")
		if strings.HasPrefix(part, "\x1b[") {
			write(part)
		} else {
			for _, r := range part {
				write(string(r))
				time.Sleep(20 * time.Millisecond)
			}
		}
		if enter {
			time.Sleep(30 * time.Millisecond)
			write("\r")
		}
	}
}

func (terminal *wizardTerminal) waitExit(t *testing.T, code int) {
	t.Helper()
	select {
	case err := <-terminal.done:
		terminal.finished = true
		if code == 0 && err != nil {
			t.Fatalf("终端流程未正常退出：%v", err)
		}
		if code != 0 {
			var exit *exec.ExitError
			if !errors.As(err, &exit) || exit.ExitCode() != code {
				t.Fatalf("取消退出状态不是%d：%v", code, err)
			}
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Control+C/信号未在限定时间内终止命令")
	}
}

func (terminal *wizardTerminal) waitTerminalRestored(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		current, err := term.GetState(terminal.fd)
		if err == nil && reflect.DeepEqual(current, terminal.initial) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("确认表单未结束并恢复终端")
}

func (terminal *wizardTerminal) assertRestored(t *testing.T) {
	t.Helper()
	// macOS 会在会话首进程退出时撤销旧 slave FD；master 仍持有同一 PTY 的终端状态。
	current, err := term.GetState(terminal.fd)
	if err != nil || !reflect.DeepEqual(current, terminal.initial) {
		t.Fatalf("退出后终端回显/termios未恢复：原始=%+v 当前=%+v 获取错误=%v", terminal.initial, current, err)
	}
	trace := terminal.trace()
	if strings.Contains(trace, "fake-pty") {
		t.Fatal("终端输出泄漏了假API key或其前缀")
	}
	if strings.LastIndex(trace, "\x1b[?25l") > strings.LastIndex(trace, "\x1b[?25h") {
		t.Fatal("退出后终端光标仍被隐藏")
	}
}

func assertWizardUnchanged(t *testing.T, dir string, before []byte) {
	t.Helper()
	after, err := os.ReadFile(filepath.Join(dir, "state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("取消向导改变了持久化state、凭证或revision")
	}
}
