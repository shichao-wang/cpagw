package provider

import (
	"fmt"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

type OAuthSession struct {
	store                                                      *store.Store
	lock                                                       *store.RunLock
	provider, connection, connectionID, baseURL, credentialRef string
	closed                                                     bool
}

// BeginOAuth 持有与网关共用的 runtime 锁，并在不持有 state.lock 时记录登录目标。
func BeginOAuth(st *store.Store, providerName, connectionName string) (*OAuthSession, error) {
	lock, err := st.AcquireRunLock()
	if err != nil {
		return nil, err
	}
	state, err := st.Read()
	if err != nil {
		lock.Close()
		return nil, err
	}
	p, ok := state.Providers[providerName]
	if !ok {
		lock.Close()
		return nil, fmt.Errorf("提供商不存在：%s", providerName)
	}
	c, ok := p.Connections[connectionName]
	if !ok {
		lock.Close()
		return nil, fmt.Errorf("连接不存在：%s/%s", providerName, connectionName)
	}
	if c.AuthType != config.AuthCodexOAuth || c.Protocol != config.Responses || c.BaseURL != config.CodexBaseURL {
		lock.Close()
		return nil, fmt.Errorf("连接不是有效的 Codex OAuth 连接")
	}
	return &OAuthSession{store: st, lock: lock, provider: providerName, connection: connectionName, connectionID: c.ID, baseURL: c.BaseURL, credentialRef: c.CredentialRef}, nil
}

// Commit 仅在连接身份、认证方式和地址均未变化时绑定成功登录凭证。
func (s *OAuthSession) Commit(credential config.OAuthCredential) error {
	if s == nil || s.closed {
		return fmt.Errorf("OAuth 会话已关闭")
	}
	if err := config.ValidateOAuthCredential(config.OAuthCredential{AccessToken: credential.AccessToken, RefreshToken: credential.RefreshToken, IDToken: credential.IDToken, AccountID: credential.AccountID, PlanType: credential.PlanType, ExpiresAt: credential.ExpiresAt, LastRefresh: credential.LastRefresh, Generation: 1}); err != nil {
		return err
	}
	ref, err := newSecretRef()
	if err != nil {
		return fmt.Errorf("生成 OAuth 凭证引用失败")
	}
	credential.Generation = 1
	return s.store.Update(func(state *config.State) error {
		p, ok := state.Providers[s.provider]
		if !ok {
			return fmt.Errorf("OAuth 登录目标已删除")
		}
		c, ok := p.Connections[s.connection]
		if !ok || c.ID != s.connectionID || c.AuthType != config.AuthCodexOAuth || c.BaseURL != s.baseURL || c.CredentialRef != s.credentialRef {
			return fmt.Errorf("OAuth 登录目标已变化，请重新登录")
		}
		oldRef := c.CredentialRef
		c.CredentialRef = ref
		state.OAuthCredentials[ref] = credential
		p.Connections[s.connection] = c
		state.Providers[s.provider] = p
		deleteIfOAuthUnreferenced(state, oldRef)
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

func LogoutOAuth(st *store.Store, providerName, connectionName string) error {
	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return st.Update(func(state *config.State) error {
		p, ok := state.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		c, ok := p.Connections[connectionName]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", providerName, connectionName)
		}
		if c.AuthType != config.AuthCodexOAuth {
			return fmt.Errorf("连接未声明 Codex OAuth")
		}
		ref := c.CredentialRef
		c.CredentialRef = ""
		p.Connections[connectionName] = c
		state.Providers[providerName] = p
		delete(state.OAuthCredentials, ref)
		return nil
	})
}

func deleteIfOAuthUnreferenced(s *config.State, ref string) {
	if ref == "" {
		return
	}
	for _, p := range s.Providers {
		for _, c := range p.Connections {
			if c.AuthType == config.AuthCodexOAuth && c.CredentialRef == ref {
				return
			}
		}
	}
	delete(s.OAuthCredentials, ref)
}
