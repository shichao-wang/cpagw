package connection

import (
	"fmt"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

type OAuthSession struct {
	store         *store.Store
	lock          *store.RunLock
	name          string
	connectionID  string
	baseURL       string
	credentialRef string
	closed        bool
}

// BeginOAuth 在不持有 state.lock 的情况下获取 runtime 锁并固定登录目标。
func BeginOAuth(st *store.Store, name string) (*OAuthSession, error) {
	lock, err := st.AcquireRunLock()
	if err != nil {
		return nil, err
	}
	state, err := st.Read()
	if err != nil {
		_ = lock.Close()
		return nil, err
	}
	c, ok := state.Connections[name]
	if !ok {
		_ = lock.Close()
		return nil, fmt.Errorf("连接不存在：%s", name)
	}
	if c.AuthType != config.AuthCodexOAuth || c.Protocol != config.Responses || c.BaseURL != config.CodexBaseURL {
		_ = lock.Close()
		return nil, fmt.Errorf("连接不是有效的 Codex OAuth 连接")
	}
	return &OAuthSession{store: st, lock: lock, name: name, connectionID: c.ID, baseURL: c.BaseURL, credentialRef: c.CredentialRef}, nil
}

// Commit 仅在连接 ID、认证方式、endpoint 与原凭证引用均未变化时保存登录结果。
func (s *OAuthSession) Commit(credential config.OAuthCredential) error {
	if s == nil || s.closed {
		return fmt.Errorf("OAuth 会话已关闭")
	}
	credential.Generation = 1
	if err := config.ValidateOAuthCredential(credential); err != nil {
		return err
	}
	ref, err := newSecretRef()
	if err != nil {
		return fmt.Errorf("生成 OAuth 凭证引用失败")
	}
	return s.store.Update(func(state *config.State) error {
		c, ok := state.Connections[s.name]
		if !ok {
			return fmt.Errorf("OAuth 登录目标已删除")
		}
		if c.ID != s.connectionID || c.AuthType != config.AuthCodexOAuth || c.BaseURL != s.baseURL || c.CredentialRef != s.credentialRef {
			return fmt.Errorf("OAuth 登录目标已变化，请重新登录")
		}
		oldRef := c.CredentialRef
		c.CredentialRef = ref
		state.OAuthCredentials[ref] = credential
		state.Connections[s.name] = c
		deleteOAuthIfUnreferenced(state, oldRef)
		return nil
	})
}

func (s *OAuthSession) Close() error {
	if s == nil || s.closed {
		return nil
	}
	s.closed = true
	return s.lock.Close()
}

// LogoutOAuth 只删除本地 OAuth 凭证，不向服务端撤销授权。
func LogoutOAuth(st *store.Store, name string) error {
	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return st.Update(func(state *config.State) error {
		c, ok := state.Connections[name]
		if !ok {
			return fmt.Errorf("连接不存在：%s", name)
		}
		if c.AuthType != config.AuthCodexOAuth {
			return fmt.Errorf("连接未声明 Codex OAuth")
		}
		ref := c.CredentialRef
		c.CredentialRef = ""
		state.Connections[name] = c
		delete(state.OAuthCredentials, ref)
		return nil
	})
}
