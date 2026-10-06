package config

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"unicode"
)

const (
	Chat      = "chat-completions"
	Anthropic = "anthropic-messages"
	Responses = "responses"
)

var Slots = []string{"opus", "sonnet", "haiku"}
var validName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_-]{0,63}$`)

type Model struct {
	ID   string `json:"id" yaml:"id"`
	Name string `json:"name,omitempty" yaml:"name,omitempty"`
}
type Connection struct {
	Name          string  `json:"name"`
	Protocol      string  `json:"protocol"`
	BaseURL       string  `json:"baseURL"`
	CredentialRef string  `json:"credentialRef"`
	Models        []Model `json:"models"`
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
	SchemaVersion int                   `json:"schemaVersion"`
	Revision      uint64                `json:"revision"`
	Listen        string                `json:"listen"`
	Connections   map[string]Connection `json:"connections"`
	Profiles      map[string]Profile    `json:"profiles"`
	Secrets       map[string]string     `json:"secrets"`
}

func NewState() *State {
	return &State{SchemaVersion: 2, Listen: "127.0.0.1:8317", Connections: map[string]Connection{}, Profiles: map[string]Profile{}, Secrets: map[string]string{}}
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
	if strings.TrimSpace(c.CredentialRef) == "" {
		return "", fmt.Errorf("连接 %s 缺少有效的 API key", c.Name)
	}
	key := s.Secrets[c.CredentialRef]
	if err := ValidateKey(key); err != nil {
		return "", fmt.Errorf("连接 %s 缺少有效的 API key", c.Name)
	}
	return key, nil
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
func (s *State) ValidateProfile(p Profile) error {
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
		if _, err := s.Key(c); err != nil {
			return err
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
