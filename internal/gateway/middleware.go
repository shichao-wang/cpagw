package gateway

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	"github.com/shichao-wang/cpagw/internal/codexoauth"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

type requestGate struct {
	store      *store.Store
	active     *atomic.Pointer[snapshot]
	token      string
	instanceID string
	ready      atomic.Bool
	oauth      *codexoauth.SDKStore
}

func (g *requestGate) middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		if !allowedRoute(c.Request.Method, c.Request.URL.Path) {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if c.Request.URL.RawPath != "" {
			c.AbortWithStatus(http.StatusNotFound)
			return
		}
		if c.Request.URL.Path == ReadyPath {
			if c.Request.Method != http.MethodGet || !secureEqual(c.Request.Header.Get(probeHeader), g.token) {
				c.AbortWithStatus(http.StatusNotFound)
				return
			}
			if g.active.Load() == nil {
				c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "网关尚未就绪"})
				return
			}
			c.Next()
			return
		}
		if !g.ready.Load() {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "网关尚未就绪"})
			return
		}
		current := g.active.Load()
		if current == nil {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "网关尚未就绪"})
			return
		}
		latest, err := g.store.Read()
		if err != nil || latest.Revision != current.revision {
			c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"error": "配置正在切换或无法读取，请稍后重试"})
			return
		}
		profileKey, ok := incomingKey(c.Request)
		if !ok {
			writeAuthError(c)
			return
		}
		profileName, ok := current.keyIndex[sha256.Sum256([]byte(profileKey))]
		if !ok {
			writeAuthError(c)
			return
		}
		profile, ok := current.profiles[profileName]
		if !ok {
			writeAuthError(c)
			return
		}
		if c.Request.Method == http.MethodGet && c.Request.URL.Path == "/v1/models" {
			g.writeCatalog(c, profile)
			return
		}
		c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, 32<<20)
		modelID, err := readRootModel(c.Request)
		if err != nil {
			writeRequestError(c, http.StatusBadRequest, "请求必须是包含 model 字段的 JSON 对象")
			return
		}
		route, ok := profile.models[modelID]
		if !ok {
			writeRequestError(c, http.StatusBadRequest, "所选 profile 未开放该模型")
			return
		}
		if route.unavailable != "" {
			writeRequestError(c, http.StatusServiceUnavailable, route.unavailable)
			return
		}
		if route.authType == config.AuthCodexOAuth {
			if g.oauth == nil || !g.oauth.Healthy(route.authID) || !cliproxy.GlobalModelRegistry().ClientSupportsModel(route.authID, route.sdkModel) {
				writeRequestError(c, http.StatusServiceUnavailable, "该 OAuth 凭证或模型不可用，请停止网关后重新登录")
				return
			}
			ctx, release, err := g.oauth.Track(c.Request.Context(), route.authID)
			if err != nil {
				writeRequestError(c, http.StatusServiceUnavailable, "该 OAuth 凭证不可用")
				return
			}
			defer release()
			c.Request = c.Request.WithContext(ctx)
		}
		if err := rewriteRequestModel(c.Request, route.sdkModel); err != nil {
			writeRequestError(c, http.StatusBadRequest, "无法解析 JSON 请求")
			return
		}

		// SDK 内部只接收本进程生成的临时 key；用户 profile key 从此不再传入 SDK。
		c.Request.Header.Del("X-Api-Key")
		c.Request.Header.Del("X-Goog-Api-Key")
		c.Request.Header.Set("Authorization", "Bearer "+current.sdkKey)
		query := c.Request.URL.Query()
		query.Del("key")
		query.Del("auth_token")
		c.Request.URL.RawQuery = query.Encode()

		writer := newTransformWriter(c.Writer, route)
		if route.authType == config.AuthCodexOAuth {
			writer.healthy = func() bool { return g.oauth.Healthy(route.authID) }
		}
		c.Writer = writer
		c.Next()
		writer.finish()
	}
}

