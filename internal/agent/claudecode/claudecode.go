// Package claudecode 将 cpagw profile 合并到 Claude Code 设置，并支持安全恢复。
package claudecode

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

const journalVersion = 1

var managedRootNames = []string{"model", "apiKeyHelper"}
var managedPickerNames = []string{"replaceBuiltInOptions"}
var managedOptionNames = []string{"label", "description"}

// snapshot 是一次 settings 文件的完整状态；恢复时仍只回滚受管字段。
type snapshot struct {
	Exists bool           `json:"exists"`
	Mode   uint32         `json:"mode,omitempty"`
	Data   map[string]any `json:"data"`
}

type fieldValue struct {
	Present bool `json:"present"`
	Value   any  `json:"value,omitempty"`
}

type fieldChange struct {
	Original fieldValue `json:"original"`
	Applied  fieldValue `json:"applied"`
}

type optionChange struct {
	OriginalPresent bool                   `json:"originalPresent"`
	Fields          map[string]fieldChange `json:"fields"`
}

type managedFields struct {
	Root    map[string]fieldChange  `json:"root"`
	Env     map[string]fieldChange  `json:"env"`
	Picker  map[string]fieldChange  `json:"picker"`
	Options map[string]optionChange `json:"options"`
}

type fieldSet struct {
	Root    map[string]fieldValue            `json:"root"`
	Env     map[string]fieldValue            `json:"env"`
	Picker  map[string]fieldValue            `json:"picker"`
	Options map[string]map[string]fieldValue `json:"options"`
}

type journal struct {
	Version          int           `json:"version"`
	SettingsPath     string        `json:"settingsPath"`
	ApplyID          string        `json:"applyID"`
	ProfileID        string        `json:"profileID"`
	ProfileName      string        `json:"profileName"`
	PriorProfileName string        `json:"priorProfileName,omitempty"`
	PriorProfileID   string        `json:"priorProfileID,omitempty"`
	Status           string        `json:"status"` // pending 表示待恢复，committed 表示已完成
	Action           string        `json:"action"` // apply 表示应用，restore 表示恢复
	Original         *snapshot     `json:"original"`
	Applied          *snapshot     `json:"applied"`
	Managed          managedFields `json:"managed"`
	Prior            fieldSet      `json:"prior,omitempty"`
	Goal             fieldSet      `json:"goal,omitempty"`
}

