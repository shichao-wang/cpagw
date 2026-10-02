package provider

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

func TestParseModelsStrictAndUnique(t *testing.T) {
	valid, err := ParseModels(strings.NewReader("- id: model-a\n  name: Alpha\n- id: model-b\n"))
	if err != nil || len(valid) != 2 {
		t.Fatalf("有效模型清单未解析：%v", err)
	}
	for _, input := range []string{
		"- id: model-a\n- id: model-a\n",
		"- id: '   '\n",
		"- id: model-a\n  unexpected: value\n",
		"- id: [broken\n",
		"- id: first\n---\n- id: second\n",
	} {
		if _, err := ParseModels(strings.NewReader(input)); err == nil {
			t.Errorf("应拒绝模型清单 %q", input)
		}
	}
}

func TestCreateInheritanceRotationAndReferences(t *testing.T) {
	st := testStore(t)
	conn := testConnection("default", "https://api.example.test")
	if err := Create(st, "provider", conn, "default-secret"); err != nil {
		t.Fatal(err)
	}
	if err := AddConnection(st, "provider", testConnection("override", "https://other.example.test"), "override-secret"); err != nil {
		t.Fatal(err)
	}

	before, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	p := before.Providers["provider"]
	inherited := p.Connections["default"]
	overridden := p.Connections["override"]
	if inherited.CredentialRef != "" || overridden.CredentialRef == "" {
		t.Fatal("默认连接应继承，override 连接应有独立凭证")
	}
	if key, _ := before.Key("provider", inherited); key != "default-secret" {
		t.Fatal("默认连接未继承默认 key")
	}
	if key, _ := before.Key("provider", overridden); key != "override-secret" {
		t.Fatal("连接专属 key 未生效")
	}

	if err := RotateDefaultKey(st, "provider", "rotated-secret"); err != nil {
		t.Fatal(err)
	}
	after, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	p = after.Providers["provider"]
	if key, _ := after.Key("provider", p.Connections["default"]); key != "rotated-secret" {
		t.Fatal("默认 key 轮换未影响继承连接")
	}
	if key, _ := after.Key("provider", p.Connections["override"]); key != "override-secret" {
		t.Fatal("默认 key 轮换覆盖了连接专属 key")
	}
	if err := RemoveConnection(st, "provider", "default"); err != nil {
		t.Fatalf("未引用的连接应可删除：%v", err)
	}
	if err := AddConnection(st, "provider", testConnection("default", "https://api.example.test"), ""); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *config.State) error {
		s.Profiles["demo"] = config.Profile{Models: map[string]config.Binding{
			"sonnet": {Provider: "provider", Connection: "default", TargetModel: "model-a"},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := RemoveConnection(st, "provider", "default"); err == nil {
		t.Fatal("仍被 profile 引用的连接应拒绝删除")
	}
	if err := Remove(st, "provider"); err == nil {
		t.Fatal("仍被 profile 引用的提供商应拒绝删除")
	}
}

func TestUpdateConnectionRejectsRemovingReferencedModel(t *testing.T) {
	st := testStore(t)
	if err := Create(st, "p", testConnection("c", "https://api.example.test"), "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *config.State) error {
		s.Profiles["demo"] = config.Profile{Models: map[string]config.Binding{
			"sonnet": {Provider: "p", Connection: "c", TargetModel: "model-a"},
		}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	models := []config.Model{{ID: "model-b"}}
	if err := UpdateConnection(st, "p", "c", nil, &models); err == nil {
		t.Fatal("不能移除仍被 profile 使用的上游模型")
	}
	models = []config.Model{{ID: "model-a"}, {ID: "model-b"}}
	if err := UpdateConnection(st, "p", "c", nil, &models); err != nil {
		t.Fatalf("新增模型且保留引用模型应成功：%v", err)
	}
}

func TestCheckUsesOnlyBoundedModelDirectoryGET(t *testing.T) {
	var called bool
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		called = true
		if r.Method != http.MethodGet || r.URL.Path != "/v1/models" {
			t.Errorf("意外请求：%s %s", r.Method, r.URL.Path)
		}
		if r.Header.Get("x-api-key") != "secret-value" || r.Header.Get("anthropic-version") == "" {
			t.Error("Anthropic 目录请求头缺失")
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"data":[]}`)
	}))
	defer server.Close()
	conn := testConnection("c", server.URL+"/v1")
	conn.Protocol = config.Anthropic
	if err := Check(context.Background(), conn, "secret-value"); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("模型目录 endpoint 未被请求")
	}
}

func TestCheckDoesNotFollowRedirectOrPrintBody(t *testing.T) {
	var redirected bool
	destination := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		redirected = true
		w.WriteHeader(http.StatusOK)
	}))
	defer destination.Close()
	bodySecret := "response-body-secret"
	source := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(http.StatusFound)
		_, _ = io.WriteString(w, bodySecret)
	}))
	defer source.Close()
	conn := testConnection("c", source.URL)
	conn.Protocol = config.Chat
	err := Check(context.Background(), conn, "api-secret")
	if err == nil || !strings.Contains(err.Error(), "302") {
		t.Fatalf("预期安全的非成功状态错误，实际为 %v", err)
	}
	if strings.Contains(err.Error(), bodySecret) || strings.Contains(err.Error(), "api-secret") || strings.Contains(err.Error(), source.URL) {
		t.Fatalf("错误信息泄漏了敏感内容：%v", err)
	}
	if redirected {
		t.Fatal("目录请求不应跟随重定向")
	}
}

func testStore(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	return st
}

func testConnection(name, baseURL string) config.Connection {
	return config.Connection{
		Name:     name,
		Protocol: config.Chat,
		BaseURL:  baseURL,
		Models:   []config.Model{{ID: "model-a"}},
	}
}
