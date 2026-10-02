package integration

import (
	"context"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestBinaryPortConflictDoesNotDisturbExistingServer(t *testing.T) {
	if testing.Short() {
		t.Skip("短测试模式跳过子进程验收")
	}
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "cpagw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cpagw")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 binary：%v\n%s", err, out)
	}
	existing := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, "原服务仍在运行") }))
	defer existing.Close()
	address := strings.TrimPrefix(existing.URL, "http://")
	for _, mode := range []string{"background", "foreground"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(tmp, mode)
			args := []string{"--state-dir", dir, "server", "start", "--listen", address}
			if mode == "foreground" {
				args = append(args, "--foreground")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("端口冲突应立即返回，而不是等待启动超时")
			}
			if err == nil || !strings.Contains(string(out), "已被占用") || !strings.Contains(string(out), address) {
				t.Fatalf("端口冲突应包含明确地址：%v %s", err, out)
			}
			for _, name := range []string{"runtime.json", "server.log"} {
				if _, err := os.Stat(filepath.Join(dir, name)); !os.IsNotExist(err) {
					t.Errorf("冲突时不应创建 %s：%v", name, err)
				}
			}
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get(existing.URL)
			if err != nil {
				t.Fatalf("原服务被影响：%v", err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || string(body) != "原服务仍在运行" {
				t.Fatalf("原服务响应异常：%v %d %s", err, response.StatusCode, body)
			}
		})
	}
}

// 通配监听（*:port）与具体 loopback 地址在 macOS 上可以同时绑成功，
// 曾经因此漏判冲突并让 cpagw 顶掉了原服务。这里用真实 binary 复现该场景。
func TestBinaryRejectsWildcardListenerOnSamePort(t *testing.T) {
	if testing.Short() {
		t.Skip("短测试模式跳过子进程验收")
	}
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "cpagw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cpagw")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 binary：%v\n%s", err, out)
	}
	// 模拟 cli-proxy-api 的 *:8317 形式：监听通配地址，而非具体 loopback。
	listener, err := net.Listen("tcp4", "0.0.0.0:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, "原服务仍在运行")
	})}
	go func() { _ = server.Serve(listener) }()
	defer server.Close()
	_, port, err := net.SplitHostPort(listener.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	address := net.JoinHostPort("127.0.0.1", port)

	for _, mode := range []string{"background", "foreground"} {
		t.Run(mode, func(t *testing.T) {
			dir := filepath.Join(tmp, "wildcard-"+mode)
			args := []string{"--state-dir", dir, "server", "start", "--listen", address}
			if mode == "foreground" {
				args = append(args, "--foreground")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			out, err := exec.CommandContext(ctx, binary, args...).CombinedOutput()
			if ctx.Err() != nil {
				t.Fatal("端口冲突应立即返回，而不是等待启动超时")
			}
			if err == nil {
				t.Fatalf("通配监听已占用 %s，cpagw 不应启动成功", address)
			}
			if !strings.Contains(string(out), "已被占用") {
				t.Fatalf("错误应说明端口已被占用：%v %s", err, out)
			}
			if _, err := os.Stat(filepath.Join(dir, "runtime.json")); !os.IsNotExist(err) {
				t.Errorf("冲突时不应创建 runtime.json：%v", err)
			}
			// 原服务必须仍能正常响应，未被 cpagw 顶掉。
			client := &http.Client{Timeout: time.Second}
			response, err := client.Get("http://" + address)
			if err != nil {
				t.Fatalf("原服务被影响：%v", err)
			}
			body, err := io.ReadAll(response.Body)
			response.Body.Close()
			if err != nil || response.StatusCode != 200 || string(body) != "原服务仍在运行" {
				t.Fatalf("原服务响应异常：%v %d %s", err, response.StatusCode, body)
			}
		})
	}
}
