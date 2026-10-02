package gateway

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestModelRewriteKeepsColonAndNestedPayload(t *testing.T) {
	original := `{ "model" : "actual", "content":[{"type":"tool_use","input":{"model":"actual"}}],"usage":{"input_tokens":9007199254740993}}`
	rewritten := rewriteModelFields([]byte(original), map[string]string{"actual": "claude-public"})
	if !json.Valid(rewritten) {
		t.Fatalf("模型改写必须保留合法 JSON：%s", rewritten)
	}
	if !bytes.Contains(rewritten, []byte(`"input":{"model":"actual"}`)) || !bytes.Contains(rewritten, []byte("9007199254740993")) {
		t.Fatal("工具参数或数字被修改")
	}
	var value struct {
		Model string `json:"model"`
	}
	if err := json.Unmarshal(rewritten, &value); err != nil || value.Model != "claude-public" {
		t.Fatalf("公开 ID 未恢复：%s", rewritten)
	}
	req := httptest.NewRequest("POST", "/v1/messages", strings.NewReader(original))
	if err := rewriteRequestModel(req, "prefix/actual"); err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(req.Body)
	if !json.Valid(body) || !bytes.Contains(body, []byte(`"model" : "prefix/actual"`)) {
		t.Fatalf("请求模型改写损坏：%s", body)
	}
}
func TestSSEOnlyRewritesMessageStartAndJoinsDataLines(t *testing.T) {
	route := modelRoute{publicID: "claude-public", targetModel: "actual", sdkModel: "prefix/actual"}
	event := []byte("event: message_start\r\ndata: {\"type\":\"message_start\",\r\ndata: \"message\":{\"model\":\"actual\",\"content\":[{\"input\":{\"model\":\"actual\"}}]}}\r\n\r\n")
	result := transformSSEEvent(event, route)
	if !bytes.Contains(result, []byte(`"model":"claude-public"`)) || !bytes.Contains(result, []byte(`"input":{"model":"actual"}`)) {
		t.Fatalf("SSE字段改写错误：%s", result)
	}
	other := []byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"delta\":{\"model\":\"actual\",\"text\":\"actual\"}}\n\n")
	if !bytes.Equal(other, transformSSEEvent(other, route)) {
		t.Fatal("非 message_start 事件必须原样保留")
	}
}
func TestFragmentedSSEIsFlushedBeforeStreamEnds(t *testing.T) {
	recorder := httptest.NewRecorder()
	context, _ := gin.CreateTestContext(recorder)
	context.Writer.Header().Set("Content-Type", "text/event-stream")
	writer := newTransformWriter(context.Writer, modelRoute{publicID: "claude-public", targetModel: "actual"})
	event := []byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"model\":\"actual\"}}\n\n")
	for _, b := range event {
		if _, err := writer.Write([]byte{b}); err != nil {
			t.Fatal(err)
		}
	}
	writer.Flush()
	if !recorder.Flushed || !strings.Contains(recorder.Body.String(), "claude-public") {
		t.Fatal("完整事件必须在 finish 前立即输出并 flush")
	}
	writer.finish()
	if recorder.Code != http.StatusOK {
		t.Fatal(recorder.Code)
	}
}
func TestRequestDuplicateModelRejected(t *testing.T) {
	for _, body := range []string{`{"model":"a","model":"b"}`, `{"model":"a"} {}`, `{"content":{"model":"a"}}`} {
		if _, _, _, err := findRootModel([]byte(body)); err == nil {
			t.Fatalf("无效请求未拒绝：%s", body)
		}
	}
}