// Apply 将指定 profile 合并到 Claude Code 设置中，并保留不相关设置。
// settingsPath 为空时使用 ~/.claude/settings.json。
func Apply(s *store.Store, profile config.Profile, settingsPath string) error {
	if s == nil {
		return fmt.Errorf("状态存储未初始化")
	}
	path, err := resolveSettingsPath(settingsPath)
	if err != nil {
		return err
	}
	if strings.TrimSpace(profile.Name) == "" || strings.TrimSpace(profile.ID) == "" {
		return fmt.Errorf("profile 缺少名称或 ID")
	}
	return store.WithLock(s.Path("state.lock"), func() error {
		return withJournalLock(s, func() error {
			state, err := s.Read()
			if err != nil {
				return err
			}
			storedProfile, ok := state.Profiles[profile.Name]
			if !ok || storedProfile.ID != profile.ID {
				return fmt.Errorf("profile 已不存在或 ID 已变化，拒绝应用")
			}
			profile = storedProfile
			if err := validateProfileModels(profile); err != nil {
				return err
			}
			secret := state.Secrets[profile.KeyRef]
			listen := strings.TrimSpace(state.Listen)
			if listen == "" || (strings.Contains(listen, "://") && !strings.HasPrefix(listen, "http://")) {
				return fmt.Errorf("网关监听地址无效")
			}
			if profile.KeyRef == "" || strings.TrimSpace(secret) == "" {
				return fmt.Errorf("profile %s 缺少有效的 API key", profile.Name)
			}
			if err := prepareJournalDir(s); err != nil {
				return err
			}
			j, err := readJournal(s)
			if err != nil {
				return err
			}
			if j != nil {
				if err := validateJournal(j); err != nil {
					return err
				}
				if err := recoverPending(s, j); err != nil {
					return err
				}
				j, err = readJournal(s)
				if err != nil {
					return err
				}
			}
			if j != nil && j.Action == "restore" {
				return fmt.Errorf("已有待完成的设置恢复，请先完成恢复")
			}
			if j != nil && j.SettingsPath != path {
				return fmt.Errorf("cpagw 已管理另一 settings 路径，拒绝切换：%s", j.SettingsPath)
			}
			if j != nil && j.Status != "committed" {
				return fmt.Errorf("cpagw 管理记录状态无效，拒绝覆盖")
			}

			current, err := readSettings(path)
			if err != nil {
				return err
			}
			if err := validateCustomHeaders(current.Data); err != nil {
				return err
			}
			var original *snapshot
			var fields managedFields
			var applyID, priorProfile, priorProfileID string
			prior := emptyFieldSet()
			if j == nil {
				original = cloneSnapshot(current)
				applyID, err = config.RandomID()
				if err != nil {
					return fmt.Errorf("生成 apply ID 失败：%w", err)
				}
				fields = emptyManagedFields()
			} else {
				if !recordIsManaged(j) || j.Action != "apply" {
					return fmt.Errorf("不存在有效的 cpagw Claude Code 管理记录")
				}
				if !matchesManaged(current.Data, appliedValues(j.Managed)) {
					return fmt.Errorf("Claude Code 受管设置已被手动修改；拒绝覆盖")
				}
				original = cloneSnapshot(j.Original)
				fields = cloneManagedFields(j.Managed)
				applyID = j.ApplyID
				priorProfile = j.ProfileName
				priorProfileID = j.ProfileID
				prior = appliedValues(j.Managed)
			}

			for _, name := range managedRootNames {
				change, ok := fields.Root[name]
				if !ok {
					change.Original = fieldValueOf(original.Data, name)
					prior.Root[name] = fieldValueOf(current.Data, name)
				}
				change.Applied = fieldValue{}
				fields.Root[name] = change
			}
			desiredEnv := profileEnvironment(profile, listen, secret)
			for key, value := range desiredEnv {
				if _, ok := fields.Env[key]; !ok {
					fields.Env[key] = fieldChange{Original: valueAt(original.Data, "env", key)}
					prior.Env[key] = valueAt(current.Data, "env", key)
				}
				fields.Env[key] = fieldChange{Original: fields.Env[key].Original, Applied: fieldValue{Present: value.Present, Value: value.Value}}
			}
			for _, name := range managedPickerNames {
				v := fieldValue{Present: true, Value: true}
				if _, ok := fields.Picker[name]; !ok {
					fields.Picker[name] = fieldChange{Original: valueAt(original.Data, "modelPicker", name)}
					prior.Picker[name] = valueAt(current.Data, "modelPicker", name)
				}
				fields.Picker[name] = fieldChange{Original: fields.Picker[name].Original, Applied: v}
			}

			activeOptions := profileOptions(profile)
			for id, option := range fields.Options {
				for _, key := range managedOptionNames {
					change, ok := option.Fields[key]
					if !ok {
						continue
					}
					// 切换 profile 时移除旧 profile 的选项，或将原有选项恢复为首次记录的标签。
					change.Applied = change.Original
					if active, ok := activeOptions[id]; ok {
						change.Applied = fieldValue{Present: true, Value: active[key]}
					}
					option.Fields[key] = change
				}
				fields.Options[id] = option
			}
			for id, option := range activeOptions {
				if _, ok := fields.Options[id]; !ok {
					baseOption, exists, err := originalOption(original.Data, id)
					if err != nil {
						return err
					}
					fields.Options[id] = optionChange{OriginalPresent: exists, Fields: map[string]fieldChange{
						"label":       {Original: mapValue(baseOption, "label")},
						"description": {Original: mapValue(baseOption, "description")},
					}}
					for _, key := range managedOptionNames {
						prior.option(id)[key] = currentOptionValue(current.Data, id, key)
					}
				}
				change := fields.Options[id]
				for key, value := range option {
					item := change.Fields[key]
					item.Applied = fieldValue{Present: true, Value: value}
					change.Fields[key] = item
				}
				fields.Options[id] = change
			}

			goal := appliedValues(fields)
			data := cloneMap(current.Data)
			if err := patchSettings(data, fields, false, original.Data); err != nil {
				return err
			}
			planned := &snapshot{Exists: true, Mode: 0600, Data: data}
			pending := &journal{
				Version: journalVersion, SettingsPath: path, ApplyID: applyID,
				ProfileID: profile.ID, ProfileName: profile.Name,
				PriorProfileName: priorProfile, PriorProfileID: priorProfileID,
				Status: "pending", Action: "apply",
				Original: original, Applied: planned, Managed: fields, Prior: prior, Goal: goal,
			}
			if err := writeJournal(s, pending); err != nil {
				return err
			}
			latest, err := readSettings(path)
			if err != nil {
				return err
			}
			if !matchesManaged(latest.Data, prior) {
				return fmt.Errorf("Claude Code 受管设置在应用期间发生变化；拒绝覆盖")
			}
			latestData := cloneMap(latest.Data)
			if err := patchSettings(latestData, fields, false, original.Data); err != nil {
				return err
			}
			planned.Data = latestData
			pending.Applied = planned
			if err := writeJournal(s, pending); err != nil {
				return err
			}
			if err := writeSettings(path, planned.Data, 0600); err != nil {
				return fmt.Errorf("写入 Claude Code settings 失败：%w", err)
			}
			readback, err := readSettings(path)
			if err != nil {
				return fmt.Errorf("读取回写后的 Claude Code settings 失败：%w", err)
			}
			if !equalJSON(readback.Data, planned.Data) {
				return fmt.Errorf("Claude Code settings 回读校验失败")
			}
			pending.Status = "committed"
			pending.Prior = fieldSet{}
			pending.Goal = fieldSet{}
			pending.Applied = readback
			return writeJournal(s, pending)
		})
	})
}

