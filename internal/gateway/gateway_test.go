package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/store"
)

func TestRewriteModelFieldsOnlyTouchesTopLevelModel(t *testing.T) {
	input := []byte(`{"model":"upstream-model","message":"upstream-model","nested":{"model":"upstream-model"},"other":{"model":"unrelated"}}`)
	got := rewriteModelFields(input, map[string]string{"upstream-model": "claude-sonnet-test"})
	var result struct {
		Model   string `json:"model"`
		Message string `json:"message"`
		Nested  struct {
			Model string `json:"model"`
		} `json:"nested"`
		Other struct {
			Model string `json:"model"`
		} `json:"other"`
	}
	if err := json.Unmarshal(got, &result); err != nil {
		t.Fatalf("decode rewritten JSON: %v", err)
	}
	if result.Model != "claude-sonnet-test" || result.Message != "upstream-model" || result.Nested.Model != "upstream-model" || result.Other.Model != "unrelated" {
		t.Fatalf("rewritten JSON changed unexpected fields: %+v", result)
	}
}

func TestTransformSSEEventPreservesFramingAndOnlyRewritesModel(t *testing.T) {
	route := modelRoute{publicID: "claude-opus-test", targetModel: "upstream-model", sdkModel: "cpagw-prefix/upstream-model"}
	input := []byte("event: message_start\r\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"upstream-model\",\"note\":\"upstream-model\"}}\r")
	got := transformSSEEvent(input, route)
	if !bytes.HasPrefix(got, []byte("event: message_start\r\ndata: ")) || !bytes.HasSuffix(got, []byte("\r")) {
		t.Fatalf("SSE framing was not preserved: %q", got)
	}
	data := bytes.TrimSuffix(bytes.TrimPrefix(got, []byte("event: message_start\r\ndata: ")), []byte("\r"))
	var result struct {
		Type    string `json:"type"`
		Message struct {
			Model string `json:"model"`
			Note  string `json:"note"`
		} `json:"message"`
	}
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatalf("decode SSE data: %v", err)
	}
	if result.Type != "message_start" || result.Message.Model != "claude-opus-test" || result.Message.Note != "upstream-model" {
		t.Fatalf("SSE model mapping changed unexpected fields: %+v", result)
	}
}

func TestFindRootModelRejectsDuplicateAndTrailingJSON(t *testing.T) {
	for _, input := range []string{
		`{"model":"first","model":"second"}`,
		`{"model":"valid"} {"model":"second"}`,
	} {
		if _, _, _, err := findRootModel([]byte(input)); err == nil {
			t.Errorf("findRootModel(%s) unexpectedly succeeded", input)
		}
	}
}

