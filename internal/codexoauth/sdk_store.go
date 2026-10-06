package codexoauth

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

var ErrDeleteUnsupported = errors.New("SDK 不得删除 cpagw 管理的 OAuth 凭证")

// SDKStore 将 cpagw 的强类型 OAuth 状态适配为 SDK 公开的 coreauth.Store。
// 它不会读取或写入 SDK 认证文件。
type SDKStore struct {
	state     *store.Store
	namespace string

	mu       sync.RWMutex
	manager  *coreauth.Manager
	bad      map[string]error
	inflight map[string]map[uint64]context.CancelFunc
	nextID   uint64
}

// NewSDKStore 创建非空的私有 SDK 认证存储。进程级命名空间可防止 SDK 全局模型注册表
// 在不同网关实例间复用客户端。
func NewSDKStore(st *store.Store, namespace string) *SDKStore {
	if namespace == "" {
		var bytes [12]byte
		if _, err := rand.Read(bytes[:]); err == nil {
			namespace = hex.EncodeToString(bytes[:])
		} else {
			namespace = fmt.Sprintf("process-%d", time.Now().UnixNano())
		}
	}
	return &SDKStore{
		state: st, namespace: namespace,
		bad: make(map[string]error), inflight: make(map[string]map[uint64]context.CancelFunc),
	}
}

// AuthID 返回持久化凭证引用对应的稳定运行时 ID。
func (s *SDKStore) AuthID(ref string) string {
	if s == nil {
		return ""
	}
	ref = strings.TrimSpace(ref)
	if ref == "" {
		return ""
	}
	sum := sha256.Sum256([]byte(ref))
	return "cpagw:" + s.namespace + ":" + hex.EncodeToString(sum[:])
}

// BindManager 将失败处理逻辑连接到 SDK CoreManager。
func (s *SDKStore) BindManager(manager *coreauth.Manager) {
	if s == nil {
		return
	}
	s.mu.Lock()
	s.manager = manager
	s.mu.Unlock()
}

// Hook 返回 SDK manager 使用的同步钩子。
func (s *SDKStore) Hook() coreauth.Hook { return s }

// Healthy 返回该 Auth 是否发生过凭证持久化失败。
func (s *SDKStore) Healthy(authID string) bool {
	if s == nil {
		return false
	}
	s.mu.RLock()
	_, failed := s.bad[authID]
	s.mu.RUnlock()
	return !failed
}

