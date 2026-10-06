package codexoauth

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy"
	coreauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	"github.com/shichao-wang/cpagw/internal/config"
	"github.com/shichao-wang/cpagw/internal/store"
)

func newTestSDKStore(t *testing.T) (*SDKStore, *store.Store, config.OAuthCredential) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "state")
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	credential := config.OAuthCredential{
		AccessToken: "access-old", RefreshToken: "refresh-old", IDToken: "id-old",
		AccountID: "account-1", PlanType: "plus", ExpiresAt: time.Now().UTC().Add(time.Hour),
		LastRefresh: time.Now().UTC().Add(-time.Minute), Generation: 1,
	}
	state := config.NewState()
	state.Connections["codex-main"] = config.Connection{ID: "connection-id-1", Name: "codex-main", AuthType: config.AuthCodexOAuth,
		Protocol: config.Responses, BaseURL: config.CodexBaseURL, CredentialRef: "oauth-ref-1",
		Models: []config.Model{{ID: "gpt-5-codex"}}}
	state.Connections["codex-unlogged"] = config.Connection{ID: "connection-id-2", Name: "codex-unlogged", AuthType: config.AuthCodexOAuth,
		Protocol: config.Responses, BaseURL: config.CodexBaseURL, Models: []config.Model{{ID: "gpt-5-codex"}}}
	state.OAuthCredentials["oauth-ref-1"] = credential
	if err := store.WriteJSON(st.Path("state.json"), state); err != nil {
		t.Fatal(err)
	}
	return NewSDKStore(st, "test-process"), st, credential
}

type pausedAuthUpdateHook struct {
	store   *SDKStore
	entered chan struct{}
	release chan struct{}
	once    sync.Once
}

func (h *pausedAuthUpdateHook) OnAuthRegistered(ctx context.Context, auth *coreauth.Auth) {
	h.store.OnAuthRegistered(ctx, auth)
}

func (h *pausedAuthUpdateHook) OnAuthUpdated(ctx context.Context, auth *coreauth.Auth) {
	h.once.Do(func() {
		close(h.entered)
		<-h.release
	})
	h.store.OnAuthUpdated(ctx, auth)
}

func (h *pausedAuthUpdateHook) OnResult(ctx context.Context, result coreauth.Result) {
	h.store.OnResult(ctx, result)
}

type lifecycleFakeExecutor struct {
	mu                     sync.Mutex
	refreshCalls           int
	executeCalls           int
	oldTokenExecutions     int
	waitForTwoUnauthorized bool
	unauthorizedReady      chan struct{}
	refreshEntered         chan struct{}
	releaseFirstRefresh    chan struct{}
}

func newLifecycleFakeExecutor(waitForTwoUnauthorized, blockFirstRefresh bool) *lifecycleFakeExecutor {
	executor := &lifecycleFakeExecutor{waitForTwoUnauthorized: waitForTwoUnauthorized}
	if waitForTwoUnauthorized {
		executor.unauthorizedReady = make(chan struct{})
	}
	if blockFirstRefresh {
		executor.refreshEntered = make(chan struct{})
		executor.releaseFirstRefresh = make(chan struct{})
	}
	return executor
}

func (e *lifecycleFakeExecutor) Identifier() string { return "codex" }

func (e *lifecycleFakeExecutor) Execute(ctx context.Context, auth *coreauth.Auth, _ cliproxyexecutor.Request, _ cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	e.mu.Lock()
	e.executeCalls++
	if err := ctx.Err(); err != nil {
		e.mu.Unlock()
		return cliproxyexecutor.Response{}, err
	}
	token := stringValue(auth.Metadata, "access_token")
	if token == "access-old" {
		e.oldTokenExecutions++
		waitForBoth := e.waitForTwoUnauthorized
		if waitForBoth && e.oldTokenExecutions == 2 {
			close(e.unauthorizedReady)
		}
		ready := e.unauthorizedReady
		e.mu.Unlock()
		if waitForBoth {
			<-ready
		}
		return cliproxyexecutor.Response{}, &coreauth.Error{
			HTTPStatus: http.StatusUnauthorized, Code: "unauthorized", Message: "expired access token",
		}
	}
	e.mu.Unlock()
	return cliproxyexecutor.Response{Payload: []byte(token)}, nil
}

