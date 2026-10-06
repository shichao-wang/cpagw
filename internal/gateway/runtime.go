package gateway

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/http"
	"reflect"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gin-gonic/gin"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	sdkhandlers "github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	cliproxy "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/shichao-wang/cpagw/internal/codexoauth"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

type runtimeOptions struct {
	transport coreauth.RoundTripperProvider
	onManager func(*coreauth.Manager)
}

func runManagedService(ctx context.Context, st *store.Store, listen, token string, active *atomic.Pointer[snapshot], initial *snapshot, runtimeState *RuntimeState, runtimeMu *sync.Mutex, options runtimeOptions) error {
	namespace, err := randomHex(16)
	if err != nil {
		return fmt.Errorf("生成认证实例标识失败")
	}
	credentials := codexoauth.NewSDKStore(st, namespace)
	manager := coreauth.NewManager(credentials, nil, credentials.Hook())
	credentials.BindManager(manager)
	gate := &requestGate{store: st, active: active, token: token, instanceID: runtimeState.InstanceID, oauth: credentials}
	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	defer func() {
		gate.ready.Store(false)
		active.Store(nil)
		for _, auth := range manager.List() {
			cliproxy.GlobalModelRegistry().UnregisterClient(auth.ID)
		}
	}()
	if err := persistSDKConfig(st, initial.config); err != nil {
		return err
	}
	callbacks := make(chan func(*sdkconfig.Config), 1)
	service, err := cliproxy.NewBuilder().WithConfig(initial.config).
		WithConfigPath(st.Path("sdk-runtime.yaml")).
		WithAuthManager(sdkauth.NewManager(credentials)).WithCoreAuthManager(manager).
		WithWatcherFactory(func(_ string, _ string, reload func(*sdkconfig.Config)) (*cliproxy.WatcherWrapper, error) {
			// 在 SDK 启动 goroutine 内规范化初始 Auth，避免首次模型注册捕获已被删除的静态记录。
			if err := reconcileStaticAuths(runCtx, manager, initial, namespace); err != nil {
				return nil, err
			}
			reload(initial.config)
			// 状态 watcher 由控制器管理；不启用 SDK 的外部配置与认证文件 watcher。
			callbacks <- reload
			return &cliproxy.WatcherWrapper{}, nil
		}).
		WithServerOptions(sdkapi.WithMiddleware(gate.middleware()), sdkapi.WithRouterConfigurator(func(engine *gin.Engine, _ *sdkhandlers.BaseAPIHandler, _ *sdkconfig.Config) {
			engine.GET(ReadyPath, func(c *gin.Context) {
				current := active.Load()
				if current == nil {
					c.AbortWithStatus(http.StatusServiceUnavailable)
					return
				}
				c.JSON(http.StatusOK, gin.H{"ready": gate.ready.Load(), "instanceID": runtimeState.InstanceID, "revision": current.revision, "activeRevision": current.revision})
			})
		})).Build()
	if err != nil {
		return fmt.Errorf("初始化上游 SDK 失败")
	}
	// Builder 会安装默认 transport，测试注入必须在 Build 后且 Run 前完成。
	if options.transport != nil {
		manager.SetRoundTripperProvider(options.transport)
	}
	if options.onManager != nil {
		options.onManager(manager)
	}
	done := make(chan error, 1)
	go func() { done <- service.Run(runCtx) }()
	defer func() {
		cancel()
		select {
		case <-done:
		case <-time.After(35 * time.Second):
		}
	}()
	var reload func(*sdkconfig.Config)
	select {
	case reload = <-callbacks:
	case err := <-done:
		done <- err
		return fmt.Errorf("启动上游 SDK 失败")
	case <-ctx.Done():
		return ctx.Err()
	}
	apply := func(next *snapshot, update bool) error {
		gate.ready.Store(false)
		if update {
			if err := persistSDKConfig(st, next.config); err != nil {
				return err
			}
			if err := reconcileStaticAuths(runCtx, manager, next, namespace); err != nil {
				return err
			}
			reload(next.config)
		}
		if err := verifySnapshot(manager, credentials, next, namespace); err != nil {
			return err
		}
		active.Store(next)
		if err := probeStartup(runCtx, listen, token, runtimeState.InstanceID, next.revision); err != nil {
			active.Store(nil)
			return err
		}
		runtimeMu.Lock()
		runtimeState.Ready = true
		runtimeState.ActiveRevision = next.revision
		runtimeState.Error = ""
		runtimeState.UpdatedAt = time.Now().UTC().Format(time.RFC3339Nano)
		err := writeRuntime(st, *runtimeState)
		runtimeMu.Unlock()
		if err != nil {
			active.Store(nil)
			return err
		}
		gate.ready.Store(true)
		return nil
	}
	if err := apply(initial, false); err != nil {
		return err
	}
	current := initial
	ticker := time.NewTicker(250 * time.Millisecond)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case err := <-done:
			done <- err
			return err
		case <-ticker.C:
			state, err := st.Read()
			if err != nil {
				return err
			}
			if state.Revision == current.revision {
				continue
			}
			gate.ready.Store(false)
			newListen, err := normalizeListen(state.Listen)
			if err != nil {
				return err
			}
			if newListen != listen {
				return fmt.Errorf("监听地址已改变，请重启网关")
			}
			next, err := compile(state, st)
			if err != nil {
				return err
			}
			if !sameOAuthTopology(current, next) {
				return fmt.Errorf("OAuth 认证配置已改变，请停止并重启网关")
			}
			next.sdkKey = current.sdkKey
			next.config.SDKConfig.APIKeys = []string{current.sdkKey}
			if err := apply(next, true); err != nil {
				return err
			}
			current = next
		}
	}
}

