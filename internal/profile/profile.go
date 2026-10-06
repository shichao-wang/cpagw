package profile

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
	"gopkg.in/yaml.v3"
)

type File struct {
	Name   *string                   `yaml:"name"`
	Agent  *string                   `yaml:"agent"`
	Models map[string]config.Binding `yaml:"models"`
}

// ParseFile 使用 KnownFields 严格解析 profile YAML，不接受 ID、KeyRef 或其他未定义字段。
func ParseFile(r io.Reader) (File, error) {
	dec := yaml.NewDecoder(r)
	dec.KnownFields(true)
	var file File
	if err := dec.Decode(&file); err != nil {
		return File{}, fmt.Errorf("profile YAML 格式无效")
	}
	var extra any
	if err := dec.Decode(&extra); err != io.EOF {
		return File{}, fmt.Errorf("profile YAML 只能包含一个文档")
	}
	return file, nil
}

// ReadFile 从路径读取 profile YAML；解析错误不包含文件内容。
func ReadFile(path string) (File, error) {
	f, err := os.Open(path)
	if err != nil {
		return File{}, fmt.Errorf("无法读取 profile 文件")
	}
	defer f.Close()
	return ParseFile(f)
}

// Create 根据 YAML 创建 profile，并仅将新生成的 cpagw_ key 返回一次。
func Create(st *store.Store, file File) (string, error) {
	if file.Name == nil || strings.TrimSpace(*file.Name) == "" {
		return "", fmt.Errorf("profile 文件必须包含 name")
	}
	name := strings.TrimSpace(*file.Name)
	if err := config.ValidateName(name); err != nil {
		return "", err
	}
	agent := "claude-code"
	if file.Agent != nil {
		agent = strings.TrimSpace(*file.Agent)
	}
	p := config.Profile{Name: name, Agent: agent, Models: file.Models}
	if err := validateDraft(p); err != nil {
		return "", err
	}
	id, err := config.RandomID()
	if err != nil {
		return "", fmt.Errorf("生成 profile ID 失败")
	}
	key, err := randomKey()
	if err != nil {
		return "", fmt.Errorf("生成 profile key 失败")
	}
	keyRef := "profile-key-" + id
	p.ID = id
	p.KeyRef = keyRef
	if err := st.Update(func(s *config.State) error {
		if _, ok := s.Profiles[name]; ok {
			return fmt.Errorf("profile 已存在：%s", name)
		}
		if err := s.ValidateProfile(p); err != nil {
			return err
		}
		for slot, binding := range p.Models {
			if strings.TrimSpace(binding.Label) == "" {
				binding.Label = binding.TargetModel
				for _, model := range s.Connections[binding.Connection].Models {
					if model.ID == binding.TargetModel && model.Name != "" {
						binding.Label = model.Name
					}
				}
			}
			if strings.TrimSpace(binding.Description) == "" {
				binding.Description = fmt.Sprintf("%s / %s", binding.Connection, binding.TargetModel)
			}
			p.Models[slot] = binding
		}
		if err := s.ValidateProfile(p); err != nil {
			return err
		}
		s.Profiles[name] = p
		s.Secrets[keyRef] = key
		return nil
	}); err != nil {
		return "", err
	}
	return key, nil
}

// Update 合并 YAML 中显式给出的字段；ID、key 及未指定的档位保持不变。
func Update(st *store.Store, name string, file File) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	return st.Update(func(s *config.State) error {
		p, ok := s.Profiles[name]
		if !ok {
			return fmt.Errorf("profile 不存在：%s", name)
		}
		if file.Name != nil && strings.TrimSpace(*file.Name) != name {
			return fmt.Errorf("profile 文件 name 必须与目标名称一致")
		}
		if file.Agent != nil {
			p.Agent = strings.TrimSpace(*file.Agent)
		}
		if file.Models != nil {
			if p.Models == nil {
				p.Models = make(map[string]config.Binding)
			}
			for slot, binding := range file.Models {
				p.Models[slot] = binding
			}
		}
		if err := s.ValidateProfile(p); err != nil {
			return err
		}
		s.Profiles[name] = p
		return nil
	})
}

// Remove 删除 profile，并仅在本地 key 不再被引用时回收对应凭证。
func Remove(st *store.Store, name string, isReferenced func(string) (bool, error)) error {
	if err := config.ValidateName(name); err != nil {
		return err
	}
	return st.Update(func(s *config.State) error {
		p, ok := s.Profiles[name]
		if !ok {
			return fmt.Errorf("profile 不存在：%s", name)
		}
		if isReferenced == nil || p.ID == "" {
			return fmt.Errorf("无法确认 agent 配置引用，拒绝删除 profile")
		}
		referenced, err := isReferenced(p.ID)
		if err != nil {
			return fmt.Errorf("无法确认 agent 配置引用")
		}
		if referenced {
			return fmt.Errorf("profile 仍被 agent 配置引用：%s", name)
		}
		delete(s.Profiles, name)
		deleteIfUnreferenced(s, p.KeyRef)
		return nil
	})
}

func validateDraft(p config.Profile) error {
	if p.Agent != "claude-code" {
		return fmt.Errorf("首期仅支持 claude-code")
	}
	if len(p.Models) != len(config.Slots) {
		return fmt.Errorf("必须配置 opus、sonnet、haiku 三个档位")
	}
	seen := make(map[string]bool, len(config.Slots))
	for _, slot := range config.Slots {
		binding, ok := p.Models[slot]
		if !ok || !strings.HasPrefix(binding.PublicModel, "claude-") || seen[binding.PublicModel] {
			return fmt.Errorf("%s 缺少唯一的公开 Claude 模型 ID", slot)
		}
		seen[binding.PublicModel] = true
	}
	return nil
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

func randomKey() (string, error) {
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	return "cpagw_" + hex.EncodeToString(b[:]), nil
}
