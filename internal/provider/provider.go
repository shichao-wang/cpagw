package provider

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shichao-wang/cpa-tui/internal/config"
	"github.com/shichao-wang/cpa-tui/internal/store"
	"gopkg.in/yaml.v3"
)

const (
	checkTimeout    = 8 * time.Second
	maxResponseSize = 64 << 10
)

// ParseModels 严格解析 YAML 模型清单，并拒绝空 ID 与重复 ID。
func ParseModels(r io.Reader) ([]config.Model, error) {
	data, err := io.ReadAll(io.LimitReader(r, (1<<20)+1))
	if err != nil || len(data) > 1<<20 {
		return nil, fmt.Errorf("无法读取模型清单或文件超过 1 MiB")
	}
	var node yaml.Node
	if err := yaml.Unmarshal(data, &node); err != nil || len(node.Content) == 0 {
		return nil, fmt.Errorf("模型清单格式无效")
	}
	dec := yaml.NewDecoder(strings.NewReader(string(data)))
	dec.KnownFields(true)
	var models []config.Model
	if node.Content[0].Kind == yaml.SequenceNode {
		if err := dec.Decode(&models); err != nil {
			return nil, fmt.Errorf("模型清单格式无效")
		}
	} else {
		var document struct {
			Models []config.Model `yaml:"models"`
		}
		if err := dec.Decode(&document); err != nil {
			return nil, fmt.Errorf("模型清单格式无效")
		}
		models = document.Models
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return nil, fmt.Errorf("模型清单只能包含一个 YAML 文档")
	}
	if err := config.ValidateModels(models); err != nil {
		return nil, err
	}
	return models, nil
}

// ValidateConnection 校验连接的名称、协议、根地址和模型清单。
func ValidateConnection(c config.Connection) error {
	if err := config.ValidateName(c.Name); err != nil {
		return err
	}
	switch c.Protocol {
	case config.Chat, config.Anthropic, config.Responses:
	default:
		return fmt.Errorf("不支持的协议：%s", c.Protocol)
	}
	if err := config.ValidateURL(c.BaseURL); err != nil {
		return err
	}
	return config.ValidateModels(c.Models)
}

// Create 创建提供商和其首个连接，API key 只写入状态文件，不返回或记录。
func Create(st *store.Store, name string, conn config.Connection, apiKey string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	if err := ValidateConnection(conn); err != nil {
		return err
	}
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("API key 不能为空")
	}
	return st.Update(func(s *config.State) error {
		if _, ok := s.Providers[name]; ok {
			return fmt.Errorf("提供商已存在：%s", name)
		}
		defaultRef, err := newSecretRef()
		if err != nil {
			return fmt.Errorf("生成凭证引用失败")
		}
		conn.CredentialRef = ""
		s.Providers[name] = config.Provider{
			Name:                 name,
			DefaultCredentialRef: defaultRef,
			Connections:          map[string]config.Connection{conn.Name: conn},
		}
		s.Secrets[defaultRef] = apiKey
		return nil
	})
}

// AddConnection 新增连接；apiKey 为空表示继承提供商默认 key。
func AddConnection(st *store.Store, providerName string, conn config.Connection, apiKey string) error {
	if err := ValidateConnection(conn); err != nil {
		return err
	}
	if apiKey != "" && strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("API key 不能为空")
	}
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		if _, exists := p.Connections[conn.Name]; exists {
			return fmt.Errorf("连接已存在：%s/%s", providerName, conn.Name)
		}
		if apiKey != "" {
			ref, err := newSecretRef()
			if err != nil {
				return fmt.Errorf("生成凭证引用失败")
			}
			conn.CredentialRef = ref
			s.Secrets[ref] = apiKey
		} else {
			conn.CredentialRef = ""
		}
		if p.Connections == nil {
			p.Connections = make(map[string]config.Connection)
		}
		p.Connections[conn.Name] = conn
		s.Providers[providerName] = p
		return nil
	})
}

// UpdateConnection 更新指定字段；协议固定，不支持原地改变。
func UpdateConnection(st *store.Store, providerName, connectionName string, baseURL *string, models *[]config.Model) error {
	if baseURL != nil {
		if err := config.ValidateURL(*baseURL); err != nil {
			return err
		}
	}
	if models != nil {
		if err := config.ValidateModels(*models); err != nil {
			return err
		}
	}
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		c, ok := p.Connections[connectionName]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", providerName, connectionName)
		}
		if models != nil {
			available := make(map[string]bool, len(*models))
			for _, m := range *models {
				available[m.ID] = true
			}
			for _, profileName := range s.References(providerName, connectionName, "") {
				profile := s.Profiles[profileName]
				for _, binding := range profile.Models {
					if binding.Provider == providerName && binding.Connection == connectionName && !available[binding.TargetModel] {
						return fmt.Errorf("无法移除仍被 profile %s 使用的模型：%s", profileName, binding.TargetModel)
					}
				}
			}
			c.Models = append([]config.Model(nil), (*models)...)
		}
		if baseURL != nil {
			c.BaseURL = *baseURL
		}
		p.Connections[connectionName] = c
		s.Providers[providerName] = p
		return nil
	})
}

