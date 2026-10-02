package store

import (
	"github.com/shichao-wang/cpa-tui/internal/config"
	"testing"
)

func TestStrictStateSchema(t *testing.T) {
	for _, data := range []string{
		`{}`,
		`{"schemaVersion":1,"providers":{},"profiles":{},"secrets":{},"unknown":true}`,
		`{"schemaVersion":1,"providers":null,"profiles":{},"secrets":{}}`,
		`{"schemaVersion":1,"providers":{},"profiles":{},"secrets":{}} {}`,
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
