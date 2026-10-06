package gateway

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/gin-gonic/gin"
)

func TestOAuthWriterRejectsBufferedSuccessAfterCredentialFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := newTransformWriter(c.Writer, modelRoute{})
	var healthy atomic.Bool
	healthy.Store(true)
	writer.healthy = healthy.Load
	writer.Header().Set("Content-Type", "application/json")
	if _, err := writer.Write([]byte(`{"content":"untrusted-success"}`)); err != nil {
		t.Fatal(err)
	}
	healthy.Store(false)
	if _, err := writer.Write([]byte(`{"content":"late-success"}`)); err == nil {
		t.Fatal("凭证失败后仍接受新的响应正文")
	}
	writer.finish()
	if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "success") {
		t.Fatal("凭证失败后仍向下游发送缓冲的成功响应")
	}
}

// 模拟收到 WriteHeader 就提交响应头的底层 writer，不能依赖 Gin 当前延迟提交的实现。
type eagerHeaderWriter struct {
	gin.ResponseWriter
}

func (w *eagerHeaderWriter) WriteHeader(code int) {
	w.ResponseWriter.WriteHeader(code)
	w.ResponseWriter.WriteHeaderNow()
}

func TestOAuthWriterDelaysJSONHeaderUntilFinish(t *testing.T) {
	for _, fail := range []bool{false, true} {
		for _, eager := range []bool{false, true} {
			t.Run(fmt.Sprintf("fail=%t/eager=%t", fail, eager), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				var base gin.ResponseWriter = c.Writer
				if eager {
					base = &eagerHeaderWriter{ResponseWriter: base}
				}
				writer := newTransformWriter(base, modelRoute{publicID: "claude-public", targetModel: "upstream-model"})
				var healthy atomic.Bool
				healthy.Store(true)
				writer.healthy = healthy.Load
				writer.Header().Set("Content-Type", "application/json")
				writer.WriteHeader(http.StatusCreated)
				if base.Written() || base.Status() != http.StatusOK || writer.Status() != http.StatusCreated {
					t.Fatal("显式状态码在最终健康检查前传给了底层 writer")
				}
				writer.WriteHeaderNow()
				if _, err := writer.WriteString(`{"model":"upstream-model","content":"buffered-success"}`); err != nil {
					t.Fatal(err)
				}
				writer.Flush()
				if base.Written() || recorder.Body.Len() != 0 || recorder.Flushed {
					t.Fatal("JSON 响应在 finish 前提交了响应头或正文")
				}
				healthy.Store(!fail)
				writer.finish()
				if fail {
					if recorder.Code != http.StatusServiceUnavailable || !strings.Contains(recorder.Body.String(), "OAuth") || strings.Contains(recorder.Body.String(), "buffered-success") {
						t.Fatal("凭证失效后没有返回脱敏的 503 错误")
					}
				} else if recorder.Code != http.StatusCreated || recorder.Body.String() != `{"model":"claude-public","content":"buffered-success"}` {
					t.Fatal("健康 JSON 响应没有保留显式状态或正确改写模型")
				}
			})
		}
	}
}

func TestWriterKeepsSSEHeaderCommitBoundary(t *testing.T) {
	for _, oauth := range []bool{false, true} {
		t.Run(fmt.Sprintf("oauth=%t", oauth), func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			base := &eagerHeaderWriter{ResponseWriter: c.Writer}
			writer := newTransformWriter(base, modelRoute{})
			if oauth {
				writer.healthy = func() bool { return true }
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.WriteHeader(http.StatusAccepted)
			if base.Written() {
				t.Fatal("SSE 状态码在流式提交边界前写出")
			}
			writer.WriteHeaderNow()
			writer.Flush()
			if base.Written() == oauth {
				t.Fatal("非 OAuth SSE 应立即提交，OAuth 空流应继续等待完整事件")
			}
			event := "event: content_block_delta\ndata: {\"text\":\"trusted\"}\n\n"
			if _, err := writer.WriteString(event); err != nil {
				t.Fatal(err)
			}
			writer.Flush()
			if recorder.Code != http.StatusAccepted || recorder.Body.String() != event || !recorder.Flushed {
				t.Fatal("完整 SSE 事件没有按原流式边界提交")
			}
			writer.finish()
		})
	}
}

