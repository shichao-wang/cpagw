package claudecode

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

func testStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.New(filepath.Join(t.TempDir(), "state"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *config.State) error {
		state.Listen = "127.0.0.1:8317"
		state.Secrets["secret-a"] = "token-a"
		state.Secrets["secret-b"] = "token-b"
		state.Profiles["alpha"] = testProfile("alpha", "profile-a", "secret-a", "alpha")
		state.Profiles["first"] = testProfile("first", "profile-a", "secret-a", "first")
		state.Profiles["second"] = testProfile("second", "profile-b", "secret-b", "second")
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return s
}

func testProfile(name, id, keyRef, prefix string) config.Profile {
	models := make(map[string]config.Binding, 3)
	for _, slot := range config.Slots {
		models[slot] = config.Binding{
			PublicModel: "claude-" + prefix + "-" + slot,
			Label:       strings.ToUpper(prefix) + " " + strings.ToUpper(slot),
			Description: "description for " + prefix + " " + slot,
		}
	}
	return config.Profile{Name: name, ID: id, KeyRef: keyRef, Models: models}
}

func writeDoc(t *testing.T, path string, value any, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(data, '\n'), mode); err != nil {
		t.Fatal(err)
	}
}

func readDoc(t *testing.T, path string) map[string]any {
	t.Helper()
	snap, err := readSettings(path)
	if err != nil {
		t.Fatal(err)
	}
	return snap.Data
}

func readObject(t *testing.T, root map[string]any, key string) map[string]any {
	t.Helper()
	value, ok := root[key].(map[string]any)
	if !ok {
		t.Fatalf("%s is not an object: %#v", key, root[key])
	}
	return value
}

func assertEqualJSON(t *testing.T, got, want any) {
	t.Helper()
	if !equalJSON(got, want) {
		t.Fatalf("JSON values differ:\n got: %#v\nwant: %#v", got, want)
	}
}

func TestApplyAndRestoreNewSettings(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), ".claude", "settings.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")

	if err := Apply(s, profile, path); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("settings permissions = %o, want 600", info.Mode().Perm())
	}
	root := readDoc(t, path)
	env := readObject(t, root, "env")
	if env["ANTHROPIC_BASE_URL"] != "http://127.0.0.1:8317" || env["ANTHROPIC_AUTH_TOKEN"] != "token-a" {
		t.Fatalf("unexpected gateway credentials: %#v", env)
	}
	if _, ok := env["ANTHROPIC_API_KEY"]; ok {
		t.Fatalf("legacy API key was not removed: %#v", env)
	}
	if env["CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY"] != "1" {
		t.Fatalf("gateway model discovery is not enabled: %#v", env)
	}
	if env["CLAUDE_CODE_SUBAGENT_MODEL"] != profile.Models["haiku"].PublicModel {
		t.Fatalf("subagent model does not use the public Haiku ID: %#v", env)
	}
	for _, key := range []string{"ANTHROPIC_MODEL", "CLAUDE_CODE_USE_BEDROCK", "CLAUDE_CODE_USE_VERTEX", "CLAUDE_CODE_USE_FOUNDRY"} {
		if _, ok := env[key]; ok {
			t.Fatalf("conflicting model source was not cleared: %s=%v", key, env[key])
		}
	}
	for _, slot := range config.Slots {
		prefix := "ANTHROPIC_DEFAULT_" + strings.ToUpper(slot) + "_MODEL"
		binding := profile.Models[slot]
		if env[prefix] != binding.PublicModel || env[prefix+"_NAME"] != binding.Label || env[prefix+"_DESCRIPTION"] != binding.Description {
			t.Fatalf("unexpected %s settings: %#v", slot, env)
		}
	}
	picker := readObject(t, root, "modelPicker")
	if picker["replaceBuiltInOptions"] != true {
		t.Fatalf("unexpected model picker toggles: %#v", picker)
	}
	if _, exists := picker["discovery"]; exists {
		t.Fatalf("unsupported modelPicker.discovery field was written: %#v", picker)
	}
	if _, exists := root["model"]; exists {
		t.Fatalf("root model override was not cleared: %#v", root)
	}
	if _, exists := root["apiKeyHelper"]; exists {
		t.Fatalf("root API key helper was not cleared: %#v", root)
	}
	options, ok := picker["options"].([]any)
	if !ok || len(options) != 3 {
		t.Fatalf("unexpected model picker options: %#v", picker["options"])
	}
	if !IsManagedProfile(s, "alpha") || IsManagedProfile(s, "other") {
		t.Fatal("managed profile protection did not match")
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("settings created by Apply should be removed after restore, stat err=%v", err)
	}
	if IsManagedProfile(s, "alpha") {
		t.Fatal("profile remained protected after restore")
	}
	if err := Restore(s, path); err == nil {
		t.Fatal("repeated restore should be rejected")
	}
}

