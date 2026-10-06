package cli

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/upgrade"
)

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, errors.New("write failed")
}

type failingAfterWritesWriter struct {
	writesRemaining int
}

func (w *failingAfterWritesWriter) Write(data []byte) (int, error) {
	if w.writesRemaining == 0 {
		return 0, errors.New("write failed")
	}
	w.writesRemaining--
	return len(data), nil
}

func TestUpgradeCommandPassesContextAndVersion(t *testing.T) {
	type contextKey struct{}
	ctx := context.WithValue(context.Background(), contextKey{}, "sentinel")
	var output bytes.Buffer
	called := false
	cmd := newUpgradeCommand(func(gotContext context.Context, gotVersion string) (upgrade.Result, error) {
		called = true
		if gotContext.Value(contextKey{}) != "sentinel" {
			t.Fatal("runner 未收到命令 context")
		}
		if gotVersion != Version().Version {
			t.Fatalf("runner 收到的版本为 %q，期望 %q", gotVersion, Version().Version)
		}
		return upgrade.Result{Version: "v2026.10.5-abc123"}, nil
	})
	cmd.SetContext(ctx)
	cmd.SetOut(&output)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("upgrade 执行失败：%v", err)
	}
	if !called {
		t.Fatal("没有调用升级 runner")
	}
	for _, want := range []string{"当前版本：", "正在检查最新 Release", "已是最新版本（v2026.10.5-abc123）"} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("输出缺少 %q：%s", want, output.String())
		}
	}
}

func TestUpgradeCommandReportsReplacementAndWarning(t *testing.T) {
	var output bytes.Buffer
	cmd := newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		return upgrade.Result{
			Version: "v2026.10.5-abc123",
			Path:    "/usr/local/bin/cpagw",
			Updated: true,
			Warning: "安装目录同步失败",
		}, nil
	})
	cmd.SetOut(&output)

	if err := cmd.Execute(); err != nil {
		t.Fatalf("upgrade 执行失败：%v", err)
	}
	for _, want := range []string{
		"已升级至 v2026.10.5-abc123",
		"/usr/local/bin/cpagw",
		"配置未修改",
		"运行中的网关需手动重启",
		"警告：安装目录同步失败",
	} {
		if !strings.Contains(output.String(), want) {
			t.Errorf("输出缺少 %q：%s", want, output.String())
		}
	}
}

func TestUpgradeCommandDoesNotRunAfterOutputFailure(t *testing.T) {
	called := false
	cmd := newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		called = true
		return upgrade.Result{}, nil
	})
	cmd.SetOut(failingWriter{})

	if err := cmd.Execute(); err == nil {
		t.Fatal("输出失败时应返回错误")
	}
	if called {
		t.Fatal("初始状态输出失败后不应调用升级 runner")
	}
}

func TestUpgradeCommandOutputFailureAfterReplacementIsExplicit(t *testing.T) {
	cmd := newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		return upgrade.Result{Version: "v2.0.0", Path: "/opt/bin/cpagw", Updated: true}, nil
	})
	cmd.SetOut(&failingAfterWritesWriter{writesRemaining: 2})

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "二进制已更新为 v2.0.0（/opt/bin/cpagw）") {
		t.Fatalf("最终输出失败时应明确二进制已更新，实际为：%v", err)
	}
}

func TestUpgradeCommandRunnerError(t *testing.T) {
	var output bytes.Buffer
	cmd := newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		return upgrade.Result{}, errors.New("网络错误")
	})
	cmd.SetOut(&output)

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "升级失败：网络错误") {
		t.Fatalf("应返回升级错误，实际为：%v", err)
	}
}

func TestUpgradeCommandErrorAfterReplacementIsExplicit(t *testing.T) {
	var output bytes.Buffer
	cmd := newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		return upgrade.Result{Version: "v2.0.0", Path: "/opt/bin/cpagw", Updated: true}, errors.New("提交记录失败")
	})
	cmd.SetOut(&output)

	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "二进制已更新为 v2.0.0（/opt/bin/cpagw）") {
		t.Fatalf("错误应明确二进制已更新，实际为：%v", err)
	}
}

func TestUpgradeCommandRejectsArguments(t *testing.T) {
	called := false
	cmd := newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		called = true
		return upgrade.Result{}, nil
	})
	cmd.SetArgs([]string{"unexpected"})

	if err := cmd.Execute(); err == nil {
		t.Fatal("upgrade 不应接受位置参数")
	}
	if called {
		t.Fatal("参数被拒绝后不应调用升级 runner")
	}
}

func TestRootUpgradeDoesNotUseStateOrRuntimeCallbacks(t *testing.T) {
	stateDir := filepath.Join(t.TempDir(), "state")
	callbackCalled := false
	root := NewCommandWithOptions(Options{
		OnChange: func() error {
			callbackCalled = true
			return nil
		},
		ProfileReferenced: func(string) (bool, error) {
			callbackCalled = true
			return false, nil
		},
	})
	for _, command := range root.Commands() {
		if command.Name() == "upgrade" {
			root.RemoveCommand(command)
			break
		}
	}
	runnerCalled := false
	root.AddCommand(newUpgradeCommand(func(context.Context, string) (upgrade.Result, error) {
		runnerCalled = true
		return upgrade.Result{Version: "v2.0.0"}, nil
	}))
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetArgs([]string{"--state-dir", stateDir, "upgrade"})

	if err := root.Execute(); err != nil {
		t.Fatalf("根命令 upgrade 执行失败：%v", err)
	}
	if !runnerCalled {
		t.Fatal("根命令未调用 stub upgrade runner")
	}
	if callbackCalled {
		t.Fatal("upgrade 不应调用运行时集成回调")
	}
	if _, err := os.Stat(stateDir); !os.IsNotExist(err) {
		t.Fatalf("upgrade 不应创建状态目录，stat 错误：%v", err)
	}
}