func TestGatewayRoutesByProfileAndUpstreamProtocol(t *testing.T) {
	type observed struct {
		kind  string
		path  string
		key   string
		model string
	}
	observedRequests := make(chan observed, 16)
	newUpstream := func(kind string) *httptest.Server {
		return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body, _ := io.ReadAll(r.Body)
			_ = r.Body.Close()
			var payload struct {
				Model string `json:"model"`
			}
			_ = json.Unmarshal(body, &payload)
			key := r.Header.Get("X-Api-Key")
			if key == "" {
				key = strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer ")
			}
			observedRequests <- observed{kind: kind, path: r.URL.Path, key: key, model: payload.Model}
			w.Header().Set("Content-Type", "application/json")
			switch kind {
			case "anthropic":
				_, _ = io.WriteString(w, `{"id":"msg_1","type":"message","role":"assistant","model":"anthropic-target","content":[{"type":"text","text":"mock reply"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":1,"output_tokens":2}}`)
			case "chat":
				_, _ = io.WriteString(w, `{"id":"chatcmpl_1","object":"chat.completion","created":1,"model":"shared-target","choices":[{"index":0,"message":{"role":"assistant","content":"mock reply"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`)
			case "responses":
				w.Header().Set("Content-Type", "text/event-stream")
				_, _ = io.WriteString(w, "event: response.completed\ndata: "+`{"type":"response.completed","response":{"id":"resp_1","object":"response","created_at":1,"status":"completed","model":"responses-target","output":[{"id":"msg_1","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"mock reply","annotations":[]}]}],"usage":{"input_tokens":1,"output_tokens":2,"total_tokens":3}}}`+"\n\n")
			}
		}))
	}

	chatA := newUpstream("chat")
	chatB := newUpstream("chat")
	anthropic := newUpstream("anthropic")
	responses := newUpstream("responses")
	defer chatA.Close()
	defer chatB.Close()
	defer anthropic.Close()
	defer responses.Close()

	listen, err := reserveLoopbackAddress(t)
	if err != nil {
		t.Fatal(err)
	}
	st, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	state := testState(listen, chatA.URL, chatB.URL, anthropic.URL, responses.URL)
	if err := st.Update(func(current *config.State) error {
		*current = *state
		return nil
	}); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	runDone := make(chan error, 1)
	go func() { runDone <- Run(ctx, st, "instance-test") }()
	t.Cleanup(func() {
		cancel()
		select {
		case <-runDone:
		case <-time.After(15 * time.Second):
			t.Error("gateway.Run did not stop after context cancellation")
		}
	})
	waitForReady(t, st)

	client := &http.Client{Timeout: 8 * time.Second, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	assertCatalog(t, client, listen, "alpha-client-secret", "alpha")
	assertCatalog(t, client, listen, "beta-client-secret", "beta")

	for _, test := range []struct {
		profile string
		key     string
		model   string
		kind    string
		upKey   string
		upModel string
	}{
		{profile: "alpha", key: "alpha-client-secret", model: "claude-opus-test", kind: "chat", upKey: "chat-key-a", upModel: "shared-target"},
		{profile: "beta", key: "beta-client-secret", model: "claude-opus-test", kind: "chat", upKey: "chat-key-b", upModel: "shared-target"},
		{profile: "alpha", key: "alpha-client-secret", model: "claude-sonnet-test", kind: "anthropic", upKey: "anthropic-key", upModel: "anthropic-target"},
		{profile: "alpha", key: "alpha-client-secret", model: "claude-haiku-test", kind: "responses", upKey: "responses-key", upModel: "responses-target"},
	} {
		t.Run(test.profile+"/"+test.model, func(t *testing.T) {
			response := postMessages(t, client, listen, test.key, test.model)
			if response.StatusCode != http.StatusOK {
				data, _ := io.ReadAll(response.Body)
				_ = response.Body.Close()
				t.Fatalf("/v1/messages status = %d, body = %s", response.StatusCode, data)
			}
			var result struct {
				Model string `json:"model"`
			}
			if err := json.NewDecoder(response.Body).Decode(&result); err != nil {
				_ = response.Body.Close()
				t.Fatalf("decode response: %v", err)
			}
			_ = response.Body.Close()
			if result.Model != test.model {
				t.Fatalf("downstream model = %q, want public ID %q", result.Model, test.model)
			}
			select {
			case got := <-observedRequests:
				if got.kind != test.kind || got.key != test.upKey || got.model != test.upModel {
					t.Fatalf("upstream route = %+v, want kind=%q key=%q model=%q", got, test.kind, test.upKey, test.upModel)
				}
			case <-time.After(2 * time.Second):
				t.Fatal("mock upstream did not receive a request")
			}
		})
	}

	unauthorized, err := http.NewRequest(http.MethodPost, "http://"+listen+"/v1/messages", strings.NewReader(`{"model":"claude-opus-test"}`))
	if err != nil {
		t.Fatal(err)
	}
	unauthorized.Header.Set("Content-Type", "application/json")
	response, err := client.Do(unauthorized)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusUnauthorized {
		t.Fatalf("missing credentials status = %d, want 401", response.StatusCode)
	}

	blocked, err := http.NewRequest(http.MethodGet, "http://"+listen+"/", nil)
	if err != nil {
		t.Fatal(err)
	}
	blocked.Header.Set("X-Api-Key", "alpha-client-secret")
	response, err = client.Do(blocked)
	if err != nil {
		t.Fatal(err)
	}
	_ = response.Body.Close()
	if response.StatusCode != http.StatusNotFound {
		t.Fatalf("unlisted route status = %d, want 404", response.StatusCode)
	}
}

