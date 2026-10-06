package codexoauth

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	sdkAuth "github.com/router-for-me/CLIProxyAPI/v8/sdk/auth"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
)

type fakeTokenStorage struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token"`
	IDToken      string `json:"id_token"`
	AccountID    string `json:"account_id"`
	PlanType     string `json:"plan_type"`
	Expired      string `json:"expired"`
	LastRefresh  string `json:"last_refresh"`
	Unexpected   string `json:"unexpected_secret"`
}

func (fakeTokenStorage) SaveTokenToFile(string) error {
	return errors.New("must not persist through SDK file storage")
}

func TestLoginUsesDeviceModeAndExtractsStorage(t *testing.T) {
	expires := time.Now().UTC().Add(time.Hour).Truncate(time.Second)
	refresh := time.Now().UTC().Add(-time.Minute).Truncate(time.Second)
	var sawNoBrowser bool
	flow := NewLoginWith(func(_ context.Context, _ *sdkconfig.Config, opts *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
		sawNoBrowser = opts.NoBrowser
		if opts.Metadata["codex_login_mode"] != "device" {
			t.Fatalf("codex_login_mode = %q", opts.Metadata["codex_login_mode"])
		}
		return &coreauth.Auth{
			Metadata: map[string]any{"access_token": "must-not-read-metadata"},
			Storage: fakeTokenStorage{
				AccessToken: "access", RefreshToken: "refresh", IDToken: "id",
				AccountID: "account", PlanType: "plus", Expired: expires.Format(time.RFC3339Nano),
				LastRefresh: refresh.Format(time.RFC3339Nano), Unexpected: "not-copied",
			},
		}, nil
	})
	credential, err := flow.Login(context.Background(), true)
	if err != nil {
		t.Fatal("Login returned an error")
	}
	if !sawNoBrowser {
		t.Fatal("NoBrowser was not forwarded")
	}
	if credential.AccessToken != "access" || credential.RefreshToken != "refresh" || credential.IDToken != "id" || credential.AccountID != "account" || credential.PlanType != "plus" {
		t.Fatalf("credential fields were not converted from storage: %#v", credential)
	}
	if !credential.ExpiresAt.Equal(expires) || !credential.LastRefresh.Equal(refresh) {
		t.Fatalf("credential timestamps mismatch: %#v", credential)
	}
	if strings.Contains(credential.AccessToken, "metadata") {
		t.Fatal("credential was read from Metadata instead of Storage")
	}
}

func TestLoginRedactsSDKError(t *testing.T) {
	const secret = "test-secret-token-value"
	flow := NewLoginWith(func(context.Context, *sdkconfig.Config, *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
		return nil, errors.New("provider returned " + secret)
	})
	_, err := flow.Login(context.Background(), true)
	if err == nil || strings.Contains(err.Error(), secret) {
		t.Fatalf("SDK error was not redacted: %v", err)
	}
}

func TestLoginRequiresCompleteStorage(t *testing.T) {
	flow := NewLoginWith(func(context.Context, *sdkconfig.Config, *sdkAuth.LoginOptions) (*coreauth.Auth, error) {
		return &coreauth.Auth{Storage: fakeTokenStorage{AccessToken: "partial"}}, nil
	})
	if _, err := flow.Login(context.Background(), true); err == nil {
		t.Fatal("expected incomplete token storage to fail")
	}
}
