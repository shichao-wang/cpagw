package integration

import (
	"bytes"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

// TestOAuthBinaryConfiguration 验证实际 binary 的 OAuth 配置入口，不发起设备登录或上游请求。
func TestOAuthBinaryConfiguration(t *testing.T) {
	if testing.Short() {
		t.Skip("短测试模式跳过 OAuth 配置子进程验收")
	}
	root := t.TempDir()
	binary := filepath.Join(root, "cpagw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cpagw")
	build.Dir = filepath.Join("..", "..")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 OAuth 测试 binary：%v\n%s", err, output)
	}
	dir := filepath.Join(root, "state")
	invoke := func(args ...string) ([]byte, error) {
		cmd := exec.Command(binary, append([]string{"--state-dir", dir}, args...)...)
		cmd.Stdin = strings.NewReader("")
		return cmd.CombinedOutput()
	}
	run := func(args ...string) []byte {
		t.Helper()
		output, err := invoke(args...)
		if err != nil {
			t.Fatalf("OAuth 离线配置命令失败：%v\n%s", err, output)
		}
		return output
	}
	models := filepath.Join(root, "models.yaml")
	if err := os.WriteFile(models, []byte("models:\n  - id: mock-codex-model\n    name: Mock Codex\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("connection", "add", "codex", "--auth-type", "codex-oauth", "--protocol", "responses", "--models", models)
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := state.Connections["codex"]
	if state.SchemaVersion != 3 || c.ID == "" || c.AuthType != config.AuthCodexOAuth || c.BaseURL != config.CodexBaseURL || c.CredentialRef != "" || len(state.Secrets) != 0 || len(state.OAuthCredentials) != 0 {
		t.Fatal("未登录 OAuth 连接未按独立 schema3 保存")
	}
	output := run("connection", "show", "codex")
	if bytes.Contains(output, []byte(c.ID)) || bytes.Contains(output, []byte("accessToken")) || bytes.Contains(output, []byte("refreshToken")) {
		t.Fatal("show 输出了内部认证身份或敏感字段")
	}
	for _, command := range []string{"login", "logout"} {
		if out := run("connection", command, "--help"); !bytes.Contains(out, []byte(command)) {
			t.Fatal("顶层 OAuth 命令未挂载")
		}
	}
	before, err := os.ReadFile(st.Path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	checkOutput, _ := invoke("connection", "check", "codex")
	if !bytes.Contains(checkOutput, []byte("OAuth")) && !bytes.Contains(checkOutput, []byte("登录")) {
		t.Fatal("OAuth check 未报告本地认证状态")
	}
	after, err := os.ReadFile(st.Path("state.json"))
	if err != nil || !bytes.Equal(before, after) {
		t.Fatal("OAuth 本地检查改变了状态")
	}
	for _, args := range [][]string{
		{"connection", "add", "wrong-protocol", "--auth-type", "codex-oauth", "--protocol", "anthropic-messages", "--models", models},
		{"connection", "add", "wrong-address", "--auth-type", "codex-oauth", "--protocol", "responses", "--base-url", "https://example.invalid", "--models", models},
	} {
		if _, err := invoke(args...); err == nil {
			t.Fatal("不安全或不支持的 OAuth 连接未被拒绝")
		}
	}
	run("connection", "logout", "codex", "--yes")
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Connections["codex"].ID != c.ID {
		t.Fatal("logout 改变了连接身份或删除了连接")
	}
	run("connection", "remove", "codex", "--yes")
	state, err = st.Read()
	if err != nil || len(state.Connections) != 0 {
		t.Fatal("空闲 OAuth 连接删除失败")
	}
}

// TestOldStateShapesNeverOverwritten 对两种同版本旧状态均拒绝，保证拒绝后原文件不变。
func TestOldStateShapesNeverOverwritten(t *testing.T) {
	for name, old := range map[string]any{
		"schema1-provider":   map[string]any{"schemaVersion": 1, "providers": map[string]any{}, "profiles": map[string]any{}, "secrets": map[string]any{}},
		"schema2-provider":   map[string]any{"schemaVersion": 2, "providers": map[string]any{}, "profiles": map[string]any{}, "secrets": map[string]any{}, "oauthCredentials": map[string]any{}},
		"schema2-connection": map[string]any{"schemaVersion": 2, "connections": map[string]any{}, "profiles": map[string]any{}, "secrets": map[string]any{}},
	} {
		t.Run(name, func(t *testing.T) {
			st, err := store.New(filepath.Join(t.TempDir(), "state"))
			if err != nil {
				t.Fatal(err)
			}
			before, err := json.Marshal(old)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(st.Path("state.json"), before, 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := st.Read(); err == nil {
				t.Fatal("旧状态未被拒绝")
			}
			called := false
			if err := st.Update(func(*config.State) error { called = true; return nil }); err == nil || called {
				t.Fatal("旧状态被允许修改")
			}
			after, err := os.ReadFile(st.Path("state.json"))
			if err != nil || !bytes.Equal(before, after) {
				t.Fatal("拒绝旧状态时改写了原文件")
			}
		})
	}
}
