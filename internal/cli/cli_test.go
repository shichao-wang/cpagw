package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

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
		Name:     "default",
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
