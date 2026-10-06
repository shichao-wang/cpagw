package config

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"
	"unicode"
)

const (
	Chat      = "chat-completions"
	Anthropic = "anthropic-messages"
	Responses = "responses"

	SchemaVersion  = 3
	AuthAPIKey     = "api-key"
	AuthCodexOAuth = "codex-oauth"
	CodexBaseURL   = "https://chatgpt.com/backend-api/codex"
)

var Slots = []string{"opus", "sonnet", "haiku"}
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Model struct {
	ID   string `json:"id" yaml:"id"`
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
}
type Connection struct {
	ID            string  `json:"id"`
	Name          string  `json:"name"`
	AuthType      string  `json:"authType"`
	Protocol      string  `json:"protocol"`
	BaseURL       string  `json:"baseURL"`
	CredentialRef string  `json:"credentialRef"`
	Models        []Model `json:"models"`
}
type OAuthCredential struct {
	AccessToken  string    `json:"accessToken"`
	RefreshToken string    `json:"refreshToken"`
	IDToken      string    `json:"idToken,omitempty"`
	AccountID    string    `json:"accountId"`
	PlanType     string    `json:"planType,omitempty"`
	ExpiresAt    time.Time `json:"expiresAt"`
	LastRefresh  time.Time `json:"lastRefresh,omitempty"`
	Generation   uint64    `json:"generation"`
}
type Binding struct {
	PublicModel string `json:"public_model" yaml:"public_model"`
	Connection  string `json:"connection" yaml:"connection"`
	TargetModel string `json:"target_model" yaml:"target_model"`
	Label       string `json:"label" yaml:"label"`
	Description string `json:"description" yaml:"description"`
}
type Profile struct {
	Name   string             `json:"name"`
	ID     string             `json:"id"`
	Agent  string             `json:"agent"`
	KeyRef string             `json:"keyRef"`
	Models map[string]Binding `json:"models"`
}
type State struct {
	SchemaVersion    int                        `json:"schemaVersion"`
	Revision         uint64                     `json:"revision"`
	Listen           string                     `json:"listen"`
	Connections      map[string]Connection      `json:"connections"`
	Profiles         map[string]Profile         `json:"profiles"`
	Secrets          map[string]string          `json:"secrets"`
	OAuthCredentials map[string]OAuthCredential `json:"oauthCredentials"`
}

func NewState() *State {
	return &State{SchemaVersion: SchemaVersion, Listen: "127.0.0.1:8317", Connections: map[string]Connection{}, Profiles: map[string]Profile{}, Secrets: map[string]string{}, OAuthCredentials: map[string]OAuthCredential{}}
}

func RandomID() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(b[:]), nil
}

func ValidateName(name string) error {
	if !validName.MatchString(name) {
		return fmt.Errorf("名称必须为 1–64 位字母、数字、下划线或连字符，且以字母或数字开头")
	}
	return nil
}

func ValidateURL(raw string) error {
	u, err := url.Parse(raw)
	if err != nil || u.Host == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return fmt.Errorf("无效的 API 根地址")
	}
	host := u.Hostname()
	if u.Scheme != "https" && !(u.Scheme == "http" && (host == "localhost" || host == "127.0.0.1" || host == "::1")) {
		return fmt.Errorf("API 地址必须为 HTTPS，HTTP 仅允许 loopback")
	}
	if strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/messages") || strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/chat/completions") || strings.HasSuffix(strings.TrimRight(u.Path, "/"), "/responses") {
		return fmt.Errorf("请提供 API 根地址，而非推理请求路径")
	}
	return nil
}

func (s *State) Key(c Connection) (string, error) {
	if c.AuthType != AuthAPIKey || strings.TrimSpace(c.CredentialRef) == "" {
		return "", fmt.Errorf("连接 %s 缺少有效的 API key", c.Name)
	}
	key := s.Secrets[c.CredentialRef]
	if err := ValidateKey(key); err != nil {
		return "", fmt.Errorf("连接 %s 缺少有效的 API key", c.Name)
	}
	return key, nil
}

