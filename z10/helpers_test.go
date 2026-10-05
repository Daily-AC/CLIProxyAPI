package z10

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
)

const (
	ownerKey = "owner-key-0123456789abcdef"
	aliceKey = "sk-z10-alice-0123456789abcdef"
	bobKey   = "sk-z10-bob-0123456789abcdef"
	mgmtKey  = "mgmt-secret-value"
)

const testFriendsYAML = `keys:
  - name: alice
    key: ` + aliceKey + `
    models: ["deepseek-v4-*", "Cline-Pass/*"]
    channels: ["DeepSeek", "cline-pass"]
    expires: 2026-12-31
  - name: bob
    key: ` + bobKey + `
    models: ["deepseek-v4-flash"]
    channels: ["deepseek"]
    enabled: false
`

// Registry provider keys of the test channels (util.OpenAICompatibleProviderKey).
const (
	deepseekProvider  = "openai-compatible-deepseek"
	clinePassProvider = "openai-compatible-cline-pass"
)

const testConfigYAML = `api-keys:
  - ` + ownerKey + `
remote-management:
  allow-remote: false
  secret-key: ` + mgmtKey + `
`

// fakeClock is a controllable clock for expiry and day-bucket tests.
type fakeClock struct {
	mu  sync.Mutex
	now time.Time
}

func newFakeClock(now time.Time) *fakeClock { return &fakeClock{now: now} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeClock) Set(now time.Time) {
	c.mu.Lock()
	c.now = now
	c.mu.Unlock()
}

func writeTestFile(t *testing.T, path, content string) {
	t.Helper()
	if errWrite := os.WriteFile(path, []byte(content), 0o600); errWrite != nil {
		t.Fatalf("write %s: %v", path, errWrite)
	}
}

// newTestRuntime writes config.yaml and friends.yaml into a temp dir and loads them.
func newTestRuntime(t *testing.T, configYAML, friendsYAML string, clock *fakeClock, mutate ...func(*Options)) *Runtime {
	t.Helper()
	dir := t.TempDir()
	configPath := filepath.Join(dir, "config.yaml")
	writeTestFile(t, configPath, configYAML)
	if friendsYAML != "" {
		writeTestFile(t, filepath.Join(dir, FriendsFileName), friendsYAML)
	}
	opts := Options{ConfigPath: configPath, Now: clock.Now, FlushDelay: time.Hour}
	for _, fn := range mutate {
		fn(&opts)
	}
	rt := NewRuntime(opts)
	t.Cleanup(rt.Close)
	return rt
}

// registerTestModels registers models for provider in the global model registry.
func registerTestModels(t *testing.T, provider string, models ...string) {
	t.Helper()
	clientID := t.Name() + "|" + provider
	infos := make([]*registry.ModelInfo, 0, len(models))
	for _, model := range models {
		infos = append(infos, &registry.ModelInfo{ID: model, Object: "model", OwnedBy: provider})
	}
	registry.GetGlobalRegistry().RegisterClient(clientID, provider, infos)
	t.Cleanup(func() { registry.GetGlobalRegistry().UnregisterClient(clientID) })
}

// registerDefaultModels registers the friend channels and owner-only providers.
func registerDefaultModels(t *testing.T) {
	t.Helper()
	registerTestModels(t, deepseekProvider, "deepseek-v4-flash", "deepseek-v4-pro")
	registerTestModels(t, clinePassProvider, "cline-pass/anthropic/claude-sonnet-4-6")
	registerTestModels(t, "claude", "claude-sonnet-4-6")
	registerTestModels(t, "codex", "gpt-5.6")
	registerTestModels(t, "gemini", "gemini-3-flash")
}

func testNow() time.Time {
	return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
}
