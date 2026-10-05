package z10

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestReloadKeepsLastGoodFriends(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	if rt.Friends().Len() != 2 {
		t.Fatalf("initial friends = %d", rt.Friends().Len())
	}

	friendsPath := filepath.Join(filepath.Dir(rt.configPath), FriendsFileName)
	writeTestFile(t, friendsPath, testFriendsYAML+"  - name: alice\n    key: sk-z10-other-0123456789abcdef\n    models: [\"x\"]\n")
	errReload := rt.Reload()
	if errReload == nil || !strings.Contains(errReload.Error(), "duplicate name") {
		t.Fatalf("Reload error = %v, want duplicate name", errReload)
	}
	if rt.Friends().Len() != 2 || rt.Friends().ByName("alice") == nil {
		t.Fatal("last good friends must be kept after a rejected file")
	}

	writeTestFile(t, friendsPath, "keys: [")
	if errSyntax := rt.Reload(); errSyntax == nil {
		t.Fatal("expected syntax error")
	}
	if rt.Friends().Len() != 2 {
		t.Fatal("last good friends must be kept after a syntax error")
	}

	if errRemove := os.Remove(friendsPath); errRemove != nil {
		t.Fatal(errRemove)
	}
	if errMissing := rt.Reload(); errMissing != nil {
		t.Fatalf("missing friends.yaml must not be an error: %v", errMissing)
	}
	if rt.Friends().Len() != 0 {
		t.Fatal("removing friends.yaml must disable all friend keys")
	}
}

func TestReloadDropsOnlyFriendsCollidingWithOwnerKeys(t *testing.T) {
	for name, configYAML := range map[string]string{
		"legacy layout": "api-keys:\n  - " + aliceKey + "\n",
		"v8 layout":     "access:\n  api-keys:\n    - " + aliceKey + "\n",
	} {
		t.Run(name, func(t *testing.T) {
			rt := newTestRuntime(t, configYAML, testFriendsYAML, newFakeClock(testNow()))
			if rt.Friends().ByName("alice") != nil || rt.Friends().ByName("bob") == nil {
				t.Fatal("only the colliding friend must be dropped")
			}
			errReload := rt.Reload()
			if errReload == nil || !strings.Contains(errReload.Error(), "alice") || strings.Contains(errReload.Error(), aliceKey) {
				t.Fatalf("Reload error = %v", errReload)
			}
		})
	}
}

func TestReloadOwnerCollisionAppliesToLastGoodFriends(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	dir := filepath.Dir(rt.configPath)
	// friends.yaml is broken, then an owner key equal to alice's key is added: the
	// last good list is kept, minus alice, so the owner key is not restricted.
	writeTestFile(t, filepath.Join(dir, FriendsFileName), "keys: [")
	writeTestFile(t, rt.configPath, testConfigYAML+"  # owner adds a key\n")
	writeTestFile(t, rt.configPath, "api-keys:\n  - "+ownerKey+"\n  - "+aliceKey+"\n")
	_ = rt.Reload()
	if rt.Friends().ByName("alice") != nil || rt.Friends().ByName("bob") == nil {
		t.Fatal("colliding friend must be dropped from the last good list")
	}
	if friend, _ := rt.Friends().match(httptestRequestWithKey(aliceKey)); friend != nil {
		t.Fatal("the owner key must no longer be treated as a friend key")
	}
	// Once the owner key is removed again, alice comes back from the last good file.
	writeTestFile(t, rt.configPath, testConfigYAML)
	_ = rt.Reload()
	if rt.Friends().ByName("alice") == nil {
		t.Fatal("alice must return when the collision is gone")
	}
}

func TestWatcherHotReloadWithRenameReplace(t *testing.T) {
	reloads := make(chan error, 16)
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()), func(o *Options) {
		o.ReloadDebounce = 10 * time.Millisecond
		o.OnWatchReload = func(err error) { reloads <- err }
	})
	rt.Start()
	if rt.Friends().ByName("alice").inactiveReason(testNow()) != "" {
		t.Fatal("alice must start active")
	}

	// Editors save by writing a temp file and renaming it over the original.
	dir := filepath.Dir(rt.configPath)
	temp := filepath.Join(dir, ".friends.yaml.swp")
	writeTestFile(t, temp, strings.Replace(testFriendsYAML, "expires: 2026-12-31", "expires: 2026-12-31\n    enabled: false", 1))
	if errRename := os.Rename(temp, filepath.Join(dir, FriendsFileName)); errRename != nil {
		t.Fatal(errRename)
	}

	deadline := time.After(10 * time.Second)
	for {
		select {
		case errReload := <-reloads:
			if errReload != nil {
				t.Fatalf("watch reload: %v", errReload)
			}
			if rt.Friends().ByName("alice").inactiveReason(testNow()) == "API key disabled" {
				return
			}
		case <-deadline:
			t.Fatal("friends.yaml change was not picked up")
		}
	}
}

func TestWatcherReloadsOnConfigChange(t *testing.T) {
	reloads := make(chan error, 16)
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()), func(o *Options) {
		o.ReloadDebounce = 10 * time.Millisecond
		o.OnWatchReload = func(err error) { reloads <- err }
	})
	rt.Start()
	writeTestFile(t, rt.configPath, testConfigYAML+"  # touched\n")
	deadline := time.After(10 * time.Second)
	select {
	case <-reloads:
	case <-deadline:
		t.Fatal("config.yaml change was not picked up")
	}
}

func httptestRequestWithKey(key string) *http.Request {
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil)
	req.Header.Set("Authorization", "Bearer "+key)
	return req
}
