package codexoauth

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/shichao-wang/cpagw/internal/config"
)

// LoginFunc 可注入，以便在不使用真实账号的情况下测试 OAuth 凭证转换。
type LoginFunc func(context.Context, *sdkconfig.Config, *auth.LoginOptions) (*coreauth.Auth, error)

// Login 运行 SDK 的 Codex 设备码 OAuth 流程，并转换内存中的令牌存储。
type Login struct {
	authenticate LoginFunc
}

// NewLogin 创建由 CLIProxyAPI 公开 Codex 认证器支持的登录流程。
func NewLogin() *Login {
	return NewLoginWith(func(ctx context.Context, cfg *sdkconfig.Config, opts *auth.LoginOptions) (*coreauth.Auth, error) {
		return auth.NewCodexAuthenticator().Login(ctx, cfg, opts)
	})
}

// NewLoginWith 创建使用可注入公开 SDK 认证器的登录流程。
func NewLoginWith(fn LoginFunc) *Login { return &Login{authenticate: fn} }

// Login 执行 OAuth 设备码登录。令牌从 Auth.Storage 提取，而不从 Metadata 提取，
// 并以 cpagw 自有的强类型凭证返回。
func (l *Login) Login(ctx context.Context, noBrowser bool) (config.OAuthCredential, error) {
	if l == nil || l.authenticate == nil {
		return config.OAuthCredential{}, errors.New("Codex OAuth 登录器未配置")
	}
	if ctx == nil {
		ctx = context.Background()
	}
	opts := &auth.LoginOptions{
		NoBrowser: noBrowser,
		Metadata:  map[string]string{"codex_login_mode": "device"},
	}
	record, err := l.authenticate(ctx, &sdkconfig.Config{}, opts)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(ctx.Err(), context.Canceled) {
			return config.OAuthCredential{}, errors.New("Codex OAuth 登录已取消")
		}
		if errors.Is(err, context.DeadlineExceeded) || errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return config.OAuthCredential{}, errors.New("Codex OAuth 登录已超时")
		}
		// SDK 错误可能包含服务端响应，因此不返回也不记录原始错误，避免泄漏令牌。
		return config.OAuthCredential{}, errors.New("Codex OAuth 登录失败；请检查网络后重试")
	}
	if record == nil || record.Storage == nil {
		return config.OAuthCredential{}, errors.New("Codex OAuth 登录未返回凭证")
	}
	credential, err := credentialFromStorage(record.Storage)
	if err != nil {
		return config.OAuthCredential{}, errors.New("Codex OAuth 登录返回的凭证格式无效")
	}
	return credential, nil
}

// credentialFromStorage 使用严格白名单。SDK 令牌存储只在内存中序列化；
// 不读取或写入其 FileName 和 Metadata。
func credentialFromStorage(storage any) (config.OAuthCredential, error) {
	var raw map[string]json.RawMessage
	data, err := json.Marshal(storage)
	if err != nil {
		return config.OAuthCredential{}, err
	}
	if err := json.Unmarshal(data, &raw); err != nil {
		return config.OAuthCredential{}, err
	}
	var result config.OAuthCredential
	readString := func(key string) string {
		var value string
		_ = json.Unmarshal(raw[key], &value)
		return strings.TrimSpace(value)
	}
	result.AccessToken = readString("access_token")
	result.RefreshToken = readString("refresh_token")
	result.IDToken = readString("id_token")
	result.AccountID = readString("account_id")
	result.PlanType = readString("plan_type")
	if result.AccessToken == "" || result.RefreshToken == "" || result.IDToken == "" || result.AccountID == "" {
		return config.OAuthCredential{}, fmt.Errorf("required Codex OAuth credential fields are missing")
	}
	if expiry := readString("expired"); expiry != "" {
		result.ExpiresAt, err = parseTimestamp(expiry)
		if err != nil {
			return config.OAuthCredential{}, fmt.Errorf("invalid expiry")
		}
	}
	if refreshed := readString("last_refresh"); refreshed != "" {
		result.LastRefresh, err = parseTimestamp(refreshed)
		if err != nil {
			return config.OAuthCredential{}, fmt.Errorf("invalid refresh time")
		}
	}
	if result.ExpiresAt.IsZero() {
		return config.OAuthCredential{}, fmt.Errorf("expiry is missing")
	}
	return result, nil
}

func parseTimestamp(raw string) (time.Time, error) {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339, "2006-01-02 15:04:05"} {
		if ts, err := time.Parse(layout, raw); err == nil {
			return ts.UTC(), nil
		}
	}
	if unix, err := time.Parse("2006-01-02T15:04:05Z07:00", raw); err == nil {
		return unix.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("invalid timestamp")
}
