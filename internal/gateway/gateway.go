package gateway

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
	"gopkg.in/yaml.v3"
)

const (
	RuntimeFileName = "runtime.json"
	RunLockFileName = store.RunLockFileName
	ReadyPath       = "/__cpagw/ready"
	probeHeader     = "X-CPAGW-Instance-Token"
)

// RuntimeState 是网关进程私有的运行状态。文件权限必须为 0600；其中的探测令牌不得展示给用户。
type RuntimeState struct {
	InstanceID     string `json:"instanceID"`
	PID            int    `json:"pid"`
	StartTime      string `json:"startTime"`
	Listen         string `json:"listen"`
	ActiveRevision uint64 `json:"activeRevision"`
	Ready          bool   `json:"ready"`
	ProbeToken     string `json:"probeToken"`
	UpdatedAt      string `json:"updatedAt"`
	Error          string `json:"error,omitempty"`
}

type modelRoute struct {
	publicID    string
	targetModel string
	sdkModel    string
	prefix      string
	label       string
	description string
	authID      string
	authType    string
	unavailable string
}

type profileSnapshot struct {
	name    string
	id      string
	models  map[string]modelRoute
	catalog []modelRoute
}

type snapshot struct {
	revision    uint64
	profiles    map[string]profileSnapshot
	keyIndex    map[[32]byte]string
	sdkKey      string
	config      *sdkconfig.Config
	connections []connectionInfo
}

// Run 在当前进程运行单个 SDK 服务，状态修订变更通过受管热更新生效。
func Run(ctx context.Context, st *store.Store, instanceID string) error {
	if st == nil {
		return fmt.Errorf("状态存储不能为空")
	}
	if strings.TrimSpace(instanceID) == "" {
		return fmt.Errorf("网关实例 ID 不能为空")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	ctx, stopSignals := signalContext(ctx)
	defer stopSignals()

	lock, err := st.AcquireRunLock()
	if err != nil {
		return err
	}
	defer lock.Close()

	current, err := st.Read()
	if err != nil {
		return err
	}
	listen, err := normalizeListen(current.Listen)
	if err != nil {
		return err
	}
	probeToken, err := randomHex(32)
	if err != nil {
		return fmt.Errorf("生成实例探测令牌失败：%w", err)
	}
	pidStart, err := processStartTime(os.Getpid())
	if err != nil {
		return fmt.Errorf("读取网关进程启动时间失败：%w", err)
	}

	var active atomic.Pointer[snapshot]
	var runtimeMu sync.Mutex
	runtimeState := RuntimeState{
		InstanceID: instanceID,
		PID:        os.Getpid(),
		StartTime:  pidStart,
		Listen:     listen,
		ProbeToken: probeToken,
		UpdatedAt:  time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeRuntime(st, runtimeState); err != nil {
		return err
	}
	defer func() {
		runtimeState.Ready = false
		runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		_ = writeRuntime(st, runtimeState)
		_ = os.Remove(st.Path("sdk-runtime.yaml"))
	}()

	initial, err := compile(current, st)
	if err != nil {
		runtimeState.Error = err.Error()
		_ = writeRuntime(st, runtimeState)
		return err
	}

	err = runManagedService(ctx, st, listen, probeToken, &active, initial, &runtimeState, &runtimeMu, runtimeOptions{})
	if err != nil && !errors.Is(err, context.Canceled) {
		runtimeState.Error = err.Error()
	}
	return err
}

func signalContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return signal.NotifyContext(ctx, os.Interrupt, syscall.SIGTERM)
}

func normalizeListen(raw string) (string, error) {
	host, portText, err := net.SplitHostPort(raw)
	if err != nil {
		return "", fmt.Errorf("监听地址无效：%w", err)
	}
	ip := net.ParseIP(host)
	if !strings.EqualFold(host, "localhost") && (ip == nil || !ip.IsLoopback()) {
		return "", fmt.Errorf("网关只允许监听 loopback 地址")
	}
	port, err := strconv.Atoi(portText)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("监听端口必须在 1–65535 之间")
	}
	return net.JoinHostPort(host, portText), nil
}

