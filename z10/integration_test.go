package z10

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
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
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// The real-server harness below is ported from the adversarial review PoCs: it builds
// the API server like cmd/server (built-in owner provider first, then z10 options) with
// one OpenAI-compatible credential per channel, all pointed at a fake upstream that
// records which channel (upstream API key) and model each request reached.

const carolKey = "sk-z10-carol-0123456789abcdef"

// serverFriendsYAML gives alice globs like the production plan and carol one model.
const serverFriendsYAML = `keys:
  - name: alice
    key: ` + aliceKey + `
    models: ["deepseek-v4-*", "cline-pass/*"]
    channels: ["deepseek", "clinepass"]
  - name: carol
    key: ` + carolKey + `
    models: ["deepseek-v4-flash"]
    channels: ["deepseek"]
`

type upstreamHit struct {
	Channel string
	Model   string
}

type testChannel struct {
	Name   string // openai-compatibility name
	Prefix string
	Models []string
}

type serverEnv struct {
	rt      *Runtime
	handler http.Handler
	dir     string

	mu   sync.Mutex
	hits []upstreamHit
}

func (e *serverEnv) resetHits() {
	e.mu.Lock()
	e.hits = nil
	e.mu.Unlock()
}

func (e *serverEnv) Hits() []upstreamHit {
	e.mu.Lock()
	defer e.mu.Unlock()
	return append([]upstreamHit(nil), e.hits...)
}

func (e *serverEnv) do(method, path string, body []byte, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, bytes.NewReader(body))
	req.RemoteAddr = "127.0.0.1:50000"
	req.Header.Set("Content-Type", "application/json")
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	e.handler.ServeHTTP(recorder, req)
	return recorder
}

// channelKey is the upstream API key the fake upstream sees for a channel.
func channelKey(name string) string { return "key-" + name }

func defaultChannels() []testChannel {
	return []testChannel{
		{Name: "deepseek", Models: []string{"deepseek-v4-flash", "deepseek-v4-pro"}},
		{Name: "ownerpaid", Models: []string{"claude-sonnet-4-6", "gpt-5.6", "gemini-3-flash"}},
	}
}

// newServerEnv builds the server. mutate may adjust the server config (for example to
// enable request logging) before the server is built.
func newServerEnv(t *testing.T, channels []testChannel, friendsYAML string, mutate ...func(*config.Config)) *serverEnv {
	t.Helper()
	gin.SetMode(gin.TestMode)
	env := &serverEnv{}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		env.mu.Lock()
		env.hits = append(env.hits, upstreamHit{Channel: strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "), Model: gjson.GetBytes(body, "model").String()})
		env.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","created":1,"model":"upstream","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":11,"completion_tokens":5,"total_tokens":16}}`))
	}))
	t.Cleanup(upstream.Close)

	env.dir = t.TempDir()
	configPath := filepath.Join(env.dir, "config.yaml")
	// config.yaml lists the channels so the admin API can validate friend channels.
	configYAML := testConfigYAML + "openai-compatibility:\n"
	for _, channel := range channels {
		configYAML += "  - name: " + channel.Name + "\n    base-url: " + upstream.URL + "/v1\n"
	}
	writeTestFile(t, configPath, configYAML)
	if friendsYAML != "" {
		writeTestFile(t, filepath.Join(env.dir, FriendsFileName), friendsYAML)
	}

	cfg := &config.Config{SDKConfig: sdkconfig.SDKConfig{APIKeys: []string{ownerKey}}}
	cfg.AuthDir = filepath.Join(env.dir, "auths")
	cfg.RemoteManagement.SecretKey = mgmtKey
	for _, fn := range mutate {
		fn(cfg)
	}

	// Same order as cmd/server/main.go.
	configaccess.Register(&cfg.SDKConfig)
	env.rt = NewRuntime(Options{ConfigPath: configPath, Now: newFakeClock(testNow()).Now, FlushDelay: time.Hour})
	env.rt.Register()
	t.Cleanup(func() {
		env.rt.Close()
		sdkaccess.UnregisterProvider(AccessProviderType)
	})

	authManager := auth.NewManager(nil, nil, nil)
	for _, channel := range channels {
		provider := util.OpenAICompatibleProviderKey(channel.Name)
		authID := "test-" + provider
		authManager.RegisterExecutor(runtimeexecutor.NewOpenAICompatExecutor(provider, cfg))
		if _, errRegister := authManager.Register(context.Background(), &auth.Auth{
			ID: authID, Provider: provider, Status: auth.StatusActive, Prefix: channel.Prefix,
			Attributes: map[string]string{"base_url": upstream.URL + "/v1", "api_key": channelKey(channel.Name)},
		}); errRegister != nil {
			t.Fatal(errRegister)
		}
		infos := make([]*registry.ModelInfo, 0, len(channel.Models))
		for _, model := range channel.Models {
			infos = append(infos, &registry.ModelInfo{ID: model, Object: "model", OwnedBy: provider})
		}
		registry.GetGlobalRegistry().RegisterClient(authID, provider, infos)
		t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(authID) })
	}

	server := api.NewServer(cfg, authManager, sdkaccess.NewManager(), configPath, env.rt.ServerOptions()...)
	env.handler = server.Handler()
	return env
}

