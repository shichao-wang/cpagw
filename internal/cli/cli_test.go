package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/profile"
	"github.com/shichao-wang/cpagw/internal/provider"
	"github.com/shichao-wang/cpagw/internal/store"
)

func TestListAndShowNeverPrintSecretsOrReferences(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	conn := config.Connection{
		ID:       "test-connection-id",
		Name:     "default",
		AuthType: config.AuthAPIKey,
		Protocol: config.Chat,
		BaseURL:  "https://api.example.test",
		Models:   []config.Model{{ID: "upstream-model"}},
	}
	if err := provider.Create(st, "example", conn, "provider-api-secret"); err != nil {
		t.Fatal(err)
	}
	models := map[string]config.Binding{}
	for _, slot := range config.Slots {
		models[slot] = config.Binding{
			PublicModel: "claude-example-" + slot,
			Provider:    "example",
			Connection:  "default",
			TargetModel: "upstream-model",
			Label:       "model-label",
		}
	}
	profileName := "demo"
	if _, err := profile.Create(st, profile.File{Name: &profileName, Models: models}); err != nil {
		t.Fatal(err)
	}
	s, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	profileKey := s.Secrets[s.Profiles["demo"].KeyRef]
	keyRef := s.Profiles["demo"].KeyRef
	providerKeyRef := s.Providers["example"].DefaultCredentialRef

	for _, args := range [][]string{
		{"profile", "list"},
		{"profile", "show", "demo"},
		{"provider", "list"},
		{"provider", "show", "example"},
	} {
		root := NewCommand()
		var output, stderr bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&stderr)
		root.SetArgs(append([]string{"--state-dir", dir}, args...))
		if err := root.Execute(); err != nil {
			t.Fatalf("%v 执行失败：%v (%s)", args, err, stderr.String())
		}
		text := output.String() + stderr.String()
		for _, secret := range []string{profileKey, "provider-api-secret", keyRef, providerKeyRef} {
			if strings.Contains(text, secret) {
				t.Fatalf("%v 输出泄漏 key 或凭证引用 %q：%s", args, secret, text)
			}
		}
	}
}

func TestConnectionAddCodexOAuthDefaultsOfficialEndpoint(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.CreateProvider(st, "openai", ""); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(dir, "models.yaml")
	if err := os.WriteFile(modelsPath, []byte("- id: gpt-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "provider", "connection", "add", "openai", "codex", "--auth-type", "codex-oauth", "--protocol", "responses", "--models", modelsPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("OAuth connection add 失败：%v", err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := state.Providers["openai"].Connections["codex"]
	if c.ID == "" || c.AuthType != config.AuthCodexOAuth || c.BaseURL != config.CodexBaseURL || c.CredentialRef != "" {
		t.Fatalf("OAuth connection 设置不正确：%#v", c)
	}
}

func TestCodexOAuthLoginAndLogoutCommands(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	if err := provider.CreateProvider(st, "openai", ""); err != nil {
		t.Fatal(err)
	}
	if err := provider.AddConnection(st, "openai", config.Connection{
		Name: "codex", AuthType: config.AuthCodexOAuth, Protocol: config.Responses,
		BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "gpt-test"}},
	}, ""); err != nil {
		t.Fatal(err)
	}
	loginCalled := false
	opts := Options{Login: func(_ context.Context, noBrowser bool) (config.OAuthCredential, error) {
		loginCalled = true
		if !noBrowser {
			t.Fatal("--no-browser 未传递给登录器")
		}
		return config.OAuthCredential{AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: "id-secret", AccountID: "private-account", ExpiresAt: time.Now().Add(time.Hour), Generation: 1}, nil
	}}
	root := NewCommandWithOptions(opts)
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--state-dir", dir, "provider", "connection", "login", "openai", "codex", "--no-browser"})
	if err := root.Execute(); err != nil {
		t.Fatalf("login 失败：%v", err)
	}
	if !loginCalled {
		t.Fatal("注入登录器未调用")
	}
	state, c, err := getConnection(st, "openai", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if c.CredentialRef == "" || state.OAuthCredentials[c.CredentialRef].AccessToken != "access-secret" {
		t.Fatal("OAuth 凭证没有绑定到目标连接")
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "id-secret", "private-account", c.CredentialRef} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("login 输出泄漏 OAuth 凭证数据 %q", secret)
		}
	}

	output.Reset()
	root = NewCommand()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--state-dir", dir, "provider", "connection", "show", "openai", "codex"})
	if err := root.Execute(); err != nil {
		t.Fatalf("connection show 失败：%v", err)
	}
	root = NewCommand()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--state-dir", dir, "provider", "check", "openai", "--connection", "codex"})
	if err := root.Execute(); err != nil {
		t.Fatalf("OAuth check 失败：%v", err)
	}
	if !strings.Contains(output.String(), "未请求通用 /models") {
		t.Fatalf("OAuth check 未表明不发送通用目录请求：%q", output.String())
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "id-secret", "private-account", c.CredentialRef} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("show/check 输出泄漏 OAuth 凭证数据 %q", secret)
		}
	}

	root = NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "provider", "connection", "logout", "openai", "codex", "--yes"})
	if err := root.Execute(); err != nil {
		t.Fatalf("logout 失败：%v", err)
	}
	_, c, err = getConnection(st, "openai", "codex")
	if err != nil {
		t.Fatal(err)
	}
	if c.AuthType != config.AuthCodexOAuth || c.CredentialRef != "" {
		t.Fatalf("logout 应保留 OAuth 连接但清除其凭证：%#v", c)
	}
}

