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
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
	"golang.org/x/sys/unix"
	"gopkg.in/yaml.v3"
)

const (
	RuntimeFileName = "runtime.json"
	RunLockFileName = ".gateway.lock"
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
}

type profileSnapshot struct {
	name    string
	id      string
	models  map[string]modelRoute
	catalog []modelRoute
}

type snapshot struct {
	revision uint64
	profiles map[string]profileSnapshot
	keyIndex map[[32]byte]string
	sdkKey   string
	config   *sdkconfig.Config
}

// Run 在当前进程以前台方式运行代理，并在状态修订变更时重建 SDK 服务。
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

	lock, err := acquireRunLock(st.Path(RunLockFileName))
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

	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := persistSDKConfig(st, initial.config); err != nil {
			return err
		}
		if err := runRevision(ctx, st, listen, probeToken, &active, initial, &runtimeState, &runtimeMu); err != nil {
			if errors.Is(err, context.Canceled) && ctx.Err() != nil {
				return ctx.Err()
			}
			return err
		}

		if err := ctx.Err(); err != nil {
			return err
		}
		runtimeState.Ready = false
		runtimeState.ActiveRevision = 0
		runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeRuntime(st, runtimeState); err != nil {
			return err
		}
		nextState, err := st.Read()
		if err != nil {
			return err
		}
		if nextState.Revision == initial.revision {
			// SDK 服务异常退出但配置未变化，不自动无限重启。
			return fmt.Errorf("代理服务意外停止，配置修订仍为 %d", initial.revision)
		}
		listen, err = normalizeListen(nextState.Listen)
		if err != nil {
			runtimeState.Error = err.Error()
			_ = writeRuntime(st, runtimeState)
			return err
		}
		runtimeState.Listen = listen
		next, err := compile(nextState, st)
		if err != nil {
			runtimeState.Ready = false
			runtimeState.Error = err.Error()
			runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			_ = writeRuntime(st, runtimeState)
			return err
		}
		runtimeState.Ready = false
		runtimeState.Error = ""
		runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		if err := writeRuntime(st, runtimeState); err != nil {
			return err
		}
		initial = next
	}
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