// Restore 仅恢复有效 cpagw Apply 记录中的受管字段，保留用户编辑和无关模型选项。
func Restore(s *store.Store, settingsPath string) error {
	if s == nil {
		return fmt.Errorf("状态存储未初始化")
	}
	path, err := resolveSettingsPath(settingsPath)
	if err != nil {
		return err
	}
	return withJournalLock(s, func() error {
		if err := prepareJournalDir(s); err != nil {
			return err
		}
		j, err := readJournal(s)
		if err != nil {
			return err
		}
		if j == nil || !recordIsManaged(j) {
			return fmt.Errorf("没有有效的 cpagw Claude Code 管理记录，拒绝恢复")
		}
		if err := validateJournal(j); err != nil {
			return err
		}
		if j.SettingsPath != path {
			return fmt.Errorf("settings 路径与 cpagw 管理记录不匹配，拒绝恢复")
		}
		if j.Status == "pending" {
			if j.Action != "restore" {
				if err := recoverPending(s, j); err != nil {
					return err
				}
				j, err = readJournal(s)
				if err != nil {
					return err
				}
				if j == nil || j.Status != "committed" {
					return fmt.Errorf("应用中断状态无法安全恢复")
				}
			} else {
				return recoverPending(s, j)
			}
		}
		if j.Action != "apply" || j.Status != "committed" {
			return fmt.Errorf("没有可恢复的 cpagw Claude Code 应用记录")
		}
		current, err := readSettings(path)
		if err != nil {
			return err
		}
		if !matchesManaged(current.Data, appliedValues(j.Managed)) {
			return fmt.Errorf("Claude Code 受管设置已被手动修改；拒绝恢复")
		}
		goal := originalValues(j.Managed)
		data := cloneMap(current.Data)
		if err := patchSettings(data, j.Managed, true, j.Original.Data); err != nil {
			return err
		}
		restored := &snapshot{Exists: true, Mode: uint32(j.Original.Mode), Data: data}
		restorePending := cloneJournal(j)
		restorePending.Status = "pending"
		restorePending.Action = "restore"
		restorePending.Prior = appliedValues(j.Managed)
		restorePending.Goal = goal
		restorePending.Applied = restored
		if err := writeJournal(s, restorePending); err != nil {
			return err
		}
		latest, err := readSettings(path)
		if err != nil {
			return err
		}
		if !matchesManaged(latest.Data, restorePending.Prior) {
			return fmt.Errorf("Claude Code 受管设置在恢复期间发生变化；拒绝覆盖")
		}
		latestData := cloneMap(latest.Data)
		if err := patchSettings(latestData, j.Managed, true, j.Original.Data); err != nil {
			return err
		}
		restorePending.Applied.Data = latestData
		if err := writeJournal(s, restorePending); err != nil {
			return err
		}
		if err := finishRestore(s, restorePending, latest); err != nil {
			return err
		}
		return nil
	})
}