func TestOAuthWriterRejectsUncommittedResponseAfterCredentialFailure(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			writer := newTransformWriter(c.Writer, modelRoute{})
			writer.healthy = func() bool { return false }
			writer.Header().Set("Content-Type", contentType)
			writer.Header().Set("Content-Length", "100")
			writer.Header().Set("Content-Encoding", "gzip")
			writer.WriteHeader(http.StatusOK)
			writer.WriteHeaderNow()
			writer.Flush()
			if n, err := writer.WriteString("late-success"); n != 0 || err == nil {
				t.Fatal("凭证失败后仍接受响应正文")
			}
			writer.finish()
			if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "late-success") {
				t.Fatal("未提交的响应没有替换为失败结果")
			}
			if recorder.Header().Get("Content-Length") != "" || recorder.Header().Get("Content-Encoding") != "" {
				t.Fatal("错误响应保留了上游正文长度或编码")
			}
		})
	}
}

func TestOAuthWriterDelaysEmptySSECommitUntilFinish(t *testing.T) {
	for _, pending := range []string{"", "event: content_block_delta\ndata: {\"text\":\"pending\"}"} {
		for _, fail := range []bool{false, true} {
			t.Run(fmt.Sprintf("pending=%t/fail=%t", pending != "", fail), func(t *testing.T) {
				recorder := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(recorder)
				writer := newTransformWriter(c.Writer, modelRoute{})
				var healthy atomic.Bool
				healthy.Store(true)
				writer.healthy = healthy.Load
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeaderNow()
				if _, err := writer.WriteString(pending); err != nil {
					t.Fatal(err)
				}
				writer.Flush()
				if c.Writer.Written() || recorder.Flushed {
					t.Fatal("没有完整事件的 OAuth SSE 提前提交了成功状态")
				}
				healthy.Store(!fail)
				writer.finish()
				if fail {
					if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "pending") {
						t.Fatal("空 SSE 在凭证失败后仍返回成功或待发事件")
					}
				} else if recorder.Code != http.StatusOK || recorder.Body.String() != pending || !recorder.Flushed {
					t.Fatal("健康 SSE 正常结束时没有完成提交")
				}
			})
		}
	}
}

func TestOAuthWriterRejectsFailureBetweenFinishAndCommitChecks(t *testing.T) {
	for _, contentType := range []string{"application/json", "text/event-stream", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			recorder := httptest.NewRecorder()
			c, _ := gin.CreateTestContext(recorder)
			writer := newTransformWriter(c.Writer, modelRoute{})
			writer.Header().Set("Content-Type", contentType)
			writer.healthy = func() bool { return true }
			if contentType != "text/plain" {
				if _, err := writer.WriteString(`{"text":"pending"}`); err != nil {
					t.Fatal(err)
				}
			}
			checks := 0
			writer.healthy = func() bool {
				checks++
				return checks == 1
			}
			writer.finish()
			if recorder.Code != http.StatusServiceUnavailable || strings.Contains(recorder.Body.String(), "pending") {
				t.Fatal("提交检查期间失效仍返回默认成功响应")
			}
		})
	}
}

func TestOAuthWriterStopsSSEAndDropsPendingEventAfterCredentialFailure(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	writer := newTransformWriter(c.Writer, modelRoute{})
	var healthy atomic.Bool
	healthy.Store(true)
	writer.healthy = healthy.Load
	writer.Header().Set("Content-Type", "text/event-stream")
	if _, err := writer.Write([]byte("event: content_block_delta\ndata: {\"text\":\"trusted\"}\n\n")); err != nil {
		t.Fatal(err)
	}
	writer.Flush()
	if _, err := writer.Write([]byte("event: content_block_delta\ndata: {\"text\":\"pending\"}")); err != nil {
		t.Fatal(err)
	}
	before := recorder.Body.String()
	healthy.Store(false)
	if _, err := writer.Write([]byte("\n\nevent: content_block_delta\ndata: {\"text\":\"late\"}\n\n")); err == nil {
		t.Fatal("凭证失败后仍接受 SSE 数据")
	}
	if err := writer.FlushError(); err == nil {
		t.Fatal("凭证失败后仍允许流式刷新")
	}
	writer.finish()
	if recorder.Body.String() != before || !strings.Contains(before, "trusted") || strings.Contains(before, "pending") {
		t.Fatal("SSE 没有保持已发事件，或在失败后发送了待发数据")
	}
}
