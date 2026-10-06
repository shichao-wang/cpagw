package store

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"syscall"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"golang.org/x/sys/unix"
)

type Store struct{ Dir string }

func DefaultDir() (string, error) {
	if d := os.Getenv("XDG_CONFIG_HOME"); d != "" {
		return filepath.Join(d, "cpagw"), nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(home, ".config", "cpagw"), nil
}
func New(dir string) (*Store, error) {
	if dir == "" {
		var err error
		dir, err = DefaultDir()
		if err != nil {
			return nil, err
		}
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	if err = EnsureDir(abs); err != nil {
		return nil, err
	}
	return &Store{Dir: abs}, nil
}
func EnsureDir(dir string) error {
	info, err := os.Lstat(dir)
	if os.IsNotExist(err) {
		if err = os.MkdirAll(dir, 0700); err != nil {
			return err
		}
		info, err = os.Lstat(dir)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("状态目录必须是实际目录")
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("敏感路径必须属于当前用户：%s", info.Name())
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("状态目录权限必须限制为 0700：%s", dir)
	}
	return nil
}
func CheckFile(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝非普通文件或符号链接：%s", path)
	}
	if stat, ok := info.Sys().(*syscall.Stat_t); !ok || stat.Uid != uint32(os.Getuid()) {
		return fmt.Errorf("敏感路径必须属于当前用户：%s", info.Name())
	}
	if info.Mode().Perm()&0077 != 0 {
		return fmt.Errorf("敏感文件权限必须限制为 0600：%s", path)
	}
	return nil
}
func WithLock(path string, fn func() error) error {
	if err := CheckFile(path); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err = unix.Flock(fd, unix.LOCK_EX); err != nil {
		return err
	}
	defer unix.Flock(fd, unix.LOCK_UN)
	return fn()
}
func AtomicWrite(path string, data []byte) error {
	if err := CheckFile(path); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".cpagw-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Chmod(0600); err == nil {
		_, err = f.Write(data)
	}
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	if err = os.Rename(tmp, path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer d.Close()
	return d.Sync()
}
func WriteJSON(path string, value any) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	return AtomicWrite(path, append(data, '\n'))
}
func (s *Store) Path(parts ...string) string {
	return filepath.Join(append([]string{s.Dir}, parts...)...)
}
func (s *Store) Read() (*config.State, error) {
	path := s.Path("state.json")
	if err := CheckFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return config.NewState(), nil
	}
	if err != nil {
		return nil, err
	}
	state := &config.State{}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err = decoder.Decode(state); err != nil {
		return nil, fmt.Errorf("状态文件不兼容、损坏或包含未知字段，拒绝覆盖；旧状态请使用新的 --state-dir 重新配置")
	}
	if decoder.Decode(new(any)) != io.EOF {
		return nil, fmt.Errorf("状态文件含多份 JSON，拒绝覆盖")
	}
	if state.SchemaVersion != 2 || state.Connections == nil || state.Profiles == nil || state.Secrets == nil {
		return nil, fmt.Errorf("状态格式不兼容或无效；旧状态不会被修改，请使用新的 --state-dir 重新配置")
	}
	return state, nil
}
func (s *Store) Update(fn func(*config.State) error) error {
	return WithLock(s.Path("state.lock"), func() error {
		state, err := s.Read()
		if err != nil {
			return err
		}
		if err = fn(state); err != nil {
			return err
		}
		state.Revision++
		return WriteJSON(s.Path("state.json"), state)
	})
}

// UpdateContext 在可取消的锁等待中原子更新状态。
func (s *Store) UpdateContext(ctx context.Context, fn func(*config.State) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	path := s.Path("state.lock")
	if err := CheckFile(path); err != nil {
		return err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err = unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		if err == nil {
			break
		}
		if err == unix.EINTR {
			if ctxErr := ctx.Err(); ctxErr != nil {
				return ctxErr
			}
			continue
		}
		if err != unix.EWOULDBLOCK && err != unix.EAGAIN {
			return err
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			return ctx.Err()
		case <-timer.C:
		}
	}
	defer unix.Flock(fd, unix.LOCK_UN)

	if err := ctx.Err(); err != nil {
		return err
	}
	state, err := s.Read()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := fn(state); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	state.Revision++
	if err := ctx.Err(); err != nil {
		return err
	}
	return WriteJSON(s.Path("state.json"), state)
}
