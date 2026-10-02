package provider

import (
	"fmt"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

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

// PatchConnection 以同一事务更新连接，防止新地址与旧 key 的中间状态被运行网关加载。
func PatchConnection(st *store.Store, pname, cname string, base *string, models *[]config.Model, key *string, inherit bool) error {
	if inherit && key != nil {
		return fmt.Errorf("不能同时覆盖和继承 API key")
	}
	if key != nil {
		if err := config.ValidateKey(*key); err != nil {
			return err
		}
	}
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[pname]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", pname)
		}
		c, ok := p.Connections[cname]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", pname, cname)
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
		p.Connections[cname] = c
		s.Providers[pname] = p
		deleteIfUnreferenced(s, oldRef)
		return nil
	})
}