func (s *State) Credential(c Connection) (string, error) {
	switch c.AuthType {
	case AuthAPIKey:
		return s.Key(c)
	case AuthCodexOAuth:
		cred, ok := s.OAuthCredentials[c.CredentialRef]
		if c.CredentialRef == "" || !ok || ValidateOAuthCredential(cred) != nil {
			return "", fmt.Errorf("连接 %s 尚未登录或凭证无效", c.Name)
		}
		return cred.AccessToken, nil
	default:
		return "", fmt.Errorf("连接认证方式无效：%s", c.Name)
	}
}

func ValidateModels(models []Model) error {
	if len(models) == 0 {
		return fmt.Errorf("模型清单不能为空")
	}
	seen := map[string]bool{}
	for _, m := range models {
		if strings.TrimSpace(m.ID) == "" || hasControl(m.ID) || hasControl(m.Name) || seen[m.ID] {
			return fmt.Errorf("模型 ID 为空、重复或包含控制字符")
		}
		seen[m.ID] = true
	}
	return nil
}

// ValidateProfileStructure 校验 profile 的结构和模型引用，不要求 OAuth 凭证已登录。
func (s *State) ValidateProfileStructure(p Profile) error {
	if p.Agent != "claude-code" {
		return fmt.Errorf("首期仅支持 claude-code")
	}
	if len(p.Models) != 3 {
		return fmt.Errorf("必须配置 opus、sonnet、haiku 三个档位")
	}
	seen := map[string]bool{}
	for _, slot := range Slots {
		b, ok := p.Models[slot]
		if !ok || !strings.HasPrefix(b.PublicModel, "claude-") || seen[b.PublicModel] {
			return fmt.Errorf("%s 缺少唯一的公开 Claude 模型 ID", slot)
		}
		if hasControl(b.PublicModel) || hasControl(b.TargetModel) || hasControl(b.Label) || hasControl(b.Description) {
			return fmt.Errorf("profile 模型信息不能包含控制字符")
		}
		seen[b.PublicModel] = true
		c, ok := s.Connections[b.Connection]
		if !ok {
			return fmt.Errorf("连接不存在：%s", b.Connection)
		}
		if err := ValidateAuthConnection(c); err != nil {
			return err
		}
		if c.AuthType == AuthAPIKey {
			if _, err := s.Key(c); err != nil {
				return err
			}
		}
		found := false
		for _, m := range c.Models {
			if m.ID == b.TargetModel {
				found = true
				break
			}
		}
		if !found {
			return fmt.Errorf("连接中未登记模型：%s", b.TargetModel)
		}
	}
	return nil
}

// ValidateProfile additionally requires every referenced OAuth connection to be logged in.
func (s *State) ValidateProfile(p Profile) error {
	if err := s.ValidateProfileStructure(p); err != nil {
		return err
	}
	for _, b := range p.Models {
		c := s.Connections[b.Connection]
		if _, err := s.Credential(c); err != nil {
			return err
		}
	}
	return nil
}

func ValidateAuthConnection(c Connection) error {
	if strings.TrimSpace(c.ID) == "" {
		return fmt.Errorf("连接缺少 ID")
	}
	switch c.AuthType {
	case AuthAPIKey:
		// 未被 profile 引用的连接可以尚未配置凭证；绑定 profile 时由
		// ValidateProfileStructure 强制要求有效 API key。
	case AuthCodexOAuth:
		if c.Protocol != Responses {
			return fmt.Errorf("Codex OAuth 仅支持 responses 协议")
		}
		if c.BaseURL != CodexBaseURL {
			return fmt.Errorf("Codex OAuth 必须使用官方 API 地址")
		}
	default:
		return fmt.Errorf("连接必须显式声明有效的认证方式")
	}
	return nil
}