// Track 将请求上下文关联到 Auth，以便后续持久化失败时同步取消
// 所有正在使用该不可信凭证的请求。
func (s *SDKStore) Track(ctx context.Context, authID string) (context.Context, func(), error) {
	if s == nil || strings.TrimSpace(authID) == "" {
		return nil, nil, errors.New("OAuth Auth 无效")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	s.mu.Lock()
	if _, failed := s.bad[authID]; failed {
		s.mu.Unlock()
		return nil, nil, errors.New("OAuth 凭证状态不健康")
	}
	tracked, cancel := context.WithCancel(ctx)
	s.nextID++
	id := s.nextID
	if s.inflight[authID] == nil {
		s.inflight[authID] = make(map[uint64]context.CancelFunc)
	}
	s.inflight[authID][id] = cancel
	s.mu.Unlock()
	var once sync.Once
	release := func() {
		once.Do(func() {
			s.mu.Lock()
			if requests := s.inflight[authID]; requests != nil {
				delete(requests, id)
				if len(requests) == 0 {
					delete(s.inflight, authID)
				}
			}
			s.mu.Unlock()
			cancel()
		})
	}
	return tracked, release, nil
}

// List 从 cpagw 唯一事实源中构造所有已登录的 OAuth 连接。
func (s *SDKStore) List(ctx context.Context) ([]*coreauth.Auth, error) {
	if s == nil || s.state == nil {
		return nil, errors.New("cpagw OAuth 状态存储未配置")
	}
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
	}
	state, err := s.state.Read()
	if err != nil {
		return nil, err
	}
	var records []*coreauth.Auth
	for providerName, provider := range state.Providers {
		for connectionName, connection := range provider.Connections {
			if connection.AuthType != config.AuthCodexOAuth || connection.CredentialRef == "" {
				continue
			}
			credential, ok := state.OAuthCredentials[connection.CredentialRef]
			if !ok || config.ValidateOAuthCredential(credential) != nil {
				continue
			}
			authID := s.AuthID(connection.CredentialRef)
			if !s.Healthy(authID) {
				continue
			}
			metadata := map[string]any{
				"access_token":         credential.AccessToken,
				"refresh_token":        credential.RefreshToken,
				"id_token":             credential.IDToken,
				"account_id":           credential.AccountID,
				"plan_type":            credential.PlanType,
				"expired":              credential.ExpiresAt.UTC().Format(time.RFC3339Nano),
				"last_refresh":         credential.LastRefresh.UTC().Format(time.RFC3339Nano),
				"cpagw_generation":     credential.Generation,
				"cpagw_connection_id":  connection.ID,
				"cpagw_provider":       providerName,
				"cpagw_connection":     connectionName,
				"cpagw_credential_ref": connection.CredentialRef,
			}
			records = append(records, &coreauth.Auth{
				ID:       authID,
				Provider: "codex",
				Prefix:   config.ConnectionPrefix(providerName, connectionName),
				Status:   coreauth.StatusActive,
				Attributes: map[string]string{
					"base_url":  config.CodexBaseURL,
					"plan_type": credential.PlanType,
				},
				Metadata: metadata,
			})
		}
	}
	return records, nil
}

// Save 仅在令牌轮换时执行条件持久化；忽略 SDK 的状态记录快照。
func (s *SDKStore) Save(ctx context.Context, auth *coreauth.Auth) (string, error) {
	if s == nil || s.state == nil || auth == nil {
		return "", errors.New("cpagw OAuth 状态存储未配置")
	}
	if !s.ownsAuthID(auth.ID) {
		return "", ErrDeleteUnsupported
	}
	ref := strings.TrimSpace(stringValue(auth.Metadata, "cpagw_credential_ref"))
	if ref == "" || auth.ID != s.AuthID(ref) {
		return s.fail(auth.ID, errors.New("OAuth 凭证绑定信息无效"))
	}
	binding := store.OAuthBinding{
		Provider:      strings.TrimSpace(stringValue(auth.Metadata, "cpagw_provider")),
		Connection:    strings.TrimSpace(stringValue(auth.Metadata, "cpagw_connection")),
		ConnectionID:  strings.TrimSpace(stringValue(auth.Metadata, "cpagw_connection_id")),
		CredentialRef: ref,
		Generation:    uintValue(auth.Metadata["cpagw_generation"]),
	}
	if binding.Provider == "" || binding.Connection == "" || binding.ConnectionID == "" || binding.Generation == 0 {
		return s.fail(auth.ID, errors.New("OAuth 凭证绑定信息无效"))
	}
	state, err := s.state.Read()
	if err != nil {
		return s.fail(auth.ID, errors.New("读取 OAuth 状态失败"))
	}
	provider, providerOK := state.Providers[binding.Provider]
	connection, connectionOK := provider.Connections[binding.Connection]
	if !providerOK || !connectionOK || connection.ID != binding.ConnectionID || connection.AuthType != config.AuthCodexOAuth || connection.BaseURL != config.CodexBaseURL || connection.CredentialRef != ref {
		return s.fail(auth.ID, errors.New("OAuth 凭证绑定已变化"))
	}
	previous, ok := state.OAuthCredentials[ref]
	if !ok {
		return s.fail(auth.ID, errors.New("OAuth 凭证版本已变化"))
	}
	credential := previous
	credential.AccessToken = stringValue(auth.Metadata, "access_token")
	credential.RefreshToken = stringValue(auth.Metadata, "refresh_token")
	if value := stringValue(auth.Metadata, "id_token"); value != "" {
		credential.IDToken = value
	}
	if value := stringValue(auth.Metadata, "account_id"); value != "" {
		credential.AccountID = value
	}
	if value := stringValue(auth.Metadata, "plan_type"); value != "" {
		credential.PlanType = value
	}
	if value, parseErr := metadataTime(auth.Metadata["expired"]); parseErr == nil && !value.IsZero() {
		credential.ExpiresAt = value
	}
	if value, parseErr := metadataTime(auth.Metadata["last_refresh"]); parseErr == nil && !value.IsZero() {
		credential.LastRefresh = value
	}
	tokensChanged := !sameCredentialTokens(previous, credential)
	if previous.Generation != binding.Generation {
		if tokensChanged {
			return s.fail(auth.ID, errors.New("OAuth 凭证版本已变化"))
		}
		if credential.AccountID != previous.AccountID {
			return s.fail(auth.ID, errors.New("OAuth 账号身份已变化"))
		}
		if ctx != nil {
			if err := ctx.Err(); err != nil {
				return "", err
			}
		}
		// 这是过期的状态快照：不写回任何数据，只把 SDK 输入恢复到
		// 持久凭证，避免后续更新继续使用旧 CAS generation 或旧元数据。
		restoreCredentialMetadata(auth, previous)
		return auth.ID, nil
	}
	changed := !sameCredential(previous, credential)
	if ctx != nil {
		if err := ctx.Err(); err != nil {
			if changed {
				return s.fail(auth.ID, errors.New("OAuth 凭证已轮换但保存操作被取消"))
			}
			return "", err
		}
	}
	if !changed {
		return auth.ID, nil
	}
	generation, err := s.state.SaveOAuth(binding, credential)
	if err != nil {
		return s.fail(auth.ID, errors.New("OAuth 凭证无法安全保存"))
	}
	// SDK 会在同一个可变 Metadata 对象中收到更新后的 cpagw generation，
	// 使后续刷新使用已保存的 CAS 版本，而不是过期状态。
	auth.Metadata["cpagw_generation"] = generation
	return auth.ID, nil
}