// IsManagedProfile 判断 profileName 是否由有效管理记录保护；切换过程会同时保护新旧 profile。
func IsManagedProfile(s *store.Store, profileName string) bool {
	if s == nil || profileName == "" {
		return false
	}
	protected := false
	if err := withJournalLock(s, func() error {
		j, err := readJournal(s)
		if err != nil {
			return err
		}
		if j == nil {
			return nil
		}
		if err := validateJournal(j); err != nil {
			protected = true
			return nil
		}
		state, err := s.Read()
		if err != nil {
			return err
		}
		profile, exists := state.Profiles[profileName]
		if !exists {
			return nil
		}
		protected = (j.ProfileName == profileName && profile.ID == j.ProfileID) ||
			(j.Status == "pending" && j.PriorProfileName == profileName && profile.ID == j.PriorProfileID)
		return nil
	}); err != nil {
		// 读取管理记录失败时按受保护处理，避免 CLI 删除可能正在使用的 profile。
		return true
	}
	return protected
}

func resolveSettingsPath(path string) (string, error) {
	if path == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("获取用户目录失败：%w", err)
		}
		path = filepath.Join(home, ".claude", "settings.json")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", fmt.Errorf("解析 settings 路径失败：%w", err)
	}
	return filepath.Clean(abs), nil
}

func validateCustomHeaders(root map[string]any) error {
	env, ok := root["env"].(map[string]any)
	if !ok {
		return nil
	}
	value, exists := env["ANTHROPIC_CUSTOM_HEADERS"]
	if !exists || value == nil {
		return nil
	}
	headers, ok := value.(string)
	if !ok {
		return fmt.Errorf("settings.env.ANTHROPIC_CUSTOM_HEADERS 格式未知，无法确认认证来源")
	}
	normalized := strings.ReplaceAll(strings.ToLower(headers), "_", "-")
	for _, marker := range []string{"authorization", "api-key", "auth-token", "access-token", "bearer "} {
		if strings.Contains(normalized, marker) {
			return fmt.Errorf("settings.env.ANTHROPIC_CUSTOM_HEADERS 可能覆盖认证信息，请移除认证类自定义头后重试")
		}
	}
	return nil
}

func validateProfileModels(profile config.Profile) error {
	if len(profile.Models) != len(config.Slots) {
		return fmt.Errorf("profile 必须配置 opus、sonnet、haiku 三个模型档位")
	}
	seen := map[string]bool{}
	for _, slot := range config.Slots {
		binding, ok := profile.Models[slot]
		if !ok || strings.TrimSpace(binding.PublicModel) == "" || seen[binding.PublicModel] {
			return fmt.Errorf("profile 的 %s 档位缺少唯一的公开模型 ID", slot)
		}
		seen[binding.PublicModel] = true
	}
	return nil
}

func profileEnvironment(profile config.Profile, listen, secret string) map[string]fieldValue {
	values := map[string]fieldValue{
		"ANTHROPIC_BASE_URL":                         {Present: true, Value: "http://" + strings.TrimPrefix(listen, "http://")},
		"ANTHROPIC_AUTH_TOKEN":                       {Present: true, Value: secret},
		"ANTHROPIC_API_KEY":                          {Present: false},
		"ANTHROPIC_MODEL":                            {Present: false},
		"CLAUDE_CODE_USE_BEDROCK":                    {Present: false},
		"CLAUDE_CODE_USE_VERTEX":                     {Present: false},
		"CLAUDE_CODE_USE_FOUNDRY":                    {Present: false},
		"CLAUDE_CODE_ENABLE_GATEWAY_MODEL_DISCOVERY": {Present: true, Value: "1"},
		"CLAUDE_CODE_SUBAGENT_MODEL":                 {Present: true, Value: profile.Models["haiku"].PublicModel},
	}
	for _, slot := range config.Slots {
		binding := profile.Models[slot]
		prefix := "ANTHROPIC_DEFAULT_" + strings.ToUpper(slot) + "_MODEL"
		values[prefix] = fieldValue{Present: true, Value: binding.PublicModel}
		values[prefix+"_NAME"] = fieldValue{Present: true, Value: binding.Label}
		values[prefix+"_DESCRIPTION"] = fieldValue{Present: true, Value: binding.Description}
	}
	return values
}