func TestRestorePreservesUnrelatedEditsAndPickerItems(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")
	opus := profile.Models["opus"].PublicModel
	baseline := map[string]any{
		"env": map[string]any{"KEEP": "before", "ANTHROPIC_API_KEY": "legacy-key", "ANTHROPIC_BASE_URL": "https://old.example"},
		"modelPicker": map[string]any{
			"customToggle": true,
			"options": []any{
				map[string]any{"model": opus, "label": "original label", "description": "original description", "extra": "original extra"},
				map[string]any{"model": "custom-model", "label": "Custom", "description": "User option"},
			},
		},
		"unrelated": map[string]any{"keep": 42},
	}
	writeDoc(t, path, baseline, 0644)
	if err := Apply(s, profile, path); err != nil {
		t.Fatal(err)
	}

	live := readDoc(t, path)
	env := readObject(t, live, "env")
	env["KEEP"] = "edited after apply"
	env["NEW_ENV"] = "keep me"
	picker := readObject(t, live, "modelPicker")
	options := picker["options"].([]any)
	for _, raw := range options {
		if item, ok := raw.(map[string]any); ok && item["model"] == opus {
			item["extra"] = "user edit on same item"
		}
	}
	options = append(options, map[string]any{"model": "user-added-model", "label": "User added", "newField": true})
	picker["options"] = options
	writeDoc(t, path, live, 0600)

	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	restored := readDoc(t, path)
	restoredEnv := readObject(t, restored, "env")
	if restoredEnv["KEEP"] != "edited after apply" || restoredEnv["NEW_ENV"] != "keep me" {
		t.Fatalf("unrelated env edits were lost: %#v", restoredEnv)
	}
	if restoredEnv["ANTHROPIC_API_KEY"] != "legacy-key" || restoredEnv["ANTHROPIC_BASE_URL"] != "https://old.example" {
		t.Fatalf("original auth settings were not restored: %#v", restoredEnv)
	}
	restoredPicker := readObject(t, restored, "modelPicker")
	if restoredPicker["customToggle"] != true {
		t.Fatalf("unrelated model picker field was lost: %#v", restoredPicker)
	}
	restoredOptions := restoredPicker["options"].([]any)
	seenCustom, seenAdded := false, false
	for _, raw := range restoredOptions {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		switch item["model"] {
		case opus:
			if item["label"] != "original label" || item["description"] != "original description" || item["extra"] != "user edit on same item" {
				t.Fatalf("original picker item was not selectively restored: %#v", item)
			}
		case "custom-model":
			seenCustom = true
		case "user-added-model":
			seenAdded = true
		}
	}
	if !seenCustom || !seenAdded {
		t.Fatalf("unrelated picker entries were lost: %#v", restoredOptions)
	}
}

func TestRestoreRejectsManagedFieldConflictWithoutPartialWrite(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")
	writeDoc(t, path, map[string]any{"env": map[string]any{"KEEP": "unchanged"}}, 0600)
	if err := Apply(s, profile, path); err != nil {
		t.Fatal(err)
	}
	before := readDoc(t, path)
	env := readObject(t, before, "env")
	env["ANTHROPIC_AUTH_TOKEN"] = "manual-token"
	writeDoc(t, path, before, 0600)
	changed := readDoc(t, path)
	if err := Restore(s, path); err == nil {
		t.Fatal("restore should reject manual change to a managed field")
	}
	after := readDoc(t, path)
	assertEqualJSON(t, after, changed)
}

