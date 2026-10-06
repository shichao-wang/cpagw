package cli

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/connection"
	"github.com/shichao-wang/cpagw/internal/profile"
	"github.com/shichao-wang/cpagw/internal/store"
	"github.com/spf13/cobra"
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
	conn := config.Connection{Name: "default", AuthType: config.AuthAPIKey, Protocol: config.Chat, BaseURL: "https://api.example.test", Models: []config.Model{{ID: "upstream-model"}}}
	if err := connection.Create(context.Background(), st, conn, "connection-api-secret"); err != nil {
		t.Fatal(err)
	}
	models := map[string]config.Binding{}
	for _, slot := range config.Slots {
		models[slot] = config.Binding{PublicModel: "claude-example-" + slot, Connection: "default", TargetModel: "upstream-model", Label: "model-label"}
	}
	profileName := "demo"
	if _, err := profile.Create(st, profile.File{Name: &profileName, Models: models}); err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	profileKey := state.Secrets[state.Profiles["demo"].KeyRef]
	keyRef := state.Profiles["demo"].KeyRef
	connectionKeyRef := state.Connections["default"].CredentialRef

	for _, args := range [][]string{
		{"profile", "list"},
		{"profile", "show", "demo"},
		{"connection", "list"},
		{"connection", "show", "default"},
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
		for _, secret := range []string{profileKey, "connection-api-secret", keyRef, connectionKeyRef} {
			if strings.Contains(text, secret) {
				t.Fatalf("%v 输出泄漏 key 或凭证引用 %q：%s", args, secret, text)
			}
		}
	}
}

func TestLegacyProviderCommandIsRejected(t *testing.T) {
	root := NewCommand()
	root.SetArgs([]string{"provider", "list"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "unknown command") {
		t.Fatalf("旧 provider 命令应被拒绝，错误为：%v", err)
	}
}

func TestConnectionAddScriptModeRequiresAllFieldsAndExplicitStdin(t *testing.T) {
	root := NewCommand()
	root.SetArgs([]string{"connection", "add", "demo"})
	root.SetIn(strings.NewReader("some-secret"))
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "--api-key-stdin") {
		t.Fatalf("非交互缺参应立即报错，错误为：%v", err)
	}
}

