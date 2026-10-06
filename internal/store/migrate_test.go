package store

import (
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
)

func TestMigrateV1ToV2AndIdempotentCurrent(t *testing.T) {
	v1 := []byte(`{"schemaVersion":1,"revision":7,"listen":"127.0.0.1:8317","providers":{"p":{"name":"p","defaultCredentialRef":"d","connections":{"c":{"name":"c","protocol":"responses","baseURL":"https://api.example.test","models":[{"id":"m"}]}}}},"profiles":{},"secrets":{"d":"secret"}}`)
	got, err := MigrateBytes(v1, config.SchemaVersion, map[int]Migration{1: migrateV1ToV2})
	if err != nil {
		t.Fatal(err)
	}
	var state config.State
	if err := json.Unmarshal(got, &state); err != nil {
		t.Fatal(err)
	}
	c := state.Providers["p"].Connections["c"]
	if state.SchemaVersion != 2 || c.AuthType != config.AuthAPIKey || c.ID == "" || state.Revision != 7 {
		t.Fatalf("迁移状态错误：%+v %+v", state, c)
	}
	unchanged, err := MigrateBytes(got, config.SchemaVersion, map[int]Migration{1: migrateV1ToV2})
	if err != nil {
		t.Fatal(err)
	}
	if string(unchanged) != string(got) {
		t.Fatal("当前版本状态不应重写或重新生成 ID")
	}
}

func TestMigrateThroughChainIsSequentialAndInMemory(t *testing.T) {
	input := []byte(`{"schemaVersion":1,"trace":[]}`)
	path := t.TempDir() + "/state.json"
	if err := os.WriteFile(path, input, 0600); err != nil {
		t.Fatal(err)
	}
	chain := map[int]Migration{
		1: testMigrationStep(t, 1, 2, "one"),
		2: testMigrationStep(t, 2, 3, "two"),
	}
	output, err := migrateThroughChain(input, 3, chain)
	if err != nil {
		t.Fatal(err)
	}
	var result struct {
		SchemaVersion int      `json:"schemaVersion"`
		Trace         []string `json:"trace"`
	}
	if err := json.Unmarshal(output, &result); err != nil {
		t.Fatal(err)
	}
	if result.SchemaVersion != 3 || strings.Join(result.Trace, ",") != "one,two" {
		t.Fatalf("迁移顺序错误：%+v", result)
	}
	if string(input) != `{"schemaVersion":1,"trace":[]}` {
		t.Fatal("迁移过程修改了调用方输入")
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(written) != string(input) {
		t.Fatal("内存迁移不应写入中间状态")
	}
}

func TestMigrateThroughChainRejectsFailureMissingAndSkippedVersions(t *testing.T) {
	input := []byte(`{"schemaVersion":1,"trace":[]}`)
	failing := map[int]Migration{
		1: testMigrationStep(t, 1, 2, "one"),
		2: func(raw json.RawMessage) (json.RawMessage, error) {
			raw[0] = '!'
			return nil, fmt.Errorf("模拟中间失败")
		},
	}
	if _, err := migrateThroughChain(input, 3, failing); err == nil {
		t.Fatal("中间步骤失败必须终止迁移")
	}
	if string(input) != `{"schemaVersion":1,"trace":[]}` {
		t.Fatal("失败步骤修改了原始输入")
	}
	if _, err := migrateThroughChain(input, 3, map[int]Migration{1: testMigrationStep(t, 1, 2, "one")}); err == nil {
		t.Fatal("缺少的相邻迁移步骤必须报错")
	}
	if _, err := migrateThroughChain(input, 3, map[int]Migration{1: testMigrationStep(t, 1, 3, "skip")}); err == nil {
		t.Fatal("迁移步骤不得跳版本")
	}
}

func testMigrationStep(t *testing.T, from, to int, label string) Migration {
	t.Helper()
	return func(raw json.RawMessage) (json.RawMessage, error) {
		var state struct {
			SchemaVersion int      `json:"schemaVersion"`
			Trace         []string `json:"trace"`
		}
		if err := json.Unmarshal(raw, &state); err != nil {
			return nil, err
		}
		if state.SchemaVersion != from {
			return nil, fmt.Errorf("预期 v%d，实际 v%d", from, state.SchemaVersion)
		}
		state.SchemaVersion = to
		state.Trace = append(state.Trace, label)
		return json.Marshal(state)
	}
}

func TestMigrateRejectsUnknownAndFuture(t *testing.T) {
	cases := []string{
		`{"schemaVersion":1,"revision":0,"listen":"x","providers":{},"profiles":{},"secrets":{},"unexpected":true}`,
		`{"schemaVersion":3}`,
	}
	for _, input := range cases {
		if _, err := MigrateBytes([]byte(input), config.SchemaVersion, map[int]Migration{1: migrateV1ToV2}); err == nil {
			t.Errorf("应拒绝状态：%s", input)
		}
	}
}

func TestMigrateWritesBackupAndRejectsV1OnNormalRead(t *testing.T) {
	s := testStore(t)
	v1 := `{"schemaVersion":1,"revision":0,"listen":"127.0.0.1:8317","providers":{},"profiles":{},"secrets":{}}`
	if err := AtomicWrite(s.StatePath(), []byte(v1)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Read(); err == nil || !strings.Contains(err.Error(), "state migrate") {
		t.Fatalf("正常读取应提示迁移，得到 %v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	backup, err := os.ReadFile(s.StatePath() + ".v1.bak")
	if err != nil || string(backup) != v1 {
		t.Fatalf("迁移备份错误：%v", err)
	}
	if err := s.Migrate(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.StatePath() + ".v1.bak"); err != nil {
		t.Fatal(err)
	}
}
