package profile

import (
	"os"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

func TestParseFileRejectsUnknownFieldsAndExtraDocuments(t *testing.T) {
	for _, input := range []string{
		"name: demo\nkeyRef: secret-ref\n",
		"name: demo\nmodels: {}\nunknown: value\n",
		"name: demo\n---\nname: second\n",
		"name: demo\nmodels:\n  sonnet:\n    public_model: claude-test\n    provider: p\n    connection: c\n    target_model: m\n",
		"name: demo\nmodels:\n  sonnet:\n    public_model: claude-test\n    connection: c\n    target_model: m\n    credentialRef: should-not-be-here\n",
	} {
		if _, err := ParseFile(strings.NewReader(input)); err == nil {
			t.Errorf("应拒绝未知字段或多文档 YAML：%q", input)
		}
	}
}

func TestCreateGeneratesPrivateKeyAndUpdatePreservesUnspecifiedFields(t *testing.T) {
	st := testStore(t)
	if err := seedConnection(st); err != nil {
		t.Fatal(err)
	}
	file := completeFile("demo")
	key, err := Create(st, file)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(key, "cpagw_") {
		t.Fatalf("生成 key 前缀错误：%q", key)
	}
	s, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	p := s.Profiles["demo"]
	if s.Secrets[p.KeyRef] != key {
		t.Fatal("本地 key 未被保存到生成的引用中")
	}
	id, keyRef := p.ID, p.KeyRef
	oldOpus := p.Models["opus"]
	newSonnet := config.Binding{
		PublicModel: "claude-updated-sonnet",
		Connection:  "default",
		TargetModel: "model-a",
		Label:       "Updated sonnet",
	}
	update := File{Models: map[string]config.Binding{"sonnet": newSonnet}}
	if err := Update(st, "demo", update); err != nil {
		t.Fatal(err)
	}
	s, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	updated := s.Profiles["demo"]
	if updated.ID != id || updated.KeyRef != keyRef || s.Secrets[keyRef] != key {
		t.Fatal("更新 profile 时 ID 或本地 key 被重置")
	}
	if updated.Models["opus"] != oldOpus || updated.Models["sonnet"] != newSonnet {
		t.Fatal("更新 profile 未保留未指定档位或未应用指定档位")
	}
}

func TestCreateValidatesAllSlotsAndReferences(t *testing.T) {
	st := testStore(t)
	if err := seedConnection(st); err != nil {
		t.Fatal(err)
	}
	file := completeFile("demo")
	delete(file.Models, "haiku")
	if _, err := Create(st, file); err == nil {
		t.Fatal("缺少 haiku 档位的 profile 不应创建")
	}
	file = completeFile("demo")
	b := file.Models["sonnet"]
	b.TargetModel = "not-registered"
	file.Models["sonnet"] = b
	if _, err := Create(st, file); err == nil {
		t.Fatal("引用未登记模型的 profile 不应创建")
	}
	file = completeFile("demo")
	b = file.Models["sonnet"]
	b.PublicModel = file.Models["opus"].PublicModel
	file.Models["sonnet"] = b
	if _, err := Create(st, file); err == nil {
		t.Fatal("重复 public model ID 不应创建")
	}
}

func TestRemoveKeepsSharedKeyReference(t *testing.T) {
	st := testStore(t)
	if err := seedConnection(st); err != nil {
		t.Fatal(err)
	}
	key, err := Create(st, completeFile("demo"))
	if err != nil {
		t.Fatal(err)
	}
	s, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	first := s.Profiles["demo"]
	second := first
	second.Name = "other"
	if err := st.Update(func(state *config.State) error { state.Profiles["other"] = second; return nil }); err != nil {
		t.Fatal(err)
	}
	wasChecked := false
	if err := Remove(st, "demo", func(id string) (bool, error) {
		wasChecked = true
		if id != first.ID {
			t.Errorf("agent 引用检查拿到意外 ID：%s", id)
		}
		return true, nil
	}); err == nil {
		t.Fatal("被 agent 引用的 profile 不应删除")
	}
	if !wasChecked {
		t.Fatal("未执行 agent 引用检查")
	}
	if err := Remove(st, "demo", func(string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	s, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Secrets[first.KeyRef]; !ok || s.Secrets[first.KeyRef] != key {
		t.Fatal("共享 profile key 引用不应被提前回收")
	}
	if err := Remove(st, "other", func(string) (bool, error) { return false, nil }); err != nil {
		t.Fatal(err)
	}
	s, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := s.Secrets[first.KeyRef]; ok {
		t.Fatal("最后一个 profile 移除后 key 应回收")
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

func seedConnection(st *store.Store) error {
	return st.Update(func(s *config.State) error {
		s.Secrets["test-key"] = "upstream-key"
		s.Connections["default"] = config.Connection{
			ID: "profile-test-connection", AuthType: config.AuthAPIKey, Name: "default", Protocol: config.Chat,
			BaseURL: "https://api.example.test", CredentialRef: "test-key",
			Models: []config.Model{{ID: "model-a"}, {ID: "model-b"}},
		}
		return nil
	})
}

func completeFile(name string) File {
	return File{
		Name:  stringPointer(name),
		Agent: stringPointer("claude-code"),
		Models: map[string]config.Binding{
			"opus":   {PublicModel: "claude-example-opus", Connection: "default", TargetModel: "model-a"},
			"sonnet": {PublicModel: "claude-example-sonnet", Connection: "default", TargetModel: "model-a"},
			"haiku":  {PublicModel: "claude-example-haiku", Connection: "default", TargetModel: "model-b"},
		},
	}
}

func stringPointer(value string) *string { return &value }
