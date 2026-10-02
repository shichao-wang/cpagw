package server

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/gateway"
	"github.com/shichao-wang/cpa-tui/internal/store"
	"golang.org/x/sys/unix"
)

const startTimeout = 15 * time.Second

type Info struct {
	State    string `json:"state"`
	PID      int    `json:"pid,omitempty"`
	Listen   string `json:"listen,omitempty"`
	Revision uint64 `json:"revision,omitempty"`
}

func readRuntime(s *store.Store) (gateway.RuntimeState, error) {
	var result gateway.RuntimeState
	path := s.Path(gateway.RuntimeFileName)
	if err := store.CheckFile(path); err != nil {
		return result, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return result, err
	}
	if err = json.Unmarshal(data, &result); err != nil {
		return result, fmt.Errorf("运行状态记录损坏")
	}
	if result.PID <= 1 || result.InstanceID == "" || result.StartTime == "" || result.ProbeToken == "" {
		return result, fmt.Errorf("运行状态记录不完整")
	}
	return result, nil
}

// ProcessStartTime 返回进程创建时间，防止陈旧 PID 指向其他进程。
func ProcessStartTime(pid int) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, "ps", "-p", strconv.Itoa(pid), "-o", "lstart=")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("进程不存在或无法读取进程身份")
	}
	text := strings.TrimSpace(string(out))
	if text == "" {
		return "", fmt.Errorf("进程身份为空")
	}
	return text, nil
}
func sameProcess(r gateway.RuntimeState) bool {
	current, err := ProcessStartTime(r.PID)
	return err == nil && current == r.StartTime
}
func probe(ctx context.Context, r gateway.RuntimeState) (uint64, error) {
	host, port, err := net.SplitHostPort(r.Listen)
	if err != nil {
		return 0, fmt.Errorf("运行记录的监听地址无效")
	}
	ip := net.ParseIP(host)
	if host != "localhost" && (ip == nil || !ip.IsLoopback()) {
		return 0, fmt.Errorf("运行记录不是 loopback 地址")
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+net.JoinHostPort(host, port)+gateway.ReadyPath, nil)
	if err != nil {
		return 0, err
	}
	req.Header.Set("X-CPAGW-Instance-Token", r.ProbeToken)
	client := http.Client{Timeout: 600 * time.Millisecond, CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}
	resp, err := client.Do(req)
	if err != nil {
		return 0, fmt.Errorf("网关未响应身份探测")
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("网关尚未就绪")
	}
	var body struct {
		Ready      bool   `json:"ready"`
		InstanceID string `json:"instanceID"`
		Revision   uint64 `json:"revision"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4096)).Decode(&body); err != nil {
		return 0, fmt.Errorf("网关探测响应无效")
	}
	if !body.Ready || body.InstanceID != r.InstanceID {
		return 0, fmt.Errorf("网关实例身份不匹配")
	}
	return body.Revision, nil
}
func Status(ctx context.Context, s *store.Store) (Info, error) {
	r, err := readRuntime(s)
	if os.IsNotExist(err) {
		return Info{State: "stopped"}, nil
	}
	if err != nil {
		return Info{}, err
	}
	info := Info{State: "stopped", PID: r.PID, Listen: r.Listen, Revision: r.ActiveRevision}
	if !sameProcess(r) {
		return info, nil
	}
	info.State = "starting"
	if r.Ready {
		info.State = "unresponsive"
	}
	if revision, err := probe(ctx, r); err == nil {
		info.State = "running"
		info.Revision = revision
	}
	return info, nil
}
func Start(ctx context.Context, s *store.Store) error {
	return store.WithLock(s.Path("start.lock"), func() error {
		info, err := Status(ctx, s)
		if err != nil {
			return err
		}
		if info.State != "stopped" {
			return fmt.Errorf("网关进程已存在（%s），请先检查状态", info.State)
		}
		if err := checkConfiguredPort(ctx, s); err != nil {
			return err
		}
		exe, err := os.Executable()
		if err != nil {
			return err
		}
		logPath := s.Path("server.log")
		if err = store.CheckFile(logPath); err != nil {
			return err
		}
		fd, err := unix.Open(logPath, unix.O_CREAT|unix.O_WRONLY|unix.O_APPEND|unix.O_NOFOLLOW, 0600)
		if err != nil {
			return err
		}
		logFile := os.NewFile(uintptr(fd), logPath)
		defer logFile.Close()
		cmd := exec.Command(exe, "--state-dir", s.Dir, "server", "start", "--foreground")
		cmd.Stdout = logFile
		cmd.Stderr = logFile
		cmd.Stdin = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
		if err = cmd.Start(); err != nil {
			return fmt.Errorf("启动网关进程失败")
		}
		done := make(chan error, 1)
		go func() { done <- cmd.Wait() }()
		waitCtx, cancel := context.WithTimeout(ctx, startTimeout)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case err := <-done:
				if err == nil {
					return fmt.Errorf("网关进程在就绪前退出")
				}
				return fmt.Errorf("网关启动失败，请检查私有 server.log")
			case <-waitCtx.Done():
				_ = cmd.Process.Signal(syscall.SIGTERM)
				return fmt.Errorf("网关启动超时或已取消")
			case <-ticker.C:
				r, readErr := readRuntime(s)
				if readErr != nil || r.PID != cmd.Process.Pid || !sameProcess(r) {
					continue
				}
				if _, probeErr := probe(waitCtx, r); probeErr == nil {
					return nil
				}
			}
		}
	})
}
func Foreground(ctx context.Context, s *store.Store) error {
	if err := checkConfiguredPort(ctx, s); err != nil {
		return err
	}
	id, err := config.RandomID()
	if err != nil {
		return err
	}
	err = gateway.Run(ctx, s, id)
	if errors.Is(err, context.Canceled) {
		return nil
	}
	return err
}
func Stop(ctx context.Context, s *store.Store) error {
	return store.WithLock(s.Path("start.lock"), func() error {
		r, err := readRuntime(s)
		if os.IsNotExist(err) {
			return fmt.Errorf("网关未运行")
		}
		if err != nil {
			return err
		}
		if !sameProcess(r) {
			return fmt.Errorf("网关未运行或 PID 已被复用；未发送信号")
		}
		if err = syscall.Kill(r.PID, syscall.SIGTERM); err != nil {
			return fmt.Errorf("停止网关失败")
		}
		waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
		ticker := time.NewTicker(100 * time.Millisecond)
		defer ticker.Stop()
		for {
			select {
			case <-waitCtx.Done():
				return fmt.Errorf("停止网关超时，不自动发送 SIGKILL")
			case <-ticker.C:
				if !sameProcess(r) {
					return nil
				}
			}
		}
	})
}

// WaitForRevision 确认已保存配置被当前实例加载，不把文件写入成功当成运行时生效。
func WaitForRevision(ctx context.Context, s *store.Store) error {
	state, err := s.Read()
	if err != nil {
		return err
	}
	info, err := Status(ctx, s)
	if err != nil {
		return err
	}
	if info.State == "stopped" {
		return nil
	}
	waitCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		info, err = Status(waitCtx, s)
		if err == nil && info.State == "running" && info.Revision >= state.Revision {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("配置已保存但尚未在运行网关生效")
		case <-ticker.C:
		}
	}
}