func allowedRoute(method, path string) bool {
	switch path {
	case ReadyPath:
		return method == http.MethodGet
	case "/v1/models":
		return method == http.MethodGet
	case "/v1/messages", "/v1/messages/count_tokens":
		return method == http.MethodPost
	default:
		return false
	}
}

func incomingKey(r *http.Request) (string, bool) {
	if len(r.Header.Values("X-Api-Key")) > 1 || len(r.Header.Values("Authorization")) > 1 {
		return "", false
	}
	apiKey := strings.TrimSpace(r.Header.Get("X-Api-Key"))
	authorization := strings.TrimSpace(r.Header.Get("Authorization"))
	bearer := ""
	if authorization != "" {
		parts := strings.SplitN(authorization, " ", 2)
		if len(parts) != 2 || !strings.EqualFold(parts[0], "Bearer") || strings.TrimSpace(parts[1]) == "" {
			return "", false
		}
		bearer = strings.TrimSpace(parts[1])
	}
	if apiKey != "" && bearer != "" && !secureEqual(apiKey, bearer) {
		return "", false
	}
	if apiKey != "" {
		return apiKey, true
	}
	return bearer, bearer != ""
}

func writeAuthError(c *gin.Context) {
	c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": gin.H{
		"type":    "authentication_error",
		"message": "无效的 API key",
	}})
}

func writeRequestError(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{
		"type":    "invalid_request_error",
		"message": message,
	}})
}

func (g *requestGate) writeCatalog(c *gin.Context, profile profileSnapshot) {
	type catalogModel struct {
		ID          string `json:"id"`
		Type        string `json:"type"`
		DisplayName string `json:"display_name"`
		Description string `json:"description,omitempty"`
		CreatedAt   string `json:"created_at"`
	}
	data := make([]catalogModel, 0, len(profile.catalog))
	for _, model := range profile.catalog {
		if model.unavailable != "" {
			continue
		}
		if model.authType == config.AuthCodexOAuth && (g.oauth == nil || !g.oauth.Healthy(model.authID) || !cliproxy.GlobalModelRegistry().ClientSupportsModel(model.authID, model.sdkModel)) {
			continue
		}
		data = append(data, catalogModel{
			ID: model.publicID, Type: "model", DisplayName: model.label,
			Description: model.description, CreatedAt: time.Unix(0, 0).UTC().Format(time.RFC3339),
		})
	}
	first, last := "", ""
	if len(data) > 0 {
		first, last = data[0].ID, data[len(data)-1].ID
	}
	c.JSON(http.StatusOK, gin.H{"data": data, "has_more": false, "first_id": first, "last_id": last})
}

func readRootModel(r *http.Request) (string, error) {
	if r.Body == nil {
		return "", errors.New("empty body")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return "", err
	}
	_ = r.Body.Close()
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	model, _, _, err := findRootModel(data)
	if err != nil {
		return "", err
	}
	return model, nil
}

func rewriteRequestModel(r *http.Request, model string) error {
	if r.Body == nil {
		return errors.New("empty body")
	}
	data, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	_ = r.Body.Close()
	_, start, end, err := findRootModel(data)
	if err != nil {
		return err
	}
	encoded, err := json.Marshal(model)
	if err != nil {
		return err
	}
	data = splice(data, start, end, encoded)
	r.Body = io.NopCloser(bytes.NewReader(data))
	r.ContentLength = int64(len(data))
	r.GetBody = func() (io.ReadCloser, error) { return io.NopCloser(bytes.NewReader(data)), nil }
	return nil
}

