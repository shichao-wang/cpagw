package store

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"

	"github.com/shichao-wang/cpagw/internal/config"
)

type Migration func(json.RawMessage) (json.RawMessage, error)

// MigrateBytes 在内存中执行严格相邻版本的迁移链。
func MigrateBytes(data []byte, current int, chain map[int]Migration) ([]byte, error) {
	var probe struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return nil, fmt.Errorf("状态文件损坏，拒绝迁移")
	}
	if probe.SchemaVersion > current {
		return nil, fmt.Errorf("状态版本高于当前程序支持范围，请升级程序")
	}
	if probe.SchemaVersion < 1 {
		return nil, fmt.Errorf("状态版本无效")
	}
	if probe.SchemaVersion == current {
		var state config.State
		if err := decodeStrict(data, &state); err != nil {
			return nil, fmt.Errorf("状态文件损坏或包含未知字段，拒绝迁移")
		}
		if err := validateMigratedState(&state); err != nil {
			return nil, err
		}
		return append([]byte(nil), data...), nil
	}
	raw, err := migrateThroughChain(data, current, chain)
	if err != nil {
		return nil, err
	}
	var state config.State
	if err := decodeStrict(raw, &state); err != nil {
		return nil, fmt.Errorf("迁移结果格式无效")
	}
	if err := validateMigratedState(&state); err != nil {
		return nil, fmt.Errorf("迁移结果校验失败：%w", err)
	}
	return json.MarshalIndent(&state, "", "  ")
}

// migrateThroughChain 在纯内存中逐版迁移，并检查每步确实只前进一个版本。
func migrateThroughChain(data []byte, target int, chain map[int]Migration) ([]byte, error) {
	raw := append([]byte(nil), data...)
	version, err := readSchemaVersion(raw)
	if err != nil {
		return nil, fmt.Errorf("状态文件损坏，拒绝迁移")
	}
	if version < 1 {
		return nil, fmt.Errorf("状态版本无效")
	}
	if version > target {
		return nil, fmt.Errorf("状态版本高于当前程序支持范围，请升级程序")
	}
	for version < target {
		step, ok := chain[version]
		if !ok {
			return nil, fmt.Errorf("缺少状态迁移：v%d→v%d", version, version+1)
		}
		next, err := step(append([]byte(nil), raw...))
		if err != nil {
			return nil, fmt.Errorf("状态迁移 v%d→v%d 失败：%w", version, version+1, err)
		}
		nextVersion, err := readSchemaVersion(next)
		if err != nil {
			return nil, fmt.Errorf("状态迁移 v%d→v%d 输出格式无效", version, version+1)
		}
		if nextVersion != version+1 {
			return nil, fmt.Errorf("状态迁移 v%d 必须输出 v%d，实际输出 v%d", version, version+1, nextVersion)
		}
		raw = append([]byte(nil), next...)
		version = nextVersion
	}
	return raw, nil
}

func readSchemaVersion(data []byte) (int, error) {
	var header struct {
		SchemaVersion int `json:"schemaVersion"`
	}
	if err := json.Unmarshal(data, &header); err != nil {
		return 0, err
	}
	return header.SchemaVersion, nil
}

func validateMigratedState(state *config.State) error {
	if err := state.Validate(); err != nil {
		return err
	}
	for _, profile := range state.Profiles {
		if err := state.ValidateProfileStructure(profile); err != nil {
			return err
		}
	}
	return nil
}

func decodeStrict(data []byte, dst any) error {
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(dst); err != nil {
		return err
	}
	if dec.Decode(new(any)) != io.EOF {
		return fmt.Errorf("多份 JSON")
	}
	return nil
}

type stateV1 struct {
	SchemaVersion int                       `json:"schemaVersion"`
	Revision      uint64                    `json:"revision"`
	Listen        string                    `json:"listen"`
	Providers     map[string]providerV1     `json:"providers"`
	Profiles      map[string]config.Profile `json:"profiles"`
	Secrets       map[string]string         `json:"secrets"`
}
type providerV1 struct {
	Name                 string                  `json:"name"`
	DefaultCredentialRef string                  `json:"defaultCredentialRef,omitempty"`
	Connections          map[string]connectionV1 `json:"connections"`
}
type connectionV1 struct {
	Name          string         `json:"name"`
	Protocol      string         `json:"protocol"`
	BaseURL       string         `json:"baseURL"`
	CredentialRef string         `json:"credentialRef,omitempty"`
	Models        []config.Model `json:"models"`
}

func migrateV1ToV2(raw json.RawMessage) (json.RawMessage, error) {
	var old stateV1
	if err := decodeStrict(raw, &old); err != nil {
		return nil, fmt.Errorf("v1 格式无效")
	}
	if old.SchemaVersion != 1 || old.Providers == nil || old.Profiles == nil || old.Secrets == nil {
		return nil, fmt.Errorf("v1 状态结构不完整")
	}
	out := &config.State{SchemaVersion: 2, Revision: old.Revision, Listen: old.Listen, Providers: map[string]config.Provider{}, Profiles: old.Profiles, Secrets: old.Secrets, OAuthCredentials: map[string]config.OAuthCredential{}}
	for pname, p := range old.Providers {
		np := config.Provider{Name: p.Name, DefaultCredentialRef: p.DefaultCredentialRef, Connections: map[string]config.Connection{}}
		for cname, c := range p.Connections {
			if c.Name != cname {
				return nil, fmt.Errorf("连接名与索引不匹配")
			}
			id, err := config.RandomID()
			if err != nil {
				return nil, fmt.Errorf("生成连接 ID 失败")
			}
			np.Connections[cname] = config.Connection{ID: id, Name: c.Name, AuthType: config.AuthAPIKey, Protocol: c.Protocol, BaseURL: c.BaseURL, CredentialRef: c.CredentialRef, Models: c.Models}
		}
		out.Providers[pname] = np
	}
	return json.Marshal(out)
}

// Migrate 备份原始状态文件后，以一次原子写入提交迁移结果。
func (s *Store) Migrate() error {
	return s.migrate(AtomicWrite)
}

func (s *Store) migrate(writeFile func(string, []byte) error) error {
	return WithLock(s.Path("state.lock"), func() error {
		path := s.StatePath()
		if err := CheckFile(path); err != nil {
			return err
		}
		data, err := os.ReadFile(path)
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		updated, err := MigrateBytes(data, config.SchemaVersion, map[int]Migration{1: migrateV1ToV2})
		if err != nil {
			return err
		}
		if bytes.Equal(data, updated) {
			return nil
		}
		// MigrateBytes 已严格验证源状态；备份名跟随实际起始 schema 版本。
		sourceVersion, err := readSchemaVersion(data)
		if err != nil {
			return fmt.Errorf("读取迁移源版本失败")
		}
		backup := fmt.Sprintf("%s.v%d.bak", path, sourceVersion)
		if err := CheckFile(backup); err != nil {
			return err
		}
		if _, err := os.Lstat(backup); err == nil {
			backupData, err := os.ReadFile(backup)
			if err != nil {
				return fmt.Errorf("读取已有迁移备份失败")
			}
			if !bytes.Equal(backupData, data) {
				return fmt.Errorf("已有迁移备份与当前源状态不同，拒绝覆盖")
			}
		} else if os.IsNotExist(err) {
			if err := writeFile(backup, data); err != nil {
				return fmt.Errorf("保存迁移备份失败")
			}
		} else {
			return err
		}
		if err := writeFile(path, updated); err != nil {
			return fmt.Errorf("提交迁移结果失败；原文件和备份均保留")
		}
		return nil
	})
}
