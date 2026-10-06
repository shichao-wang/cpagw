package store

import (
	"fmt"
	"path/filepath"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"golang.org/x/sys/unix"
)

const RunLockFileName = ".gateway.lock"

type OAuthBinding struct {
	Provider      string
	Connection    string
	ConnectionID  string
	CredentialRef string
	Generation    uint64
}

// SaveOAuth 仅通过 CAS 写入 token，不修改 State.Revision。
func (s *Store) SaveOAuth(binding OAuthBinding, credential config.OAuthCredential) (uint64, error) {
	var generation uint64
	err := WithLock(s.Path("state.lock"), func() error {
		state, err := s.Read()
		if err != nil {
			return err
		}
		p, ok := state.Providers[binding.Provider]
		if !ok {
			return fmt.Errorf("OAuth 凭证绑定已失效")
		}
		c, ok := p.Connections[binding.Connection]
		if !ok || c.ID != binding.ConnectionID || c.AuthType != config.AuthCodexOAuth || c.BaseURL != config.CodexBaseURL || c.CredentialRef != binding.CredentialRef {
			return fmt.Errorf("OAuth 凭证绑定已变化")
		}
		var previous config.OAuthCredential
		if binding.CredentialRef != "" {
			var exists bool
			previous, exists = state.OAuthCredentials[binding.CredentialRef]
			if !exists || previous.Generation != binding.Generation {
				return fmt.Errorf("OAuth 凭证版本已变化")
			}
			if credential.AccountID != previous.AccountID {
				return fmt.Errorf("OAuth 账号身份已变化")
			}
		} else if binding.Generation != 0 {
			return fmt.Errorf("OAuth 凭证版本无效")
		}
		if strings.TrimSpace(binding.CredentialRef) == "" {
			return fmt.Errorf("OAuth 凭证引用不能为空")
		}
		credential.Generation = previous.Generation
		if credential == previous {
			generation = previous.Generation
			return nil
		}
		credential.Generation = binding.Generation + 1
		if err := config.ValidateOAuthCredential(credential); err != nil {
			return err
		}
		state.OAuthCredentials[binding.CredentialRef] = credential
		if err := state.Validate(); err != nil {
			return err
		}
		if err := WriteJSON(s.Path("state.json"), state); err != nil {
			return err
		}
		generation = credential.Generation
		return nil
	})
	return generation, err
}

type RunLock struct{ fd int }

// AcquireRunLock 获取与网关共用的非阻塞独占锁，串行化 OAuth runtime 变更。
func (s *Store) AcquireRunLock() (*RunLock, error) {
	path := s.Path(RunLockFileName)
	if err := CheckFile(path); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, err
	}
	if err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("网关正在运行或认证状态被占用")
	}
	return &RunLock{fd: fd}, nil
}
func (l *RunLock) Close() error {
	if l == nil || l.fd < 0 {
		return nil
	}
	_ = unix.Flock(l.fd, unix.LOCK_UN)
	err := unix.Close(l.fd)
	l.fd = -1
	return err
}

func (s *Store) OAuthBinding(provider, connection string) (OAuthBinding, error) {
	state, err := s.Read()
	if err != nil {
		return OAuthBinding{}, err
	}
	p, ok := state.Providers[provider]
	if !ok {
		return OAuthBinding{}, fmt.Errorf("提供商不存在：%s", provider)
	}
	c, ok := p.Connections[connection]
	if !ok {
		return OAuthBinding{}, fmt.Errorf("连接不存在：%s/%s", provider, connection)
	}
	if c.AuthType != config.AuthCodexOAuth {
		return OAuthBinding{}, fmt.Errorf("连接未声明 Codex OAuth")
	}
	generation := uint64(0)
	if cred, ok := state.OAuthCredentials[c.CredentialRef]; ok {
		generation = cred.Generation
	}
	return OAuthBinding{Provider: provider, Connection: connection, ConnectionID: c.ID, CredentialRef: c.CredentialRef, Generation: generation}, nil
}

// StatePath 返回状态文件路径，供状态迁移命令使用。
func (s *Store) StatePath() string { return filepath.Join(s.Dir, "state.json") }