func (e *lifecycleFakeExecutor) ExecuteStream(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (*cliproxyexecutor.StreamResult, error) {
	return nil, errors.New("stream execution not used in this test")
}

func (e *lifecycleFakeExecutor) Refresh(_ context.Context, auth *coreauth.Auth) (*coreauth.Auth, error) {
	e.mu.Lock()
	e.refreshCalls++
	rotation := e.refreshCalls
	var release <-chan struct{}
	if rotation == 1 && e.releaseFirstRefresh != nil {
		close(e.refreshEntered)
		release = e.releaseFirstRefresh
	}
	e.mu.Unlock()
	if release != nil {
		<-release
	}
	if auth.Metadata == nil {
		auth.Metadata = make(map[string]any)
	}
	auth.Metadata["access_token"] = fmt.Sprintf("access-rotation-%d", rotation)
	auth.Metadata["refresh_token"] = fmt.Sprintf("refresh-rotation-%d", rotation)
	auth.Metadata["expired"] = time.Now().UTC().Add(48 * time.Hour).Format(time.RFC3339Nano)
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	return auth, nil
}

func (e *lifecycleFakeExecutor) CountTokens(context.Context, *coreauth.Auth, cliproxyexecutor.Request, cliproxyexecutor.Options) (cliproxyexecutor.Response, error) {
	return cliproxyexecutor.Response{}, nil
}

func (e *lifecycleFakeExecutor) HttpRequest(context.Context, *coreauth.Auth, *http.Request) (*http.Response, error) {
	return nil, nil
}

func (e *lifecycleFakeExecutor) counts() (refresh, execute int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refreshCalls, e.executeCalls
}

func registerLifecycleTestModel(t *testing.T, authID string) {
	t.Helper()
	registry := cliproxy.GlobalModelRegistry()
	registry.RegisterClient(authID, "codex", []*cliproxy.ModelInfo{{ID: "gpt-5-codex"}})
	t.Cleanup(func() { registry.UnregisterClient(authID) })
}

func TestSDKStoreListLoadsOnlyManagedLoggedOAuth(t *testing.T) {
	sdkStore, _, credential := newTestSDKStore(t)
	got, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("List returned %d auth records, want 1", len(got))
	}
	auth := got[0]
	if auth.ID != sdkStore.AuthID("oauth-ref-1") || auth.Provider != "codex" || auth.Prefix != config.ConnectionPrefix("codex-main") {
		t.Fatalf("unexpected auth identity: %#v", auth)
	}
	if auth.Attributes["base_url"] != config.CodexBaseURL || auth.Attributes["plan_type"] != credential.PlanType {
		t.Fatalf("unexpected auth attributes: %#v", auth.Attributes)
	}
	if _, ok := auth.Attributes["api_key"]; ok {
		t.Fatal("OAuth Auth must not expose api_key")
	}
	if auth.Metadata["access_token"] != credential.AccessToken || auth.Metadata["cpagw_generation"] != uint64(1) {
		t.Fatalf("OAuth metadata missing token or source generation: %#v", auth.Metadata)
	}
	if auth.Metadata["cpagw_connection_id"] != "connection-id-1" {
		t.Fatalf("connection identity missing: %#v", auth.Metadata)
	}
}