func profileOptions(profile config.Profile) map[string]map[string]any {
	options := make(map[string]map[string]any, len(profile.Models))
	for _, binding := range profile.Models {
		options[binding.PublicModel] = map[string]any{"label": binding.Label, "description": binding.Description}
	}
	return options
}

func emptyManagedFields() managedFields {
	return managedFields{Root: map[string]fieldChange{}, Env: map[string]fieldChange{}, Picker: map[string]fieldChange{}, Options: map[string]optionChange{}}
}

func emptyFieldSet() fieldSet {
	return fieldSet{Root: map[string]fieldValue{}, Env: map[string]fieldValue{}, Picker: map[string]fieldValue{}, Options: map[string]map[string]fieldValue{}}
}

func (f *fieldSet) option(id string) map[string]fieldValue {
	if f.Options == nil {
		f.Options = map[string]map[string]fieldValue{}
	}
	if f.Options[id] == nil {
		f.Options[id] = map[string]fieldValue{}
	}
	return f.Options[id]
}

func appliedValues(fields managedFields) fieldSet {
	out := emptyFieldSet()
	for key, change := range fields.Root {
		out.Root[key] = change.Applied
	}
	for key, change := range fields.Env {
		out.Env[key] = change.Applied
	}
	for key, change := range fields.Picker {
		out.Picker[key] = change.Applied
	}
	for id, option := range fields.Options {
		for key, change := range option.Fields {
			out.option(id)[key] = change.Applied
		}
	}
	return out
}

func originalValues(fields managedFields) fieldSet {
	out := emptyFieldSet()
	for key, change := range fields.Root {
		out.Root[key] = change.Original
	}
	for key, change := range fields.Env {
		out.Env[key] = change.Original
	}
	for key, change := range fields.Picker {
		out.Picker[key] = change.Original
	}
	for id, option := range fields.Options {
		for key, change := range option.Fields {
			out.option(id)[key] = change.Original
		}
	}
	return out
}

func matchesManaged(data map[string]any, expected fieldSet) bool {
	for key, value := range expected.Root {
		if !sameValue(fieldValueOf(data, key), value) {
			return false
		}
	}
	for key, value := range expected.Env {
		if !sameValue(valueAt(data, "env", key), value) {
			return false
		}
	}
	for key, value := range expected.Picker {
		if !sameValue(valueAt(data, "modelPicker", key), value) {
			return false
		}
	}
	for id, fields := range expected.Options {
		for key, value := range fields {
			if !sameValue(currentOptionValue(data, id, key), value) {
				return false
			}
		}
	}
	return true
}

func patchSettings(data map[string]any, fields managedFields, restore bool, original map[string]any) error {
	if data == nil {
		return fmt.Errorf("settings 根节点必须是 JSON 对象")
	}
	for key, change := range fields.Root {
		value := change.Applied
		if restore {
			value = change.Original
		}
		if value.Present {
			data[key] = value.Value
		} else {
			delete(data, key)
		}
	}
	for key, change := range fields.Env {
		value := change.Applied
		if restore {
			value = change.Original
		}
		env, err := object(data, "env", value.Present)
		if err != nil {
			return err
		}
		if value.Present {
			env[key] = value.Value
		} else if env != nil {
			delete(env, key)
		}
	}
	pickerNeeded := len(fields.Picker) > 0 || len(fields.Options) > 0
	var picker map[string]any
	if pickerNeeded {
		var err error
		picker, err = object(data, "modelPicker", true)
		if err != nil {
			return err
		}
	}
	for key, change := range fields.Picker {
		value := change.Applied
		if restore {
			value = change.Original
		}
		if value.Present {
			picker[key] = value.Value
		} else {
			delete(picker, key)
		}
	}
	if len(fields.Options) > 0 {
		if err := patchOptions(picker, fields, restore, original); err != nil {
			return err
		}
	}

	// 删除本次创建且已经空掉的容器，但不删除原先存在的空对象。
	if env, ok := data["env"].(map[string]any); ok && len(env) == 0 {
		if _, existed := original["env"]; !existed {
			delete(data, "env")
		}
	}
	if picker, ok := data["modelPicker"].(map[string]any); ok && len(picker) == 0 {
		if _, existed := original["modelPicker"]; !existed {
			delete(data, "modelPicker")
		}
	}
	return nil
}

