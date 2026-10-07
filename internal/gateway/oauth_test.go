package gateway

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/connection"
	"github.com/shichao-wang/cpagw/internal/store"
)

type observedOAuthRequest struct{ key, account, host, model string }
type oauthTransport struct{ requests chan observedOAuthRequest }

func (m *oauthTransport) RoundTripperFor(*coreauth.Auth) http.RoundTripper { return m }
func (m *oauthTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	var body struct {
		Model string `json:"model"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		return nil, err
	}
	m.requests <- observedOAuthRequest{r.Header.Get("Authorization"), r.Header.Get("Chatgpt-Account-Id"), r.URL.Host, body.Model}
	payload := `{"type":"response.completed","response":{"id":"resp_mock","object":"response","created_at":1,"status":"completed","model":"gpt-5.5","output":[{"id":"msg_mock","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"mock reply","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}}`
	return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader("event: response.completed\ndata: " + payload + "\n\n")), Request: r}, nil
}

func oauthTestState(t *testing.T) (*store.Store, *config.State) {
	t.Helper()
	st, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	listen, err := reserveLoopbackAddress(t)
	if err != nil {
		t.Fatal(err)
	}
	state := config.NewState()
	state.Listen = listen
	for _, name := range []string{"a", "b", "api", "unlogged"} {
		c := config.Connection{ID: "connection-" + name, Name: name, AuthType: config.AuthCodexOAuth, Protocol: config.Responses, BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "gpt-5.5"}, {ID: "unavailable-model"}}}
		if name == "api" {
			c.AuthType = config.AuthAPIKey
			c.BaseURL = "https://mock.example.test"
			c.CredentialRef = "api-key"
			state.Secrets[c.CredentialRef] = "api-key-test"
		} else if name != "unlogged" {
			c.CredentialRef = "oauth-" + name
			state.OAuthCredentials[c.CredentialRef] = config.OAuthCredential{AccessToken: "access-" + name, RefreshToken: "refresh-" + name, AccountID: "account-" + name, PlanType: "pro", ExpiresAt: time.Now().Add(7 * 24 * time.Hour), LastRefresh: time.Now(), Generation: 1}
		}
		state.Connections[name] = c
		profile := config.Profile{Name: name, ID: "profile-" + name, Agent: "claude-code", KeyRef: "client-" + name, Models: map[string]config.Binding{}}
		state.Secrets[profile.KeyRef] = "client-secret-" + name
		for _, slot := range config.Slots {
			profile.Models[slot] = config.Binding{PublicModel: "claude-" + slot + "-test", Connection: name, TargetModel: "gpt-5.5", Label: name + " " + slot, Description: "mock route"}
		}
		state.Profiles[name] = profile
	}
	if err := st.Update(func(current *config.State) error { *current = *state; return nil }); err != nil {
		t.Fatal(err)
	}
	return st, state
}

func TestLoggedOAuthWithoutProfileIsRegistered(t *testing.T) {
	st, state := oauthTestState(t)
	state.Connections["orphan"] = config.Connection{
		ID: "connection-orphan", Name: "orphan", AuthType: config.AuthCodexOAuth,
		Protocol: config.Responses, BaseURL: config.CodexBaseURL, CredentialRef: "oauth-orphan",
		Models: []config.Model{{ID: "gpt-5.5"}},
	}
	state.OAuthCredentials["oauth-orphan"] = config.OAuthCredential{
		AccessToken: "access-orphan", RefreshToken: "refresh-orphan", AccountID: "account-orphan",
		PlanType: "pro", ExpiresAt: time.Now().Add(7 * 24 * time.Hour), LastRefresh: time.Now(), Generation: 1,
	}
	if err := st.Update(func(current *config.State) error { *current = *state; return nil }); err != nil {
		t.Fatal(err)
	}
	manager := startManagedTest(t, st, runtimeOptions{transport: &oauthTransport{requests: make(chan observedOAuthRequest, 1)}})
	for _, auth := range manager.List() {
		if auth.Metadata["account_id"] == "account-orphan" {
			return
		}
	}
	t.Fatal("没有被 profile 引用的已登录 OAuth 连接未注册")
}

func startManagedTest(t *testing.T, st *store.Store, options runtimeOptions) *coreauth.Manager {
	t.Helper()
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	compiled, err := compile(state, st)
	if err != nil {
		t.Fatal(err)
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	var active atomic.Pointer[snapshot]
	var mu sync.Mutex
	runtimeState := RuntimeState{InstanceID: "oauth-test", Listen: state.Listen, ProbeToken: "private-test-probe"}
	if err := writeRuntime(st, runtimeState); err != nil {
		t.Fatal(err)
	}
	managers := make(chan *coreauth.Manager, 1)
	options.onManager = func(manager *coreauth.Manager) { managers <- manager }
	done := make(chan error, 1)
	go func() {
		err := runManagedService(ctx, st, state.Listen, runtimeState.ProbeToken, &active, compiled, &runtimeState, &mu, options)
		if err != nil && !errors.Is(err, context.Canceled) {
			runtimeState.Ready = false
			runtimeState.Error = err.Error()
			_ = writeRuntime(st, runtimeState)
		}
		done <- err
	}()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("网关关闭超时")
		}
		lock.Close()
	})
	manager := <-managers
	waitForReady(t, st)
	return manager
}

func waitRevision(t *testing.T, st *store.Store) {
	t.Helper()
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime, err := decodeRuntime(st)
		if err == nil && runtime.Ready && runtime.ActiveRevision == state.Revision {
			request, _ := http.NewRequest(http.MethodGet, "http://"+runtime.Listen+ReadyPath, nil)
			request.Header.Set(probeHeader, runtime.ProbeToken)
			response, err := http.DefaultClient.Do(request)
			if err == nil {
				var payload struct {
					Ready    bool   `json:"ready"`
					Revision uint64 `json:"activeRevision"`
				}
				decodeErr := json.NewDecoder(response.Body).Decode(&payload)
				response.Body.Close()
				if decodeErr == nil && payload.Ready && payload.Revision == state.Revision {
					return
				}
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("配置未热更新")
}

func TestOAuthAndAPIKeySameModelIsolationAndHotReload(t *testing.T) {
	st, state := oauthTestState(t)
	transport := &oauthTransport{requests: make(chan observedOAuthRequest, 32)}
	manager := startManagedTest(t, st, runtimeOptions{transport: transport})
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	epochs := map[string]uint64{}
	for _, auth := range manager.List() {
		if auth.Attributes["api_key"] == "" {
			epochs[auth.ID] = auth.RegistrationEpoch
		}
	}
	if len(epochs) != 2 || len(manager.List()) != 3 {
		t.Fatalf("受管 Auth 数量错误：%d", len(manager.List()))
	}
	for _, name := range []string{"a", "b", "api"} {
		response := postMessages(t, client, state.Listen, "client-secret-"+name, "claude-opus-test")
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("%s 路由失败：%d %s", name, response.StatusCode, body)
		}
		got := <-transport.requests
		if name == "api" {
			if got.key != "Bearer api-key-test" || got.account != "" || got.host != "mock.example.test" {
				t.Fatal("API key 路由与 OAuth 串线")
			}
		} else if got.key != "Bearer access-"+name || got.account != "account-"+name || got.host != "chatgpt.com" {
			t.Fatal("OAuth 账号串线")
		}
		if got.model != "gpt-5.5" {
			t.Fatal("上游 model 未还原")
		}
	}
	response := postMessages(t, client, state.Listen, "client-secret-unlogged", "claude-opus-test")
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("未登录 OAuth 不应继承默认 key")
	}
	for _, operation := range []func() error{
		func() error {
			session, err := connection.BeginOAuth(st, "a")
			if session != nil {
				session.Close()
			}
			return err
		},
		func() error { return connection.LogoutOAuth(st, "a") },
		func() error {
			auth := config.AuthAPIKey
			return connection.Patch(st, "a", nil, nil, nil, &auth)
		},
	} {
		if operation() == nil {
			t.Fatal("运行中应拒绝 OAuth 生命周期操作")
		}
	}
	for i := 0; i < 3; i++ {
		beforeRefresh, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		var refreshed *coreauth.Auth
		for _, auth := range manager.List() {
			if auth.Metadata["account_id"] == "account-a" {
				refreshed = auth
				break
			}
		}
		if refreshed == nil {
			t.Fatal("OAuth Auth 在热更新前丢失")
		}
		refreshed.Metadata["access_token"] = fmt.Sprintf("rotated-access-a-%d", i)
		refreshed.Metadata["refresh_token"] = fmt.Sprintf("rotated-refresh-a-%d", i)
		if _, err := manager.Update(context.Background(), refreshed); err != nil {
			t.Fatal(err)
		}
		afterRefresh, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		if afterRefresh.Revision != beforeRefresh.Revision || afterRefresh.OAuthCredentials["oauth-a"].Generation != uint64(i+2) {
			t.Fatal("令牌轮换改变了结构 revision 或未推进凭证版本")
		}
		if err := st.Update(func(s *config.State) error { s.Secrets["api-key"] = fmt.Sprintf("rotated-key-%d", i); return nil }); err != nil {
			t.Fatal(err)
		}
		waitRevision(t, st)
		if len(manager.List()) != 3 {
			t.Fatal("热更新留下重复静态 Auth")
		}
		for id, epoch := range epochs {
			auth, ok := manager.GetByID(id)
			if !ok || auth.RegistrationEpoch != epoch {
				t.Fatal("热更新重新加载了 OAuth Auth")
			}
		}
		response := postMessages(t, client, state.Listen, "client-secret-api", "claude-opus-test")
		body, _ := io.ReadAll(response.Body)
		response.Body.Close()
		if response.StatusCode != 200 {
			t.Fatalf("API key 热轮换失败：%d %s", response.StatusCode, body)
		}
		if got := <-transport.requests; got.key != fmt.Sprintf("Bearer rotated-key-%d", i) {
			t.Fatal("使用了旧 API key")
		}
		response = postMessages(t, client, state.Listen, "client-secret-a", "claude-opus-test")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal("热更新后轮换过的 OAuth 凭证不可用")
		}
		if got := <-transport.requests; got.key != fmt.Sprintf("Bearer rotated-access-a-%d", i) || got.account != "account-a" {
			t.Fatal("热更新丢失了新 OAuth 令牌或串线到其他账号")
		}
	}
	if err := st.Update(func(s *config.State) error {
		profile := s.Profiles["a"]
		b := profile.Models["opus"]
		b.TargetModel = "unavailable-model"
		profile.Models["opus"] = b
		s.Profiles["a"] = profile
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitRevision(t, st)
	response = postMessages(t, client, state.Listen, "client-secret-a", "claude-opus-test")
	response.Body.Close()
	if response.StatusCode != http.StatusServiceUnavailable {
		t.Fatal("未注册模型应 fail-closed")
	}
	for id, epoch := range epochs {
		auth, ok := manager.GetByID(id)
		if !ok || auth.RegistrationEpoch != epoch {
			t.Fatal("模型更新不应重新 Load OAuth")
		}
	}
	for _, auth := range manager.List() {
		if auth.Attributes["api_key"] != "" && !coreauth.IsConfigAPIKeyAuth(auth) {
			t.Fatal("静态认证未明确分类")
		}
	}
	for id := range epochs {
		if len(cliproxy.GlobalModelRegistry().GetModelsForClient(id)) == 0 {
			t.Fatal("当前 OAuth client 缺少套餐模型目录")
		}
	}
}
