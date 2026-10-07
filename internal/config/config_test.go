package config

import (
	"testing"
	"time"
)

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
	c := Connection{ID: "id", Name: "model", AuthType: AuthAPIKey, CredentialRef: "secret"}
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

func TestOAuthAuthIsExplicitAndAllowsUnloggedProfileStructure(t *testing.T) {
	s := NewState()
	s.Secrets["secret"] = "provider-key"
	c := Connection{ID: "oauth-id", Name: "oauth", AuthType: AuthCodexOAuth, Protocol: Responses, BaseURL: CodexBaseURL, Models: []Model{{ID: "m"}}}
	s.Connections[c.Name] = c
	if _, err := s.Key(c); err == nil {
		t.Fatal("OAuth 不得被解释为 API key")
	}
	if err := s.Validate(); err != nil {
		t.Fatalf("未登录 OAuth 连接应是合法状态：%v", err)
	}
	p := Profile{Agent: "claude-code", Models: map[string]Binding{}}
	for _, slot := range Slots {
		p.Models[slot] = Binding{PublicModel: "claude-" + slot + "-test", Connection: "oauth", TargetModel: "m"}
	}
	if err := s.ValidateProfileStructure(p); err != nil {
		t.Fatalf("合法但未登录的 OAuth profile 结构应通过：%v", err)
	}
	if err := s.ValidateProfile(p); err == nil {
		t.Fatal("认证就绪校验必须要求 OAuth 已登录")
	}
	c.AuthType = ""
	if ValidateAuthConnection(c) == nil {
		t.Fatal("空 AuthType 必须拒绝")
	}
	c.AuthType = AuthCodexOAuth
	c.BaseURL = "https://other.example.test"
	if ValidateAuthConnection(c) == nil {
		t.Fatal("OAuth 必须使用官方 endpoint")
	}
}

func TestProfileValidation(t *testing.T) {
	s := NewState()
	s.Secrets["k"] = "valid-key"
	s.Connections["c"] = Connection{ID: "connection-id", Name: "c", AuthType: AuthAPIKey, Protocol: Anthropic, BaseURL: "https://api.example.test", CredentialRef: "k", Models: []Model{{ID: "upstream"}}}
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
	s.Connections["c"] = Connection{ID: "id", Name: "c", AuthType: AuthAPIKey, Protocol: Chat, BaseURL: "https://api.example.test", CredentialRef: "k", Models: []Model{{ID: "upstream"}}}
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

func TestUnreferencedAPIConnectionMayLackKeyButProfileFailsClosed(t *testing.T) {
	s := NewState()
	s.Connections["idle"] = Connection{ID: "idle-id", Name: "idle", AuthType: AuthAPIKey, Protocol: Chat, BaseURL: "https://api.example.test", Models: []Model{{ID: "m"}}}
	if err := s.Validate(); err != nil {
		t.Fatalf("未被引用且缺少 API key 的连接可保留：%v", err)
	}
	p := Profile{Name: "demo", ID: "profile-id", Agent: "claude-code", KeyRef: "profile-key", Models: map[string]Binding{}}
	s.Secrets[p.KeyRef] = "valid-profile-key"
	for _, slot := range Slots {
		p.Models[slot] = Binding{PublicModel: "claude-" + slot, Connection: "idle", TargetModel: "m"}
	}
	s.Profiles[p.Name] = p
	if err := s.Validate(); err == nil {
		t.Fatal("被 profile 引用的 API key 连接缺少凭证时必须 fail-closed")
	}
}

func TestValidateRejectsDuplicateConnectionIDsAndUnboundOAuth(t *testing.T) {
	s := NewState()
	s.Secrets["key"] = "valid-key"
	s.Connections["one"] = Connection{ID: "same", Name: "one", AuthType: AuthAPIKey, Protocol: Chat, BaseURL: "https://api.example.test", CredentialRef: "key", Models: []Model{{ID: "m"}}}
	s.Connections["two"] = Connection{ID: "same", Name: "two", AuthType: AuthAPIKey, Protocol: Chat, BaseURL: "https://api.example.test", CredentialRef: "key", Models: []Model{{ID: "m"}}}
	if err := s.Validate(); err == nil {
		t.Fatal("连接 ID 必须唯一")
	}
	delete(s.Connections, "two")
	s.OAuthCredentials["orphan"] = OAuthCredential{AccessToken: "a", RefreshToken: "r", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour), Generation: 1}
	if err := s.Validate(); err == nil {
		t.Fatal("未绑定 OAuth 凭证必须拒绝")
	}
}