func patchOptions(picker map[string]any, fields managedFields, restore bool, original map[string]any) error {
	raw, exists := picker["options"]
	var options []any
	if exists {
		var ok bool
		options, ok = raw.([]any)
		if !ok {
			return fmt.Errorf("settings.modelPicker.options 必须是数组")
		}
	}
	for id, option := range fields.Options {
		indices := matchingOptionIndices(options, id)
		if len(indices) > 1 {
			return fmt.Errorf("modelPicker.options 中存在重复 model ID：%s", id)
		}
		var item map[string]any
		if len(indices) == 1 {
			var ok bool
			item, ok = options[indices[0]].(map[string]any)
			if !ok {
				return fmt.Errorf("modelPicker.options 项格式无效：%s", id)
			}
		}
		if item == nil {
			item = map[string]any{"model": id}
		}
		for key, change := range option.Fields {
			value := change.Applied
			if restore {
				value = change.Original
			}
			if value.Present {
				item[key] = value.Value
			} else {
				delete(item, key)
			}
		}
		// 工具创建的 picker 项若只剩 model 标识，则删除；其他字段由用户新增时予以保留。
		if !option.OriginalPresent && noManagedOptionFields(item) && len(item) == 1 {
			if len(indices) == 1 {
				options = append(options[:indices[0]], options[indices[0]+1:]...)
			}
			continue
		}
		if len(indices) == 1 {
			options[indices[0]] = item
		} else {
			options = append(options, item)
		}
	}
	if restore && len(options) == 0 {
		originalPicker, _ := original["modelPicker"].(map[string]any)
		_, wasPresent := originalPicker["options"]
		if wasPresent {
			picker["options"] = []any{}
		} else {
			delete(picker, "options")
		}
	} else if len(options) > 0 || exists {
		picker["options"] = options
	}
	return nil
}

func noManagedOptionFields(item map[string]any) bool {
	return !fieldValueOf(item, "label").Present && !fieldValueOf(item, "description").Present
}

func matchingOptionIndices(options []any, id string) []int {
	var indices []int
	for i, raw := range options {
		if item, ok := raw.(map[string]any); ok {
			if model, ok := item["model"].(string); ok && model == id {
				indices = append(indices, i)
			}
		}
	}
	return indices
}

func originalOption(data map[string]any, id string) (map[string]any, bool, error) {
	picker, ok := data["modelPicker"].(map[string]any)
	if !ok {
		return nil, false, nil
	}
	options, ok := picker["options"].([]any)
	if !ok {
		if _, exists := picker["options"]; exists {
			return nil, false, fmt.Errorf("初始 settings.modelPicker.options 必须是数组")
		}
		return nil, false, nil
	}
	indices := matchingOptionIndices(options, id)
	if len(indices) > 1 {
		return nil, false, fmt.Errorf("初始 modelPicker.options 中存在重复 model ID：%s", id)
	}
	if len(indices) == 0 {
		return nil, false, nil
	}
	item, ok := options[indices[0]].(map[string]any)
	if !ok {
		return nil, false, fmt.Errorf("初始 modelPicker.options 项格式无效：%s", id)
	}
	return item, true, nil
}

func currentOptionValue(data map[string]any, id, key string) fieldValue {
	picker, ok := data["modelPicker"].(map[string]any)
	if !ok {
		return fieldValue{}
	}
	options, ok := picker["options"].([]any)
	if !ok {
		return fieldValue{}
	}
	indices := matchingOptionIndices(options, id)
	if len(indices) != 1 {
		return fieldValue{}
	}
	item, ok := options[indices[0]].(map[string]any)
	if !ok {
		return fieldValue{}
	}
	return fieldValueOf(item, key)
}

func object(root map[string]any, key string, create bool) (map[string]any, error) {
	value, exists := root[key]
	if !exists {
		if !create {
			return nil, nil
		}
		m := map[string]any{}
		root[key] = m
		return m, nil
	}
	m, ok := value.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("settings.%s 必须是 JSON 对象", key)
	}
	return m, nil
}

