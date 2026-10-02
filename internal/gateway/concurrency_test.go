package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

func TestConcurrentProfilesWithSamePublicID(t *testing.T) {
	mock := func(marker string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			fmt.Fprintf(w, `{"id":"chatcmpl_mock","object":"chat.completion","created":1,"model":"shared-target","choices":[{"index":0,"message":{"role":"assistant","content":%q},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`, marker)
		}))
	}
	a, b := mock("alpha"), mock("beta")
	defer a.Close()
	defer b.Close()
	address, err := reserveLoopbackAddress(t)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	state := testState(address, a.URL, b.URL, a.URL, a.URL)
	for name, p := range state.Profiles {
		for slot, binding := range p.Models {
			binding.Provider = "chat-provider"
			binding.Connection = "connection-a"
			if name == "beta" {
				binding.Connection = "connection-b"
			}
			binding.TargetModel = "shared-target"
			p.Models[slot] = binding
		}
		state.Profiles[name] = p
	}
	if err = s.Update(func(target *config.State) error { *target = *state; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, "concurrent-instance") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("并发验收服务未退出")
		}
	})
	waitForReady(t, s)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	var wg sync.WaitGroup
	failures := make(chan error, 20)
	for i := range 20 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			name, key := "alpha", "alpha-client-secret"
			if i%2 == 1 {
				name, key = "beta", "beta-client-secret"
			}
			req, err := http.NewRequest("POST", "http://"+address+"/v1/messages", bytes.NewBufferString(`{"model":"claude-opus-test","max_tokens":8,"messages":[{"role":"user","content":"hello"}]}`))
			if err != nil {
				failures <- err
				return
			}
			req.Header.Set("X-Api-Key", key)
			req.Header.Set("Content-Type", "application/json")
			resp, err := client.Do(req)
			if err != nil {
				failures <- err
				return
			}
			defer resp.Body.Close()
			var result struct {
				Model   string `json:"model"`
				Content []struct {
					Text string `json:"text"`
				} `json:"content"`
			}
			if err = json.NewDecoder(resp.Body).Decode(&result); err != nil {
				failures <- err
				return
			}
			if resp.StatusCode != 200 || result.Model != "claude-opus-test" || len(result.Content) != 1 || result.Content[0].Text != name {
				failures <- fmt.Errorf("同公开 ID 并发路由串线：profile=%s status=%d model=%s", name, resp.StatusCode, result.Model)
			}
		}(i)
	}
	wg.Wait()
	close(failures)
	for err := range failures {
		t.Error(err)
	}
}
