package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/profile"
	"github.com/shichao-wang/cpa-tui/internal/provider"
	"github.com/shichao-wang/cpa-tui/internal/store"
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