func testState(listen, chatA, chatB, anthropic, responses string) *config.State {
	state := config.NewState()
	state.Listen = listen
	state.Secrets = map[string]string{
		"chat-a": "chat-key-a", "chat-b": "chat-key-b", "anthropic": "anthropic-key", "responses": "responses-key",
		"alpha-key": "alpha-client-secret", "beta-key": "beta-client-secret",
	}
	shared := []config.Model{{ID: "shared-target", Name: "Shared target"}}
	anthropicModels := []config.Model{{ID: "anthropic-target", Name: "Anthropic target"}}
	responsesModels := []config.Model{{ID: "responses-target", Name: "Responses target"}}
	state.Providers = map[string]config.Provider{
		"chat-provider": {Name: "Chat", Connections: map[string]config.Connection{
			"connection-a": {Name: "connection-a", Protocol: config.Chat, BaseURL: chatA, CredentialRef: "chat-a", Models: shared},
			"connection-b": {Name: "connection-b", Protocol: config.Chat, BaseURL: chatB, CredentialRef: "chat-b", Models: shared},
		}},
		"anthropic-provider": {Name: "Anthropic", Connections: map[string]config.Connection{
			"connection": {Name: "connection", Protocol: config.Anthropic, BaseURL: anthropic, CredentialRef: "anthropic", Models: anthropicModels},
		}},
		"responses-provider": {Name: "Responses", Connections: map[string]config.Connection{
			"connection": {Name: "connection", Protocol: config.Responses, BaseURL: responses, CredentialRef: "responses", Models: responsesModels},
		}},
	}
	state.Profiles = map[string]config.Profile{
		"alpha": {
			Name: "alpha", ID: "profile-alpha", Agent: "claude-code", KeyRef: "alpha-key",
			Models: map[string]config.Binding{
				"opus":   {PublicModel: "claude-opus-test", Provider: "chat-provider", Connection: "connection-a", TargetModel: "shared-target", Label: "Alpha Opus", Description: "Alpha chat route"},
				"sonnet": {PublicModel: "claude-sonnet-test", Provider: "anthropic-provider", Connection: "connection", TargetModel: "anthropic-target", Label: "Alpha Sonnet", Description: "Alpha Anthropic route"},
				"haiku":  {PublicModel: "claude-haiku-test", Provider: "responses-provider", Connection: "connection", TargetModel: "responses-target", Label: "Alpha Haiku", Description: "Alpha Responses route"},
			},
		},
		"beta": {
			Name: "beta", ID: "profile-beta", Agent: "claude-code", KeyRef: "beta-key",
			Models: map[string]config.Binding{
				"opus":   {PublicModel: "claude-opus-test", Provider: "chat-provider", Connection: "connection-b", TargetModel: "shared-target", Label: "Beta Opus", Description: "Beta chat route"},
				"sonnet": {PublicModel: "claude-sonnet-test", Provider: "anthropic-provider", Connection: "connection", TargetModel: "anthropic-target", Label: "Beta Sonnet", Description: "Beta Anthropic route"},
				"haiku":  {PublicModel: "claude-haiku-test", Provider: "responses-provider", Connection: "connection", TargetModel: "responses-target", Label: "Beta Haiku", Description: "Beta Responses route"},
			},
		},
	}
	return state
}

func reserveLoopbackAddress(t *testing.T) (string, error) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		return "", err
	}
	address := listener.Addr().String()
	if err := listener.Close(); err != nil {
		return "", err
	}
	return address, nil
}

func waitForReady(t *testing.T, st *store.Store) RuntimeState {
	t.Helper()
	deadline := time.After(10 * time.Second)
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	for {
		select {
		case <-deadline:
			t.Fatal("gateway did not become ready")
		default:
		}
		runtimeState, err := decodeRuntime(st)
		if err == nil && runtimeState.Ready {
			request, _ := http.NewRequest(http.MethodGet, "http://"+runtimeState.Listen+ReadyPath, nil)
			request.Header.Set(probeHeader, runtimeState.ProbeToken)
			response, err := client.Do(request)
			if err == nil {
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK {
					return runtimeState
				}
			}
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func assertCatalog(t *testing.T, client *http.Client, listen, key, profilePrefix string) {
	t.Helper()
	request, err := http.NewRequest(http.MethodGet, "http://"+listen+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("X-Api-Key", key)
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		data, _ := io.ReadAll(response.Body)
		t.Fatalf("catalog status = %d: %s", response.StatusCode, data)
	}
	var payload struct {
		Data []struct {
			ID          string `json:"id"`
			DisplayName string `json:"display_name"`
			Description string `json:"description"`
		} `json:"data"`
	}
	if err := json.NewDecoder(response.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	if len(payload.Data) != 3 {
		t.Fatalf("catalog returned %d models, want exactly 3", len(payload.Data))
	}
	for _, model := range payload.Data {
		if !strings.HasSuffix(model.ID, "-test") || !strings.HasPrefix(strings.ToLower(model.DisplayName), profilePrefix+" ") || !strings.HasPrefix(strings.ToLower(model.Description), profilePrefix+" ") {
			t.Errorf("profile %s catalog model leaked or lacks metadata: %+v", profilePrefix, model)
		}
	}
}

func postMessages(t *testing.T, client *http.Client, listen, key, model string) *http.Response {
	t.Helper()
	body, err := json.Marshal(map[string]any{
		"model": model, "max_tokens": 8,
		"messages": []map[string]string{{"role": "user", "content": "hello"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	request, err := http.NewRequest(http.MethodPost, "http://"+listen+"/v1/messages", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("X-Api-Key", key)
	request.Header.Set("anthropic-version", "2023-06-01")
	return mustDo(t, client, request)
}

func mustDo(t *testing.T, client *http.Client, request *http.Request) *http.Response {
	t.Helper()
	response, err := client.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	return response
}