func TestProfileSwitchRestoresFirstBaseline(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	first := testProfile("first", "profile-a", "secret-a", "first")
	second := testProfile("second", "profile-b", "secret-b", "second")
	firstOpus := first.Models["opus"].PublicModel
	baseline := map[string]any{
		"env": map[string]any{"ANTHROPIC_API_KEY": "original-key", "CUSTOM": "value"},
		"modelPicker": map[string]any{"options": []any{
			map[string]any{"model": firstOpus, "label": "baseline label", "description": "baseline description", "origin": "user"},
		}},
		"model":        "claude-previous-model",
		"apiKeyHelper": "credential-helper",
		"other":        "untouched",
	}
	writeDoc(t, path, baseline, 0644)
	if err := Apply(s, first, path); err != nil {
		t.Fatal(err)
	}
	applyIDBefore := mustJournal(t, s).ApplyID
	if err := Apply(s, second, path); err != nil {
		t.Fatal(err)
	}
	j := mustJournal(t, s)
	if j.ApplyID != applyIDBefore || j.ProfileID != second.ID || j.ProfileName != second.Name {
		t.Fatalf("profile switch did not retain one management session: %#v", j)
	}
	live := readDoc(t, path)
	liveEnv := readObject(t, live, "env")
	if liveEnv["ANTHROPIC_AUTH_TOKEN"] != "token-b" || liveEnv["ANTHROPIC_DEFAULT_OPUS_MODEL"] != second.Models["opus"].PublicModel {
		t.Fatalf("second profile was not applied: %#v", liveEnv)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	got := readDoc(t, path)
	assertEqualJSON(t, got, baseline)
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0644 {
		t.Fatalf("restored permissions = %o, want original 644", info.Mode().Perm())
	}
}

func TestRestoreDoesNotDeleteOriginallyEmptySettings(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	writeDoc(t, path, map[string]any{}, 0644)
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("pre-existing empty settings file was removed: %v", err)
	}
	assertEqualJSON(t, readDoc(t, path), map[string]any{})
}

func TestRestoreRequiresManagedRecord(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Restore(s, path); err == nil {
		t.Fatal("restore without an Apply record should fail")
	}
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, path); err == nil {
		t.Fatal("repeated restore should fail")
	}
}