// findRootModel 只允许改写请求顶层的 model，避免碰触工具参数或其他嵌套对象。
func findRootModel(data []byte) (string, int, int, error) {
	dec := json.NewDecoder(bytes.NewReader(data))
	token, err := dec.Token()
	if err != nil {
		return "", 0, 0, err
	}
	open, ok := token.(json.Delim)
	if !ok || open != '{' {
		return "", 0, 0, errors.New("root is not object")
	}
	var model string
	var modelStart, modelEnd int
	found := false
	for dec.More() {
		keyToken, err := dec.Token()
		if err != nil {
			return "", 0, 0, err
		}
		key, ok := keyToken.(string)
		if !ok {
			return "", 0, 0, errors.New("object key is not string")
		}
		start := skipJSONSpace(data, int(dec.InputOffset()))
		if start < len(data) && data[start] == ':' {
			start = skipJSONSpace(data, start+1)
		}
		if key == "model" {
			if found {
				return "", 0, 0, errors.New("duplicate model field")
			}
			value, err := dec.Token()
			if err != nil {
				return "", 0, 0, err
			}
			model, ok = value.(string)
			if !ok || strings.TrimSpace(model) == "" {
				return "", 0, 0, errors.New("model must be a non-empty string")
			}
			modelStart, modelEnd = start, int(dec.InputOffset())
			found = true
			continue
		}
		if err := skipJSONValue(dec); err != nil {
			return "", 0, 0, err
		}
	}
	closeToken, err := dec.Token()
	if err != nil {
		return "", 0, 0, err
	}
	if closeToken != json.Delim('}') {
		return "", 0, 0, errors.New("malformed JSON object")
	}
	if !found {
		return "", 0, 0, errors.New("missing model")
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			return "", 0, 0, errors.New("trailing JSON value")
		}
		return "", 0, 0, err
	}
	return model, modelStart, modelEnd, nil
}

