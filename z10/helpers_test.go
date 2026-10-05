package z10

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
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
    expires: 2026-12-31
  - name: bob
    key: ` + bobKey + `
    models: ["deepseek-v4-flash"]
    enabled: false
`

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

func testNow() time.Time {
	return time.Date(2026, 10, 5, 12, 0, 0, 0, time.UTC)
}
