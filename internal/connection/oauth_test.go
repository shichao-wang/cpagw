package connection

import (
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
)

func TestOAuthLoginLogoutAndAPIKeyIsolation(t *testing.T) {
	st := testStore(t)
	if err := Create(t.Context(), st, config.Connection{Name: "oauth", AuthType: config.AuthCodexOAuth, Protocol: config.Responses, BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "gpt-5-codex"}}}, ""); err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := state.Connections["oauth"]
	if c.ID == "" || c.CredentialRef != "" {
		t.Fatalf("OAuth connection not initialized: %+v", c)
	}
	if _, err := state.Key(c); err == nil {
		t.Fatal("OAuth connection must not be treated as an API key")
	}
	session, err := BeginOAuth(st, "oauth")
	if err != nil {
		t.Fatal(err)
	}
	credential := config.OAuthCredential{AccessToken: "access-token", RefreshToken: "refresh-token", AccountID: "account-id", ExpiresAt: time.Now().Add(time.Hour)}
	if err := session.Commit(credential); err != nil {
		_ = session.Close()
		t.Fatal(err)
	}
	if err := session.Close(); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c = state.Connections["oauth"]
	if state.OAuthCredentials[c.CredentialRef].AccessToken != "access-token" {
		t.Fatal("OAuth credential not committed")
	}
	if err := LogoutOAuth(st, "oauth"); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c = state.Connections["oauth"]
	if c.CredentialRef != "" || len(state.OAuthCredentials) != 0 {
		t.Fatal("logout must remove only its credential")
	}
}

func TestOAuthMutationRequiresRuntimeLock(t *testing.T) {
	st := testStore(t)
	if err := Create(t.Context(), st, config.Connection{Name: "oauth", AuthType: config.AuthCodexOAuth, Protocol: config.Responses, BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "m"}}}, ""); err != nil {
		t.Fatal(err)
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := BeginOAuth(st, "oauth"); err == nil {
		t.Fatal("login should fail while runtime lock is held")
	}
	if err := LogoutOAuth(st, "oauth"); err == nil {
		t.Fatal("logout should fail while runtime lock is held")
	}
	if err := Remove(st, "oauth"); err == nil {
		t.Fatal("OAuth delete should fail while runtime lock is held")
	}
	lock.Close()
}
