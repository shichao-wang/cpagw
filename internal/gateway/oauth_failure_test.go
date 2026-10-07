package gateway

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

type failingOAuthTransport struct {
	*oauthTransport
	onResponse func(*http.Request) error
}

func (m *failingOAuthTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return m }
func (m *failingOAuthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	response, err := m.oauthTransport.RoundTrip(r)
	if err != nil {
		return nil, err
	}
	if err := m.onResponse(r); err != nil {
		response.Body.Close()
		return nil, err
	}
	// 故意返回成功，不遵守取消；响应侧仍须拒绝失败凭证的成功正文。
	return response, nil
}

func TestOAuthPersistenceFailureDuringRequestRejectsSuccessfulUpstreamResponse(t *testing.T) {
	st, state := oauthTestState(t)
	var activeManager atomic.Pointer[coreauth.Manager]
	transport := &failingOAuthTransport{
		oauthTransport: &oauthTransport{requests: make(chan observedOAuthRequest, 8)},
		onResponse: func(r *http.Request) error {
			if r.Header.Get("Chatgpt-Account-Id") != "account-a" {
				return nil
			}
			manager := activeManager.Load()
			var auth *coreauth.Auth
			for _, candidate := range manager.List() {
				if candidate.Metadata["account_id"] == "account-a" {
					auth = candidate
					break
				}
			}
			if auth == nil {
				return fmt.Errorf("测试 OAuth Auth 未加载")
			}
			current, err := st.Read()
			if err != nil {
				return err
			}
			credential := current.OAuthCredentials["oauth-a"]
			credential.AccessToken = "newer-access-a"
			credential.RefreshToken = "newer-refresh-a"
			if _, err := st.SaveOAuth(store.OAuthBinding{Connection: "a", ConnectionID: "connection-a", CredentialRef: "oauth-a", Generation: 1}, credential); err != nil {
				return err
			}
			auth.Metadata["access_token"] = "stale-rotation-access"
			auth.Metadata["refresh_token"] = "stale-rotation-refresh"
			_, err = manager.Update(context.Background(), auth)
			return err
		},
	}
	activeManager.Store(startManagedTest(t, st, runtimeOptions{transport: transport}))
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	response := postMessages(t, client, state.Listen, "client-secret-a", "claude-opus-test")
	body, err := io.ReadAll(response.Body)
	response.Body.Close()
	if err != nil || response.StatusCode != http.StatusServiceUnavailable || strings.Contains(string(body), "mock reply") {
		t.Fatal("凭证保存失败后仍交付了成功上游响应")
	}
	if got := <-transport.requests; got.account != "account-a" {
		t.Fatal("未在目标账号请求中模拟保存失败")
	}
	response = postMessages(t, client, state.Listen, "client-secret-b", "claude-opus-test")
	response.Body.Close()
	if response.StatusCode != http.StatusOK {
		t.Fatal("其他账号受保存失败影响")
	}
	if got := <-transport.requests; got.account != "account-b" {
		t.Fatal("其他账号请求没有保持隔离")
	}
}

func TestOAuthCASFailureRevokesOnlyTargetAndReloadCannotRestore(t *testing.T) {
	st, state := oauthTestState(t)
	transport := &oauthTransport{requests: make(chan observedOAuthRequest, 8)}
	manager := startManagedTest(t, st, runtimeOptions{transport: transport})
	var auth *coreauth.Auth
	for _, candidate := range manager.List() {
		if candidate.Metadata["account_id"] == "account-a" {
			auth = candidate
			break
		}
	}
	if auth == nil {
		t.Fatal("OAuth Auth 未加载")
	}
	before, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	credential := before.OAuthCredentials["oauth-a"]
	credential.AccessToken = "newer-access-a"
	credential.RefreshToken = "newer-refresh-a"
	if _, err := st.SaveOAuth(store.OAuthBinding{Connection: "a", ConnectionID: "connection-a", CredentialRef: "oauth-a", Generation: 1}, credential); err != nil {
		t.Fatal(err)
	}
	auth.Metadata["access_token"] = "stale-rotation-access"
	auth.Metadata["refresh_token"] = "stale-rotation-refresh"
	// SDK 吞掉 Save 错误，适配层必须在 Update 返回前移除该 Auth。
	if _, err := manager.Update(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	if _, exists := manager.GetByID(auth.ID); exists {
		t.Fatal("CAS 失败后 Auth 仍可被选择")
	}
	if len(cliproxy.GlobalModelRegistry().GetModelsForClient(auth.ID)) != 0 {
		t.Fatal("失败 Auth 留下模型注册")
	}
	after, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.OAuthCredentials["oauth-a"].AccessToken != "newer-access-a" {
		t.Fatal("过期刷新结果覆盖了新凭证或增加结构 revision")
	}
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for i := 0; i < 2; i++ {
		request, err := http.NewRequest(http.MethodGet, "http://"+state.Listen+"/v1/models", nil)
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("X-Api-Key", "client-secret-a")
		catalog := mustDo(t, client, request)
		var payload struct {
			Data []json.RawMessage `json:"data"`
		}
		decodeErr := json.NewDecoder(catalog.Body).Decode(&payload)
		catalog.Body.Close()
		if catalog.StatusCode != http.StatusOK || decodeErr != nil || len(payload.Data) != 0 {
			t.Fatal("失败 OAuth 连接仍出现在模型目录中")
		}
		response := postMessages(t, client, state.Listen, "client-secret-a", "claude-opus-test")
		response.Body.Close()
		if response.StatusCode != http.StatusServiceUnavailable {
			t.Fatal("不健康凭证未被 gate 拒绝")
		}
		response = postMessages(t, client, state.Listen, "client-secret-b", "claude-opus-test")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal("其他账号受失败影响")
		}
		if got := <-transport.requests; got.account != "account-b" {
			t.Fatal("失败后回退到其他账号")
		}
		if i == 0 {
			if err := st.Update(func(s *config.State) error {
				p := s.Profiles["a"]
				b := p.Models["opus"]
				b.Label = "changed label"
				p.Models["opus"] = b
				s.Profiles["a"] = p
				return nil
			}); err != nil {
				t.Fatal(err)
			}
			waitRevision(t, st)
		}
	}
}