func runRevision(ctx context.Context, st *store.Store, listen, token string, active *atomic.Pointer[snapshot], cfg *snapshot, runtimeState *RuntimeState, runtimeMu *sync.Mutex) error {
	host, portText, err := net.SplitHostPort(listen)
	if err != nil {
		return err
	}
	port, err := strconv.Atoi(portText)
	if err != nil {
		return fmt.Errorf("监听端口无效")
	}
	gate := &requestGate{store: st, active: active, token: token, instanceID: runtimeState.InstanceID}
	var startupErr error
	runCtx, cancel := context.WithCancel(ctx)
	serviceConfig := cfg.config
	serviceConfig.Host = host
	serviceConfig.Port = port

	service, err := cliproxy.NewBuilder().
		WithConfig(serviceConfig).
		WithConfigPath(st.Path("sdk-runtime.yaml")).
		WithWatcherFactory(func(string, string, func(*sdkconfig.Config)) (*cliproxy.WatcherWrapper, error) {
			// 网关只以状态文件为事实源；SDK 的文件 watcher 不允许反向改变运行配置。
			return &cliproxy.WatcherWrapper{}, nil
		}).
		WithHooks(cliproxy.Hooks{OnAfterStart: func(*cliproxy.Service) {
			active.Store(cfg)
			if err := probeStartup(runCtx, listen, token, runtimeState.InstanceID, cfg.revision); err != nil {
				active.Store(nil)
				startupErr = err
				cancel()
				return
			}
			gate.ready.Store(true)
			runtimeMu.Lock()
			runtimeState.Ready = true
			runtimeState.ActiveRevision = cfg.revision
			runtimeState.Error = ""
			runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
			if err := writeRuntime(st, *runtimeState); err != nil {
				gate.ready.Store(false)
				active.Store(nil)
				runtimeState.Ready = false
				startupErr = err
				runtimeMu.Unlock()
				cancel()
				return
			}
			runtimeMu.Unlock()
			gate.ready.Store(true)
		}}).
		WithServerOptions(
			sdkapi.WithMiddleware(gate.middleware()),
			sdkapi.WithRouterConfigurator(func(engine *gin.Engine, _ *sdkhandlers.BaseAPIHandler, _ *sdkconfig.Config) {
				engine.GET(ReadyPath, func(c *gin.Context) {
					current := active.Load()
					if current == nil {
						c.AbortWithStatusJSON(http.StatusServiceUnavailable, gin.H{"ready": false})
						return
					}
					c.JSON(http.StatusOK, gin.H{
						"ready": gate.ready.Load(), "instanceID": runtimeState.InstanceID,
						"revision": current.revision, "activeRevision": current.revision,
					})
				})
			}),
		).Build()
	if err != nil {
		cancel()
		return fmt.Errorf("初始化上游 SDK 失败：%w", err)
	}

	serviceDone := make(chan error, 1)
	go func() { serviceDone <- service.Run(runCtx) }()
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	defer cancel()
	for {
		select {
		case err := <-serviceDone:
			gate.ready.Store(false)
			active.Store(nil)
			if startupErr != nil {
				return startupErr
			}
			return err
		case <-ctx.Done():
			gate.ready.Store(false)
			active.Store(nil)
			cancel()
			select {
			case <-serviceDone:
			case <-time.After(35 * time.Second):
				return fmt.Errorf("等待代理服务关闭超时")
			}
			return ctx.Err()
		case <-ticker.C:
			latest, err := st.Read()
			if err != nil {
				gate.ready.Store(false)
				active.Store(nil)
				cancel()
				<-serviceDone
				return err
			}
			if latest.Revision != cfg.revision {
				gate.ready.Store(false)
				active.Store(nil)
				runtimeMu.Lock()
				runtimeState.Ready = false
				runtimeState.ActiveRevision = 0
				runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
				err := writeRuntime(st, *runtimeState)
				runtimeMu.Unlock()
				if err != nil {
					cancel()
					<-serviceDone
					return err
				}
				cancel()
				<-serviceDone
				return nil
			}
		}
	}
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
	for name, profile := range state.Profiles {
		if strings.TrimSpace(profile.ID) == "" {
			return nil, fmt.Errorf("profile %s 缺少 ID", name)
		}
		if _, exists := usedIDs[profile.ID]; exists {
			return nil, fmt.Errorf("profile ID 重复：%s", profile.ID)
		}
		usedIDs[profile.ID] = struct{}{}
		if err := state.ValidateProfile(profile); err != nil {
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
			connection := state.Connections[binding.Connection]
			connectionKey := binding.Connection
			info, exists := connections[connectionKey]
			if !exists {
				prefix := uniquePrefix(binding.Connection)
				info = connectionInfo{name: binding.Connection, prefix: prefix, protocol: connection.Protocol, baseURL: connection.BaseURL}
				info.apiKey, err = state.Key(connection)
				if err != nil {
					return nil, err
				}
				connections[connectionKey] = info
				if err := appendSDKConnection(cfg, info, connection.Models); err != nil {
					return nil, err
				}
			}
			route := modelRoute{
				publicID: binding.PublicModel, targetModel: binding.TargetModel,
				sdkModel: info.prefix + "/" + binding.TargetModel, prefix: info.prefix,
				label: binding.Label, description: binding.Description,
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
	name     string
	prefix   string
	protocol string
	baseURL  string
	apiKey   string
}

func appendSDKConnection(cfg *sdkconfig.Config, info connectionInfo, models []config.Model) error {
	if strings.TrimSpace(info.baseURL) == "" {
		return fmt.Errorf("连接 %s 缺少 API 根地址", info.name)
	}
	if err := config.ValidateURL(info.baseURL); err != nil {
		return fmt.Errorf("连接 %s 地址无效：%w", info.name, err)
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
		return fmt.Errorf("连接 %s 的上游协议不受支持：%s", info.name, info.protocol)
	}
	return nil
}

func uniquePrefix(connection string) string {
	sum := sha256.Sum256([]byte(connection))
	return "cpagw-" + hex.EncodeToString(sum[:8])
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

func acquireRunLock(path string) (*os.File, error) {
	if err := store.CheckFile(path); err != nil {
		return nil, err
	}
	fd, err := unix.Open(path, unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return nil, fmt.Errorf("打开网关进程锁失败：%w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, syscall.EWOULDBLOCK) || errors.Is(err, syscall.EAGAIN) {
			return nil, fmt.Errorf("网关实例已在运行")
		}
		return nil, fmt.Errorf("获取网关进程锁失败：%w", err)
	}
	return file, nil
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