func valueAt(root map[string]any, objectName, key string) fieldValue {
	obj, ok := root[objectName].(map[string]any)
	if !ok {
		return fieldValue{}
	}
	return fieldValueOf(obj, key)
}

func fieldValueOf(obj map[string]any, key string) fieldValue {
	value, ok := obj[key]
	return fieldValue{Present: ok, Value: value}
}

func mapValue(obj map[string]any, key string) fieldValue {
	return fieldValueOf(obj, key)
}

func sameValue(a, b fieldValue) bool {
	return a.Present == b.Present && (!a.Present || equalJSON(a.Value, b.Value))
}

func equalJSON(a, b any) bool {
	left, err := json.Marshal(a)
	if err != nil {
		return false
	}
	right, err := json.Marshal(b)
	return err == nil && bytes.Equal(left, right)
}

func readSettings(path string) (*snapshot, error) {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return &snapshot{Exists: false, Data: map[string]any{}}, nil
	}
	if err != nil {
		return nil, fmt.Errorf("读取 settings 文件信息失败：%w", err)
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("拒绝非普通 settings 文件或符号链接：%s", path)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("读取 settings 文件失败：%w", err)
	}
	var root map[string]any
	if err := json.Unmarshal(data, &root); err != nil {
		return nil, fmt.Errorf("settings JSON 格式无效，拒绝覆盖：%w", err)
	}
	if root == nil {
		return nil, fmt.Errorf("settings 根节点必须是 JSON 对象")
	}
	return &snapshot{Exists: true, Mode: uint32(info.Mode().Perm()), Data: root}, nil
}

func writeSettings(path string, data map[string]any, mode uint32) error {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return fmt.Errorf("创建 settings 目录失败：%w", err)
	}
	if info, err := os.Lstat(path); err == nil {
		if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("拒绝写入非普通 settings 文件或符号链接：%s", path)
		}
		// 先收紧已有 0644 settings 的权限，再走 store 的安全原子写入。
		if info.Mode().Perm() != 0600 {
			if err := os.Chmod(path, 0600); err != nil {
				return fmt.Errorf("收紧 settings 文件权限失败：%w", err)
			}
		}
	} else if !os.IsNotExist(err) {
		return err
	}
	encoded, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	if err := store.AtomicWrite(path, append(encoded, '\n')); err != nil {
		return err
	}
	if mode != 0 && mode != 0600 {
		if err := os.Chmod(path, os.FileMode(mode).Perm()); err != nil {
			return fmt.Errorf("恢复 settings 文件权限失败：%w", err)
		}
	}
	return nil
}

func removeSettings(path string) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() || info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("拒绝删除非普通 settings 文件或符号链接：%s", path)
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func resolveSettingsSnapshot(path string, data map[string]any, original *snapshot, action string) error {
	if action == "restore" && !original.Exists && len(data) == 0 {
		return removeSettings(path)
	}
	mode := uint32(0600)
	if action == "restore" && original.Exists {
		mode = original.Mode
	}
	return writeSettings(path, data, mode)
}

func withJournalLock(s *store.Store, fn func() error) error {
	return store.WithLock(s.Path("claudecode.lock"), fn)
}

func prepareJournalDir(s *store.Store) error {
	return store.EnsureDir(s.Path("claudecode"))
}

func journalPath(s *store.Store) string { return s.Path("claudecode", "settings.json") }

func readJournal(s *store.Store) (*journal, error) {
	path := journalPath(s)
	if err := store.CheckFile(path); err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var j journal
	if err := json.Unmarshal(data, &j); err != nil {
		return nil, fmt.Errorf("cpagw Claude Code 管理记录损坏，拒绝覆盖")
	}
	return &j, nil
}

func writeJournal(s *store.Store, j *journal) error {
	if j == nil {
		return fmt.Errorf("管理记录为空")
	}
	return store.WriteJSON(journalPath(s), j)
}

func deleteJournal(s *store.Store) error {
	path := journalPath(s)
	if err := store.CheckFile(path); err != nil {
		return err
	}
	if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
		return err
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}

func recordIsManaged(j *journal) bool {
	return j != nil && j.Version == journalVersion && j.SettingsPath != "" && j.ApplyID != "" && j.ProfileID != "" && j.ProfileName != "" && j.Original != nil && j.Applied != nil && len(j.Managed.Root)+len(j.Managed.Env)+len(j.Managed.Picker)+len(j.Managed.Options) > 0
}