func skipJSONValue(dec *json.Decoder) error {
	token, err := dec.Token()
	if err != nil {
		return err
	}
	delim, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	switch delim {
	case '{':
		for dec.More() {
			if _, err := dec.Token(); err != nil {
				return err
			}
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	case '[':
		for dec.More() {
			if err := skipJSONValue(dec); err != nil {
				return err
			}
		}
		_, err = dec.Token()
		return err
	default:
		return fmt.Errorf("unexpected JSON delimiter")
	}
}

func skipJSONSpace(data []byte, offset int) int {
	for offset < len(data) {
		switch data[offset] {
		case ' ', '\t', '\r', '\n':
			offset++
		default:
			return offset
		}
	}
	return offset
}

func splice(data []byte, start, end int, replacement []byte) []byte {
	out := make([]byte, 0, len(data)-(end-start)+len(replacement))
	out = append(out, data[:start]...)
	out = append(out, replacement...)
	out = append(out, data[end:]...)
	return out
}

// transformWriter 仅在响应 JSON/SSE 的结构化 model 字段上做公开 ID 还原。
type transformWriter struct {
	gin.ResponseWriter
	route        modelRoute
	status       int
	mode         int // 0 未判定，1 JSON，2 SSE，3 透传
	jsonBody     bytes.Buffer
	streamBuffer bytes.Buffer
	written      bool
	committed    bool
	healthy      func() bool
}

func (w *transformWriter) checkHealth() error {
	if w.healthy != nil && !w.healthy() {
		return errors.New("OAuth 凭证不可用")
	}
	return nil
}

func newTransformWriter(base gin.ResponseWriter, route modelRoute) *transformWriter {
	return &transformWriter{ResponseWriter: base, route: route, status: http.StatusOK}
}

func (w *transformWriter) WriteHeader(code int) {
	if w.committed || w.checkHealth() != nil {
		return
	}
	// 状态与正文在最终健康检查或 SSE 写出边界统一提交，不依赖底层延迟写头。
	w.status = code
}

func (w *transformWriter) WriteHeaderNow() {
	if w.checkHealth() != nil {
		return
	}
	w.written = true
	if w.chooseMode() == 2 && (w.healthy == nil || w.committed) {
		w.commitHeader()
	}
}

func (w *transformWriter) Write(data []byte) (int, error) {
	if err := w.checkHealth(); err != nil {
		return 0, err
	}
	w.written = true
	switch w.chooseMode() {
	case 1:
		return w.jsonBody.Write(data)
	case 2:
		return w.writeSSE(data)
	default:
		w.commitHeader()
		return w.writeDownstream(data)
	}
}

func (w *transformWriter) WriteString(value string) (int, error) {
	return w.Write([]byte(value))
}

func (w *transformWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

func (w *transformWriter) Size() int {
	if w.mode == 1 {
		return w.jsonBody.Len()
	}
	return w.ResponseWriter.Size()
}

func (w *transformWriter) Written() bool { return w.written || w.ResponseWriter.Written() }

func (w *transformWriter) Flush() {
	if w.checkHealth() != nil {
		return
	}
	if w.chooseMode() == 2 {
		// OAuth 空流延后提交，避免凭证在首个事件前失效却返回空的 200。
		if w.healthy != nil && !w.committed {
			return
		}
		w.commitHeader()
		w.ResponseWriter.Flush()
		return
	}
	if flusher, ok := w.ResponseWriter.(interface{ Flush() }); ok && w.mode == 3 {
		flusher.Flush()
	}
}

func (w *transformWriter) FlushError() error {
	if err := w.checkHealth(); err != nil {
		return err
	}
	w.Flush()
	return nil
}

func (w *transformWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *transformWriter) chooseMode() int {
	if w.mode != 0 {
		return w.mode
	}
	contentType := strings.ToLower(w.Header().Get("Content-Type"))
	switch {
	case strings.Contains(contentType, "text/event-stream"):
		w.mode = 2
	case strings.Contains(contentType, "json"):
		w.mode = 1
	default:
		w.mode = 3
	}
	return w.mode
}

func (w *transformWriter) writeDownstream(data []byte) (int, error) {
	if err := w.checkHealth(); err != nil {
		return 0, err
	}
	return w.ResponseWriter.Write(data)
}

func (w *transformWriter) commitHeader() {
	if w.committed || w.checkHealth() != nil {
		return
	}
	w.committed = true
	w.ResponseWriter.WriteHeader(w.Status())
	w.ResponseWriter.WriteHeaderNow()
}

func (w *transformWriter) writeSSE(data []byte) (int, error) {
	if !w.committed && w.healthy == nil {
		w.commitHeader()
	}
	_, _ = w.streamBuffer.Write(data)
	for {
		pending := w.streamBuffer.Bytes()
		boundary, width := findSSEBoundary(pending)
		if boundary < 0 {
			break
		}
		event := append([]byte(nil), pending[:boundary]...)
		separator := append([]byte(nil), pending[boundary:boundary+width]...)
		w.streamBuffer.Next(boundary + width)
		if err := w.checkHealth(); err != nil {
			return len(data), err
		}
		w.commitHeader()
		if _, err := w.writeDownstream(transformSSEEvent(event, w.route)); err != nil {
			return len(data), err
		}
		if _, err := w.writeDownstream(separator); err != nil {
			return len(data), err
		}
	}
	return len(data), nil
}

func findSSEBoundary(data []byte) (int, int) {
	lf := bytes.Index(data, []byte("\n\n"))
	crlf := bytes.Index(data, []byte("\r\n\r\n"))
	if lf < 0 && crlf < 0 {
		return -1, 0
	}
	if crlf >= 0 && (lf < 0 || crlf <= lf) {
		return crlf, 4
	}
	return lf, 2
}

// transformSSEEvent 按完整事件合并多行 data，只改 message_start.message.model。
func transformSSEEvent(event []byte, route modelRoute) []byte {
	lines := bytes.Split(event, []byte("\n"))
	var values [][]byte
	first := -1
	for index, line := range lines {
		trimmed := bytes.TrimSuffix(line, []byte("\r"))
		if !bytes.HasPrefix(trimmed, []byte("data:")) {
			continue
		}
		if first == -1 {
			first = index
		}
		value := trimmed[len("data:"):]
		if len(value) > 0 && value[0] == ' ' {
			value = value[1:]
		}
		values = append(values, value)
	}
	if first == -1 {
		return event
	}
	payload := bytes.Join(values, []byte("\n"))
	rewritten := rewriteMessageStart(payload, responseModelMap(route))
	if bytes.Equal(payload, rewritten) {
		return event
	}
	var out bytes.Buffer
	for index, line := range lines {
		trimmed := bytes.TrimSuffix(line, []byte("\r"))
		if bytes.HasPrefix(trimmed, []byte("data:")) {
			if index != first {
				continue
			}
			line = append([]byte("data: "), rewritten...)
			if bytes.HasSuffix(lines[index], []byte("\r")) {
				line = append(line, '\r')
			}
		}
		out.Write(line)
		if index < len(lines)-1 {
			out.WriteByte('\n')
		}
	}
	return out.Bytes()
}

func responseModelMap(route modelRoute) map[string]string {
	return map[string]string{route.targetModel: route.publicID, route.sdkModel: route.publicID}
}

// rewriteModelFields 只改写响应顶层 model，不进入正文、工具参数或其他嵌套对象。
func rewriteModelFields(data []byte, mapping map[string]string) []byte {
	model, start, end, err := findRootModel(data)
	if err != nil {
		return data
	}
	replacement, ok := mapping[model]
	if !ok {
		return data
	}
	encoded, err := json.Marshal(replacement)
	if err != nil {
		return data
	}
	return splice(data, start, end, encoded)
}

func rewriteMessageStart(data []byte, mapping map[string]string) []byte {
	var event map[string]json.RawMessage
	if json.Unmarshal(data, &event) != nil {
		return data
	}
	var kind string
	if json.Unmarshal(event["type"], &kind) != nil || kind != "message_start" {
		return data
	}
	message, ok := event["message"]
	if !ok {
		return data
	}
	changed := rewriteModelFields(message, mapping)
	if bytes.Equal(message, changed) {
		return data
	}
	event["message"] = changed
	encoded, err := json.Marshal(event)
	if err != nil {
		return data
	}
	return encoded
}

func (w *transformWriter) rejectUnhealthy() bool {
	if w.checkHealth() == nil {
		return false
	}
	// JSON 尚未提交时替换为脱敏错误；已开始的 SSE 只停止后续输出，不缓冲整条流。
	w.jsonBody.Reset()
	w.streamBuffer.Reset()
	if !w.ResponseWriter.Written() {
		w.status = http.StatusServiceUnavailable
		w.Header().Del("Content-Length")
		w.Header().Del("Content-Encoding")
		w.Header().Set("Content-Type", "application/json")
		w.ResponseWriter.WriteHeader(w.status)
		_, _ = w.ResponseWriter.Write([]byte(`{"error":{"type":"invalid_request_error","message":"该 OAuth 凭证不可用，请停止网关后重新登录"}}`))
	}
	return true
}

func (w *transformWriter) finish() {
	// 提交检查期间也可能失效；未提交时不能让 net/http 默认返回空的 200。
	defer w.rejectUnhealthy()
	if w.rejectUnhealthy() {
		return
	}
	switch w.chooseMode() {
	case 1:
		body := rewriteModelFields(w.jsonBody.Bytes(), responseModelMap(w.route))
		w.Header().Del("Content-Length")
		w.commitHeader()
		_, _ = w.writeDownstream(body)
	case 2:
		w.commitHeader()
		if w.streamBuffer.Len() > 0 {
			pending := w.streamBuffer.Bytes()
			_, _ = w.writeDownstream(transformSSEEvent(pending, w.route))
			w.streamBuffer.Reset()
		}
		w.Flush()
	case 3:
		if !w.committed {
			w.commitHeader()
		}
	}
}

func (w *transformWriter) Pusher() http.Pusher {
	pusher, _ := w.ResponseWriter.(http.Pusher)
	return pusher
}
