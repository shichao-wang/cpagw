package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/shichao-wang/cpagw/internal/config"
)

func TestStaticAuthHotReloadReordersIndexesAndRemovesClients(t *testing.T) {
	st, state := oauthTestState(t)
	state.Connections = testState(state.Listen, "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9").Connections
	state.Profiles = map[string]config.Profile{}
	for ref, value := range testState(state.Listen, "", "", "", "").Secrets {
		state.Secrets[ref] = value
	}
	state.OAuthCredentials = map[string]config.OAuthCredential{}
	if err := st.Update(func(s *config.State) error { *s = *state; return nil }); err != nil {
		t.Fatal(err)
	}
	manager := startManagedTest(t, st, runtimeOptions{})
	check := func(expected int) {
		t.Helper()
		current, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		compiled, err := compile(current, st)
		if err != nil {
			t.Fatal(err)
		}
		if len(manager.List()) != expected {
			t.Fatalf("静态 Auth 数量为 %d，预期 %d", len(manager.List()), expected)
		}
		for _, auth := range manager.List() {
			if !coreauth.IsConfigAPIKeyAuth(auth) {
				t.Fatal("静态 Auth 分类错误")
			}
			index, err := strconv.Atoi(auth.Attributes["config_index"])
			if err != nil {
				t.Fatal(err)
			}
			var key, base, prefix string
			switch auth.Provider {
			case "claude":
				e := compiled.config.ClaudeKey[index]
				key, base, prefix = e.APIKey, e.BaseURL, e.Prefix
			case "codex":
				e := compiled.config.CodexKey[index]
				key, base, prefix = e.APIKey, e.BaseURL, e.Prefix
			default:
				e := compiled.config.OpenAICompatibility[index]
				key, base, prefix = e.APIKeyEntries[0].APIKey, e.BaseURL, e.Prefix
			}
			if auth.Attributes["api_key"] != key || auth.Attributes["base_url"] != base || auth.Prefix != prefix {
				t.Fatal("config_index 未与当前配置对齐")
			}
		}
	}
	check(4)
	for i := 0; i < 3; i++ {
		if err := st.Update(func(s *config.State) error {
			for cname, connection := range s.Connections {
				connection.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", 10000+i)
				s.Secrets[connection.CredentialRef] = fmt.Sprintf("key-revision-%d-%s", i, cname)
				s.Connections[cname] = connection
			}
			for _, protocol := range []string{config.Chat, config.Anthropic, config.Responses} {
				name := "aaa-" + protocol
				connection := config.Connection{ID: fmt.Sprintf("new-%s-%d", protocol, i), Name: name, AuthType: config.AuthAPIKey, Protocol: protocol, BaseURL: "http://127.0.0.1:9", CredentialRef: "new-" + protocol, Models: []config.Model{{ID: "new-model"}}}
				s.Connections[name] = connection
				s.Secrets[connection.CredentialRef] = "new-key-test"
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		waitRevision(t, st)
		check(7)
	}
	if err := st.Update(func(s *config.State) error {
		for name := range s.Connections {
			if strings.HasPrefix(name, "aaa-") {
				delete(s.Secrets, s.Connections[name].CredentialRef)
				delete(s.Connections, name)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitRevision(t, st)
	check(4)
}

func TestOAuthTopologyChangeStopsGatewayAndCleansRegistry(t *testing.T) {
	st, _ := oauthTestState(t)
	manager := startManagedTest(t, st, runtimeOptions{})
	var authID string
	for _, auth := range manager.List() {
		if auth.Metadata["account_id"] == "account-a" {
			authID = auth.ID
			break
		}
	}
	if authID == "" {
		t.Fatal("OAuth Auth 未注册")
	}
	if err := st.Update(func(state *config.State) error {
		connection := state.Connections["a"]
		connection.ID = "connection-a-replaced"
		state.Connections["a"] = connection
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		runtime, err := decodeRuntime(st)
		if err == nil && !runtime.Ready && strings.Contains(runtime.Error, "OAuth") {
			if len(cliproxy.GlobalModelRegistry().GetModelsForClient(authID)) != 0 {
				t.Fatal("OAuth topology 变化停止网关后仍留下模型注册")
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("OAuth topology 变化未停止网关并报告重启")
}

func TestListenerChangeStopsGatewayAndReportsRestart(t *testing.T) {
	st, initial := oauthTestState(t)
	state := testState(initial.Listen, "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9")
	if err := st.Update(func(s *config.State) error { *s = *state; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, st, "listener-change-test") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(15 * time.Second):
			t.Error("网关关闭超时")
		}
	})
	waitForReady(t, st)
	listen, err := reserveLoopbackAddress(t)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.Update(func(s *config.State) error { s.Listen = listen; return nil }); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-done:
		done <- err
		if err == nil || !strings.Contains(err.Error(), "请重启网关") {
			t.Fatalf("监听地址变更未提示重启：%v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("监听地址变更后网关未停止")
	}
	runtime, err := decodeRuntime(st)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.Ready || !strings.Contains(runtime.Error, "请重启网关") {
		t.Fatal("运行状态仍宣称就绪或丢失重启原因")
	}
}