func validateJournal(j *journal) error {
	if !recordIsManaged(j) || (j.Status != "pending" && j.Status != "committed") || (j.Action != "apply" && j.Action != "restore") {
		return fmt.Errorf("cpagw Claude Code 管理记录无效，拒绝操作")
	}
	if !filepath.IsAbs(j.SettingsPath) {
		return fmt.Errorf("cpagw 管理记录中的 settings 路径无效")
	}
	return nil
}

func recoverPending(s *store.Store, j *journal) error {
	if j.Status != "pending" {
		return nil
	}
	current, err := readSettings(j.SettingsPath)
	if err != nil {
		return err
	}
	if matchesManaged(current.Data, j.Goal) {
		if j.Action == "restore" {
			return finishRestore(s, j, current)
		}
		j.Status = "committed"
		j.Prior, j.Goal = fieldSet{}, fieldSet{}
		j.Applied = current
		return writeJournal(s, j)
	}
	if !matchesManaged(current.Data, j.Prior) {
		return fmt.Errorf("发现中断的 Claude Code 操作且受管字段已变化；拒绝自动续写")
	}
	data := cloneMap(current.Data)
	if j.Action == "apply" {
		if err := patchSettings(data, j.Managed, false, j.Original.Data); err != nil {
			return err
		}
		if err := writeSettings(j.SettingsPath, data, 0600); err != nil {
			return err
		}
		readback, err := readSettings(j.SettingsPath)
		if err != nil {
			return err
		}
		if !matchesManaged(readback.Data, j.Goal) || !equalJSON(readback.Data, data) {
			return fmt.Errorf("中断恢复后的 settings 回读校验失败")
		}
		j.Applied = readback
		j.Status = "committed"
		j.Prior, j.Goal = fieldSet{}, fieldSet{}
		return writeJournal(s, j)
	}
	if j.Action == "restore" {
		if err := patchSettings(data, j.Managed, true, j.Original.Data); err != nil {
			return err
		}
		return finishRestore(s, j, current)
	}
	return fmt.Errorf("未知的中断操作类型")
}

func finishRestore(s *store.Store, j *journal, current *snapshot) error {
	data := cloneMap(current.Data)
	if !matchesManaged(data, j.Goal) {
		if !matchesManaged(data, j.Prior) {
			return fmt.Errorf("恢复中断后受管字段冲突，拒绝继续")
		}
		if err := patchSettings(data, j.Managed, true, j.Original.Data); err != nil {
			return err
		}
	}
	if err := resolveSettingsSnapshot(j.SettingsPath, data, j.Original, "restore"); err != nil {
		return fmt.Errorf("恢复 Claude Code settings 失败：%w", err)
	}
	readback, err := readSettings(j.SettingsPath)
	if err != nil {
		return err
	}
	if !matchesManaged(readback.Data, j.Goal) || !equalJSON(readback.Data, data) {
		return fmt.Errorf("恢复 Claude Code settings 回读校验失败")
	}
	return deleteJournal(s)
}

func cloneSnapshot(in *snapshot) *snapshot {
	if in == nil {
		return nil
	}
	return &snapshot{Exists: in.Exists, Mode: in.Mode, Data: cloneMap(in.Data)}
}

func cloneMap(in map[string]any) map[string]any {
	if in == nil {
		return map[string]any{}
	}
	encoded, err := json.Marshal(in)
	if err != nil {
		return map[string]any{}
	}
	var out map[string]any
	if err := json.Unmarshal(encoded, &out); err != nil || out == nil {
		return map[string]any{}
	}
	return out
}

func cloneManagedFields(in managedFields) managedFields {
	encoded, _ := json.Marshal(in)
	var out managedFields
	_ = json.Unmarshal(encoded, &out)
	if out.Root == nil {
		out.Root = map[string]fieldChange{}
	}
	if out.Env == nil {
		out.Env = map[string]fieldChange{}
	}
	if out.Picker == nil {
		out.Picker = map[string]fieldChange{}
	}
	if out.Options == nil {
		out.Options = map[string]optionChange{}
	}
	return out
}

func cloneJournal(in *journal) *journal {
	encoded, _ := json.Marshal(in)
	var out journal
	_ = json.Unmarshal(encoded, &out)
	return &out
}
