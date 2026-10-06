package store

import (
	"testing"
	"time"

	"github.com/shichao-wang/cpagw/internal/config"
)

func TestSaveOAuthCASDoesNotChangeRevision(t *testing.T) {
	s := testStore(t)
	cred := config.OAuthCredential{AccessToken: "access-1", RefreshToken: "refresh-1", AccountID: "account", ExpiresAt: time.Now().Add(time.Hour), Generation: 1}
	if err := s.Update(func(st *config.State) error {
		st.OAuthCredentials["oauth-ref"] = cred
		st.Providers["p"] = config.Provider{Name: "p", Connections: map[string]config.Connection{"c": {ID: "connection-id", Name: "c", AuthType: config.AuthCodexOAuth, Protocol: config.Responses, BaseURL: config.CodexBaseURL, CredentialRef: "oauth-ref", Models: []config.Model{{ID: "m"}}}}}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	before, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	binding := OAuthBinding{Provider: "p", Connection: "c", ConnectionID: "connection-id", CredentialRef: "oauth-ref", Generation: 1}
	cred.AccessToken = "access-2"
	cred.RefreshToken = "refresh-2"
	generation, err := s.SaveOAuth(binding, cred)
	if err != nil {
		t.Fatal(err)
	}
	if generation != 2 {
		t.Fatalf("generation=%d", generation)
	}
	after, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != before.Revision {
		t.Fatalf("token 保存不应变更结构 revision: %d→%d", before.Revision, after.Revision)
	}
	if after.OAuthCredentials["oauth-ref"].AccessToken != "access-2" || after.OAuthCredentials["oauth-ref"].Generation != 2 {
		t.Fatal("token CAS 未写入")
	}
	currentBinding := binding
	currentBinding.Generation = 2
	wrongAccount := cred
	wrongAccount.AccountID = "other-account"
	if _, err := s.SaveOAuth(currentBinding, wrongAccount); err == nil {
		t.Fatal("刷新不得切换 OAuth 账号身份")
	}
	updated := cred
	updated.IDToken = "id-token-2"
	updated.ExpiresAt = updated.ExpiresAt.Add(time.Hour)
	updated.LastRefresh = time.Now()
	generation, err = s.SaveOAuth(currentBinding, updated)
	if err != nil || generation != 3 {
		t.Fatalf("同 token 元数据更新应持久化并递增 generation：%d %v", generation, err)
	}
	stateWithMetadata, err := s.Read()
	if err != nil {
		t.Fatal(err)
	}
	stored := stateWithMetadata.OAuthCredentials["oauth-ref"]
	if stored.Generation != 3 || stored.IDToken != updated.IDToken || !stored.ExpiresAt.Equal(updated.ExpiresAt) || !stored.LastRefresh.Equal(updated.LastRefresh) {
		t.Fatalf("OAuth 元数据未按预期保存：%+v", stored)
	}
	if stateWithMetadata.Revision != before.Revision {
		t.Fatal("token 元数据保存不应修改结构 revision")
	}
	currentBinding.Generation = 3
	unchangedGeneration, err := s.SaveOAuth(currentBinding, stored)
	if err != nil || unchangedGeneration != 3 {
		t.Fatalf("完全相同的凭证应 no-op：%d %v", unchangedGeneration, err)
	}
	if _, err := s.SaveOAuth(binding, cred); err == nil {
		t.Fatal("陈旧 generation 应拒绝")
	}
}

func TestRunLockIsNonblockingAndShared(t *testing.T) {
	s := testStore(t)
	lock, err := s.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AcquireRunLock(); err == nil {
		t.Fatal("同一 runtime 锁应拒绝并发获取")
	}
	if err := lock.Close(); err != nil {
		t.Fatal(err)
	}
	second, err := s.AcquireRunLock()
	if err != nil {
		t.Fatal(err)
	}
	second.Close()
}
