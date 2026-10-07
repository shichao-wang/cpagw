package connection

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
	"gopkg.in/yaml.v3"
)

const (
	checkTimeout    = 8 * time.Second
	maxResponseSize = 64 << 10
)

// ParseModels 严格解析 YAML 模型清单，并拒绝空 ID、重复 ID 和控制字符。
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
	if c.AuthType != config.AuthAPIKey && c.AuthType != config.AuthCodexOAuth {
		return fmt.Errorf("连接必须显式声明有效的认证方式")
	}
	if c.AuthType == config.AuthCodexOAuth && (c.Protocol != config.Responses || c.BaseURL != config.CodexBaseURL) {
		return fmt.Errorf("Codex OAuth 仅支持 responses 协议和官方 API 地址")
	}
	return config.ValidateModels(c.Models)
}

// Create 新建全局唯一连接，并为其保存独立凭证。
func Create(ctx context.Context, st *store.Store, c config.Connection, apiKey string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateConnection(c); err != nil {
		return err
	}
	if c.AuthType == config.AuthAPIKey {
		if err := config.ValidateKey(apiKey); err != nil {
			return err
		}
	} else if apiKey != "" {
		return fmt.Errorf("Codex OAuth 连接不能接收 API key")
	}
	id, err := config.RandomID()
	if err != nil {
		return fmt.Errorf("生成连接 ID 失败")
	}
	c.ID = id
	return withOAuthRunLock(st, c.AuthType == config.AuthCodexOAuth, func() error {
		return st.UpdateContext(ctx, func(s *config.State) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			if _, ok := s.Connections[c.Name]; ok {
				return fmt.Errorf("连接已存在：%s", c.Name)
			}
			if c.AuthType == config.AuthAPIKey {
				ref, err := newSecretRef()
				if err != nil {
					return fmt.Errorf("生成凭证引用失败")
				}
				c.CredentialRef = ref
				s.Secrets[ref] = apiKey
			} else {
				c.CredentialRef = ""
			}
			if s.Connections == nil {
				s.Connections = make(map[string]config.Connection)
			}
			s.Connections[c.Name] = c
			return nil
		})
	})
}

// Patch 在同一事务内更新连接地址、模型清单、认证方式和凭证。
func Patch(st *store.Store, name string, base *string, models *[]config.Model, key *string, authType *string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	if base != nil {
		if err := config.ValidateURL(*base); err != nil {
			return err
		}
	}
	if models != nil {
		if err := config.ValidateModels(*models); err != nil {
			return err
		}
	}
	if authType != nil && *authType != config.AuthAPIKey && *authType != config.AuthCodexOAuth {
		return fmt.Errorf("不支持的认证方式：%s", *authType)
	}
	if key != nil {
		if err := config.ValidateKey(*key); err != nil {
			return err
		}
	}
	before, err := st.Read()
	if err != nil {
		return err
	}
	original, ok := before.Connections[name]
	if !ok {
		return fmt.Errorf("连接不存在：%s", name)
	}
	requiresRunLock := authType != nil && *authType != original.AuthType && (original.AuthType == config.AuthCodexOAuth || *authType == config.AuthCodexOAuth)
	return withOAuthRunLock(st, requiresRunLock, func() error {
		return st.Update(func(s *config.State) error {
			c, ok := s.Connections[name]
			if !ok {
				return fmt.Errorf("连接不存在：%s", name)
			}
			if c.ID != original.ID || c.AuthType != original.AuthType {
				return fmt.Errorf("连接已变化，请重试更新")
			}
			oldRef := c.CredentialRef
			if base != nil {
				c.BaseURL = *base
			}
			if models != nil {
				available := make(map[string]bool, len(*models))
				for _, m := range *models {
					available[m.ID] = true
				}
				for _, profileName := range s.References(name, "") {
					for _, binding := range s.Profiles[profileName].Models {
						if binding.Connection == name && !available[binding.TargetModel] {
							return fmt.Errorf("无法移除仍被 profile %s 使用的模型：%s", profileName, binding.TargetModel)
						}
					}
				}
				c.Models = append([]config.Model(nil), (*models)...)
			}
			if authType != nil && *authType != c.AuthType {
				if *authType == config.AuthCodexOAuth {
					if key != nil {
						return fmt.Errorf("切换到 Codex OAuth 时不能同时设置 API key")
					}
					c.AuthType = config.AuthCodexOAuth
					c.CredentialRef = ""
				} else {
					if key == nil {
						return fmt.Errorf("切换到 API key 认证时必须显式提供 API key")
					}
					ref, err := newSecretRef()
					if err != nil {
						return fmt.Errorf("生成凭证引用失败")
					}
					c.AuthType = config.AuthAPIKey
					c.CredentialRef = ref
					s.Secrets[ref] = *key
				}
			} else if key != nil {
				if c.AuthType != config.AuthAPIKey {
					return fmt.Errorf("Codex OAuth 连接不能设置 API key")
				}
				ref, err := newSecretRef()
				if err != nil {
					return fmt.Errorf("生成凭证引用失败")
				}
				c.CredentialRef = ref
				s.Secrets[ref] = *key
			}
			if err := ValidateConnection(c); err != nil {
				return err
			}
			if c.AuthType == config.AuthAPIKey {
				if _, err := s.Key(c); err != nil {
					return err
				}
			}
			s.Connections[name] = c
			deleteIfUnreferenced(s, oldRef)
			deleteOAuthIfUnreferenced(s, oldRef)
			return nil
		})
	})
}

// Remove 拒绝删除仍被 profile 使用的连接。
func Remove(st *store.Store, name string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	state, err := st.Read()
	if err != nil {
		return err
	}
	original, ok := state.Connections[name]
	if !ok {
		return fmt.Errorf("连接不存在：%s", name)
	}
	return withOAuthRunLock(st, original.AuthType == config.AuthCodexOAuth, func() error {
		return st.Update(func(s *config.State) error {
			c, ok := s.Connections[name]
			if !ok {
				return fmt.Errorf("连接不存在：%s", name)
			}
			if c.ID != original.ID || c.AuthType != original.AuthType {
				return fmt.Errorf("连接已变化，请重试删除")
			}
			if refs := s.References(name, ""); len(refs) != 0 {
				return fmt.Errorf("连接仍被 profile 使用：%s", strings.Join(refs, ", "))
			}
			delete(s.Connections, name)
			deleteIfUnreferenced(s, c.CredentialRef)
			deleteOAuthIfUnreferenced(s, c.CredentialRef)
			return nil
		})
	})
}

// Check 仅通过有限时长与响应体的 GET 请求检查模型目录，不发送推理请求。
func Check(ctx context.Context, c config.Connection, apiKey string) error {
	if err := ValidateConnection(c); err != nil {
		return err
	}
	if err := config.ValidateKey(apiKey); err != nil {
		return fmt.Errorf("连接缺少有效的 API key")
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

func withOAuthRunLock(st *store.Store, required bool, fn func() error) error {
	if !required {
		return fn()
	}
	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()
	return fn()
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
	for _, c := range s.Connections {
		if c.AuthType == config.AuthAPIKey && c.CredentialRef == ref {
			return
		}
	}
	for _, p := range s.Profiles {
		if p.KeyRef == ref {
			return
		}
	}
	delete(s.Secrets, ref)
}

func deleteOAuthIfUnreferenced(s *config.State, ref string) {
	if ref == "" {
		return
	}
	for _, c := range s.Connections {
		if c.AuthType == config.AuthCodexOAuth && c.CredentialRef == ref {
			return
		}
	}
	delete(s.OAuthCredentials, ref)
}
