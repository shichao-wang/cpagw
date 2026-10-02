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
func TestCredentialInheritance(t *testing.T) {
	s := NewState()
	s.Secrets["default"] = "a"
	s.Secrets["override"] = "b"
	s.Providers["deepseek"] = Provider{Name: "deepseek", DefaultCredentialRef: "default"}
	c := Connection{Name: "chat"}
	if got, err := s.Key("deepseek", c); err != nil || got != "a" {
		t.Fatalf("默认继承失败：%s %v", got, err)
	}
	c.CredentialRef = "override"
	if got, err := s.Key("deepseek", c); err != nil || got != "b" {
		t.Fatalf("覆盖失败：%s %v", got, err)
	}
	s.Secrets["default"] = "rotated"
	if got, _ := s.Key("deepseek", c); got != "b" {
		t.Fatal("默认轮换覆盖了独立 key")
	}
	c.CredentialRef = "missing"
	if _, err := s.Key("deepseek", c); err == nil {
		t.Fatal("缺失的覆盖 key 不应回退到默认")
	}
}
func TestProfileValidation(t *testing.T) {
	s := NewState()
	s.Secrets["k"] = "key"
	s.Providers["p"] = Provider{Name: "p", DefaultCredentialRef: "k", Connections: map[string]Connection{"c": {Name: "c", Protocol: Anthropic, Models: []Model{{ID: "upstream"}}}}}
	p := Profile{Agent: "claude-code", Models: map[string]Binding{}}
	for _, slot := range Slots {
		p.Models[slot] = Binding{PublicModel: "claude-" + slot + "-test", Provider: "p", Connection: "c", TargetModel: "upstream"}
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
