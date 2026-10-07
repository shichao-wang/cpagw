package connection

import (
	"context"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
)

func TestAuthSwitchRequiresStoppedRuntimeAndDoesNotInheritCredentials(t *testing.T) {
	st := testStore(t)
	api := config.Connection{Name: "c", AuthType: config.AuthAPIKey, Protocol: config.Responses, BaseURL: "https://api.example.test", Models: []config.Model{{ID: "m"}}}
	if err := Create(context.Background(), st, api, "api-key"); err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	oldRef := state.Connections["c"].CredentialRef

	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	apiType := config.AuthAPIKey
	newKey := "rotated-key"
	if err := Patch(st, "c", nil, nil, &newKey, &apiType); err != nil {
		t.Fatalf("API-key 更新应可热应用：%v", err)
	}
	oauthType := config.AuthCodexOAuth
	officialURL := config.CodexBaseURL
	if err := Patch(st, "c", &officialURL, nil, nil, &oauthType); err == nil {
		t.Fatal("运行中的 gateway 必须拒绝 OAuth 认证切换")
	}
	_ = lock.Close()

	if err := Patch(st, "c", &officialURL, nil, nil, &oauthType); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c := state.Connections["c"]
	if c.AuthType != config.AuthCodexOAuth || c.CredentialRef != "" {
		t.Fatalf("切换 OAuth 不应继承 API key：%+v", c)
	}
	if _, exists := state.Secrets[oldRef]; exists {
		t.Fatal("认证切换后未引用的旧 API key 应回收")
	}
	apiType = config.AuthAPIKey
	if err := Patch(st, "c", nil, nil, nil, &apiType); err == nil {
		t.Fatal("切回 API key 必须显式提供 key")
	}
	newKey = "explicit-key"
	if err := Patch(st, "c", nil, nil, &newKey, &apiType); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	c = state.Connections["c"]
	if key, err := state.Key(c); err != nil || key != newKey {
		t.Fatalf("切回 API key 未保存显式凭证：%v", err)
	}
}