// usageSignal fires after the z10 plugin (registered earlier) handled a friend record.
type usageSignal struct{ records chan coreusage.Record }

func (s *usageSignal) HandleUsage(_ context.Context, record coreusage.Record) {
	if strings.HasPrefix(record.APIKey, PrincipalPrefix) {
		s.records <- record
	}
}

// TestIntegrationRealServer proves the hook order (global middleware before route auth,
// provider registration before NewServer, router configurator for admin routes) end to
// end with a real OpenAI-compatible executor, covering design criteria 1-5 and the owner
// regression.
func TestIntegrationRealServer(t *testing.T) {
	env := newServerEnv(t, defaultChannels(), serverFriendsYAML)
	signal := &usageSignal{records: make(chan coreusage.Record, 16)}
	coreusage.RegisterNamedPlugin("z10-integration-signal", signal)

	// Criterion 1: listed model 200, others 403 without reaching upstream.
	if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(aliceKey)); recorder.Code != http.StatusOK {
		t.Fatalf("friend allowed model: %d %s", recorder.Code, recorder.Body.String())
	}
	for _, model := range []string{"claude-sonnet-4-6", "gemini-3-flash", "gpt-5.6"} {
		if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody(model), bearer(aliceKey)); recorder.Code != http.StatusForbidden {
			t.Fatalf("friend %s: %d %s", model, recorder.Code, recorder.Body.String())
		}
	}
	if hits := env.Hits(); len(hits) != 1 || hits[0] != (upstreamHit{Channel: channelKey("deepseek"), Model: "deepseek-v4-flash"}) {
		t.Fatalf("upstream hits = %+v", hits)
	}

	// Owner regression: the owner reaches every model.
	if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("claude-sonnet-4-6"), bearer(ownerKey)); recorder.Code != http.StatusOK {
		t.Fatalf("owner: %d %s", recorder.Code, recorder.Body.String())
	}
	if hits := env.Hits(); len(hits) != 2 || hits[1].Channel != channelKey("ownerpaid") {
		t.Fatalf("owner upstream hits = %+v", hits)
	}

	// Criterion 2: model lists only show allowed models.
	friendList := env.do(http.MethodGet, "/v1/models", nil, bearer(aliceKey)).Body.String()
	if ids := gjson.Get(friendList, "data.#.id").String(); ids != `["deepseek-v4-flash","deepseek-v4-pro"]` && ids != `["deepseek-v4-pro","deepseek-v4-flash"]` {
		t.Fatalf("friend /v1/models = %s", friendList)
	}
	ownerList := env.do(http.MethodGet, "/v1/models", nil, bearer(ownerKey)).Body.String()
	if !strings.Contains(ownerList, "claude-sonnet-4-6") || !strings.Contains(ownerList, "deepseek-v4-flash") {
		t.Fatalf("owner /v1/models = %s", ownerList)
	}
	for _, headers := range []map[string]string{{"Anthropic-Version": "2023-06-01"}, {"User-Agent": "grok-shell/1.0"}} {
		headers["Authorization"] = "Bearer " + aliceKey
		if body := env.do(http.MethodGet, "/v1/models", nil, headers).Body.String(); strings.Contains(body, "sonnet") || strings.Contains(body, "gpt-5.6") {
			t.Fatalf("friend list leaks hidden models: %s", body)
		}
	}
	if body := env.do(http.MethodGet, "/v1beta/models", nil, bearer(aliceKey)).Body.String(); strings.Contains(body, "claude-sonnet-4-6") || !strings.Contains(body, "deepseek-v4-flash") {
		t.Fatalf("friend /v1beta/models = %s", body)
	}

	// Criterion 3: WebSocket and unlisted endpoints are rejected.
	if recorder := env.do(http.MethodGet, "/v1/responses", nil, map[string]string{"Authorization": "Bearer " + aliceKey, "Upgrade": "websocket", "Connection": "Upgrade"}); recorder.Code != http.StatusForbidden {
		t.Fatalf("websocket: %d", recorder.Code)
	}
	for _, path := range []string{"/v1/images/generations", "/v1/realtime/client_secrets", "/v1beta/interactions", "/v1/responses/compact"} {
		if recorder := env.do(http.MethodPost, path, chatBody("deepseek-v4-flash"), bearer(aliceKey)); recorder.Code != http.StatusForbidden {
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
	alice := env.rt.Usage().Report().Friends["alice"]
	if alice == nil || alice.Requests != 1 || alice.InputTokens != 11 || alice.OutputTokens != 5 || alice.TotalTokens != 16 || alice.Models["deepseek-v4-flash"].TotalTokens != 16 {
		t.Fatalf("alice usage = %+v", alice)
	}
	usage := env.do(http.MethodGet, "/z10/usage", nil, bearer(mgmtKey))
	if usage.Code != http.StatusOK || gjson.Get(usage.Body.String(), "friends.alice.total_tokens").Int() != 16 {
		t.Fatalf("/z10/usage: %d %s", usage.Code, usage.Body.String())
	}
	if recorder := env.do(http.MethodGet, "/z10/usage", nil, nil); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("/z10/usage without key: %d", recorder.Code)
	}
	if recorder := env.do(http.MethodGet, "/z10/friends", nil, bearer(aliceKey)); recorder.Code != http.StatusForbidden {
		t.Fatalf("/z10/friends with friend key: %d", recorder.Code)
	}
	if body := env.do(http.MethodGet, "/z10/friends", nil, bearer(mgmtKey)).Body.String(); strings.Contains(body, aliceKey) || strings.Contains(body, carolKey) || !strings.Contains(body, `"channels"`) {
		t.Fatalf("/z10/friends: %s", body)
	}

	// Criterion 4: disabling a key takes effect on reload, without a restart.
	writeTestFile(t, filepath.Join(env.dir, FriendsFileName), strings.Replace(serverFriendsYAML, `channels: ["deepseek", "clinepass"]`, `channels: ["deepseek", "clinepass"]`+"\n    enabled: false", 1))
	if errReload := env.rt.Reload(); errReload != nil {
		t.Fatal(errReload)
	}
	if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(aliceKey)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("disabled friend: %d %s", recorder.Code, recorder.Body.String())
	}

	// Usage survives a restart.
	if errFlush := env.rt.Usage().Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	restarted := NewUsageStore(filepath.Join(env.dir, UsageFileName), time.Now, time.Hour)
	if errLoad := restarted.Load(); errLoad != nil || restarted.Report().Friends["alice"].TotalTokens != 16 {
		t.Fatalf("persisted usage: %v %+v", errLoad, restarted.Report().Friends["alice"])
	}
}