func TestSDKStoreSaveRefreshDoesNotBumpRevisionAndAdvancesSourceGeneration(t *testing.T) {
	sdkStore, st, before := newTestSDKStore(t)
	ctx := context.Background()
	auths, err := sdkStore.List(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auth := auths[0]
	stateBefore, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	previousRevision := stateBefore.Revision
	auth.Metadata["access_token"] = "access-new"
	auth.Metadata["refresh_token"] = "refresh-new"
	auth.Metadata["expired"] = time.Now().UTC().Add(2 * time.Hour).Format(time.RFC3339Nano)
	auth.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	id, err := sdkStore.Save(ctx, auth)
	if err != nil {
		t.Fatal(err)
	}
	if id != auth.ID {
		t.Fatalf("Save ID = %q, want %q", id, auth.ID)
	}
	if got := uintValue(auth.Metadata["cpagw_generation"]); got != 2 {
		t.Fatalf("runtime cpagw_generation = %d, want 2", got)
	}
	stateAfter, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if stateAfter.Revision != previousRevision {
		t.Fatalf("Revision changed from %d to %d during token-only save", previousRevision, stateAfter.Revision)
	}
	saved := stateAfter.OAuthCredentials["oauth-ref-1"]
	if saved.Generation != 2 || saved.AccessToken != "access-new" || saved.RefreshToken != "refresh-new" {
		t.Fatalf("rotated credential not saved: %#v", saved)
	}
	if saved.IDToken != before.IDToken || saved.AccountID != before.AccountID {
		t.Fatal("token refresh overwrote stable account fields")
	}
}

func TestSDKStoreRefreshSynchronizesManagerGeneration(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	manager := coreauth.NewManager(sdkStore, nil, sdkStore.Hook())
	sdkStore.BindManager(manager)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(context.Background(), auths[0]); err != nil {
		t.Fatal(err)
	}
	for rotation := 1; rotation <= 2; rotation++ {
		updated, ok := manager.GetByID(auths[0].ID)
		if !ok {
			t.Fatal("registered auth missing")
		}
		updated.Metadata["access_token"] = fmt.Sprintf("access-refreshed-%d", rotation)
		updated.Metadata["refresh_token"] = fmt.Sprintf("refresh-refreshed-%d", rotation)
		updated.Metadata["expired"] = time.Now().UTC().Add(time.Duration(rotation+1) * time.Hour).Format(time.RFC3339Nano)
		updated.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
		if _, err := manager.Update(context.Background(), updated); err != nil {
			t.Fatalf("rotation %d: %v", rotation, err)
		}
		live, ok := manager.GetByID(auths[0].ID)
		if !ok {
			t.Fatal("refreshed auth missing")
		}
		wantGeneration := uint64(rotation + 1)
		if got := uintValue(live.Metadata["cpagw_generation"]); got != wantGeneration {
			t.Fatalf("after rotation %d, manager cpagw_generation = %d, want %d", rotation, got, wantGeneration)
		}
		state, err := st.Read()
		if err != nil {
			t.Fatal(err)
		}
		saved := state.OAuthCredentials["oauth-ref-1"]
		if saved.Generation != wantGeneration || saved.AccessToken != fmt.Sprintf("access-refreshed-%d", rotation) {
			t.Fatalf("after rotation %d, source credential out of sync: %#v", rotation, saved)
		}
	}
}

func TestSDKStoreSDK401RefreshSerializesSameIDAndPreservesConcurrentUpdate(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	executor := newLifecycleFakeExecutor(true, true)
	manager := coreauth.NewManager(sdkStore, nil, sdkStore.Hook())
	manager.RegisterExecutor(executor)
	sdkStore.BindManager(manager)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := auths[0]
	registerLifecycleTestModel(t, auth.ID)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	type executionResult struct {
		response cliproxyexecutor.Response
		err      error
	}
	results := make(chan executionResult, 2)
	for i := 0; i < 2; i++ {
		go func() {
			response, err := manager.Execute(context.Background(), []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5-codex"}, cliproxyexecutor.Options{})
			results <- executionResult{response: response, err: err}
		}()
	}
	select {
	case <-executor.refreshEntered:
	case <-time.After(2 * time.Second):
		t.Fatal("SDK did not enter the fake OAuth refresh")
	}

	current, ok := manager.GetByID(auth.ID)
	if !ok {
		t.Fatal("auth missing while refresh was in flight")
	}
	current.Metadata["structural_marker"] = "changed-during-refresh"
	if _, err := manager.Update(context.Background(), current); err != nil {
		t.Fatalf("concurrent structural update failed: %v", err)
	}
	close(executor.releaseFirstRefresh)

	for i := 0; i < 2; i++ {
		select {
		case result := <-results:
			if result.err != nil {
				t.Fatalf("concurrent Execute %d failed: %v", i+1, result.err)
			}
			if got := string(result.response.Payload); got != "access-rotation-1" {
				t.Fatalf("concurrent Execute %d payload = %q, want refreshed token", i+1, got)
			}
		case <-time.After(3 * time.Second):
			t.Fatal("concurrent Execute did not finish")
		}
	}
	if refreshes, calls := executor.counts(); refreshes != 1 || calls != 4 {
		t.Fatalf("refresh/execute calls = %d/%d, want 1/4", refreshes, calls)
	}
	live, ok := manager.GetByID(auth.ID)
	if !ok || live.Metadata["structural_marker"] != "changed-during-refresh" {
		t.Fatalf("concurrent structural metadata was not preserved: %#v", live)
	}
	if got := uintValue(live.Metadata["cpagw_generation"]); got != 2 {
		t.Fatalf("runtime generation = %d, want 2", got)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved := state.OAuthCredentials["oauth-ref-1"]
	if saved.Generation != 2 || saved.AccessToken != "access-rotation-1" || saved.RefreshToken != "refresh-rotation-1" {
		t.Fatalf("SDK refresh was not persisted: %#v", saved)
	}

	for rotation := 2; rotation <= 3; rotation++ {
		refreshed, err := manager.ForceRefreshAuth(context.Background(), auth.ID)
		if err != nil {
			t.Fatalf("SDK refresh rotation %d failed: %v", rotation, err)
		}
		wantToken := fmt.Sprintf("access-rotation-%d", rotation)
		if got := stringValue(refreshed.Metadata, "access_token"); got != wantToken {
			t.Fatalf("SDK rotation %d access token = %q, want %q", rotation, got, wantToken)
		}
		if got := uintValue(refreshed.Metadata["cpagw_generation"]); got != uint64(rotation+1) {
			t.Fatalf("SDK rotation %d cpagw generation = %d, want %d", rotation, got, rotation+1)
		}
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved = state.OAuthCredentials["oauth-ref-1"]
	if saved.Generation != 4 || saved.AccessToken != "access-rotation-3" || saved.RefreshToken != "refresh-rotation-3" {
		t.Fatalf("consecutive SDK rotations were not persisted: %#v", saved)
	}
}

func TestSDKStoreFailedRefreshCancelsTrackAndBlocks401Recovery(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	executor := newLifecycleFakeExecutor(false, false)
	manager := coreauth.NewManager(sdkStore, nil, sdkStore.Hook())
	manager.RegisterExecutor(executor)
	sdkStore.BindManager(manager)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := auths[0]
	registerLifecycleTestModel(t, auth.ID)
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}

	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	credential := state.OAuthCredentials["oauth-ref-1"]
	credential.Generation++
	state.OAuthCredentials["oauth-ref-1"] = credential
	if err := store.WriteJSON(st.Path("state.json"), state); err != nil {
		t.Fatal(err)
	}

	tracked, release, err := sdkStore.Track(context.Background(), auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	_, executeErr := manager.Execute(tracked, []string{"codex"}, cliproxyexecutor.Request{Model: "gpt-5-codex"}, cliproxyexecutor.Options{})
	if !errors.Is(executeErr, context.Canceled) {
		t.Fatalf("Execute error = %v, want canceled after OAuth persistence failure", executeErr)
	}
	select {
	case <-tracked.Done():
	default:
		t.Fatal("failed OAuth persistence did not cancel the tracked request")
	}
	if sdkStore.Healthy(auth.ID) {
		t.Fatal("failed OAuth persistence left the Auth healthy")
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatal("failed OAuth Auth remained registered after 401 recovery")
	}
	if refreshes, calls := executor.counts(); refreshes != 1 || calls != 2 {
		t.Fatalf("refresh/execute calls = %d/%d, want 1/2 (the retry must observe cancellation)", refreshes, calls)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved := state.OAuthCredentials["oauth-ref-1"]
	if saved.Generation != credential.Generation || saved.AccessToken != credential.AccessToken {
		t.Fatalf("failed refresh altered durable credential: %#v", saved)
	}
}

func TestSDKStoreConcurrentMarkResultDuringGenerationSync(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	hook := &pausedAuthUpdateHook{
		store: sdkStore, entered: make(chan struct{}), release: make(chan struct{}),
	}
	manager := coreauth.NewManager(sdkStore, nil, hook)
	sdkStore.BindManager(manager)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := manager.Register(context.Background(), auths[0]); err != nil {
		t.Fatal(err)
	}
	updated, ok := manager.GetByID(auths[0].ID)
	if !ok {
		t.Fatal("registered Auth missing")
	}
	updated.Metadata["access_token"] = "access-concurrent-refresh"
	updated.Metadata["refresh_token"] = "refresh-concurrent-refresh"
	updateDone := make(chan error, 1)
	go func() {
		_, err := manager.Update(context.Background(), updated)
		updateDone <- err
	}()
	select {
	case <-hook.entered:
	case <-time.After(time.Second):
		close(hook.release)
		t.Fatal("manager update did not reach the generation-sync hook")
	}

	markDone := make(chan struct{})
	go func() {
		manager.MarkResult(context.Background(), coreauth.Result{
			AuthID: auths[0].ID, Model: "gpt-5-codex", Success: true,
		})
		close(markDone)
	}()
	select {
	case <-markDone:
	case <-time.After(time.Second):
		close(hook.release)
		t.Fatal("concurrent MarkResult did not finish")
	}
	close(hook.release)
	if err := <-updateDone; err != nil {
		t.Fatalf("token rotation update failed: %v", err)
	}
	if !sdkStore.Healthy(auths[0].ID) {
		t.Fatal("concurrent unchanged MarkResult snapshot marked OAuth unhealthy")
	}
	live, ok := manager.GetByID(auths[0].ID)
	if !ok {
		t.Fatal("concurrent MarkResult removed OAuth Auth")
	}
	if got := uintValue(live.Metadata["cpagw_generation"]); got != 2 {
		t.Fatalf("manager generation after concurrent MarkResult = %d, want 2", got)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	saved := state.OAuthCredentials["oauth-ref-1"]
	if saved.Generation != 2 || saved.AccessToken != "access-concurrent-refresh" || saved.RefreshToken != "refresh-concurrent-refresh" {
		t.Fatalf("concurrent MarkResult changed rotated credential: %#v", saved)
	}
}

func TestSDKStoreIgnoresUnchangedSDKBookkeeping(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	before := state.Revision
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auths[0].StatusMessage = "SDK bookkeeping"
	if _, err := sdkStore.Save(context.Background(), auths[0]); err != nil {
		t.Fatal(err)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if state.Revision != before || state.OAuthCredentials["oauth-ref-1"].Generation != 1 {
		t.Fatal("non-token SDK state changed cpagw credential state")
	}
}

func TestSDKStoreIgnoresStaleUnchangedCredentialSnapshot(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	stale := auths[0]
	manager := coreauth.NewManager(sdkStore, nil, sdkStore.Hook())
	sdkStore.BindManager(manager)
	if _, err := manager.Register(context.Background(), stale); err != nil {
		t.Fatal(err)
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	state.Revision = 7
	credential := state.OAuthCredentials["oauth-ref-1"]
	credential.Generation = 2
	credential.PlanType = "pro"
	credential.ExpiresAt = time.Now().UTC().Add(8 * time.Hour).Truncate(time.Second)
	credential.LastRefresh = time.Now().UTC().Add(-30 * time.Second).Truncate(time.Second)
	state.OAuthCredentials["oauth-ref-1"] = credential
	if err := store.WriteJSON(st.Path("state.json"), state); err != nil {
		t.Fatal(err)
	}

	updated, ok := manager.GetByID(stale.ID)
	if !ok {
		t.Fatal("registered Auth missing")
	}
	updated.StatusMessage = "SDK bookkeeping"
	if _, err := manager.Update(context.Background(), updated); err != nil {
		t.Fatalf("unchanged stale snapshot should be ignored: %v", err)
	}
	if !sdkStore.Healthy(stale.ID) {
		t.Fatal("unchanged stale snapshot marked the Auth unhealthy")
	}
	live, ok := manager.GetByID(stale.ID)
	if !ok {
		t.Fatal("unchanged stale snapshot removed the registered Auth")
	}
	if got := uintValue(live.Metadata["cpagw_generation"]); got != credential.Generation {
		t.Fatalf("runtime generation = %d, want durable generation %d", got, credential.Generation)
	}
	if live.Metadata["plan_type"] != credential.PlanType {
		t.Fatalf("runtime plan type = %v, want durable value %q", live.Metadata["plan_type"], credential.PlanType)
	}
	if got, err := metadataTime(live.Metadata["expired"]); err != nil || !got.Equal(credential.ExpiresAt) {
		t.Fatalf("runtime expiry = %v, %v; want durable value %v", got, err, credential.ExpiresAt)
	}
	if got, err := metadataTime(live.Metadata["last_refresh"]); err != nil || !got.Equal(credential.LastRefresh) {
		t.Fatalf("runtime last_refresh = %v, %v; want durable value %v", got, err, credential.LastRefresh)
	}
	if live.StatusMessage != "SDK bookkeeping" {
		t.Fatalf("SDK runtime status was not preserved: %q", live.StatusMessage)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	current := state.OAuthCredentials["oauth-ref-1"]
	if current != credential || state.Revision != 7 {
		t.Fatalf("stale bookkeeping snapshot changed durable credential: %#v, want %#v, revision %d", current, credential, state.Revision)
	}

	live.Metadata["access_token"] = "access-after-stale-snapshot"
	live.Metadata["refresh_token"] = "refresh-after-stale-snapshot"
	live.Metadata["expired"] = time.Now().UTC().Add(12 * time.Hour).Format(time.RFC3339Nano)
	live.Metadata["last_refresh"] = time.Now().UTC().Format(time.RFC3339Nano)
	if _, err := manager.Update(context.Background(), live); err != nil {
		t.Fatalf("legal rotation after stale snapshot was rejected: %v", err)
	}
	live, ok = manager.GetByID(stale.ID)
	if !ok || uintValue(live.Metadata["cpagw_generation"]) != 3 || live.Metadata["access_token"] != "access-after-stale-snapshot" {
		t.Fatalf("legal post-stale rotation did not advance runtime credential: %#v", live)
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	current = state.OAuthCredentials["oauth-ref-1"]
	if current.Generation != 3 || current.AccessToken != "access-after-stale-snapshot" || state.Revision != 7 {
		t.Fatalf("legal post-stale rotation was not persisted: %#v, revision %d", current, state.Revision)
	}
}

func TestSDKStoreRejectsStaleSnapshotWithDifferentAccount(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := auths[0]
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	credential := state.OAuthCredentials["oauth-ref-1"]
	credential.Generation++
	state.OAuthCredentials["oauth-ref-1"] = credential
	if err := store.WriteJSON(st.Path("state.json"), state); err != nil {
		t.Fatal(err)
	}
	auth.Metadata["account_id"] = "different-account"
	if _, err := sdkStore.Save(context.Background(), auth); err == nil {
		t.Fatal("stale snapshot with a different account identity was accepted")
	}
	if sdkStore.Healthy(auth.ID) {
		t.Fatal("account identity mismatch did not mark OAuth unhealthy")
	}
	state, err = st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.OAuthCredentials["oauth-ref-1"]; got != credential {
		t.Fatalf("account mismatch changed durable credential: %#v, want %#v", got, credential)
	}
}

func TestSDKStoreSaveFailureCancelsInflightAndRemovesAuth(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	manager := coreauth.NewManager(sdkStore, nil, sdkStore.Hook())
	sdkStore.BindManager(manager)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := auths[0]
	if _, err := manager.Register(context.Background(), auth); err != nil {
		t.Fatal(err)
	}
	tracked, release, err := sdkStore.Track(context.Background(), auth.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	// 凭证版本已推进且快照携带不同令牌，必须作为过期轮换拒绝。
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	credential := state.OAuthCredentials["oauth-ref-1"]
	credential.Generation = 2
	state.OAuthCredentials["oauth-ref-1"] = credential
	if err := store.WriteJSON(st.Path("state.json"), state); err != nil {
		t.Fatal(err)
	}
	auth.Metadata["access_token"] = "access-stale-rotation"
	if _, err := sdkStore.Save(context.Background(), auth); err == nil {
		t.Fatal("expected failed conditional save")
	}
	select {
	case <-tracked.Done():
	case <-time.After(time.Second):
		t.Fatal("in-flight request was not canceled")
	}
	if sdkStore.Healthy(auth.ID) {
		t.Fatal("failed credential save remained healthy")
	}
	if _, ok := manager.GetByID(auth.ID); ok {
		t.Fatal("failed OAuth Auth remained registered")
	}
	if _, _, err := sdkStore.Track(context.Background(), auth.ID); err == nil {
		t.Fatal("unhealthy OAuth accepted a new request")
	}
}

func TestSDKStoreCanceledSaveAfterRotationFailsClosed(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	auths, err := sdkStore.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	auth := auths[0]
	auth.Metadata["access_token"] = "access-rotated-but-not-saved"
	auth.Metadata["refresh_token"] = "refresh-rotated-but-not-saved"
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := sdkStore.Save(ctx, auth); err == nil {
		t.Fatal("canceled rotated credential save should fail")
	}
	if sdkStore.Healthy(auth.ID) {
		t.Fatal("canceled token rotation did not mark the Auth unhealthy")
	}
	state, err := st.Read()
	if err != nil {
		t.Fatal(err)
	}
	if got := state.OAuthCredentials["oauth-ref-1"].Generation; got != 1 {
		t.Fatalf("canceled Save changed durable generation to %d", got)
	}
}

func TestSDKStoreDeleteCannotRemoveDomainState(t *testing.T) {
	sdkStore, st, _ := newTestSDKStore(t)
	if err := sdkStore.Delete(context.Background(), sdkStore.AuthID("oauth-ref-1")); !errors.Is(err, ErrDeleteUnsupported) {
		t.Fatalf("Delete error = %v", err)
	}
	if _, err := os.Stat(st.Path("state.json")); err != nil {
		t.Fatal("Delete modified cpagw source state")
	}
}
