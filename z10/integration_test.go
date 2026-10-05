package z10

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	runtimeexecutor "github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor"
	_ "github.com/router-for-me/CLIProxyAPI/v8/internal/translator"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// usageSignal fires after the z10 plugin (registered earlier) handled a friend record.
type usageSignal struct{ records chan coreusage.Record }

func (s *usageSignal) HandleUsage(_ context.Context, record coreusage.Record) {
	if strings.HasPrefix(record.APIKey, PrincipalPrefix) {
		s.records <- record
	}
}

// TestIntegrationRealServer drives the real API server built like cmd/server: built-in
// access provider first, then z10 options, a real OpenAI-compatible executor and a fake
// upstream. It proves the hook order (global middleware before route auth, provider
// registration before NewServer, router configurator for admin routes) end to end.
func TestIntegrationRealServer(t *testing.T) {
	gin.SetMode(gin.TestMode)
	var (
		upstreamMu     sync.Mutex
		upstreamModels []string
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		upstreamMu.Lock()
		upstreamModels = append(upstreamModels, gjson.GetBytes(body, "model").String())
		upstreamMu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`))
	}))
	defer upstream.Close()
	upstreamCalls := func() []string {
		upstreamMu.Lock()
		defer upstreamMu.Unlock()
		return append([]string(nil), upstreamModels...)
	}

	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeTestFile(t, configPath, testConfigYAML)
	writeTestFile(t, filepath.Join(dir, FriendsFileName), testFriendsYAML)

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{ownerKey}}}
	cfg.AuthDir = filepath.Join(dir, "auths")
	cfg.RemoteManagement.SecretKey = mgmtKey

	// Same order as cmd/server/main.go.
	configaccess.Register(&cfg.SDKConfig)
	rt := NewRuntime(Options{ConfigPath: configPath, Now: newFakeClock(testNow()).Now, FlushDelay: time.Hour})
	rt.Register()
	t.Cleanup(func() {
		rt.Close()
		sdkaccess.UnregisterProvider(AccessProviderType)
	})
	signal := &usageSignal{records: make(chan coreusage.Record, 16)}
	coreusage.RegisterNamedPlugin("z10-integration-signal", signal)

	const provider, authID = "z10-test-compat", "z10-test-compat-1"
	authManager := auth.NewManager(nil, nil, nil)
	authManager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(provider, cfg))
	if _, errRegister := authManager.Register(context.Background(), &auth.Auth{
		ID: authID, Provider: provider, Status: auth.StatusActive,
		Attributes: map[string]string{"base_url": upstream.URL + "/v1", "api_key": "upstream-key"},
	}); errRegister != nil {
		t.Fatal(errRegister)
	}
	registry.GetGlobalRegistry().RegisterClient(authID, provider, []*registry.ModelInfo{
		{ID: "deepseek-v4-flash", Object: "model", OwnedBy: "test"},
		{ID: "claude-sonnet-4-6", Object: "model", OwnedBy: "test"},
	})
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })

	server := api.NewServer(cfg, authManager, sdkaccess.NewManager(), configPath, rt.ServerOptions()...)
	handler := server.Handler()
	do := func(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewReader(body))
		req.RemoteAddr = "127.0.0.1:50000"
		req.Header.Set("Content-Type", "application/json")
		for key, value := range headers {
			req.Header.Set(key, value)
		}
		recorder := httptest.NewRecorder()
		handler.ServeHTTP(recorder, req)
		return recorder
	}

	// Criterion 1: listed model 200, others 403 without reaching upstream.
	if recorder := do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(aliceKey)); recorder.Code != http.StatusOK {
		t.Fatalf("friend allowed model: %d %s", recorder.Code, recorder.Body.String())
	}
	for _, model := range []string{"claude-sonnet-4-6", "gemini-3-flash", "gpt-5.6"} {
		if recorder := do(http.MethodPost, "/v1/chat/completions", chatBody(model), bearer(aliceKey)); recorder.Code != http.StatusForbidden {
			t.Fatalf("friend %s: %d %s", model, recorder.Code, recorder.Body.String())
		}
	}
	if calls := upstreamCalls(); len(calls) != 1 || calls[0] != "deepseek-v4-flash" {
		t.Fatalf("upstream calls = %v", calls)
	}

	// Criterion 6 (owner regression): the owner reaches every model.
	if recorder := do(http.MethodPost, "/v1/chat/completions", chatBody("claude-sonnet-4-6"), bearer(ownerKey)); recorder.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", recorder.Code, recorder.Body.String())
	}
	if calls := upstreamCalls(); len(calls) != 2 || calls[1] != "claude-sonnet-4-6" {
		t.Fatalf("owner upstream calls = %v", calls)
	}

	// Criterion 2: model lists only show allowed models.
	friendList := do(http.MethodGet, "/v1/models", nil, bearer(aliceKey)).Body.String()
	if ids := gjson.Get(friendList, "data.#.id").String(); ids != `["deepseek-v4-flash"]` {
		t.Fatalf("friend /v1/models = %s", friendList)
	}
	ownerList := do(http.MethodGet, "/v1/models", nil, bearer(ownerKey)).Body.String()
	if !strings.Contains(ownerList, "claude-sonnet-4-6") || !strings.Contains(ownerList, "deepseek-v4-flash") {
		t.Fatalf("owner /v1/models = %s", ownerList)
	}
	geminiList := do(http.MethodGet, "/v1beta/models", nil, bearer(aliceKey)).Body.String()
	if strings.Contains(geminiList, "claude-sonnet-4-6") {
		t.Fatalf("friend /v1beta/models = %s", geminiList)
	}

	// Criterion 3: WebSocket and unlisted endpoints are rejected.
	if recorder := do(http.MethodGet, "/v1/responses", nil, map[string]string{"Authorization": "Bearer " + aliceKey, "Upgrade": "websocket", "Connection": "Upgrade"}); recorder.Code != http.StatusForbidden {
		t.Fatalf("websocket: %d", recorder.Code)
	}
	for _, path := range []string{"/v1/images/generations", "/v1/realtime/client_secrets", "/v1beta/interactions", "/v1/responses/compact"} {
		if recorder := do(http.MethodPost, path, chatBody("deepseek-v4-flash"), bearer(aliceKey)); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s: %d", path, recorder.Code)
		}
	}

	// Criterion 5: tokens recorded from the real usage pipeline, readable by the admin.
	select {
	case record := <-signal.records:
		if record.APIKey != "friend:alice" {
			t.Fatalf("usage principal = %q", record.APIKey)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("no usage record for the friend request")
	}
	alice := rt.Usage().Report().Friends["alice"]
	if alice == nil || alice.Requests != 1 || alice.InputTokens != 11 || alice.OutputTokens != 5 || alice.TotalTokens != 16 || alice.Models["deepseek-v4-flash"].TotalTokens != 16 {
		t.Fatalf("alice usage = %+v", alice)
	}
	usage := do(http.MethodGet, "/z10/usage", nil, bearer(mgmtKey))
	if usage.Code != http.StatusOK || gjson.Get(usage.Body.String(), "friends.alice.total_tokens").Int() != 16 {
		t.Fatalf("/z10/usage: %d %s", usage.Code, usage.Body.String())
	}
	if recorder := do(http.MethodGet, "/z10/usage", nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("/z10/usage without key: %d", recorder.Code)
	}
	if recorder := do(http.MethodGet, "/z10/friends", nil, bearer(aliceKey)); recorder.Code != http.StatusForbidden {
		t.Fatalf("/z10/friends with friend key: %d", recorder.Code)
	}
	if body := do(http.MethodGet, "/z10/friends", nil, bearer(mgmtKey)).Body.String(); strings.Contains(body, aliceKey) || strings.Contains(body, bobKey) {
		t.Fatalf("/z10/friends leaks keys: %s", body)
	}

	// Criterion 4: disabling a key takes effect on reload, without a restart.
	writeTestFile(t, filepath.Join(dir, FriendsFileName), strings.Replace(testFriendsYAML, "expires: 2026-12-31", "expires: 2026-12-31\n    enabled: false", 1))
	if errReload := rt.Reload(); errReload != nil {
		t.Fatal(errReload)
	}
	if recorder := do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(aliceKey)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("disabled friend: %d %s", recorder.Code, recorder.Body.String())
	}

	// Usage survives a restart.
	if errFlush := rt.Usage().Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	restarted := NewUsageStore(filepath.Join(dir, UsageFileName), time.Now, time.Hour)
	if errLoad := restarted.Load(); errLoad != nil || restarted.Report().Friends["alice"].TotalTokens != 16 {
		t.Fatalf("persisted usage: %v %+v", errLoad, restarted.Report().Friends["alice"])
	}
}