// Delete 明确不支持；只能通过 cpagw 领域操作删除 OAuth 凭证。
func (s *SDKStore) Delete(context.Context, string) error { return ErrDeleteUnsupported }

func (s *SDKStore) ownsAuthID(authID string) bool {
	prefix := "cpagw:" + s.namespace + ":"
	if !strings.HasPrefix(authID, prefix) {
		return false
	}
	digest, err := hex.DecodeString(strings.TrimPrefix(authID, prefix))
	return err == nil && len(digest) == sha256.Size
}

func (s *SDKStore) fail(authID string, cause error) (string, error) {
	s.mu.Lock()
	if _, exists := s.bad[authID]; !exists {
		s.bad[authID] = cause
	}
	for _, cancel := range s.inflight[authID] {
		cancel()
	}
	delete(s.inflight, authID)
	manager := s.manager
	s.mu.Unlock()
	if manager != nil {
		manager.Remove(context.Background(), authID)
	}
	cliproxy.GlobalModelRegistry().UnregisterClient(authID)
	return "", cause
}

func (s *SDKStore) syncOrRemove(ctx context.Context, auth *coreauth.Auth) {
	if auth == nil {
		return
	}
	if !s.Healthy(auth.ID) {
		s.mu.RLock()
		manager := s.manager
		s.mu.RUnlock()
		if manager != nil {
			manager.Remove(ctx, auth.ID)
		}
		cliproxy.GlobalModelRegistry().UnregisterClient(auth.ID)
		return
	}
	// Manager 会先安装 Auth 的副本，再调用 Store.Save。如果 Save 推进了
	// cpagw 令牌 generation，也必须将该记录同步回 manager 当前记录，且不能递归持久化。
	s.mu.RLock()
	manager := s.manager
	s.mu.RUnlock()
	if manager == nil {
		return
	}
	current, ok := manager.GetByID(auth.ID)
	if !ok || !sameRuntimeTokens(current, auth) {
		return
	}
	if current.Metadata == nil {
		current.Metadata = make(map[string]any)
	}
	credentialFields := []string{
		"access_token", "refresh_token", "id_token", "account_id", "plan_type",
		"expired", "last_refresh", "cpagw_generation",
	}
	changed := false
	for _, key := range credentialFields {
		if current.Metadata[key] != auth.Metadata[key] {
			current.Metadata[key] = auth.Metadata[key]
			changed = true
		}
	}
	if !changed {
		return
	}
	if ctx == nil {
		ctx = context.Background()
	}
	if _, err := manager.Update(coreauth.WithSkipPersist(ctx), current); err != nil {
		_, _ = s.fail(auth.ID, errors.New("OAuth 运行时凭证版本同步失败"))
	}
}

