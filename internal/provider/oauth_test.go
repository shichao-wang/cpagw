package provider

import (
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
)

func TestOAuthLoginLogoutAndAPIKeyIsolation(t *testing.T) {
	st := testStore(t)
	if err := CreateProvider(st, "p", "provider-default-key"); err != nil {
		t.Fatal(err)
	}
	conn := config.Connection{Name: "oauth", AuthType: config.AuthCodexOAuth, Protocol: config.Responses, BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "gpt-5-codex"}}}
	if err := AddConnection(st, "p", conn, ""); err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := state.Providers["p"].Connections["oauth"]
	if c.ID == "" || c.CredentialRef != "" {
		t.Fatalf("OAuth connection not initialized: %+v", c)
	}
	if _, err := state.Key("p", c); err == nil {
		t.Fatal("OAuth connection must not inherit provider API key")
	}
	if state.ValidateProfileStructure(config.Profile{Agent: "claude-code", Models: map[string]config.Binding{}}) == nil {
		t.Fatal("malformed profile unexpectedly accepted")
	}
	session, err := BeginOAuth(st, "p", "oauth")
	if err != nil {
		t.Fatal(err)
	}
	credential := config.OAuthCredential{AccessToken: "access-token", RefreshToken: "refresh-token", AccountID: "account-id", ExpiresAt: time.Now().Add(time.Hour)}
	if err := session.Commit(credential); err != nil {
		session.Close()
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c = state.Providers["p"].Connections["oauth"]
	if state.OAuthCredentials[c.CredentialRef].AccessToken != "access-token" {
		t.Fatal("OAuth credential not committed")
	}
	originalRef := c.CredentialRef
	authType := config.AuthCodexOAuth
	if err := PatchConnectionWithAuth(st, "p", "oauth", nil, nil, nil, false, &authType); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Providers["p"].Connections["oauth"].CredentialRef != originalRef {
		t.Fatal("显式重选相同认证方式不得清除登录")
	}
	if err := LogoutOAuth(st, "p", "oauth"); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c = state.Providers["p"].Connections["oauth"]
	if c.CredentialRef != "" || len(state.OAuthCredentials) != 0 {
		t.Fatal("logout must remove only its credential")
	}
}

func TestAPIKeyChangesRemainHotWhileRuntimeLockHeld(t *testing.T) {
	st := testStore(t)
	if err := Create(st, "p", testConnection("c", "https://api.example.test"), "secret"); err != nil {
		t.Fatal(err)
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	authType := config.AuthAPIKey
	if err := PatchConnectionWithAuth(st, "p", "c", nil, nil, nil, false, &authType); err != nil {
		t.Fatalf("同认证类型的 API key patch 应支持热更新：%v", err)
	}
	if err := RemoveConnection(st, "p", "c"); err != nil {
		t.Fatalf("API key 连接删除不应要求网关停止：%v", err)
	}
	if err := Remove(st, "p"); err != nil {
		t.Fatalf("仅 API key 的 provider 删除不应要求网关停止：%v", err)
	}
}

func TestOAuthAuthTransitionRequiresRuntimeLock(t *testing.T) {
	st := testStore(t)
	if err := Create(st, "p", testConnection("c", "https://api.example.test"), "secret"); err != nil {
		t.Fatal(err)
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	authType := config.AuthCodexOAuth
	if err := PatchConnectionWithAuth(st, "p", "c", nil, nil, nil, false, &authType); err == nil {
		t.Fatal("API key→OAuth 切换必须拒绝运行中的网关")
	}
}

func TestOAuthMutationRequiresRuntimeLock(t *testing.T) {
	st := testStore(t)
	if err := CreateProvider(st, "p", ""); err != nil {
		t.Fatal(err)
	}
	if err := AddConnection(st, "p", config.Connection{Name: "oauth", AuthType: config.AuthCodexOAuth, Protocol: config.Responses, BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "m"}}}, ""); err != nil {
		t.Fatal(err)
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginOAuth(st, "p", "oauth"); err == nil {
		t.Fatal("login should fail while gateway lock is held")
	}
	if err := LogoutOAuth(st, "p", "oauth"); err == nil {
		t.Fatal("logout should fail while gateway lock is held")
	}
	if err := RemoveConnection(st, "p", "oauth"); err == nil {
		t.Fatal("OAuth delete should fail while gateway lock is held")
	}
	lock.Close()
}