// RotateDefaultKey 更新提供商默认凭证；显式覆盖 key 的连接保持不变。
func RotateDefaultKey(st *store.Store, providerName, apiKey string) error {
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("API key 不能为空")
	}
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		ref, err := newSecretRef()
		if err != nil {
			return fmt.Errorf("生成凭证引用失败")
		}
		oldRef := p.DefaultCredentialRef
		p.DefaultCredentialRef = ref
		s.Providers[providerName] = p
		s.Secrets[ref] = apiKey
		deleteIfUnreferenced(s, oldRef)
		return nil
	})
}

// SetConnectionKey 设置连接专属 key；apiKey 为空则恢复继承默认 key。
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
		oldRef := c.CredentialRef
		if apiKey == "" {
			c.CredentialRef = ""
		} else {
			if strings.TrimSpace(apiKey) == "" {
				return fmt.Errorf("API key 不能为空")
			}
			ref, err := newSecretRef()
			if err != nil {
				return fmt.Errorf("生成凭证引用失败")
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

// RemoveConnection 拒绝删除仍被 profile 使用的连接。
func RemoveConnection(st *store.Store, providerName, connectionName string) error {
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		c, ok := p.Connections[connectionName]
		if !ok {
			return fmt.Errorf("连接不存在：%s/%s", providerName, connectionName)
		}
		if refs := s.References(providerName, connectionName, ""); len(refs) != 0 {
			return fmt.Errorf("连接仍被 profile 使用：%s", strings.Join(refs, ", "))
		}
		delete(p.Connections, connectionName)
		s.Providers[providerName] = p
		deleteIfUnreferenced(s, c.CredentialRef)
		return nil
	})
}

// Remove 删除提供商及其未再引用的凭证。
func Remove(st *store.Store, providerName string) error {
	return st.Update(func(s *config.State) error {
		p, ok := s.Providers[providerName]
		if !ok {
			return fmt.Errorf("提供商不存在：%s", providerName)
		}
		if refs := s.References(providerName, "", ""); len(refs) != 0 {
			return fmt.Errorf("提供商仍被 profile 使用：%s", strings.Join(refs, ", "))
		}
		refs := []string{p.DefaultCredentialRef}
		for _, c := range p.Connections {
			refs = append(refs, c.CredentialRef)
		}
		delete(s.Providers, providerName)
		for _, ref := range refs {
			deleteIfUnreferenced(s, ref)
		}
		return nil
	})
}

// Check 仅通过有超时和响应上限的 GET 请求检查模型目录，不发送推理请求。
func Check(ctx context.Context, c config.Connection, apiKey string) error {
	if err := ValidateConnection(c); err != nil {
		return err
	}
	if strings.TrimSpace(apiKey) == "" {
		return fmt.Errorf("连接缺少 API key")
	}
	endpoint := strings.TrimRight(c.BaseURL, "/")
	if c.Protocol == config.Anthropic {
		if !strings.HasSuffix(endpoint, "/v1") {
			endpoint += "/v1"
		}
		endpoint += "/models"
	} else {
		endpoint += "/models"
	}
	ctx, cancel := context.WithTimeout(ctx, checkTimeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return fmt.Errorf("目录请求失败")
	}
	if c.Protocol == config.Anthropic {
		req.Header.Set("x-api-key", apiKey)
		req.Header.Set("anthropic-version", "2023-06-01")
	} else {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	}
	client := &http.Client{
		Timeout: checkTimeout,
		CheckRedirect: func(_ *http.Request, _ []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("模型目录请求失败；请检查连接地址和网络")
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, maxResponseSize))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return fmt.Errorf("模型目录返回 HTTP %d", resp.StatusCode)
	}
	return nil
}

func newSecretRef() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "secret-" + hex.EncodeToString(b[:]), nil
}

func deleteIfUnreferenced(s *config.State, ref string) {
	if ref == "" {
		return
	}
	for _, p := range s.Providers {
		if p.DefaultCredentialRef == ref {
			return
		}
		for _, c := range p.Connections {
			if c.CredentialRef == ref {
				return
			}
		}
	}
	for _, p := range s.Profiles {
		if p.KeyRef == ref {
			return
		}
	}
	delete(s.Secrets, ref)
}
