package server

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

// listenOn 在指定地址上启动真实监听，用于复现通配监听与 loopback 共存的情况。
func listenOn(t *testing.T, network, address string) net.Listener {
	t.Helper()
	listener, err := net.Listen(network, address)
	if err != nil {
		if network == "tcp6" {
			t.Skipf("当前环境不支持 IPv6：%v", err)
		}
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	return listener
}

func loopbackPort(t *testing.T, listener net.Listener) string {
	t.Helper()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	return port
}

func TestCheckListenAvailableRejectsOccupiedPort(t *testing.T) {
	for _, test := range []struct{ name, network, address string }{
		{"IPv4", "tcp4", "127.0.0.1:0"},
		{"IPv6", "tcp6", "[::1]:0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := listenOn(t, test.network, test.address)
			address := listener.Addr().String()
			err := checkListenAvailable(context.Background(), address)
			if err == nil || !strings.Contains(err.Error(), "已被占用") || !strings.Contains(err.Error(), address) {
				t.Fatalf("端口冲突提示不明确：%v", err)
			}
			connection, err := net.DialTimeout(test.network, address, time.Second)
			if err != nil {
				t.Fatalf("预检查影响了原监听服务：%v", err)
			}
			connection.Close()
		})
	}
}

// macOS 上通配监听与具体 loopback 地址可以同时绑成功，只检查目标地址会漏判。
func TestCheckListenAvailableRejectsWildcardCoexistence(t *testing.T) {
	for _, test := range []struct{ name, network, wildcard string }{
		{"IPv4 wildcard", "tcp4", "0.0.0.0:0"},
		{"IPv6 wildcard", "tcp6", "[::]:0"},
	} {
		t.Run(test.name, func(t *testing.T) {
			listener := listenOn(t, test.network, test.wildcard)
			port := loopbackPort(t, listener)
			address := net.JoinHostPort("127.0.0.1", port)
			err := checkListenAvailable(context.Background(), address)
			if err == nil || !strings.Contains(err.Error(), "已被占用") {
				t.Fatalf("通配监听必须阻止同端口启动，实际：%v", err)
			}
			if !strings.Contains(err.Error(), address) {
				t.Fatalf("错误应包含待启动地址 %s：%v", address, err)
			}
		})
	}
}

// 具体地址上的监听也要被识别，避免被 ::1 的连通性掩盖。
func TestCheckListenAvailableRejectsConcreteLoopback(t *testing.T) {
	listener := listenOn(t, "tcp4", "127.0.0.1:0")
	address := listener.Addr().String()
	if err := checkListenAvailable(context.Background(), address); err == nil {
		t.Fatal("已有监听未被检出")
	}
}

func TestProbeTargetsCoverLoopbackAddresses(t *testing.T) {
	targets := probeTargets("127.0.0.1", "8317")
	if len(targets) != 2 || targets[0] != "127.0.0.1:8317" || targets[1] != "[::1]:8317" {
		t.Fatalf("探测目标应覆盖两个 loopback 地址：%v", targets)
	}
	targets = probeTargets("::1", "8317")
	if len(targets) != 2 || targets[0] != "[::1]:8317" || targets[1] != "127.0.0.1:8317" {
		t.Fatalf("IPv6 入口也应覆盖两个地址：%v", targets)
	}
}

func TestCheckListenAvailableReleasesFreePort(t *testing.T) {
	listener, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	address := listener.Addr().String()
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err = checkListenAvailable(context.Background(), address); err != nil {
		t.Fatal(err)
	}
	listener, err = net.Listen("tcp4", address)
	if err != nil {
		t.Fatalf("端口未被预检查释放：%v", err)
	}
	listener.Close()
}

func TestCheckListenAvailableRejectsInvalidAddressAndCancellation(t *testing.T) {
	for _, address := range []string{"invalid", "0.0.0.0:8317", "example.com:8317", "127.0.0.1:0", "127.0.0.1:65536", "127.0.0.1:abc"} {
		if err := checkListenAvailable(context.Background(), address); err == nil {
			t.Errorf("无效地址未被拒绝：%s", address)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := checkListenAvailable(ctx, "127.0.0.1:8317"); !errors.Is(err, context.Canceled) {
		t.Fatalf("取消操作应直接返回：%v", err)
	}
}

func TestStartPortConflictDoesNotLaunchProcess(t *testing.T) {
	for _, test := range []struct {
		name  string
		start func(context.Context, *store.Store) error
	}{{"background", Start}, {"foreground", Foreground}} {
		t.Run(test.name, func(t *testing.T) {
			listener := listenOn(t, "tcp4", "127.0.0.1:0")
			address := listener.Addr().String()
			s, err := store.New(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			if err = s.Update(func(state *config.State) error { state.Listen = address; return nil }); err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			if err = test.start(ctx, s); err == nil || !strings.Contains(err.Error(), "已被占用") {
				t.Fatalf("启动未在端口预检查处失败：%v", err)
			}
			for _, name := range []string{"server.log", "runtime.json", "sdk-runtime.yaml", ".gateway.lock"} {
				if _, err := os.Stat(s.Path(name)); !os.IsNotExist(err) {
					t.Errorf("冲突时不应创建 %s：%v", name, err)
				}
			}
		})
	}
}
