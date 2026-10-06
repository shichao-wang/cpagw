package provider

import (
	"errors"
	"fmt"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

var errRuntimeLockRequired = errors.New("该操作需要网关停止")

// CreateProvider 仅创建提供商容器，协议、地址和模型由连接单独配置。
func CreateProvider(st *store.Store, name, key string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	if key != "" {
		if err := config.ValidateKey(key); err != nil {
			return err
		}
	}
	return st.Update(func(s *config.State) error {
		if _, ok := s.Providers[name]; ok {
			return fmt.Errorf("提供商已存在：%s", name)
		}
		p := config.Provider{Name: name, Connections: map[string]config.Connection{}}
		if key != "" {
			ref, err := newSecretRef()
			if err != nil {
				return err
			}
			p.DefaultCredentialRef = ref
			s.Secrets[ref] = key
		}
		s.Providers[name] = p
		return nil
	})
}

// PatchConnection 保留原 API key patch 语义，不允许隐式切换认证方式。
func PatchConnection(st *store.Store, pname, cname string, base *string, models *[]config.Model, key *string, inherit bool) error {
	return PatchConnectionWithAuth(st, pname, cname, base, models, key, inherit, nil)
}

// PatchConnectionWithAuth 同事务更新连接配置；认证方式变更必须显式指定。
func PatchConnectionWithAuth(st *store.Store, pname, cname string, base *string, models *[]config.Model, key *string, inherit bool, authType *string) error {
	if inherit && key != nil {
		return fmt.Errorf("不能同时覆盖和继承 API key")
	}
	if key != nil {
		if err := config.ValidateKey(*key); err != nil {
			return err
		}
	}
	err := patchConnectionUpdate(st, pname, cname, base, models, key, inherit, authType, false)
	if !errors.Is(err, errRuntimeLockRequired) {
		return err
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return patchConnectionUpdate(st, pname, cname, base, models, key, inherit, authType, true)
}

func patchConnectionUpdate(st *store.Store, pname, cname string, base *string, models *[]config.Model, key *string, inherit bool, authType *string, hasRuntimeLock bool) error {
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[pname]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", pname)
		}
		c, ok := p.Connections[cname]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", pname, cname)
		}
		oldAuth := c.AuthType
		if authType != nil && *authType != config.AuthAPIKey && *authType != config.AuthCodexOAuth {
			return fmt.Errorf("连接必须显式声明有效的认证方式")
		}
		if authType != nil && *authType != oldAuth && (oldAuth == config.AuthCodexOAuth || *authType == config.AuthCodexOAuth) && !hasRuntimeLock {
			return errRuntimeLockRequired
		}
		if authType != nil {
			if *authType != oldAuth && *authType == config.AuthAPIKey && key == nil && !inherit {
				return fmt.Errorf("切换为 API key 时必须显式提供 key 或选择继承")
			}
			c.AuthType = *authType
		} else if key != nil || inherit {
			if oldAuth != config.AuthAPIKey {
				return fmt.Errorf("OAuth 连接不能通过 API key 接口修改凭证")
			}
		}
		if base != nil {
			c.BaseURL = *base
		}
		if models != nil {
			available := map[string]bool{}
			for _, m := range *models {
				available[m.ID] = true
			}
			for _, profileName := range s.References(pname, cname, "") {
				for _, b := range s.Profiles[profileName].Models {
					if b.Provider == pname && b.Connection == cname && !available[b.TargetModel] {
						return fmt.Errorf("模型仍被 profile %s 使用", profileName)
					}
				}
			}
			c.Models = append([]config.Model(nil), (*models)...)
		}
		if err := ValidateConnection(c); err != nil {
			return err
		}
		oldRef := c.CredentialRef
		if oldAuth != c.AuthType {
			c.CredentialRef = ""
		}
		if c.AuthType == config.AuthAPIKey {
			if inherit {
				c.CredentialRef = ""
			}
			if key != nil {
				ref, err := newSecretRef()
				if err != nil {
					return err
				}
				c.CredentialRef = ref
				s.Secrets[ref] = *key
			}
			if _, err := s.Key(pname, c); err != nil {
				return err
			}
		} else if key != nil || inherit {
			return fmt.Errorf("Codex OAuth 不能设置或继承 API key")
		}
		p.Connections[cname] = c
		s.Providers[pname] = p
		if oldRef != c.CredentialRef {
			deleteIfUnreferenced(s, oldRef)
			deleteIfOAuthUnreferenced(s, oldRef)
		}
		return nil
	})
}

func SetConnectionKey(st *store.Store, providerName, connectionName, apiKey string) error {
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		c, ok := p.Connections[connectionName]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", providerName, connectionName)
		}
		if c.AuthType != config.AuthAPIKey {
			return fmt.Errorf("OAuth 连接不能设置 API key")
		}
		oldRef := c.CredentialRef
		if apiKey == "" {
			c.CredentialRef = ""
		} else {
			if err := config.ValidateKey(apiKey); err != nil {
				return err
			}
			ref, err := newSecretRef()
			if err != nil {
				return err
			}
			c.CredentialRef = ref
			s.Secrets[ref] = apiKey
		}
		p.Connections[connectionName] = c
		s.Providers[providerName] = p
		deleteIfUnreferenced(s, oldRef)
		return nil
	})
}

func RemoveConnection(st *store.Store, providerName, connectionName string) error {
	err := removeConnectionUpdate(st, providerName, connectionName, false)
	if !errors.Is(err, errRuntimeLockRequired) {
		return err
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return removeConnectionUpdate(st, providerName, connectionName, true)
}
func removeConnectionUpdate(st *store.Store, providerName, connectionName string, hasRuntimeLock bool) error {
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		c, ok := p.Connections[connectionName]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", providerName, connectionName)
		}
		if c.AuthType == config.AuthCodexOAuth && !hasRuntimeLock {
			return errRuntimeLockRequired
		}
		if refs := s.References(providerName, connectionName, ""); len(refs) != 0 {
			return fmt.Errorf("连接仍被 profile 使用：%s", strings.Join(refs, ", "))
		}
		delete(p.Connections, connectionName)
		s.Providers[providerName] = p
		deleteIfUnreferenced(s, c.CredentialRef)
		deleteIfOAuthUnreferenced(s, c.CredentialRef)
		return nil
	})
}
func Remove(st *store.Store, providerName string) error {
	err := removeProviderUpdate(st, providerName, false)
	if !errors.Is(err, errRuntimeLockRequired) {
		return err
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return removeProviderUpdate(st, providerName, true)
}
func removeProviderUpdate(st *store.Store, providerName string, hasRuntimeLock bool) error {
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		for _, c := range p.Connections {
			if c.AuthType == config.AuthCodexOAuth && !hasRuntimeLock {
				return errRuntimeLockRequired
			}
		}
		if refs := s.References(providerName, "", ""); len(refs) != 0 {
			return fmt.Errorf("提供商仍被 profile 使用：%s", strings.Join(refs, ", "))
		}
		refs := []string{p.DefaultCredentialRef}
		for _, c := range p.Connections {
			refs = append(refs, c.CredentialRef)
			if c.AuthType == config.AuthCodexOAuth {
				delete(s.OAuthCredentials, c.CredentialRef)
			}
		}
		delete(s.Providers, providerName)
		for _, ref := range refs {
			deleteIfUnreferenced(s, ref)
			deleteIfOAuthUnreferenced(s, ref)
		}
		return nil
	})
}