func TestApiKeyRequiresExplicitStdinInNonInteractiveMode(t *testing.T) {
	modelPath := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(modelPath, []byte("- id: upstream-model\\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	root.SetArgs([]string{"provider", "add", "demo"})
	root.SetIn(strings.NewReader("some-secret"))
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--api-key-stdin") {
		t.Fatalf("非交互模式应要求明确的 stdin 标志，错误为：%v", err)
	}
}

func TestVersionCommand(t *testing.T) {
	// 直接构建时未注入版本，此处仅覆盖空值路径；注入路径由 formatVersion 的表驱动测试覆盖。
	root := NewCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"version"})
	if err := root.Execute(); err != nil {
		t.Fatalf("version 执行失败：%v", err)
	}
	if got := output.String(); !strings.Contains(got, "cpagw unknown") {
		t.Fatalf("version 未报告未知版本：%q", got)
	}
}

func TestVersionFlagsRejected(t *testing.T) {
	// 不设置 root.Version，故 cobra 不注册 --version 与 -v，两者都应被拒绝。
	for _, arg := range []string{"--version", "-v"} {
		root := NewCommand()
		var output bytes.Buffer
		root.SetOut(&output)
		root.SetErr(&output)
		root.SetArgs([]string{arg})
		if err := root.Execute(); err == nil {
			t.Fatalf("%s 应被拒绝，实际输出：%q", arg, output.String())
		}
	}
}

func TestStateMigrateCommand(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	v1 := `{"schemaVersion":1,"revision":3,"listen":"127.0.0.1:8317","providers":{},"profiles":{},"secrets":{}}`
	statePath := filepath.Join(dir, "state.json")
	if err := os.WriteFile(statePath, []byte(v1), 0600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	var output bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--state-dir", dir, "state", "migrate"})
	if err := root.Execute(); err != nil {
		t.Fatalf("state migrate 执行失败：%v", err)
	}
	migrated, err := os.ReadFile(statePath)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(migrated), `"schemaVersion": 2`) {
		t.Fatalf("状态未迁移到 v2：%s", migrated)
	}
	backup, err := os.ReadFile(statePath + ".v1.bak")
	if err != nil {
		t.Fatal(err)
	}
	if string(backup) != v1 {
		t.Fatalf("迁移备份不等于原始状态：%s", backup)
	}
}

func TestStateMigrateRejectsRunningGateway(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	root := NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "state", "migrate"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "停止网关") {
		t.Fatalf("应拒绝在网关锁占用时迁移，得到：%v", err)
	}
}

func TestFormatVersion(t *testing.T) {
	cases := []struct {
		name  string
		info  VersionInfo
		want  []string
		omits []string
	}{
		{
			name: "全部注入",
			info: VersionInfo{Version: "v1.2.3", Commit: "abc123", Date: "2026-10-05T19:40:00Z"},
			want: []string{"cpagw v1.2.3", "commit: abc123", "构建时间: 2026-10-05T19:40:00Z"},
		},
		{
			name:  "仅版本号时省略其余行",
			info:  VersionInfo{Version: "v1.2.3"},
			want:  []string{"cpagw v1.2.3"},
			omits: []string{"commit:", "构建时间:"},
		},
		{
			name:  "均未注入",
			info:  VersionInfo{},
			want:  []string{"cpagw unknown"},
			omits: []string{"commit:", "构建时间:"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := formatVersion(tc.info)
			for _, w := range tc.want {
				if !strings.Contains(got, w) {
					t.Errorf("缺少 %q，实际输出：%q", w, got)
				}
			}
			for _, o := range tc.omits {
				if strings.Contains(got, o) {
					t.Errorf("不应包含 %q，实际输出：%q", o, got)
				}
			}
		})
	}
}