func TestApplyRejectsMalformedJSONWithoutChangingIt(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	original := []byte("{\"env\":")
	if err := os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err == nil {
		t.Fatal("Apply should reject malformed JSON")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(original) {
		t.Fatalf("malformed settings changed: %q", got)
	}
}

func TestApplyRejectsSettingsSymlink(t *testing.T) {
	s := testStore(t)
	dir := t.TempDir()
	actual := filepath.Join(dir, "actual.json")
	link := filepath.Join(dir, "settings.json")
	if err := os.WriteFile(actual, []byte(`{"keep":true}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(actual, link); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), link); err == nil {
		t.Fatal("Apply should reject a settings symlink")
	}
	got, err := os.ReadFile(actual)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != `{"keep":true}` {
		t.Fatalf("symlink target changed: %q", got)
	}
}

func TestInterruptedApplyIsRecoveredBeforeRestore(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")
	if err := Apply(s, profile, path); err != nil {
		t.Fatal(err)
	}
	j := mustJournal(t, s)
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	j.Status = "pending"
	j.Action = "apply"
	j.PriorProfileName = ""
	j.Prior = originalValues(j.Managed)
	j.Goal = appliedValues(j.Managed)
	if err := writeJournal(s, j); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("interrupted apply did not complete and restore cleanly: %v", err)
	}
	if IsManagedProfile(s, profile.Name) {
		t.Fatal("profile remained managed after interrupted operation recovery")
	}
}

func TestApplyUsesClaudeSettingsUnderTemporaryHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)
	s := testStore(t)
	path := filepath.Join(home, ".claude", "settings.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")
	if err := Apply(s, profile, ""); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("default settings path was not used under temporary HOME: %v", err)
	}
	if err := Restore(s, ""); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsUnmanagedSettingsPath(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	wrongPath := filepath.Join(t.TempDir(), "other.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")
	if err := Apply(s, profile, path); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, wrongPath); err == nil {
		t.Fatal("restore at an unmanaged settings path should fail")
	}
	if _, err := os.Stat(path); err != nil || !IsManagedProfile(s, profile.Name) {
		t.Fatalf("mismatched restore affected the managed settings: %v", err)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
}

func TestRestoreRejectsMalformedSettingsWithoutChangingIt(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err != nil {
		t.Fatal(err)
	}
	malformed := []byte("{invalid")
	if err := os.WriteFile(path, malformed, 0600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, path); err == nil {
		t.Fatal("restore should reject malformed JSON")
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(malformed) {
		t.Fatalf("malformed settings changed: %q", got)
	}
}

func TestInterruptedRestoreIsCompletedOnNextRestore(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	profile := testProfile("alpha", "profile-a", "secret-a", "alpha")
	if err := Apply(s, profile, path); err != nil {
		t.Fatal(err)
	}
	j := mustJournal(t, s)
	j.Status = "pending"
	j.Action = "restore"
	j.Prior = appliedValues(j.Managed)
	j.Goal = originalValues(j.Managed)
	j.Applied = cloneSnapshot(j.Original)
	if err := writeJournal(s, j); err != nil {
		t.Fatal(err)
	}
	// 模拟 settings 已恢复（本例原先不存在，因此目标文件已删除），但进程在
	// 删除管理记录前中断。
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	if IsManagedProfile(s, profile.Name) {
		t.Fatal("恢复中断后管理记录未清除")
	}
}

func TestApplyRejectsAuthenticationHeadersInCustomHeaders(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	original := map[string]any{"env": map[string]any{"ANTHROPIC_CUSTOM_HEADERS": "Authorization: Bearer old-token"}}
	writeDoc(t, path, original, 0600)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err == nil {
		t.Fatal("认证类 custom headers should cause a conflict")
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(after) != string(before) {
		t.Fatal("conflicting custom headers were changed")
	}
}

func TestApplyUsesCurrentStoredProfileAndRejectsStaleIdentity(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	stale := testProfile("alpha", "profile-a", "revoked-ref", "stale")
	if err := Apply(s, stale, path); err != nil {
		t.Fatal(err)
	}
	if token := readObject(t, readDoc(t, path), "env")["ANTHROPIC_AUTH_TOKEN"]; token != "token-a" {
		t.Fatalf("Apply used a stale caller profile instead of the stored profile: %v", token)
	}
	if err := Restore(s, path); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *config.State) error {
		profile := state.Profiles["alpha"]
		profile.ID = "replacement-id"
		state.Profiles["alpha"] = profile
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err == nil {
		t.Fatal("Apply should reject a replaced profile with the same name")
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("stale profile created settings: %v", err)
	}
}

func TestIsManagedProfileFailsClosedForInvalidJournal(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err != nil {
		t.Fatal(err)
	}
	j := mustJournal(t, s)
	j.Status = "invalid"
	if err := writeJournal(s, j); err != nil {
		t.Fatal(err)
	}
	if !IsManagedProfile(s, "alpha") {
		t.Fatal("invalid management journal should fail closed")
	}
}

func TestIsManagedProfileUsesProfileIDWhenNamesAreReused(t *testing.T) {
	s := testStore(t)
	path := filepath.Join(t.TempDir(), "settings.json")
	if err := Apply(s, testProfile("alpha", "profile-a", "secret-a", "alpha"), path); err != nil {
		t.Fatal(err)
	}
	if err := s.Update(func(state *config.State) error {
		profile := state.Profiles["alpha"]
		profile.ID = "replacement-id"
		state.Profiles["alpha"] = profile
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if IsManagedProfile(s, "alpha") {
		t.Fatal("management record for an old profile ID blocked its replacement")
	}
}

func mustJournal(t *testing.T, s *store.Store) *journal {
	t.Helper()
	j, err := readJournal(s)
	if err != nil {
		t.Fatal(err)
	}
	if j == nil {
		t.Fatal("management journal is missing")
	}
	return j
}