func (s *SDKStore) OnAuthRegistered(ctx context.Context, auth *coreauth.Auth) {
	s.syncOrRemove(ctx, auth)
}
func (s *SDKStore) OnAuthUpdated(ctx context.Context, auth *coreauth.Auth) { s.syncOrRemove(ctx, auth) }
func (s *SDKStore) OnResult(context.Context, coreauth.Result)              {}

func stringValue(values map[string]any, key string) string {
	if values == nil {
		return ""
	}
	value, _ := values[key].(string)
	return strings.TrimSpace(value)
}

func restoreCredentialMetadata(auth *coreauth.Auth, credential config.OAuthCredential) {
	if auth == nil {
		return
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = credential.AccessToken
	auth.Metadata["refresh_token"] = credential.RefreshToken
	auth.Metadata["id_token"] = credential.IDToken
	auth.Metadata["account_id"] = credential.AccountID
	auth.Metadata["plan_type"] = credential.PlanType
	auth.Metadata["expired"] = credential.ExpiresAt.UTC().Format(time.RFC3339Nano)
	auth.Metadata["last_refresh"] = credential.LastRefresh.UTC().Format(time.RFC3339Nano)
	auth.Metadata["cpagw_generation"] = credential.Generation
}

func uintValue(value any) uint64 {
	switch value := value.(type) {
	case uint64:
		return value
	case uint:
		return uint64(value)
	case int:
		if value > 0 {
			return uint64(value)
		}
	case int64:
		if value > 0 {
			return uint64(value)
		}
	case float64:
		if value > 0 {
			return uint64(value)
		}
	case json.Number:
		parsed, _ := value.Int64()
		if parsed > 0 {
			return uint64(parsed)
		}
	}
	return 0
}

func metadataTime(value any) (time.Time, error) {
	switch value := value.(type) {
	case time.Time:
		return value.UTC(), nil
	case string:
		return parseTimestamp(value)
	case json.Number:
		seconds, err := value.Int64()
		if err == nil && seconds > 0 {
			return time.Unix(seconds, 0).UTC(), nil
		}
	}
	return time.Time{}, errors.New("时间戳格式无效")
}

func sameCredential(a, b config.OAuthCredential) bool {
	return sameCredentialTokens(a, b) && a.AccountID == b.AccountID && a.PlanType == b.PlanType &&
		a.ExpiresAt.Equal(b.ExpiresAt) && a.LastRefresh.Equal(b.LastRefresh)
}

func sameCredentialTokens(a, b config.OAuthCredential) bool {
	return a.AccessToken == b.AccessToken && a.RefreshToken == b.RefreshToken && a.IDToken == b.IDToken
}

func sameRuntimeTokens(a, b *coreauth.Auth) bool {
	if a == nil || b == nil {
		return false
	}
	for _, key := range []string{"access_token", "refresh_token", "id_token"} {
		if stringValue(a.Metadata, key) != stringValue(b.Metadata, key) {
			return false
		}
	}
	return true
}