// Review PoC 1: a second channel registering an allowed name (or a name matching the
// glob) must not serve friend traffic.
func TestIntegrationSharedModelNameAcrossChannels(t *testing.T) {
	channels := append(defaultChannels(), testChannel{
		Name:   "subscription", // e.g. an OAuth subscription whose catalog lists DeepSeek models
		Models: []string{"deepseek-v4-flash", "deepseek-v4-1-flash"},
	})
	env := newServerEnv(t, channels, serverFriendsYAML)

	for _, key := range []string{aliceKey, carolKey} {
		for range 8 {
			recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(key))
			if recorder.Code != http.StatusForbidden || !strings.Contains(recorder.Body.String(), "channel this API key cannot use") {
				t.Fatalf("shared name: %d %s", recorder.Code, recorder.Body.String())
			}
		}
	}
	if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-1-flash"), bearer(aliceKey)); recorder.Code != http.StatusForbidden {
		t.Fatalf("glob-only model on another channel: %d", recorder.Code)
	}
	if hits := env.Hits(); len(hits) != 0 {
		t.Fatalf("denied requests reached upstream: %+v", hits)
	}
	for range 4 {
		if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-pro"), bearer(aliceKey)); recorder.Code != http.StatusOK {
			t.Fatalf("exclusive model: %d %s", recorder.Code, recorder.Body.String())
		}
	}
	for _, hit := range env.Hits() {
		if hit.Channel != channelKey("deepseek") {
			t.Fatalf("friend traffic reached %s", hit.Channel)
		}
	}
	list := env.do(http.MethodGet, "/v1/models", nil, bearer(aliceKey)).Body.String()
	if ids := gjson.Get(list, "data.#.id").String(); ids != `["deepseek-v4-pro"]` {
		t.Fatalf("friend list must hide shared names: %s", list)
	}
	if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(ownerKey)); recorder.Code != http.StatusOK {
		t.Fatalf("owner: %d", recorder.Code)
	}
}

