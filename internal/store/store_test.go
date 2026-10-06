package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
	"golang.org/x/sys/unix"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	d := filepath.Join(t.TempDir(), "state")
	s, err := New(d)
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestConcurrentUpdates(t *testing.T) {
	s := testStore(t)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := s.Update(func(st *config.State) error { return nil }); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	st, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if st.Revision != 20 {
		t.Fatalf("更新丢失：%d", st.Revision)
	}
	info, err := os.Stat(s.Path("state.json"))
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatal("敏感文件权限错误")
	}
}
func TestInvalidStateDoesNotOverwrite(t *testing.T) {
	s := testStore(t)
	bad := []byte(`{"schemaVersion":99}`)
	if err := AtomicWrite(s.Path("state.json"), bad); err != nil {
		t.Fatal(err)
	}
	if s.Update(func(st *config.State) error { return nil }) == nil {
		t.Fatal("不能覆盖未知格式")
	}
	got, _ := os.ReadFile(s.Path("state.json"))
	if string(got) != string(bad) {
		t.Fatal("损坏文件被覆盖")
	}
}
func TestSymlinkRejected(t *testing.T) {
	s := testStore(t)
	target := filepath.Join(t.TempDir(), "target")
	if err := os.WriteFile(target, []byte("original"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, s.Path("state.json")); err != nil {
		t.Fatal(err)
	}
	if s.Update(func(st *config.State) error { return nil }) == nil {
		t.Fatal("必须拒绝 symlink")
	}
	got, _ := os.ReadFile(target)
	if string(got) != "original" {
		t.Fatal("目标文件被修改")
	}
}
func TestUnsafePermissions(t *testing.T) {
	s := testStore(t)
	if err := os.WriteFile(s.Path("state.json"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil {
		t.Fatal("宽松权限敏感文件应拒绝")
	}
}

func TestUpdateContextCancellationBeforeLock(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.UpdateContext(ctx, func(*config.State) error { return nil }); !errors.Is(err, context.Canceled) {
		t.Fatalf("已取消 context 应返回 context.Canceled，实际为 %v", err)
	}
	state, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 {
		t.Fatalf("取消后 revision 改变：%d", state.Revision)
	}
}

func TestUpdateContextCancellationWhileWaitingKeepsOriginalLock(t *testing.T) {
	s := testStore(t)
	locked := make(chan struct{})
	release := make(chan struct{})
	lockDone := make(chan error, 1)
	go func() {
		lockDone <- WithLock(s.Path("state.lock"), func() error {
			close(locked)
			<-release
			return nil
		})
	}()
	<-locked

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancel()
	started := time.Now()
	err := s.UpdateContext(ctx, func(*config.State) error { return nil })
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("锁等待应返回 deadline exceeded，实际为 %v", err)
	}
	if elapsed := time.Since(started); elapsed > time.Second {
		t.Fatalf("锁等待取消未及时返回：%s", elapsed)
	}

	// 等待中的调用不得释放持锁方的锁。
	probe := openLockFile(t, s.Path("state.lock"))
	lockErr := unix.Flock(probe, unix.LOCK_EX|unix.LOCK_NB)
	if lockErr == nil {
		_ = unix.Flock(probe, unix.LOCK_UN)
	}
	close(release)
	if err := <-lockDone; err != nil {
		t.Fatal(err)
	}
	if lockErr == nil {
		t.Fatal("等待取消错误地释放了原持锁方的锁")
	}

	if err := s.UpdateContext(context.Background(), func(*config.State) error { return nil }); err != nil {
		t.Fatalf("原锁释放后更新失败：%v", err)
	}
}

func TestUpdateContextCallbackCancellationDoesNotWriteAndReleasesLock(t *testing.T) {
	s := testStore(t)
	ctx, cancel := context.WithCancel(context.Background())
	err := s.UpdateContext(ctx, func(state *config.State) error {
		state.Secrets["test"] = "must-not-write"
		cancel()
		return nil
	})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("callback 取消应返回 context.Canceled，实际为 %v", err)
	}
	state, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != 0 || len(state.Secrets) != 0 {
		t.Fatalf("callback 取消后状态被写入：revision=%d secrets=%v", state.Revision, state.Secrets)
	}
	if err := s.UpdateContext(context.Background(), func(*config.State) error { return nil }); err != nil {
		t.Fatalf("callback 取消后状态锁未释放：%v", err)
	}
}

func openLockFile(t *testing.T, path string) int {
	t.Helper()
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Close(fd) })
	return fd
}
