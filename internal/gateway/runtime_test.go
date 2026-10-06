package gateway

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/shichao-wang/cpagw/internal/config"
)

func TestStaticAuthHotReloadReordersIndexesAndRemovesClients(t *testing.T) {
	st, state := oauthTestState(t)
	state.Providers = testState(state.Listen, "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9", "http://127.0.0.1:9").Providers
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
			for pname, p := range s.Providers {
				for cname, c := range p.Connections {
					c.BaseURL = fmt.Sprintf("http://127.0.0.1:%d", 10000+i)
					s.Secrets[c.CredentialRef] = fmt.Sprintf("key-revision-%d-%s", i, cname)
					p.Connections[cname] = c
				}
				first := config.Connection{ID: "new-" + pname, Name: "aaa", AuthType: config.AuthAPIKey, Protocol: p.Connections[map[string]string{"chat-provider": "connection-a", "anthropic-provider": "connection", "responses-provider": "connection"}[pname]].Protocol, BaseURL: "http://127.0.0.1:9", CredentialRef: "new-" + pname, Models: []config.Model{{ID: "new-model"}}}
				p.Connections["aaa"] = first
				s.Secrets[first.CredentialRef] = "new-key-test"
				s.Providers[pname] = p
			}
			return nil
		}); err != nil {
			t.Fatal(err)
		}
		waitRevision(t, st)
		check(7)
	}
	if err := st.Update(func(s *config.State) error {
		for pname, p := range s.Providers {
			delete(p.Connections, "aaa")
			s.Providers[pname] = p
			delete(s.Secrets, "new-"+pname)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitRevision(t, st)
	check(4)
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
