package connection

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
		"- id: \"model\\nunsafe\"\n",
		"- id: model\n  name: \"unsafe\\tname\"\n",
	} {
		if _, err := ParseModels(strings.NewReader(input)); err == nil {
			t.Errorf("应拒绝模型清单 %q", input)
		}
	}
}

func TestCreateKeyRotationAndSharedCredentialGC(t *testing.T) {
	st := testStore(t)
	if err := Create(st, testConnection("default", "https://api.example.test"), "first-secret"); err != nil {
		t.Fatal(err)
	}
	before, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	first := before.Connections["default"]
	if key, err := before.Key(first); err != nil || key != "first-secret" {
		t.Fatalf("连接独立 key 未保存：%v", err)
	}
	if first.CredentialRef == "" {
		t.Fatal("连接缺少独立 credentialRef")
	}
	if err := Create(st, testConnection("default", "https://other.example.test"), "duplicate-secret"); err == nil {
		t.Fatal("全局重名连接应拒绝")
	}
	if err := st.Update(func(s *config.State) error {
		other := testConnection("other", "https://other.example.test")
		other.CredentialRef = first.CredentialRef
		s.Connections[other.Name] = other
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	newKey := "rotated-secret"
	if err := Patch(st, "default", nil, nil, &newKey); err != nil {
		t.Fatal(err)
	}
	after, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	rotated := after.Connections["default"]
	if key, err := after.Key(rotated); err != nil || key != newKey {
		t.Fatalf("连接 key 轮换失败：%v", err)
	}
	if _, ok := after.Secrets[first.CredentialRef]; !ok {
		t.Fatal("其他连接仍引用旧 key 时不得回收 secret")
	}
	if err := Remove(st, "other"); err != nil {
		t.Fatal(err)
	}
	after, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := after.Secrets[first.CredentialRef]; ok {
		t.Fatal("最后一个连接移除后旧 secret 应回收")
	}
}

func TestPatchIsAtomicAndGuardsReferencedModels(t *testing.T) {
	st := testStore(t)
	if err := Create(st, testConnection("c", "https://api.example.test"), "secret"); err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *config.State) error {
		s.Profiles["demo"] = config.Profile{Models: map[string]config.Binding{"sonnet": {Connection: "c", TargetModel: "model-a"}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	newBase := "https://other.example.test"
	newModels := []config.Model{{ID: "model-b"}}
	newKey := "new-secret"
	if err := Patch(st, "c", &newBase, &newModels, &newKey); err == nil {
		t.Fatal("仍被 profile 引用的模型不得移除")
	}
	after, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	beforeConnection, afterConnection := before.Connections["c"], after.Connections["c"]
	if after.Revision != before.Revision || afterConnection.BaseURL != beforeConnection.BaseURL || afterConnection.CredentialRef != beforeConnection.CredentialRef || len(afterConnection.Models) != len(beforeConnection.Models) || afterConnection.Models[0] != beforeConnection.Models[0] {
		t.Fatal("失败 patch 未保持事务原子性")
	}
	if _, ok := after.Secrets["new-secret"]; ok {
		t.Fatal("失败 patch 不得残留新凭证")
	}
	if refs := after.References("c", "model-a"); len(refs) != 1 || refs[0] != "demo" {
		t.Fatalf("引用检查结果错误：%v", refs)
	}
	if err := Remove(st, "c"); err == nil {
		t.Fatal("仍被 profile 引用的连接应拒绝删除")
	}
}

func TestCheckUsesBoundedModelDirectoryGET(t *testing.T) {
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

func TestCheckDoesNotFollowRedirectOrPrintSensitiveData(t *testing.T) {
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
		t.Fatalf("错误信息泄漏敏感内容：%v", err)
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
		Name: name, Protocol: config.Chat, BaseURL: baseURL,
		Models: []config.Model{{ID: "model-a"}, {ID: "model-b"}},
	}
}
