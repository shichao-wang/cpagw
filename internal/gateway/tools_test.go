package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

func TestToolCallsAcrossUpstreamProtocols(t *testing.T) {
	mock := func(kind string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "application/json")
			switch kind {
			case "chat":
				io.WriteString(w, `{"id":"chat_tool","object":"chat.completion","created":1,"model":"shared-target","choices":[{"index":0,"message":{"role":"assistant","content":null,"tool_calls":[{"id":"call_1","type":"function","function":{"name":"read_file","arguments":"{\"path\":\"demo.txt\",\"model\":\"shared-target\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
			case "anthropic":
				io.WriteString(w, `{"id":"msg_tool","type":"message","role":"assistant","model":"anthropic-target","content":[{"type":"tool_use","id":"call_1","name":"read_file","input":{"path":"demo.txt","model":"anthropic-target"}}],"stop_reason":"tool_use","usage":{"input_tokens":1,"output_tokens":2}}`)
			case "responses":
				w.Header().Set("Content-Type", "text/event-stream")
				io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_tool","object":"response","status":"completed","model":"responses-target","output":[{"type":"function_call","id":"fc_1","call_id":"call_1","name":"read_file","arguments":"{\"path\":\"demo.txt\",\"model\":\"responses-target\"}"}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`+"\n\n")
			}
		}))
	}
	chat, anthropic, responses := mock("chat"), mock("anthropic"), mock("responses")
	defer chat.Close()
	defer anthropic.Close()
	defer responses.Close()
	address, err := reserveLoopbackAddress(t)
	if err != nil {
		t.Fatal(err)
	}
	s, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	state := testState(address, chat.URL, chat.URL, anthropic.URL, responses.URL)
	if err = s.Update(func(current *config.State) error { *current = *state; return nil }); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, s, "tools-instance") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("工具调用验收服务未退出")
		}
	})
	waitForReady(t, s)
	client := &http.Client{Timeout: 5 * time.Second}
	defer client.CloseIdleConnections()
	for _, target := range []struct{ Slot, Model string }{{"opus", "shared-target"}, {"sonnet", "anthropic-target"}, {"haiku", "responses-target"}} {
		public := "claude-" + target.Slot + "-test"
		payload := fmt.Sprintf(`{"model":%q,"max_tokens":20,"messages":[{"role":"user","content":"读取文件"}],"tools":[{"name":"read_file","description":"读取测试文件","input_schema":{"type":"object","properties":{"path":{"type":"string"},"model":{"type":"string"}},"required":["path"]}}]}`, public)
		req, err := http.NewRequest("POST", "http://"+address+"/v1/messages", bytes.NewBufferString(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", "alpha-client-secret")
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		data, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			t.Fatal(err)
		}
		var result struct {
			Model   string `json:"model"`
			Content []struct {
				Type, Name string
				Input      map[string]any
			} `json:"content"`
		}
		if resp.StatusCode != 200 || json.Unmarshal(data, &result) != nil {
			t.Fatalf("%s 工具转换失败：%d %s", target.Slot, resp.StatusCode, data)
		}
		found := false
		for _, block := range result.Content {
			if block.Type == "tool_use" && block.Name == "read_file" {
				found = true
				if block.Input["model"] != target.Model || block.Input["path"] != "demo.txt" {
					t.Errorf("%s 的工具参数被改写：%v", target.Slot, block.Input)
				}
			}
		}
		if !found || result.Model != public {
			t.Fatalf("%s 缺少正确模型或工具块：%s", target.Slot, data)
		}
	}
}
