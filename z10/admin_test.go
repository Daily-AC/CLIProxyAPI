package z10

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func adminRequest(engine *gin.Engine, path, remoteAddr string, headers map[string]string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.RemoteAddr = remoteAddr
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func newAdminEngine(rt *Runtime) *gin.Engine {
	gin.SetMode(gin.TestMode)
	engine := gin.New()
	engine.Use(rt.Middleware())
	rt.registerAdminRoutes(engine)
	return engine
}

func TestAdminRoutesRequireManagementKey(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	rt.Usage().RecordRequest("alice", "deepseek-v4-flash", false)
	engine := newAdminEngine(rt)
	const local, remote = "127.0.0.1:40000", "203.0.113.9:40000"

	for _, path := range []string{"/z10/usage", "/z10/friends"} {
		if recorder := adminRequest(engine, path, local, nil); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s without key: %d", path, recorder.Code)
		}
		if recorder := adminRequest(engine, path, local, bearer("wrong-key")); recorder.Code != http.StatusUnauthorized {
			t.Fatalf("%s wrong key: %d", path, recorder.Code)
		}
		// Plaintext secret-key in config.yaml is accepted like upstream (hashed in memory).
		ok := adminRequest(engine, path, local, bearer(mgmtKey))
		if ok.Code != http.StatusOK {
			t.Fatalf("%s with key: %d %s", path, ok.Code, ok.Body.String())
		}
		if alt := adminRequest(engine, path, local, map[string]string{"X-Management-Key": mgmtKey}); alt.Code != http.StatusOK {
			t.Fatalf("%s with X-Management-Key: %d", path, alt.Code)
		}
		for _, secret := range []string{aliceKey, bobKey, ownerKey, mgmtKey} {
			if strings.Contains(ok.Body.String(), secret) {
				t.Fatalf("%s leaks a secret: %s", path, ok.Body.String())
			}
		}
		if recorder := adminRequest(engine, path, remote, bearer(mgmtKey)); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s remote without allow-remote: %d", path, recorder.Code)
		}
		// Friend keys never reach the admin routes.
		if recorder := adminRequest(engine, path, local, bearer(aliceKey)); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s with friend key: %d", path, recorder.Code)
		}
	}

	friends := adminRequest(engine, "/z10/friends", local, bearer(mgmtKey)).Body.String()
	if gjson.Get(friends, "friends.#").Int() != 2 || gjson.Get(friends, "friends.0.name").String() != "alice" ||
		!gjson.Get(friends, "friends.0.active").Bool() || gjson.Get(friends, "friends.1.active").Bool() ||
		gjson.Get(friends, "friends.0.expires").String() != "2026-12-31" || !gjson.Get(friends, "friends.0.last_used").Exists() {
		t.Fatalf("friends view = %s", friends)
	}
	usage := adminRequest(engine, "/z10/usage", local, bearer(mgmtKey)).Body.String()
	if gjson.Get(usage, "friends.alice.requests").Int() != 1 {
		t.Fatalf("usage view = %s", usage)
	}

	// Management settings follow config.yaml reloads.
	writeTestFile(t, rt.configPath, strings.Replace(testConfigYAML, "allow-remote: false", "allow-remote: true", 1))
	if errReload := rt.Reload(); errReload != nil {
		t.Fatal(errReload)
	}
	if recorder := adminRequest(engine, "/z10/usage", remote, bearer(mgmtKey)); recorder.Code != http.StatusOK {
		t.Fatalf("remote after allow-remote: %d", recorder.Code)
	}
	writeTestFile(t, rt.configPath, "api-keys: ["+ownerKey+"]\nremote-management:\n  allow-remote: true\n  secret-key: rotated-secret-value\n")
	if errReload := rt.Reload(); errReload != nil {
		t.Fatal(errReload)
	}
	if recorder := adminRequest(engine, "/z10/usage", local, bearer(mgmtKey)); recorder.Code != http.StatusUnauthorized {
		t.Fatalf("old secret after rotation: %d", recorder.Code)
	}
	if recorder := adminRequest(engine, "/z10/usage", local, bearer("rotated-secret-value")); recorder.Code != http.StatusOK {
		t.Fatalf("new secret after rotation: %d", recorder.Code)
	}
}

func TestAdminRoutesWithoutManagementSecret(t *testing.T) {
	rt := newTestRuntime(t, "api-keys: ["+ownerKey+"]\n", testFriendsYAML, newFakeClock(testNow()))
	engine := newAdminEngine(rt)
	if recorder := adminRequest(engine, "/z10/usage", "127.0.0.1:1", bearer("anything")); recorder.Code != http.StatusForbidden {
		t.Fatalf("no management secret: %d", recorder.Code)
	}
}
