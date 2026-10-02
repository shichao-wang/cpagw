package store

import (
	"github.com/shichao-wang/cpagw/internal/config"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
