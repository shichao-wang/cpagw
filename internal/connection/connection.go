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
	return config.ValidateModels(c.Models)
}

// Create 新建全局唯一连接，并为其保存独立凭证。
func Create(ctx context.Context, st *store.Store, c config.Connection, apiKey string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := ValidateConnection(c); err != nil {
		return err
	}
	if err := config.ValidateKey(apiKey); err != nil {
		return err
	}
	return st.UpdateContext(ctx, func(s *config.State) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if _, ok := s.Connections[c.Name]; ok {
			return fmt.Errorf("连接已存在：%s", c.Name)
		}
		ref, err := newSecretRef()
		if err != nil {
			return fmt.Errorf("生成凭证引用失败")
		}
		c.CredentialRef = ref
		if s.Connections == nil {
			s.Connections = make(map[string]config.Connection)
		}
		s.Connections[c.Name] = c
		s.Secrets[ref] = apiKey
		return nil
	})
}

// Patch 在同一事务内更新连接地址、模型清单和凭证。
func Patch(st *store.Store, name string, base *string, models *[]config.Model, key *string) error {
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
	if key != nil {
		if err := config.ValidateKey(*key); err != nil {
			return err
		}
	}
	return st.Update(func(s *config.State) error {
		c, ok := s.Connections[name]
		if !ok {
			return fmt.Errorf("连接不存在：%s", name)
		}
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
		oldRef := c.CredentialRef
		if key != nil {
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
		if _, err := s.Key(c); err != nil {
			return err
		}
		s.Connections[name] = c
		deleteIfUnreferenced(s, oldRef)
		return nil
	})
}

// Remove 拒绝删除仍被 profile 使用的连接。
func Remove(st *store.Store, name string) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	return st.Update(func(s *config.State) error {
		c, ok := s.Connections[name]
		if !ok {
			return fmt.Errorf("连接不存在：%s", name)
		}
		if refs := s.References(name, ""); len(refs) != 0 {
			return fmt.Errorf("连接仍被 profile 使用：%s", strings.Join(refs, ", "))
		}
		delete(s.Connections, name)
		deleteIfUnreferenced(s, c.CredentialRef)
		return nil
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
		if c.CredentialRef == ref {
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