// Review PoC 1b: a prefixed credential (force-model-prefix off) also registers bare names.
func TestIntegrationPrefixedChannel(t *testing.T) {
	channels := append(defaultChannels(), testChannel{
		Name: "clinepass", Prefix: "cline-pass",
		Models: []string{"deepseek-v4-flash", "cline-pass/deepseek-v4-flash", "claude-sonnet-4-6", "cline-pass/claude-sonnet-4-6"},
	})
	env := newServerEnv(t, channels, serverFriendsYAML)

	// carol may not use clinepass, which also serves the bare name.
	for range 6 {
		if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-flash"), bearer(carolKey)); recorder.Code != http.StatusForbidden {
			t.Fatalf("carol: %d %s", recorder.Code, recorder.Body.String())
		}
	}
	if hits := env.Hits(); len(hits) != 0 {
		t.Fatalf("carol reached upstream: %+v", hits)
	}
	// alice may use both channels.
	for _, model := range []string{"cline-pass/claude-sonnet-4-6", "deepseek-v4-flash"} {
		if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody(model), bearer(aliceKey)); recorder.Code != http.StatusOK {
			t.Fatalf("alice %s: %d %s", model, recorder.Code, recorder.Body.String())
		}
	}
	for _, hit := range env.Hits() {
		if hit.Channel != channelKey("clinepass") && hit.Channel != channelKey("deepseek") {
			t.Fatalf("alice traffic reached %s", hit.Channel)
		}
	}
	// The bare name is served by clinepass and the owner channel: denied.
	env.resetHits()
	if recorder := env.do(http.MethodPost, "/v1/chat/completions", chatBody("claude-sonnet-4-6"), bearer(aliceKey)); recorder.Code != http.StatusForbidden || len(env.Hits()) != 0 {
		t.Fatalf("alice bare claude: %d", recorder.Code)
	}
}

// Review PoC 2 (body tricks) end to end: nothing reaches upstream.
func TestIntegrationBodyTricks(t *testing.T) {
	env := newServerEnv(t, defaultChannels(), serverFriendsYAML)
	messages := `"messages":[{"role":"user","content":"hi"}]`
	for name, body := range map[string][]byte{
		"BOM duplicate":     append([]byte("\xEF\xBB\xBF"), []byte(`{"model":"deepseek-v4-flash","model":"claude-sonnet-4-6",`+messages+`}`)...),
		"garbage duplicate": []byte(`x{"model":"deepseek-v4-flash","model":"claude-sonnet-4-6",` + messages + `}`),
		"case variants":     []byte(`{"model":"deepseek-v4-flash","Model":"claude-sonnet-4-6","MODEL":"gpt-5.6",` + messages + `}`),
	} {
		if recorder := env.do(http.MethodPost, "/v1/chat/completions", body, bearer(aliceKey)); recorder.Code != http.StatusBadRequest {
			t.Fatalf("%s: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
	if hits := env.Hits(); len(hits) != 0 {
		t.Fatalf("body tricks reached upstream: %+v", hits)
	}
}

// Review PoC 3: friend-controlled model names must not grow usage storage.
func TestIntegrationUsageGrowth(t *testing.T) {
	env := newServerEnv(t, defaultChannels(), serverFriendsYAML)
	long := strings.Repeat("a", 64<<10)
	for i := range 50 {
		env.do(http.MethodPost, "/v1/chat/completions", chatBody("deepseek-v4-"+strconv.Itoa(i)+"-"+long), bearer(aliceKey))
	}
	if usage := env.rt.Usage().Report().Friends["alice"]; usage != nil {
		t.Fatalf("rejected requests created %d model buckets", len(usage.Models))
	}
	if errFlush := env.rt.Usage().Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	if _, errStat := os.Stat(filepath.Join(env.dir, UsageFileName)); !os.IsNotExist(errStat) {
		t.Fatalf("usage file written for rejected requests: %v", errStat)
	}
	if hits := env.Hits(); len(hits) != 0 {
		t.Fatalf("rejected requests reached upstream: %d", len(hits))
	}
}
