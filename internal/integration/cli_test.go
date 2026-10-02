package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/store"
)

// TestBinaryFlow 通过实际 binary 验证用户命令、服务 socket 与配置接管，不访问真实上游。
func TestBinaryFlow(t *testing.T) {
	if testing.Short() {
		t.Skip("短测试模式跳过子进程验收")
	}
	tmp := t.TempDir()
	binary := filepath.Join(tmp, "cpagw")
	build := exec.Command("go", "build", "-o", binary, "./cmd/cpagw")
	build.Dir = filepath.Join("..", "..")
	if out, err := build.CombinedOutput(); err != nil {
		t.Fatalf("构建 binary：%v\n%s", err, out)
	}
	dir := filepath.Join(tmp, "state")
	invoke := func(input string, args ...string) (string, error) {
		cmd := exec.Command(binary, append([]string{"--state-dir", dir}, args...)...)
		cmd.Stdin = strings.NewReader(input)
		out, err := cmd.CombinedOutput()
		return string(out), err
	}
	run := func(input string, args ...string) string {
		t.Helper()
		out, err := invoke(input, args...)
		if err != nil {
			t.Fatalf("%v: %v\n%s", args, err, out)
		}
		return out
	}
	var calls atomic.Int64
	var upstreamKey atomic.Value
	upstreamKey.Store("upstream-test-key")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		expectedKey := upstreamKey.Load().(string)
		if r.URL.Path != "/v1/messages" || (r.Header.Get("X-Api-Key") != expectedKey && r.Header.Get("Authorization") != "Bearer "+expectedKey) {
			t.Errorf("上游路径/认证不匹配：%s", r.URL.Path)
			w.WriteHeader(401)
			return
		}
		var body map[string]any
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if body["model"] != "actual" {
			t.Errorf("上游模型错误：%v", body["model"])
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{"id":"msg_mock","type":"message","role":"assistant","model":"actual","content":[{"type":"text","text":"来自 mock 上游"}],"stop_reason":"end_turn","stop_sequence":null,"usage":{"input_tokens":2,"output_tokens":4}}`)
	}))
	defer upstream.Close()
	models := filepath.Join(tmp, "models.yaml")
	if err := os.WriteFile(models, []byte("models:\n  - id: actual\n    name: 实际模型\n"), 0600); err != nil {
		t.Fatal(err)
	}
	run("upstream-test-key\n", "provider", "add", "alpha", "--api-key-stdin")
	run("", "provider", "connection", "add", "alpha", "anth", "--protocol", "anthropic-messages", "--base-url", upstream.URL, "--models", models)
	bindings := filepath.Join(tmp, "bindings.yaml")
	var yaml strings.Builder
	yaml.WriteString("models:\n")
	for _, slot := range []string{"opus", "sonnet", "haiku"} {
		fmt.Fprintf(&yaml, "  %s:\n    public_model: claude-%s-test\n    provider: alpha\n    connection: anth\n    target_model: actual\n    label: mock-%s\n    description: mock 路由\n", slot, slot, slot)
	}
	if err := os.WriteFile(bindings, []byte(yaml.String()), 0600); err != nil {
		t.Fatal(err)
	}
	output := run("", "profile", "create", "daily", "--agent", "claude-code", "--file", bindings)
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	key := state.Secrets[state.Profiles["daily"].KeyRef]
	if key == "" || strings.Contains(output, key) {
		t.Fatal("profile key 必须保存且不能默认输出")
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	listen := listener.Addr().String()
	listener.Close()
	run("", "server", "start", "--listen", listen)
	t.Cleanup(func() { _, _ = invoke("", "server", "stop") })
	status := run("", "server", "status")
	if !strings.Contains(status, `"state":"running"`) {
		t.Fatalf("服务未就绪：%s", status)
	}
	client := &http.Client{Timeout: 5 * time.Second}
	request := func(method, path, body string) (int, []byte) {
		t.Helper()
		req, err := http.NewRequest(method, "http://"+listen+path, bytes.NewBufferString(body))
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("X-Api-Key", key)
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			t.Fatal(err)
		}
		return resp.StatusCode, data
	}
	code, body := request("GET", "/v1/models?limit=1000", "")
	if code != 200 || !bytes.Contains(body, []byte("mock-sonnet")) {
		t.Fatalf("模型目录：%d %s", code, body)
	}
	code, body = request("POST", "/v1/messages", `{"model":"claude-sonnet-test","max_tokens":20,"messages":[{"role":"user","content":"Hi"}]}`)
	if code != 200 || !bytes.Contains(body, []byte(`"model":"claude-sonnet-test"`)) {
		t.Fatalf("模型路由：%d %s", code, body)
	}
	prior := calls.Load()
	code, _ = request("POST", "/v1/messages", `{"model":"actual","messages":[]}`)
	if code < 400 || calls.Load() != prior {
		t.Fatal("未知公开模型不能调用上游")
	}
	// 验证轮换提供商默认 key 无需重建 profile 或重新 apply。
	upstreamKey.Store("rotated-key")
	run("rotated-key\n", "provider", "update", "alpha", "--api-key-stdin")
	code, body = request("POST", "/v1/messages", `{"model":"claude-sonnet-test","max_tokens":20,"messages":[{"role":"user","content":"Hi"}]}`)
	if code != 200 {
		t.Fatalf("轮换后路由失败：%d %s", code, body)
	}
	settings := filepath.Join(tmp, "settings.json")
	original := []byte(`{"unrelated":{"keep":true}}`)
	if err := os.WriteFile(settings, original, 0644); err != nil {
		t.Fatal(err)
	}
	run("", "profile", "apply", "daily", "--settings", settings)
	if out, err := invoke("", "profile", "delete", "daily", "--yes"); err == nil {
		t.Fatalf("不能删除受管 profile：%s", out)
	}
	run("", "profile", "restore", "--settings", settings)
	restored, err := os.ReadFile(settings)
	if err != nil {
		t.Fatal(err)
	}
	var restoredObject map[string]any
	if err = json.Unmarshal(restored, &restoredObject); err != nil {
		t.Fatal(err)
	}
	if len(restoredObject) != 1 || restoredObject["unrelated"] == nil {
		t.Fatalf("恢复不保留原配置：%s", restored)
	}
	if _, err := invoke("", "profile", "restore", "--settings", settings); err == nil {
		t.Fatal("重复恢复必须拒绝")
	}
	run("", "server", "stop")
	status = run("", "server", "status")
	if !strings.Contains(status, `"state":"stopped"`) {
		t.Fatalf("服务未停止：%s", status)
	}
	run("", "profile", "delete", "daily", "--yes")
	run("", "provider", "remove", "alpha", "--yes")
}