func probeStartup(ctx context.Context, listen, token, instanceID string, revision uint64) error {
	address := "http://" + listen + ReadyPath
	client := &http.Client{Timeout: 500 * time.Millisecond, Transport: &http.Transport{Proxy: nil}}
	defer client.CloseIdleConnections()
	deadline := time.NewTimer(5 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	var lastErr error
	for {
		request, err := http.NewRequestWithContext(ctx, http.MethodGet, address, nil)
		if err == nil {
			request.Header.Set(probeHeader, token)
			response, err := client.Do(request)
			if err == nil {
				var payload struct {
					Ready          bool   `json:"ready"`
					InstanceID     string `json:"instanceID"`
					ActiveRevision uint64 `json:"activeRevision"`
				}
				decodeErr := json.NewDecoder(response.Body).Decode(&payload)
				_ = response.Body.Close()
				if response.StatusCode == http.StatusOK && decodeErr == nil && payload.InstanceID == instanceID && payload.ActiveRevision == revision {
					return nil
				}
				lastErr = fmt.Errorf("实例就绪探测返回无效响应")
			} else {
				lastErr = err
			}
		} else {
			lastErr = err
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline.C:
			return fmt.Errorf("等待实例就绪超时：%w", lastErr)
		case <-ticker.C:
		}
	}
}

func compile(state *config.State, st *store.Store) (*snapshot, error) {
	if state == nil {
		return nil, fmt.Errorf("状态为空")
	}
	listen, err := normalizeListen(state.Listen)
	if err != nil {
		return nil, err
	}
	host, portText, _ := net.SplitHostPort(listen)
	port, _ := strconv.Atoi(portText)
	sdkKey, err := randomHex(32)
	if err != nil {
		return nil, fmt.Errorf("生成 SDK 内部认证密钥失败：%w", err)
	}
	out := &snapshot{
		revision: state.Revision,
		profiles: make(map[string]profileSnapshot, len(state.Profiles)),
		keyIndex: make(map[[32]byte]string, len(state.Profiles)),
		sdkKey:   sdkKey,
	}
	cfg := &sdkconfig.Config{}
	cfg.SDKConfig.APIKeys = []string{sdkKey}
	cfg.SDKConfig.ForceModelPrefix = true
	cfg.Host = host
	cfg.Port = port
	cfg.CommercialMode = true
	cfg.LoggingToFile = false
	cfg.DisableClaudeCloakMode = true
	cfg.RemoteManagement.DisableControlPanel = true
	cfg.AuthDir = filepath.Join(st.Dir, "sdk-auth")
	if err := store.EnsureDir(cfg.AuthDir); err != nil {
		return nil, fmt.Errorf("初始化私有 SDK 认证目录失败：%w", err)
	}

	usedIDs := map[string]struct{}{}
	usedKeys := map[[32]byte]string{}
	connections := map[string]connectionInfo{}
	var connectionKeys []string
	for pname, provider := range state.Providers {
		for cname, connection := range provider.Connections {
			info := connectionInfo{provider: pname, name: cname, id: connection.ID, authType: connection.AuthType,
				credentialRef: connection.CredentialRef, prefix: config.ConnectionPrefix(pname, cname),
				protocol: connection.Protocol, baseURL: connection.BaseURL, models: connection.Models}
			switch connection.AuthType {
			case config.AuthAPIKey:
				info.apiKey, err = state.Key(pname, connection)
				if err != nil {
					// 未被 profile 使用的待配置连接不进入 SDK，也不影响其他连接。
					if len(state.References(pname, cname, "")) == 0 {
						continue
					}
					return nil, err
				}
			case config.AuthCodexOAuth:
			default:
				return nil, fmt.Errorf("连接必须显式声明有效的认证方式")
			}
			key := pname + "\x00" + cname
			connections[key] = info
			connectionKeys = append(connectionKeys, key)
		}
	}
	sort.Strings(connectionKeys)
	for _, key := range connectionKeys {
		info := connections[key]
		out.connections = append(out.connections, info)
		if info.authType == config.AuthAPIKey {
			if err := appendSDKConnection(cfg, info, info.models); err != nil {
				return nil, err
			}
		}
	}
	for name, profile := range state.Profiles {
		if strings.TrimSpace(profile.ID) == "" {
			return nil, fmt.Errorf("profile %s 缺少 ID", name)
		}
		if _, exists := usedIDs[profile.ID]; exists {
			return nil, fmt.Errorf("profile ID 重复：%s", profile.ID)
		}
		usedIDs[profile.ID] = struct{}{}
		if err := state.ValidateProfileStructure(profile); err != nil {
			return nil, fmt.Errorf("profile %s 无效：%w", name, err)
		}
		if strings.TrimSpace(profile.KeyRef) == "" || strings.TrimSpace(state.Secrets[profile.KeyRef]) == "" {
			return nil, fmt.Errorf("profile %s 缺少下游 API key", name)
		}
		keyHash := sha256.Sum256([]byte(state.Secrets[profile.KeyRef]))
		if previous, exists := usedKeys[keyHash]; exists {
			return nil, fmt.Errorf("profile %s 与 %s 共用同一 API key", name, previous)
		}
		usedKeys[keyHash] = name
		out.keyIndex[keyHash] = name
		ps := profileSnapshot{name: name, id: profile.ID, models: make(map[string]modelRoute, len(profile.Models))}
		for _, slot := range config.Slots {
			binding := profile.Models[slot]
			info := connections[binding.Provider+"\x00"+binding.Connection]
			route := modelRoute{
				publicID: binding.PublicModel, targetModel: binding.TargetModel,
				sdkModel: info.prefix + "/" + binding.TargetModel, prefix: info.prefix,
				label: binding.Label, description: binding.Description, authType: info.authType,
			}
			ps.models[route.publicID] = route
			ps.catalog = append(ps.catalog, route)
		}
		out.profiles[name] = ps
	}
	out.config = cfg
	return out, nil
}

type connectionInfo struct {
	provider      string
	name          string
	prefix        string
	protocol      string
	baseURL       string
	apiKey        string
	id            string
	authType      string
	credentialRef string
	models        []config.Model
}

func appendSDKConnection(cfg *sdkconfig.Config, info connectionInfo, models []config.Model) error {
	if strings.TrimSpace(info.baseURL) == "" {
		return fmt.Errorf("连接 %s/%s 缺少 API 根地址", info.provider, info.name)
	}
	if err := config.ValidateURL(info.baseURL); err != nil {
		return fmt.Errorf("连接 %s/%s 地址无效：%w", info.provider, info.name, err)
	}
	switch info.protocol {
	case config.Anthropic:
		entry := sdkconfig.ClaudeKey{APIKey: info.apiKey, BaseURL: info.baseURL, Prefix: info.prefix}
		if err := decodeModels(&entry, models); err != nil {
			return err
		}
		cfg.ClaudeKey = append(cfg.ClaudeKey, entry)
	case config.Responses:
		disabled := true
		entry := sdkconfig.CodexKey{APIKey: info.apiKey, BaseURL: info.baseURL, Prefix: info.prefix, Websockets: false, DisableCodexCloaking: &disabled}
		if err := decodeModels(&entry, models); err != nil {
			return err
		}
		cfg.CodexKey = append(cfg.CodexKey, entry)
	case config.Chat:
		entry := sdkconfig.OpenAICompatibility{
			Name: info.prefix, BaseURL: info.baseURL, Prefix: info.prefix,
			APIKeyEntries: []sdkconfig.OpenAICompatibilityAPIKey{{APIKey: info.apiKey}},
		}
		for _, model := range models {
			entry.Models = append(entry.Models, sdkconfig.OpenAICompatibilityModel{Name: model.ID, Alias: model.ID, DisplayName: model.Name, ForceMapping: true})
		}
		cfg.OpenAICompatibility = append(cfg.OpenAICompatibility, entry)
	default:
		return fmt.Errorf("连接 %s/%s 的上游协议不受支持：%s", info.provider, info.name, info.protocol)
	}
	return nil
}

func persistSDKConfig(st *store.Store, cfg *sdkconfig.Config) error {
	data, err := yaml.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("生成私有 SDK 配置失败：%w", err)
	}
	if err := store.AtomicWrite(st.Path("sdk-runtime.yaml"), data); err != nil {
		return fmt.Errorf("保存私有 SDK 配置失败：%w", err)
	}
	return nil
}

func writeRuntime(st *store.Store, state RuntimeState) error {
	if st == nil {
		return fmt.Errorf("状态存储不能为空")
	}
	if err := store.WriteJSON(st.Path(RuntimeFileName), state); err != nil {
		return fmt.Errorf("写入网关运行状态失败：%w", err)
	}
	return nil
}

func randomHex(size int) (string, error) {
	data := make([]byte, size)
	if _, err := rand.Read(data); err != nil {
		return "", err
	}
	return hex.EncodeToString(data), nil
}

func secureEqual(a, b string) bool {
	return len(a) == len(b) && subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}

func decodeRuntime(st *store.Store) (RuntimeState, error) {
	var state RuntimeState
	path := st.Path(RuntimeFileName)
	if err := store.CheckFile(path); err != nil {
		return state, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}
	if err := json.Unmarshal(data, &state); err != nil {
		return RuntimeState{}, fmt.Errorf("网关运行状态损坏")
	}
	return state, nil
}
