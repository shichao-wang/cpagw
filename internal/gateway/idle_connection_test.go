package gateway

import (
	"net/http"
	"strings"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/shichao-wang/cpagw/internal/config"
)

func idleAPIKeyConnections(prefix string) map[string]config.Connection {
	connections := map[string]config.Connection{}
	for _, protocol := range []string{config.Chat, config.Anthropic, config.Responses} {
		name := prefix + "-" + protocol
		connections[name] = config.Connection{
			ID: "idle-" + name, Name: name, AuthType: config.AuthAPIKey,
			Protocol: protocol, BaseURL: "https://mock.example.test",
			Models: []config.Model{{ID: "gpt-5.5"}},
		}
	}
	return connections
}

func TestCompileSkipsOnlyUnreferencedAPIKeyConnectionsWithoutCredentials(t *testing.T) {
	st, state := oauthTestState(t)
	for name, connection := range idleAPIKeyConnections("initial") {
		state.Connections[name] = connection
	}
	compiled, err := compile(state, st)
	if err != nil {
		t.Fatalf("闲置连接阻断了有效配置：%v", err)
	}
	if len(compiled.connections) != 4 || len(staticAuths(compiled, "test")) != 1 || len(compiled.config.CodexKey) != 1 || len(compiled.config.ClaudeKey) != 0 || len(compiled.config.OpenAICompatibility) != 0 {
		t.Fatal("待配置连接被编译为可选的 SDK 认证或模型配置")
	}
	for name := range idleAPIKeyConnections("initial") {
		t.Run(name, func(t *testing.T) {
			profile := state.Profiles["a"]
			for slot, binding := range profile.Models {
				binding.Connection = name
				profile.Models[slot] = binding
			}
			state.Profiles["a"] = profile
			if _, err := compile(state, st); err == nil {
				t.Fatal("被 profile 引用的连接缺少凭证时没有拒绝启动")
			}
		})
	}
}

func TestIdleAPIKeyConnectionsDoNotInterruptStartupOrHotReload(t *testing.T) {
	st, state := oauthTestState(t)
	if err := st.Update(func(s *config.State) error {
		for name, connection := range idleAPIKeyConnections("initial") {
			s.Connections[name] = connection
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	transport := &oauthTransport{requests: make(chan observedOAuthRequest, 8)}
	manager := startManagedTest(t, st, runtimeOptions{transport: transport})
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	check := func(expected int) {
		t.Helper()
		if len(manager.List()) != expected {
			t.Fatalf("Auth 数量为 %d，预期 %d", len(manager.List()), expected)
		}
		response := postMessages(t, client, state.Listen, "client-secret-a", "claude-opus-test")
		response.Body.Close()
		if response.StatusCode != http.StatusOK {
			t.Fatal("闲置连接影响了现有 profile 推理")
		}
		if got := <-transport.requests; got.account != "account-a" {
			t.Fatal("现有 profile 的账号路由发生变化")
		}
	}
	check(3)
	if err := st.Update(func(s *config.State) error {
		for name, connection := range idleAPIKeyConnections("added") {
			s.Connections[name] = connection
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitRevision(t, st)
	check(3)
	if err := st.Update(func(s *config.State) error {
		for name, connection := range s.Connections {
			if strings.HasPrefix(name, "initial-") || strings.HasPrefix(name, "added-") {
				connection.CredentialRef = "idle-key-" + name
				s.Connections[name] = connection
				s.Secrets[connection.CredentialRef] = "idle-test-key"
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitRevision(t, st)
	check(9)
	var idleAuthIDs []string
	for _, auth := range manager.List() {
		if auth.Attributes["api_key"] == "idle-test-key" {
			idleAuthIDs = append(idleAuthIDs, auth.ID)
		}
	}
	if len(idleAuthIDs) != 6 {
		t.Fatal("补齐凭证后待配置连接没有热更新为就绪状态")
	}
	if err := st.Update(func(s *config.State) error {
		for name, connection := range s.Connections {
			if strings.HasPrefix(name, "initial-") || strings.HasPrefix(name, "added-") {
				delete(s.Secrets, connection.CredentialRef)
				connection.CredentialRef = ""
				s.Connections[name] = connection
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitRevision(t, st)
	check(3)
	for _, id := range idleAuthIDs {
		if len(cliproxy.GlobalModelRegistry().GetModelsForClient(id)) != 0 {
			t.Fatal("待配置连接留下了可选的模型注册")
		}
	}
}
