package config

import "testing"

func TestURLs(t *testing.T) {
	for _, raw := range []string{"https://api.deepseek.com", "https://api.deepseek.com/anthropic", "http://127.0.0.1:1234", "http://[::1]:8080"} {
		if err := ValidateURL(raw); err != nil {
			t.Errorf("%s: %v", raw, err)
		}
	}
	for _, raw := range []string{"http://example.com", "file:///tmp/x", "https://user:secret@example.com", "https://example.com?secret=x", "https://example.com/v1/messages", "", "https://example.com/#x"} {
		if ValidateURL(raw) == nil {
			t.Errorf("地址应被拒绝：%s", raw)
		}
	}
}

func TestKeyRequiresConnectionCredential(t *testing.T) {
	s := NewState()
	s.Secrets["secret"] = "valid-key"
	s.Secrets[""] = "must-not-be-used"
	c := Connection{Name: "model", CredentialRef: "secret"}
	if got, err := s.Key(c); err != nil || got != "valid-key" {
		t.Fatalf("连接 key 解析失败：%s %v", got, err)
	}
	for _, ref := range []string{"missing", "", " \t"} {
		c.CredentialRef = ref
		if _, err := s.Key(c); err == nil {
			t.Fatalf("凭证引用 %q 不应回退", ref)
		}
	}
}

func TestProfileValidation(t *testing.T) {
	s := NewState()
	s.Secrets["k"] = "valid-key"
	s.Connections["c"] = Connection{Name: "c", Protocol: Anthropic, CredentialRef: "k", Models: []Model{{ID: "upstream"}}}
	p := Profile{Agent: "claude-code", Models: map[string]Binding{}}
	for _, slot := range Slots {
		p.Models[slot] = Binding{PublicModel: "claude-" + slot + "-test", Connection: "c", TargetModel: "upstream"}
	}
	if err := s.ValidateProfile(p); err != nil {
		t.Fatal(err)
	}
	b := p.Models["sonnet"]
	b.PublicModel = p.Models["opus"].PublicModel
	p.Models["sonnet"] = b
	if s.ValidateProfile(p) == nil {
		t.Fatal("重复公开 ID 不应通过")
	}
}

func TestModelAndBindingTextRejectControlCharacters(t *testing.T) {
	for _, models := range [][]Model{
		{{ID: "model\nsecret"}},
		{{ID: "model", Name: "line\tbreak"}},
	} {
		if ValidateModels(models) == nil {
			t.Fatalf("应拒绝包含控制字符的模型清单：%q", models)
		}
	}
	s := NewState()
	s.Secrets["k"] = "valid-key"
	s.Connections["c"] = Connection{Name: "c", Protocol: Chat, CredentialRef: "k", Models: []Model{{ID: "upstream"}}}
	p := Profile{Agent: "claude-code", Models: map[string]Binding{}}
	for _, slot := range Slots {
		p.Models[slot] = Binding{PublicModel: "claude-" + slot, Connection: "c", TargetModel: "upstream"}
	}
	b := p.Models["opus"]
	b.Label = "bad\nlabel"
	p.Models["opus"] = b
	if s.ValidateProfile(p) == nil {
		t.Fatal("应拒绝包含控制字符的展示文本")
	}
}
