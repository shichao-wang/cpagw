package store

import (
	"os"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
)

func TestStrictStateSchema(t *testing.T) {
	for _, data := range []string{
		`{}`,
		`{"schemaVersion":1,"providers":{},"profiles":{},"secrets":{}}`,
		`{"schemaVersion":2,"providers":{},"connections":{},"profiles":{},"secrets":{}}`,
		`{"schemaVersion":2,"connections":{},"profiles":{},"secrets":{},"unknown":true}`,
		`{"schemaVersion":2,"connections":null,"profiles":{},"secrets":{}}`,
		`{"schemaVersion":2,"connections":{},"profiles":null,"secrets":{}}`,
		`{"schemaVersion":2,"connections":{},"profiles":{},"secrets":{}} {}`,
	} {
		t.Run(data, func(t *testing.T) {
			s := testStore(t)
			if err := AtomicWrite(s.Path("state.json"), []byte(data)); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Read(); err == nil {
				t.Fatal("无效状态不应通过校验")
			}
		})
	}
}

func TestLegacyStateIsNeverOverwritten(t *testing.T) {
	for _, data := range []string{
		`{"schemaVersion":1,"revision":3,"listen":"127.0.0.1:8317","providers":{},"profiles":{},"secrets":{}}`,
		`{"schemaVersion":2,"revision":3,"listen":"127.0.0.1:8317","connections":{},"profiles":{"old":{"models":{"opus":{"provider":"p"}}}},"secrets":{}}`,
	} {
		s := testStore(t)
		original := []byte(data)
		if err := AtomicWrite(s.Path("state.json"), original); err != nil {
			t.Fatal(err)
		}
		if _, err := s.Read(); err == nil || !strings.Contains(err.Error(), "新的 --state-dir") {
			t.Fatalf("旧状态错误应提示使用新的 --state-dir：%v", err)
		}
		if err := s.Update(func(st *config.State) error { return nil }); err == nil {
			t.Fatal("旧格式状态不得被覆盖")
		}
		got, err := os.ReadFile(s.Path("state.json"))
		if err != nil {
			t.Fatal(err)
		}
		if string(got) != data {
			t.Fatal("拒绝旧格式后原文件字节发生变化")
		}
	}
}

func TestUpdateRollback(t *testing.T) {
	s := testStore(t)
	if err := s.Update(func(st *config.State) error { st.Listen = "127.0.0.1:9999"; return nil }); err != nil {
		t.Fatal(err)
	}
	before, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	err = s.Update(func(st *config.State) error { st.Listen = "127.0.0.1:10000"; return config.ValidateName("bad/name") })
	if err == nil {
		t.Fatal("操作应失败")
	}
	after, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision || after.Listen != before.Listen {
		t.Fatal("失败事务不应影响持久状态")
	}
}