func TestConnectionUpdatePreservesKeyUnlessRotated(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	original := config.Connection{Name: "demo", AuthType: config.AuthAPIKey, Protocol: config.Chat, BaseURL: "https://old.example.test", Models: []config.Model{{ID: "old-model"}}}
	if err := connection.Create(context.Background(), st, original, "old-api-secret"); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(modelPath, []byte("- id: new-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "connection", "update", "demo", "--base-url", "https://new.example.test", "--models", modelPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("不带 key 更新失败：%v", err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	updated := state.Connections["demo"]
	if updated.BaseURL != "https://new.example.test" || updated.Models[0].ID != "new-model" || state.Secrets[updated.CredentialRef] != "old-api-secret" {
		t.Fatalf("更新未保留预期地址、模型或 key：%+v", updated)
	}

	root = NewCommand()
	root.SetIn(strings.NewReader("rotated-api-secret\n"))
	root.SetArgs([]string{"--state-dir", dir, "connection", "update", "demo", "--api-key-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("key 轮换失败：%v", err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	updated = state.Connections["demo"]
	if state.Secrets[updated.CredentialRef] != "rotated-api-secret" {
		t.Fatal("key 轮换未生效")
	}
}

func TestConnectionAuthTypeSwitchRequiresExplicitAPIKey(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "state")
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	c := config.Connection{Name: "switch", AuthType: config.AuthAPIKey, Protocol: config.Responses, BaseURL: "https://api.example.test", Models: []config.Model{{ID: "model"}}}
	if err := connection.Create(context.Background(), st, c, "initial-api-secret"); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "connection", "update", "switch", "--auth-type", config.AuthCodexOAuth})
	if err := root.Execute(); err != nil {
		t.Fatalf("切换到 OAuth 失败：%v", err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	oauthConn := state.Connections["switch"]
	if oauthConn.AuthType != config.AuthCodexOAuth || oauthConn.BaseURL != config.CodexBaseURL || oauthConn.CredentialRef != "" {
		t.Fatalf("OAuth 认证切换不正确：%+v", oauthConn)
	}

	root = NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "connection", "update", "switch", "--auth-type", config.AuthAPIKey})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "必须显式提供 API key") {
		t.Fatalf("切回 API key 未要求显式 key：%v", err)
	}
	root = NewCommand()
	root.SetIn(strings.NewReader("new-api-secret\n"))
	root.SetArgs([]string{"--state-dir", dir, "connection", "update", "switch", "--auth-type", config.AuthAPIKey, "--api-key-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("显式提供 key 后切换失败：%v", err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	apiConn := state.Connections["switch"]
	if apiConn.AuthType != config.AuthAPIKey || state.Secrets[apiConn.CredentialRef] != "new-api-secret" {
		t.Fatal("显式 API key 切换没有保存新凭证")
	}
}

func TestCodexOAuthAddLoginCheckAndLogout(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	modelsPath := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(modelsPath, []byte("- id: codex-test\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "connection", "add", "codex", "--auth-type", config.AuthCodexOAuth, "--protocol", config.Responses, "--models", modelsPath})
	if err := root.Execute(); err != nil {
		t.Fatalf("OAuth 连接创建失败：%v", err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	created := state.Connections["codex"]
	if created.ID == "" || created.AuthType != config.AuthCodexOAuth || created.BaseURL != config.CodexBaseURL || created.CredentialRef != "" {
		t.Fatalf("OAuth 连接默认值错误：%+v", created)
	}

	loginCalls := 0
	opts := Options{Login: func(_ context.Context, noBrowser bool) (config.OAuthCredential, error) {
		loginCalls++
		if !noBrowser {
			t.Fatal("--no-browser 未传递给登录器")
		}
		return config.OAuthCredential{AccessToken: "access-secret", RefreshToken: "refresh-secret", IDToken: "identity-secret", AccountID: "private-account", ExpiresAt: time.Now().Add(time.Hour)}, nil
	}}
	var output bytes.Buffer
	root = NewCommandWithOptions(opts)
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--state-dir", dir, "connection", "login", "codex", "--no-browser"})
	if err := root.Execute(); err != nil {
		t.Fatalf("OAuth 登录失败：%v", err)
	}
	if loginCalls != 1 {
		t.Fatalf("fake login 调用次数错误：%d", loginCalls)
	}
	for _, secret := range []string{"access-secret", "refresh-secret", "identity-secret", "private-account"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("登录输出泄漏敏感值 %q", secret)
		}
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	loggedIn := state.Connections["codex"]
	if loggedIn.ID != created.ID || loggedIn.CredentialRef == "" || state.OAuthCredentials[loggedIn.CredentialRef].AccessToken != "access-secret" {
		t.Fatal("登录凭证没有绑定到原连接")
	}

	output.Reset()
	root = NewCommand()
	root.SetOut(&output)
	root.SetErr(&output)
	root.SetArgs([]string{"--state-dir", dir, "connection", "check", "codex"})
	if err := root.Execute(); err != nil {
		t.Fatalf("OAuth 本地 check 失败：%v", err)
	}
	if !strings.Contains(output.String(), "未请求通用 /models") {
		t.Fatalf("OAuth check 未说明不发送通用模型目录请求：%q", output.String())
	}

	root = NewCommand()
	root.SetArgs([]string{"--state-dir", dir, "connection", "logout", "codex", "--yes"})
	if err := root.Execute(); err != nil {
		t.Fatalf("OAuth logout 失败：%v", err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	loggedOut := state.Connections["codex"]
	if loggedOut.ID != created.ID || loggedOut.AuthType != config.AuthCodexOAuth || loggedOut.CredentialRef != "" || len(state.OAuthCredentials) != 0 {
		t.Fatalf("logout 未保留连接身份或未清除本地 OAuth：%+v", loggedOut)
	}
}

func TestConnectionAddScriptModeKeepsSecretsOutOfOutput(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(modelPath, []byte("- id: upstream-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	root := NewCommand()
	var output, stderr bytes.Buffer
	root.SetOut(&output)
	root.SetErr(&stderr)
	root.SetIn(strings.NewReader("script-api-secret\n"))
	root.SetArgs([]string{"--state-dir", dir, "connection", "add", "demo", "--protocol", config.Chat, "--base-url", "https://api.example.test", "--models", modelPath, "--api-key-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("脚本模式添加失败：%v (%s)", err, stderr.String())
	}
	if text := output.String() + stderr.String(); strings.Contains(text, "script-api-secret") {
		t.Fatalf("命令输出泄漏 key：%q", text)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := state.Connections["demo"]
	if state.Secrets[c.CredentialRef] != "script-api-secret" {
		t.Fatal("连接 key 未保存")
	}
}

type fakePrompter struct {
	terminal       bool
	lines          []string
	selections     []string
	secret         string
	confirm        bool
	lineErr        error
	secretErr      error
	selectErr      error
	lineCalls      int
	secretCalls    int
	selectCalls    int
	confirmCalls   int
	selectedMenus  [][]promptOption
	selectionTitle []string
}

func (p *fakePrompter) IsTerminal() bool { return p.terminal }
func (p *fakePrompter) ReadLine(_ context.Context, _ string) (string, error) {
	p.lineCalls++
	if p.lineErr != nil {
		return "", p.lineErr
	}
	if len(p.lines) == 0 {
		return "", io.EOF
	}
	line := p.lines[0]
	p.lines = p.lines[1:]
	return line, nil
}
func (p *fakePrompter) ReadSecret(_ context.Context, _ string) (string, error) {
	p.secretCalls++
	return p.secret, p.secretErr
}
func (p *fakePrompter) Select(_ context.Context, title string, options []promptOption) (string, error) {
	p.selectCalls++
	p.selectionTitle = append(p.selectionTitle, title)
	p.selectedMenus = append(p.selectedMenus, append([]promptOption(nil), options...))
	if p.selectErr != nil {
		return "", p.selectErr
	}
	if len(p.selections) == 0 {
		return "", io.EOF
	}
	value := p.selections[0]
	p.selections = p.selections[1:]
	return value, nil
}
func (p *fakePrompter) Confirm(_ context.Context, _ string) (bool, error) {
	p.confirmCalls++
	p.selectionTitle = append(p.selectionTitle, "确认")
	p.selectedMenus = append(p.selectedMenus, []promptOption{{label: "取消", value: "false"}, {label: "确认", value: "true"}})
	return p.confirm, nil
}

func TestConnectionStdinModeDoesNotEnterWizardOnTTY(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	modelPath := filepath.Join(t.TempDir(), "models.yaml")
	if err := os.WriteFile(modelPath, []byte("- id: upstream-model\n"), 0600); err != nil {
		t.Fatal(err)
	}
	p := &fakePrompter{terminal: true, lines: []string{"should-not-be-read"}, secret: "ignored"}
	root := &cobra.Command{Use: "cpagw"}
	root.AddCommand(newConnectionCommandWithPrompter(func() (*store.Store, error) { return store.New(dir) }, Options{}, p))
	root.SetIn(strings.NewReader("tty-script-secret\n"))
	root.SetArgs([]string{"connection", "add", "demo", "--protocol", config.Chat, "--base-url", "https://api.example.test", "--models", modelPath, "--api-key-stdin"})
	if err := root.Execute(); err != nil {
		t.Fatalf("TTY 下完整 stdin 模式失败：%v", err)
	}
	if p.lineCalls != 0 || p.secretCalls != 0 || p.selectCalls != 0 || p.confirmCalls != 0 {
		t.Fatalf("stdin 模式进入了交互提示：line=%d secret=%d confirm=%d", p.lineCalls, p.secretCalls, p.confirmCalls)
	}

	p = &fakePrompter{terminal: true, lines: []string{"must-not-be-consumed"}}
	root = &cobra.Command{Use: "cpagw"}
	root.AddCommand(newConnectionCommandWithPrompter(func() (*store.Store, error) { return store.New(dir) }, Options{}, p))
	root.SetIn(strings.NewReader("another-secret"))
	root.SetArgs([]string{"connection", "add", "demo", "--api-key-stdin"})
	if err := root.Execute(); err == nil || !strings.Contains(err.Error(), "必须同时提供") {
		t.Fatalf("TTY 下 stdin 缺少普通参数应立即失败，错误：%v", err)
	}
	if p.lineCalls != 0 || p.secretCalls != 0 || p.selectCalls != 0 || p.confirmCalls != 0 {
		t.Fatalf("不完整 stdin 模式消费了交互输入：line=%d secret=%d confirm=%d", p.lineCalls, p.secretCalls, p.confirmCalls)
	}
}

func TestConnectionWizardCreatesWithoutLeakingSecret(t *testing.T) {
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	p := &fakePrompter{terminal: true, lines: []string{"demo", "https://api.example.test", "upstream-model=Display", "other-model"}, selections: []string{config.AuthAPIKey, config.Chat, "direct", "continue", "finish"}, secret: "wizard-api-secret", confirm: true}
	var output, stderr bytes.Buffer
	// 用私有命令构造器注入向导输入，避免扩大公开 Options。
	cmd := newConnectionCommandWithPrompter(func() (*store.Store, error) { return store.New(dir) }, Options{}, p)
	isolated := &cobra.Command{Use: "cpagw"}
	isolated.AddCommand(cmd)
	isolated.SetOut(&output)
	isolated.SetErr(&stderr)
	isolated.SetArgs([]string{"connection", "add"})
	if err := isolated.Execute(); err != nil {
		t.Fatalf("向导创建失败：%v (%s)", err, stderr.String())
	}
	if text := output.String() + stderr.String(); strings.Contains(text, "wizard-api-secret") {
		t.Fatalf("向导输出泄漏 key：%q", text)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	created := state.Connections["demo"]
	if state.Secrets[created.CredentialRef] != "wizard-api-secret" {
		t.Fatal("向导没有保存连接 key")
	}
	if len(created.Models) != 2 || created.Models[0].ID != "upstream-model" || created.Models[0].Name != "Display" || created.Models[1].ID != "other-model" {
		t.Fatalf("继续录入/完成菜单生成了错误模型清单：%+v", created.Models)
	}
	wantMenus := [][]promptOption{
		{{label: "API key（默认）", value: config.AuthAPIKey}, {label: "Codex OAuth", value: config.AuthCodexOAuth}},
		{{label: "Chat Completions", value: config.Chat}, {label: "Anthropic Messages", value: config.Anthropic}, {label: "Responses", value: config.Responses}},
		{{label: "直接录入", value: "direct"}, {label: "YAML 文件", value: "yaml"}},
		{{label: "完成录入", value: "finish"}, {label: "继续添加模型", value: "continue"}},
		{{label: "完成录入", value: "finish"}, {label: "继续添加模型", value: "continue"}},
		{{label: "取消", value: "false"}, {label: "确认", value: "true"}},
	}
	if len(p.selectedMenus) != len(wantMenus) {
		t.Fatalf("菜单次数不符：got %d, want %d", len(p.selectedMenus), len(wantMenus))
	}
	for i := range wantMenus {
		if len(p.selectedMenus[i]) != len(wantMenus[i]) {
			t.Fatalf("第 %d 个菜单选项数错误：%+v", i+1, p.selectedMenus[i])
		}
		for j := range wantMenus[i] {
			if p.selectedMenus[i][j] != wantMenus[i][j] {
				t.Fatalf("第 %d 个菜单顺序错误：got %+v, want %+v", i+1, p.selectedMenus[i], wantMenus[i])
			}
		}
	}
}

func TestConnectionWizardCancellationAndInvalidInputDoNotSave(t *testing.T) {
	for _, tc := range []struct {
		name string
		p    *fakePrompter
	}{
		{name: "取消确认", p: &fakePrompter{terminal: true, lines: []string{"demo", "https://api.example.test", "upstream-model"}, selections: []string{config.AuthAPIKey, config.Chat, "direct", "finish"}, secret: "wizard-api-secret", confirm: false}},
		{name: "EOF", p: &fakePrompter{terminal: true, lines: []string{"demo"}}},
		{name: "控制字符", p: &fakePrompter{terminal: true, lines: []string{"bad\x1fname"}}},
		{name: "选择 I/O 错误", p: &fakePrompter{terminal: true, lines: []string{"demo"}, selectErr: errors.New("terminal input failure")}},
		{name: "取消读取 key", p: &fakePrompter{terminal: true, lines: []string{"demo", "https://api.example.test", "upstream-model"}, selections: []string{config.AuthAPIKey, config.Chat, "direct", "finish"}, secretErr: context.Canceled}},
		{name: "无效 key", p: &fakePrompter{terminal: true, lines: []string{"demo", "https://api.example.test", "upstream-model"}, selections: []string{config.AuthAPIKey, config.Chat, "direct", "finish"}, secret: "bad key", confirm: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.Chmod(dir, 0700); err != nil {
				t.Fatal(err)
			}
			open := func() (*store.Store, error) { return store.New(dir) }
			root := &cobra.Command{Use: "cpagw"}
			root.AddCommand(newConnectionCommandWithPrompter(open, Options{}, tc.p))
			var output, stderr bytes.Buffer
			root.SetOut(&output)
			root.SetErr(&stderr)
			root.SetArgs([]string{"connection", "add"})
			if err := root.Execute(); err == nil {
				t.Fatal("预期向导失败")
			}
			if tc.p.secret != "" && strings.Contains(output.String()+stderr.String(), tc.p.secret) {
				t.Fatal("向导错误输出泄漏输入的 key")
			}
			st, err := store.New(dir)
			if err != nil {
				t.Fatal(err)
			}
			state, err := st.Read()
			if err != nil {
				t.Fatal(err)
			}
			if len(state.Connections) != 0 {
				t.Fatalf("失败向导保存了部分状态：%v", state.Connections)
			}
		})
	}
}

func TestSanitizeCredentialErrorPreservesCancellation(t *testing.T) {
	if err := sanitizeCredentialError(context.Canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("凭证错误脱敏覆盖了取消状态：%v", err)
	}
	if err := sanitizeCredentialError(errors.New("read failed")); err == nil || !strings.Contains(err.Error(), "API key 输入无效") {
		t.Fatalf("普通凭证读取错误未脱敏：%v", err)
	}
}

func TestVersionCommand(t *testing.T) {
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

func TestFormatVersion(t *testing.T) {
	cases := []struct {
		name  string
		info  VersionInfo
		want  []string
		omits []string
	}{
		{name: "全部注入", info: VersionInfo{Version: "v1.2.3", Commit: "abc123", Date: "2026-10-05T19:40:00Z"}, want: []string{"cpagw v1.2.3", "commit: abc123", "构建时间: 2026-10-05T19:40:00Z"}},
		{name: "仅版本号时省略其余行", info: VersionInfo{Version: "v1.2.3"}, want: []string{"cpagw v1.2.3"}, omits: []string{"commit:", "构建时间:"}},
		{name: "均未注入", info: VersionInfo{}, want: []string{"cpagw unknown"}, omits: []string{"commit:", "构建时间:"}},
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