func ValidateOAuthCredential(c OAuthCredential) error {
	if c.AccessToken == "" || c.RefreshToken == "" || c.AccountID == "" || c.Generation == 0 || c.ExpiresAt.IsZero() {
		return fmt.Errorf("OAuth 凭证字段不完整")
	}
	if ValidateKey(c.AccessToken) != nil || ValidateKey(c.RefreshToken) != nil || (c.IDToken != "" && ValidateKey(c.IDToken) != nil) {
		return fmt.Errorf("OAuth 凭证包含无效字符")
	}
	return nil
}

func (s *State) Validate() error {
	if s.SchemaVersion != SchemaVersion || s.Connections == nil || s.Profiles == nil || s.Secrets == nil || s.OAuthCredentials == nil {
		return fmt.Errorf("不支持或无效的状态格式；旧状态不会被修改，请使用新的 --state-dir 重新配置")
	}
	connectionIDs := map[string]bool{}
	oauthRefs := map[string]bool{}
	for name, c := range s.Connections {
		if c.Name != name {
			return fmt.Errorf("连接名称与索引不匹配：%s", name)
		}
		if err := ValidateName(name); err != nil {
			return err
		}
		if err := ValidateAuthConnection(c); err != nil {
			return err
		}
		switch c.Protocol {
		case Chat, Anthropic, Responses:
		default:
			return fmt.Errorf("不支持的协议：%s", c.Protocol)
		}
		if err := ValidateURL(c.BaseURL); err != nil {
			return err
		}
		if err := ValidateModels(c.Models); err != nil {
			return err
		}
		if connectionIDs[c.ID] {
			return fmt.Errorf("连接 ID 重复")
		}
		connectionIDs[c.ID] = true
		if c.AuthType == AuthAPIKey {
			if _, exists := s.OAuthCredentials[c.CredentialRef]; c.CredentialRef != "" && exists {
				return fmt.Errorf("API key 连接不能引用 OAuth 凭证")
			}
		} else if c.CredentialRef != "" {
			if _, exists := s.OAuthCredentials[c.CredentialRef]; !exists {
				return fmt.Errorf("OAuth 连接引用的凭证记录不存在")
			}
			if _, exists := s.Secrets[c.CredentialRef]; exists {
				return fmt.Errorf("OAuth 凭证引用与 API key 表冲突")
			}
			if oauthRefs[c.CredentialRef] {
				return fmt.Errorf("OAuth 凭证不能跨连接共享")
			}
			oauthRefs[c.CredentialRef] = true
		}
	}
	for ref, cred := range s.OAuthCredentials {
		if ref == "" || ValidateOAuthCredential(cred) != nil || !oauthRefs[ref] {
			return fmt.Errorf("OAuth 凭证记录无效或未绑定连接")
		}
	}
	for ref, secret := range s.Secrets {
		if strings.TrimSpace(ref) == "" || ValidateKey(secret) != nil {
			return fmt.Errorf("API key 凭证记录无效")
		}
	}
	for name, profile := range s.Profiles {
		if profile.Name != name {
			return fmt.Errorf("profile 名称与索引不匹配：%s", name)
		}
		if err := ValidateName(name); err != nil {
			return err
		}
		if strings.TrimSpace(profile.ID) == "" {
			return fmt.Errorf("profile 缺少 ID：%s", name)
		}
		if err := s.ValidateProfileStructure(profile); err != nil {
			return fmt.Errorf("profile %s 无效：%w", name, err)
		}
		if profile.KeyRef == "" || ValidateKey(s.Secrets[profile.KeyRef]) != nil {
			return fmt.Errorf("profile %s 缺少有效 key", name)
		}
	}
	return nil
}

func ConnectionPrefix(name string) string {
	sum := sha256.Sum256([]byte(name))
	return "cpagw-" + hex.EncodeToString(sum[:8])
}

func (s *State) References(connectionName, targetModel string) []string {
	var names []string
	for name, p := range s.Profiles {
		for _, b := range p.Models {
			if b.Connection == connectionName && (targetModel == "" || b.TargetModel == targetModel) {
				names = append(names, name)
				break
			}
		}
	}
	return names
}

func hasControl(value string) bool {
	for _, r := range value {
		if unicode.IsControl(r) {
			return true
		}
	}
	return false
}