func staticAuthID(namespace, connectionID string) string {
	sum := sha256.Sum256([]byte(namespace + "\x00" + connectionID))
	return "cpagw-key-" + hex.EncodeToString(sum[:16])
}

func staticAuths(next *snapshot, namespace string) map[string]*coreauth.Auth {
	out := make(map[string]*coreauth.Auth)
	indexes := map[string]int{}
	for _, connection := range next.connections {
		if connection.authType != config.AuthAPIKey {
			continue
		}
		provider := ""
		attrs := map[string]string{
			"api_key": connection.apiKey, "base_url": connection.baseURL,
			"config_index":                  strconv.Itoa(indexes[connection.protocol]),
			coreauth.AttributeAuthKind:      coreauth.AuthKindAPIKey,
			coreauth.AttributeSourceBackend: coreauth.AuthSourceConfig,
		}
		indexes[connection.protocol]++
		switch connection.protocol {
		case config.Chat:
			provider = "openai-compatible-" + connection.prefix
			attrs["compat_name"] = connection.prefix
			attrs["provider_key"] = provider
		case config.Anthropic:
			provider = "claude"
		case config.Responses:
			provider = "codex"
			attrs[coreauth.AttributeCodexDisableCloaking] = "true"
		}
		id := staticAuthID(namespace, connection.id)
		out[id] = &coreauth.Auth{ID: id, Provider: provider, Prefix: connection.prefix, Status: coreauth.StatusActive, Attributes: attrs}
	}
	return out
}

func reconcileStaticAuths(ctx context.Context, manager *coreauth.Manager, next *snapshot, namespace string) error {
	wanted := staticAuths(next, namespace)
	for _, auth := range manager.List() {
		if !coreauth.IsConfigAPIKeyAuth(auth) {
			continue
		}
		if _, exists := wanted[auth.ID]; !exists {
			manager.Remove(ctx, auth.ID)
			cliproxy.GlobalModelRegistry().UnregisterClient(auth.ID)
		}
	}
	for id, auth := range wanted {
		old, exists := manager.GetByID(id)
		var err error
		if !exists {
			_, err = manager.Register(coreauth.WithSkipPersist(ctx), auth)
		} else if old.Provider != auth.Provider || old.Prefix != auth.Prefix || !reflect.DeepEqual(old.Attributes, auth.Attributes) {
			_, err = manager.Update(coreauth.WithSkipPersist(ctx), auth)
		}
		if err != nil {
			return fmt.Errorf("同步连接认证失败")
		}
	}
	return nil
}

func verifySnapshot(manager *coreauth.Manager, credentials *codexoauth.SDKStore, next *snapshot, namespace string) error {
	for id, expected := range staticAuths(next, namespace) {
		actual, ok := manager.GetByID(id)
		if !ok || actual.Provider != expected.Provider || actual.Prefix != expected.Prefix || !reflect.DeepEqual(actual.Attributes, expected.Attributes) {
			return fmt.Errorf("连接认证配置校验失败")
		}
	}
	byPrefix := map[string]connectionInfo{}
	for _, connection := range next.connections {
		byPrefix[connection.prefix] = connection
	}
	registry := cliproxy.GlobalModelRegistry()
	for name, profile := range next.profiles {
		for publicID, route := range profile.models {
			connection := byPrefix[route.prefix]
			if connection.authType == config.AuthAPIKey {
				route.authID = staticAuthID(namespace, connection.id)
				if !registry.ClientSupportsModel(route.authID, route.sdkModel) {
					return fmt.Errorf("连接模型注册校验失败：%s/%s", connection.provider, connection.name)
				}
			} else {
				route.authID = credentials.AuthID(connection.credentialRef)
				if connection.credentialRef == "" {
					route.unavailable = "该 OAuth 连接尚未登录"
				} else if !credentials.Healthy(route.authID) {
					route.unavailable = "该 OAuth 凭证不可用，请停止网关后重新登录"
				} else if !registry.ClientSupportsModel(route.authID, route.sdkModel) {
					route.unavailable = "该 OAuth 账号未注册所选模型"
				}
			}
			profile.models[publicID] = route
		}
		for i, route := range profile.catalog {
			profile.catalog[i] = profile.models[route.publicID]
		}
		next.profiles[name] = profile
	}
	return nil
}

func sameOAuthTopology(a, b *snapshot) bool {
	topology := func(s *snapshot) map[string]string {
		out := map[string]string{}
		for _, c := range s.connections {
			if c.authType == config.AuthCodexOAuth {
				out[c.id] = c.provider + "\x00" + c.name + "\x00" + c.baseURL + "\x00" + c.credentialRef
			}
		}
		return out
	}
	return reflect.DeepEqual(topology(a), topology(b))
}
