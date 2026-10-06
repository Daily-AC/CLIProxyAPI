package z10

import (
	"encoding/base64"
	"fmt"
	"io"
	"io/fs"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
	"golang.org/x/crypto/bcrypt"
)

// adminConfigYAML has enabled, disabled and model-less openai-compatibility channels
// plus non-compat providers, which must never be offered to friends. The management
// key is stored as a min-cost bcrypt hash so the many admin requests stay fast.
var adminConfigYAML = strings.Replace(testConfigYAML, "secret-key: "+mgmtKey, "secret-key: \""+minCostHash(mgmtKey)+"\"", 1) + `openai-compatibility:
  - name: DeepSeek
    base-url: https://deepseek.invalid/v1
  - name: cline-pass
    base-url: https://cline.invalid/v1
  - name: Retired
    disabled: true
    base-url: https://retired.invalid/v1
  - name: Empty
    base-url: https://empty.invalid/v1
claude-api-key:
  - api-key: claude-upstream-0123456789
codex-api-key:
  - api-key: codex-upstream-0123456789
`

func minCostHash(secret string) string {
	hash, errHash := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.MinCost)
	if errHash != nil {
		panic(errHash)
	}
	return string(hash)
}

// adminDo sends an admin request from localhost. A body implies a JSON Content-Type;
// a header value of "" removes that header.
func adminDo(engine *gin.Engine, method, path, body string, headers map[string]string) *httptest.ResponseRecorder {
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	req.RemoteAddr = "127.0.0.1:40000"
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for key, value := range headers {
		if value == "" {
			req.Header.Del(key)
			continue
		}
		req.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	return recorder
}

func withHeaders(base map[string]string, extra map[string]string) map[string]string {
	out := map[string]string{}
	for key, value := range base {
		out[key] = value
	}
	for key, value := range extra {
		out[key] = value
	}
	return out
}

func newAdminRuntime(t *testing.T, friendsYAML string, mutate ...func(*Options)) (*Runtime, *gin.Engine) {
	t.Helper()
	rt := newTestRuntime(t, adminConfigYAML, friendsYAML, newFakeClock(testNow()), mutate...)
	return rt, newAdminEngine(rt)
}

func readFile(t *testing.T, path string) string {
	t.Helper()
	data, errRead := os.ReadFile(path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	return string(data)
}

// fileFriends parses friends.yaml from disk with the loader.
func fileFriends(t *testing.T, rt *Runtime) *FriendSet {
	t.Helper()
	set, errParse := ParseFriends([]byte(readFile(t, rt.friendsPath)))
	if errParse != nil {
		t.Fatalf("friends.yaml on disk is invalid: %v", errParse)
	}
	return set
}

func friendNames(set *FriendSet) string {
	var names []string
	for _, friend := range set.Friends() {
		names = append(names, friend.Name)
	}
	return strings.Join(names, ",")
}

func requireAdminError(t *testing.T, recorder *httptest.ResponseRecorder, status int, contains string) {
	t.Helper()
	body := recorder.Body.String()
	if recorder.Code != status {
		t.Fatalf("status = %d, want %d: %s", recorder.Code, status, body)
	}
	message := gjson.Get(body, "error")
	if message.Type != gjson.String || !strings.Contains(message.String(), contains) || len(gjson.Parse(body).Map()) != 1 {
		t.Fatalf("error body = %s, want {\"error\": ...%q...}", body, contains)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("error response without Cache-Control: no-store")
	}
}

// logCapture records every logrus entry (message and fields) while installed.
type logCapture struct {
	mu      sync.Mutex
	builder strings.Builder
}

func (c *logCapture) Levels() []log.Level { return log.AllLevels }

func (c *logCapture) Fire(entry *log.Entry) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	fmt.Fprintf(&c.builder, "%s %v\n", entry.Message, entry.Data)
	return nil
}

func (c *logCapture) String() string {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.builder.String()
}

func captureLogs(t *testing.T) *logCapture {
	t.Helper()
	capture := &logCapture{}
	logger := log.StandardLogger()
	previousLevel, previousOut := logger.GetLevel(), logger.Out
	previousHooks := logger.ReplaceHooks(log.LevelHooks{})
	logger.AddHook(capture)
	logger.SetLevel(log.TraceLevel)
	logger.SetOutput(io.Discard)
	t.Cleanup(func() {
		logger.ReplaceHooks(previousHooks)
		logger.SetLevel(previousLevel)
		logger.SetOutput(previousOut)
	})
	return capture
}

func TestAdminChannels(t *testing.T) {
	_, engine := newAdminRuntime(t, testFriendsYAML)
	registerDefaultModels(t)
	registerTestModels(t, "openai-compatible-retired", "retired-model")

	recorder := adminDo(engine, http.MethodGet, "/z10/channels", "", bearer(mgmtKey))
	want := `{"channels":[` +
		`{"name":"DeepSeek","models":["deepseek-v4-flash","deepseek-v4-pro"]},` +
		`{"name":"cline-pass","models":["cline-pass/anthropic/claude-sonnet-4-6"]},` +
		`{"name":"Empty","models":[]}]}`
	if recorder.Code != http.StatusOK || recorder.Body.String() != want {
		t.Fatalf("/z10/channels = %d %s\nwant %s", recorder.Code, recorder.Body.String(), want)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing Cache-Control: no-store")
	}
}

func TestAdminCreateFriend(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)

	recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"dave","channels":["deepseek"]}`, bearer(mgmtKey))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	body := recorder.Body.String()
	key := gjson.Get(body, "key").String()
	raw, errDecode := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(key, friendKeyPrefix))
	if !strings.HasPrefix(key, friendKeyPrefix) || errDecode != nil || len(raw) != 32 {
		t.Fatalf("generated key has the wrong shape (len %d, err %v)", len(key), errDecode)
	}
	friend := gjson.Get(body, "friend")
	if friend.Get("name").String() != "dave" || friend.Get("models").Raw != `["*"]` || friend.Get("channels").Raw != `["DeepSeek"]` ||
		!friend.Get("enabled").Bool() || !friend.Get("active").Bool() || friend.Get("inactive_reason").String() != "" ||
		!friend.Get("inactive_reason").Exists() || friend.Get("expires").Exists() || friend.Get("requests").Int() != 0 ||
		!friend.Get("total_tokens").Exists() || len(gjson.Parse(body).Map()) != 2 {
		t.Fatalf("create response = %s", body)
	}
	if recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("missing Cache-Control: no-store")
	}

	// Active immediately, without waiting for the watcher.
	if active := rt.Friends().ByName("dave"); active == nil || active.key != key {
		t.Fatal("created friend is not active in memory")
	}
	if matched, _ := rt.Friends().match(httptestRequestWithKey(key)); matched == nil || matched.Name != "dave" {
		t.Fatal("created key is not recognized")
	}
	onDisk := fileFriends(t, rt)
	if friendNames(onDisk) != "alice,bob,dave" || onDisk.ByName("dave").key != key || onDisk.ByName("alice").key != aliceKey || onDisk.ByName("bob").key != bobKey {
		t.Fatalf("friends.yaml = %s", readFile(t, rt.friendsPath))
	}
	if info, errStat := os.Stat(rt.friendsPath); errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("friends.yaml mode = %v %v", info.Mode(), errStat)
	}

	// Unicode names, explicit options.
	recorder = adminDo(engine, http.MethodPost, "/z10/friends",
		`{"name":"小明","channels":["DeepSeek","cline-pass","DEEPSEEK"],"models":[" deepseek-v4-* "],"expires":"2026-12-31","enabled":false}`, bearer(mgmtKey))
	friend = gjson.Get(recorder.Body.String(), "friend")
	if recorder.Code != http.StatusCreated || friend.Get("name").String() != "小明" || friend.Get("channels").Raw != `["DeepSeek","cline-pass"]` ||
		friend.Get("models").Raw != `["deepseek-v4-*"]` || friend.Get("expires").String() != "2026-12-31" ||
		friend.Get("enabled").Bool() || friend.Get("active").Bool() || friend.Get("inactive_reason").String() != "disabled" {
		t.Fatalf("create 小明: %d %s", recorder.Code, recorder.Body.String())
	}
	if gjson.Get(recorder.Body.String(), "key").String() == key {
		t.Fatal("keys must be unique")
	}
	recorder = adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"`+strings.Repeat("n", 32)+`","channels":["Empty"]}`, bearer(mgmtKey))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("32-character name: %d %s", recorder.Code, recorder.Body.String())
	}
	if friendNames(fileFriends(t, rt)) != "alice,bob,dave,小明,"+strings.Repeat("n", 32) {
		t.Fatalf("order not kept: %s", friendNames(fileFriends(t, rt)))
	}
}

func TestAdminCreateFriendValidation(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	before := readFile(t, rt.friendsPath)
	cases := []struct {
		name    string
		body    string
		headers map[string]string
		status  int
		message string
	}{
		{"space in name", `{"name":"al ice","channels":["DeepSeek"]}`, nil, 400, "name must be"},
		{"long name", `{"name":"` + strings.Repeat("a", 33) + `","channels":["DeepSeek"]}`, nil, 400, "name must be"},
		{"dots-only name", `{"name":"..","channels":["DeepSeek"]}`, nil, 400, "name must be"},
		{"slash in name", `{"name":"a/b","channels":["DeepSeek"]}`, nil, 400, "name must be"},
		{"missing name", `{"channels":["DeepSeek"]}`, nil, 400, "name must be"},
		{"duplicate name", `{"name":"alice","channels":["DeepSeek"]}`, nil, 409, `friend "alice" already exists`},
		{"unknown channel", `{"name":"x","channels":["DeepSeek","Nope"]}`, nil, 400, `channel "Nope" is not an enabled openai-compatibility channel`},
		{"disabled channel", `{"name":"x","channels":["Retired"]}`, nil, 400, `channel "Retired"`},
		{"non-compat channel", `{"name":"x","channels":["claude"]}`, nil, 400, `channel "claude"`},
		{"blank channel", `{"name":"x","channels":[" "]}`, nil, 400, `channel " "`},
		{"missing channels", `{"name":"x"}`, nil, 400, "channels must not be empty"},
		{"empty channels", `{"name":"x","channels":[]}`, nil, 400, "channels must not be empty"},
		{"empty models", `{"name":"x","channels":["DeepSeek"],"models":[]}`, nil, 400, "models must not be empty"},
		{"blank model", `{"name":"x","channels":["DeepSeek"],"models":["deepseek-*"," "]}`, nil, 400, "empty pattern"},
		{"bad expires", `{"name":"x","channels":["DeepSeek"],"expires":"next-week"}`, nil, 400, "YYYY-MM-DD or RFC3339"},
		{"client-chosen key", `{"name":"x","channels":["DeepSeek"],"key":"sk-z10-chosen-0123456789"}`, nil, 400, `unknown field "key"`},
		{"wrong type", `{"name":"x","channels":"DeepSeek"}`, nil, 400, "invalid request body"},
		{"array body", `[{"name":"x","channels":["DeepSeek"]}]`, nil, 400, "JSON object"},
		{"null body", `null`, nil, 400, "JSON object"},
		{"trailing data", `{"name":"x","channels":["DeepSeek"]} {}`, nil, 400, "single JSON object"},
		{"syntax error", `{"name":"x",`, nil, 400, "invalid request body"},
		{"text/plain", `{"name":"x","channels":["DeepSeek"]}`, map[string]string{"Content-Type": "text/plain"}, 415, "application/json"},
		{"form", `name=x`, map[string]string{"Content-Type": "application/x-www-form-urlencoded"}, 415, "application/json"},
		{"no content type", `{"name":"x","channels":["DeepSeek"]}`, map[string]string{"Content-Type": ""}, 415, "application/json"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			recorder := adminDo(engine, http.MethodPost, "/z10/friends", tc.body, withHeaders(bearer(mgmtKey), tc.headers))
			requireAdminError(t, recorder, tc.status, tc.message)
		})
	}
	if readFile(t, rt.friendsPath) != before || friendNames(rt.Friends()) != "alice,bob" {
		t.Fatal("rejected requests must not change friends.yaml or the active keys")
	}
	// A JSON media type with parameters is accepted.
	recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"x","channels":["DeepSeek"]}`,
		withHeaders(bearer(mgmtKey), map[string]string{"Content-Type": "application/json; charset=utf-8"}))
	if recorder.Code != http.StatusCreated {
		t.Fatalf("charset parameter: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAdminUpdateFriend(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	var responses []string
	patch := func(name, body string) *httptest.ResponseRecorder {
		recorder := adminDo(engine, http.MethodPatch, "/z10/friends/"+name, body, bearer(mgmtKey))
		responses = append(responses, recorder.Body.String())
		return recorder
	}

	recorder := patch("alice", `{"enabled":false}`)
	friend := gjson.Get(recorder.Body.String(), "friend")
	if recorder.Code != http.StatusOK || friend.Get("enabled").Bool() || friend.Get("active").Bool() || friend.Get("inactive_reason").String() != "disabled" {
		t.Fatalf("disable: %d %s", recorder.Code, recorder.Body.String())
	}
	if rt.Friends().ByName("alice").inactiveReason(testNow()) != "API key disabled" {
		t.Fatal("disable must apply immediately")
	}

	// testNow is 2026-10-05; a date-only expiry is valid through the end of that day UTC+8.
	recorder = patch("alice", `{"enabled":true,"expires":"2026-10-04"}`)
	friend = gjson.Get(recorder.Body.String(), "friend")
	if recorder.Code != http.StatusOK || friend.Get("expires").String() != "2026-10-04" || friend.Get("inactive_reason").String() != "expired" || !friend.Get("enabled").Bool() {
		t.Fatalf("expire: %d %s", recorder.Code, recorder.Body.String())
	}

	recorder = patch("alice", `{"expires":""}`)
	friend = gjson.Get(recorder.Body.String(), "friend")
	if recorder.Code != http.StatusOK || friend.Get("expires").Exists() || !friend.Get("active").Bool() {
		t.Fatalf("clear expires: %d %s", recorder.Code, recorder.Body.String())
	}
	if rt.Friends().ByName("alice").ExpiresAt != (time.Time{}) {
		t.Fatal("cleared expiry must apply immediately")
	}

	recorder = patch("alice", `{"channels":["CLINE-PASS"],"models":["cline-pass/*"]}`)
	friend = gjson.Get(recorder.Body.String(), "friend")
	if recorder.Code != http.StatusOK || friend.Get("channels").Raw != `["cline-pass"]` || friend.Get("models").Raw != `["cline-pass/*"]` || !friend.Get("active").Bool() {
		t.Fatalf("channels/models: %d %s", recorder.Code, recorder.Body.String())
	}
	if alice := rt.Friends().ByName("alice"); !alice.matchesPattern("cline-pass/x") || alice.matchesPattern("deepseek-v4-flash") {
		t.Fatal("new models must apply immediately")
	}

	if recorder = patch("alice", `{}`); recorder.Code != http.StatusOK || gjson.Get(recorder.Body.String(), "friend.name").String() != "alice" {
		t.Fatalf("empty patch: %d %s", recorder.Code, recorder.Body.String())
	}

	for _, body := range responses {
		if strings.Contains(body, aliceKey) {
			t.Fatal("PATCH response leaks the key")
		}
	}
	onDisk := fileFriends(t, rt)
	if friendNames(onDisk) != "alice,bob" || onDisk.ByName("alice").key != aliceKey || rt.Friends().ByName("alice").key != aliceKey {
		t.Fatal("PATCH must keep keys and order")
	}

	before := readFile(t, rt.friendsPath)
	for _, tc := range []struct {
		name, path, body string
		headers          map[string]string
		status           int
		message          string
	}{
		{"unknown friend", "nobody", `{"enabled":false}`, nil, 404, `friend "nobody" not found`},
		{"rename", "alice", `{"name":"alicia"}`, nil, 400, `unknown field "name"`},
		{"key change", "alice", `{"key":"sk-z10-chosen-0123456789"}`, nil, 400, `unknown field "key"`},
		{"empty channels", "alice", `{"channels":[]}`, nil, 400, "channels must not be empty"},
		{"disabled channel", "alice", `{"channels":["Retired"]}`, nil, 400, `channel "Retired"`},
		{"empty models", "alice", `{"models":[]}`, nil, 400, "models must not be empty"},
		{"bad expires", "alice", `{"expires":"soon"}`, nil, 400, "YYYY-MM-DD or RFC3339"},
		{"wrong type", "alice", `{"enabled":"yes"}`, nil, 400, "invalid request body"},
		{"text/plain", "alice", `{"enabled":false}`, map[string]string{"Content-Type": "text/plain"}, 415, "application/json"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			recorder := adminDo(engine, http.MethodPatch, "/z10/friends/"+tc.path, tc.body, withHeaders(bearer(mgmtKey), tc.headers))
			requireAdminError(t, recorder, tc.status, tc.message)
		})
	}
	if readFile(t, rt.friendsPath) != before {
		t.Fatal("rejected PATCH requests must not change friends.yaml")
	}
}

func TestAdminDeleteFriend(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	jsonType := map[string]string{"Content-Type": "application/json"}

	recorder := adminDo(engine, http.MethodDelete, "/z10/friends/bob", "", withHeaders(bearer(mgmtKey), jsonType))
	if recorder.Code != http.StatusNoContent || recorder.Body.Len() != 0 || recorder.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("delete: %d %q", recorder.Code, recorder.Body.String())
	}
	if rt.Friends().ByName("bob") != nil || friendNames(fileFriends(t, rt)) != "alice" || fileFriends(t, rt).ByName("alice").key != aliceKey {
		t.Fatal("bob must be removed from memory and friends.yaml, alice kept")
	}
	requireAdminError(t, adminDo(engine, http.MethodDelete, "/z10/friends/bob", "", withHeaders(bearer(mgmtKey), jsonType)), 404, `friend "bob" not found`)
	requireAdminError(t, adminDo(engine, http.MethodDelete, "/z10/friends/alice", "", withHeaders(bearer(mgmtKey), map[string]string{"Content-Type": "text/plain"})), 415, "application/json")
	if rt.Friends().ByName("alice") == nil {
		t.Fatal("a rejected DELETE must not remove the friend")
	}

	// A body-less DELETE may omit Content-Type. The file stays, so the feature stays on.
	if recorder = adminDo(engine, http.MethodDelete, "/z10/friends/alice", "", bearer(mgmtKey)); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete without Content-Type: %d %s", recorder.Code, recorder.Body.String())
	}
	if rt.Friends().Len() != 0 || fileFriends(t, rt).Len() != 0 {
		t.Fatal("all friends must be gone")
	}
}

func TestAdminFriendsListIncludesUsageTotals(t *testing.T) {
	clock := newFakeClock(testNow())
	rt := newTestRuntime(t, adminConfigYAML, testFriendsYAML, clock)
	engine := newAdminEngine(rt)
	rt.Usage().RecordRequest("alice", "deepseek-v4-flash", false)
	rt.Usage().RecordRequest("alice", "deepseek-v4-flash", true)
	rt.Usage().RecordTokens("alice", "deepseek-v4-flash", coreusage.Detail{InputTokens: 10, OutputTokens: 5, TotalTokens: 15})

	body := adminDo(engine, http.MethodGet, "/z10/friends", "", bearer(mgmtKey)).Body.String()
	alice, bob := gjson.Get(body, "friends.0"), gjson.Get(body, "friends.1")
	if alice.Get("requests").Int() != 2 || alice.Get("failed_requests").Int() != 1 || alice.Get("total_tokens").Int() != 15 ||
		!alice.Get("active").Bool() || alice.Get("inactive_reason").String() != "" || !alice.Get("last_used").Exists() {
		t.Fatalf("alice = %s", alice.Raw)
	}
	var bobFields []string
	bob.ForEach(func(key, _ gjson.Result) bool {
		bobFields = append(bobFields, key.String())
		return true
	})
	if strings.Join(bobFields, ",") != "name,models,channels,enabled,active,inactive_reason,requests,failed_requests,total_tokens" ||
		bob.Get("inactive_reason").String() != "disabled" || bob.Get("requests").Int() != 0 {
		t.Fatalf("bob = %s", bob.Raw)
	}

	clock.Set(time.Date(2027, 1, 2, 0, 0, 0, 0, time.UTC))
	body = adminDo(engine, http.MethodGet, "/z10/friends", "", bearer(mgmtKey)).Body.String()
	if gjson.Get(body, "friends.0.inactive_reason").String() != "expired" || gjson.Get(body, "friends.0.active").Bool() {
		t.Fatalf("expired alice = %s", gjson.Get(body, "friends.0").Raw)
	}
}

func TestAdminWriteRoutesRequireManagementKey(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	const remote = "203.0.113.9:40000"
	jsonType := map[string]string{"Content-Type": "application/json"}
	routes := []struct {
		method, path, body string
		ok                 int
	}{
		{http.MethodGet, "/z10/channels", "", http.StatusOK},
		{http.MethodPost, "/z10/friends", `{"name":"frank","channels":["DeepSeek"]}`, http.StatusCreated},
		{http.MethodPatch, "/z10/friends/frank", `{"enabled":false}`, http.StatusOK},
		{http.MethodDelete, "/z10/friends/frank", "", http.StatusNoContent},
	}
	for _, route := range routes {
		before := readFile(t, rt.friendsPath)
		for _, headers := range []map[string]string{jsonType, withHeaders(jsonType, bearer("wrong-key"))} {
			if recorder := adminDo(engine, route.method, route.path, route.body, headers); recorder.Code != http.StatusUnauthorized {
				t.Fatalf("%s %s without a valid key: %d", route.method, route.path, recorder.Code)
			}
		}
		if recorder := adminDo(engine, route.method, route.path, route.body, withHeaders(jsonType, bearer(aliceKey))); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s with a friend key: %d", route.method, route.path, recorder.Code)
		}
		req := httptest.NewRequest(route.method, route.path, strings.NewReader(route.body))
		req.RemoteAddr = remote
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+mgmtKey)
		remoteRecorder := httptest.NewRecorder()
		engine.ServeHTTP(remoteRecorder, req)
		if remoteRecorder.Code != http.StatusForbidden {
			t.Fatalf("%s %s remote without allow-remote: %d", route.method, route.path, remoteRecorder.Code)
		}
		if readFile(t, rt.friendsPath) != before {
			t.Fatalf("unauthorized %s %s changed friends.yaml", route.method, route.path)
		}
		// A valid request also resets the failed-attempt counter of the management handler.
		if recorder := adminDo(engine, route.method, route.path, route.body, withHeaders(jsonType, bearer(mgmtKey))); recorder.Code != route.ok {
			t.Fatalf("%s %s with the management key: %d %s", route.method, route.path, recorder.Code, recorder.Body.String())
		}
	}
}

func TestAdminKeyOnlyInCreateResponse(t *testing.T) {
	logs := captureLogs(t)
	rt, engine := newAdminRuntime(t, testFriendsYAML)

	created := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"erin","channels":["DeepSeek"]}`, bearer(mgmtKey))
	key := gjson.Get(created.Body.String(), "key").String()
	if created.Code != http.StatusCreated || !strings.HasPrefix(key, friendKeyPrefix) {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	rt.Usage().RecordRequest("erin", "deepseek-v4-flash", false)

	var responses []string
	for _, step := range []struct{ method, path, body string }{
		{http.MethodGet, "/z10/friends", ""},
		{http.MethodPatch, "/z10/friends/erin", `{"enabled":false}`},
		{http.MethodGet, "/z10/usage", ""},
		{http.MethodGet, "/z10/channels", ""},
		{http.MethodPost, "/z10/friends", `{"name":"erin","channels":["DeepSeek"]}`},
		{http.MethodDelete, "/z10/friends/erin", ""},
	} {
		recorder := adminDo(engine, step.method, step.path, step.body, bearer(mgmtKey))
		responses = append(responses, recorder.Body.String()+fmt.Sprint(recorder.Header()))
	}
	for _, secret := range []string{key, aliceKey, bobKey} {
		for index, response := range responses {
			if strings.Contains(response, secret) {
				t.Fatalf("response %d leaks a key: %s", index, response)
			}
		}
		if strings.Contains(logs.String(), secret) {
			t.Fatalf("logs leak a key:\n%s", logs.String())
		}
	}
	for _, want := range []string{"z10: friend created map[name:erin]", "z10: friend updated map[name:erin]", "z10: friend deleted map[name:erin]"} {
		if !strings.Contains(logs.String(), want) {
			t.Fatalf("logs lack %q:\n%s", want, logs.String())
		}
	}
}

func TestAdminConcurrentCreates(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	const count = 16
	codes := make([]int, count)
	keys := make([]string, count)
	var wg sync.WaitGroup
	for i := range count {
		wg.Add(1)
		go func() {
			defer wg.Done()
			recorder := adminDo(engine, http.MethodPost, "/z10/friends", fmt.Sprintf(`{"name":"friend-%02d","channels":["DeepSeek"]}`, i), bearer(mgmtKey))
			codes[i] = recorder.Code
			keys[i] = gjson.Get(recorder.Body.String(), "key").String()
		}()
	}
	wg.Wait()
	onDisk := fileFriends(t, rt)
	if rt.Friends().Len() != 2+count || onDisk.Len() != 2+count {
		t.Fatalf("friends: memory %d, file %d, want %d", rt.Friends().Len(), onDisk.Len(), 2+count)
	}
	for i := range count {
		name := fmt.Sprintf("friend-%02d", i)
		if codes[i] != http.StatusCreated || onDisk.ByName(name) == nil || onDisk.ByName(name).key != keys[i] || rt.Friends().ByName(name).key != keys[i] {
			t.Fatalf("%s: status %d, entry lost or key mismatch", name, codes[i])
		}
	}

	// Concurrent creates of one name: exactly one wins.
	statuses := make(chan int, 8)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			statuses <- adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"same","channels":["DeepSeek"]}`, bearer(mgmtKey)).Code
		}()
	}
	wg.Wait()
	close(statuses)
	counts := map[int]int{}
	for status := range statuses {
		counts[status]++
	}
	if counts[http.StatusCreated] != 1 || counts[http.StatusConflict] != 7 {
		t.Fatalf("same-name statuses = %v", counts)
	}
}

func TestAdminWriteReplacesFileAtomically(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	dir := filepath.Dir(rt.friendsPath)
	infoBefore, errBefore := os.Stat(rt.friendsPath)
	if errBefore != nil {
		t.Fatal(errBefore)
	}
	if recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"gus","channels":["DeepSeek"]}`, bearer(mgmtKey)); recorder.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	infoAfter, errAfter := os.Stat(rt.friendsPath)
	if errAfter != nil {
		t.Fatal(errAfter)
	}
	if os.SameFile(infoBefore, infoAfter) {
		t.Fatal("friends.yaml must be replaced by rename, not rewritten in place")
	}
	if infoAfter.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %v", infoAfter.Mode())
	}
	entries, errList := os.ReadDir(dir)
	if errList != nil {
		t.Fatal(errList)
	}
	for _, entry := range entries {
		if entry.Name() != "config.yaml" && entry.Name() != FriendsFileName {
			t.Fatalf("unexpected file left behind: %s", entry.Name())
		}
	}
	if !strings.HasPrefix(readFile(t, rt.friendsPath), friendsFileHeader) {
		t.Fatal("friends.yaml lacks the managed-file header")
	}

	// A failed write changes nothing, on disk or in memory.
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions")
	}
	before := readFile(t, rt.friendsPath)
	if errChmod := os.Chmod(dir, 0o500); errChmod != nil {
		t.Fatal(errChmod)
	}
	t.Cleanup(func() { _ = os.Chmod(dir, 0o700) })
	recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"hal","channels":["DeepSeek"]}`, bearer(mgmtKey))
	_ = os.Chmod(dir, 0o700)
	requireAdminError(t, recorder, http.StatusInternalServerError, "failed to write friends.yaml")
	if readFile(t, rt.friendsPath) != before || rt.Friends().ByName("hal") != nil {
		t.Fatal("a failed write must not change friends.yaml or the active keys")
	}
}

func TestAdminCreateTurnsFeatureOn(t *testing.T) {
	rt, engine := newAdminRuntime(t, "")
	if _, errStat := os.Stat(rt.friendsPath); !os.IsNotExist(errStat) {
		t.Fatalf("friends.yaml must not exist yet: %v", errStat)
	}
	if body := adminDo(engine, http.MethodGet, "/z10/friends", "", bearer(mgmtKey)).Body.String(); body != `{"friends":[]}` {
		t.Fatalf("empty list = %s", body)
	}
	if recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"ida","channels":["DeepSeek"]}`, bearer(mgmtKey)); recorder.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	info, errStat := os.Stat(rt.friendsPath)
	if errStat != nil || info.Mode().Perm() != 0o600 || rt.Friends().ByName("ida") == nil {
		t.Fatalf("friends.yaml after create: %v %v", info, errStat)
	}
}

// The admin API edits the keys in effect: when friends.yaml was broken by hand, the last
// good keys plus the change are written back.
func TestAdminWriteUsesActiveKeysWhenFileIsInvalid(t *testing.T) {
	rt, engine := newAdminRuntime(t, testFriendsYAML)
	writeTestFile(t, rt.friendsPath, "keys: [")
	if errReload := rt.Reload(); errReload == nil {
		t.Fatal("expected the broken file to be rejected")
	}
	if recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"jo","channels":["DeepSeek"]}`, bearer(mgmtKey)); recorder.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	if onDisk := fileFriends(t, rt); friendNames(onDisk) != "alice,bob,jo" || onDisk.ByName("alice").key != aliceKey {
		t.Fatalf("friends.yaml = %s", readFile(t, rt.friendsPath))
	}
}

// The watcher sees the admin write as a normal change; reloading it changes nothing.
func TestAdminWriteThenWatcherReload(t *testing.T) {
	reloads := make(chan error, 16)
	rt, engine := newAdminRuntime(t, testFriendsYAML, func(o *Options) {
		o.ReloadDebounce = 10 * time.Millisecond
		o.OnWatchReload = func(err error) { reloads <- err }
	})
	rt.Start()
	recorder := adminDo(engine, http.MethodPost, "/z10/friends", `{"name":"kim","channels":["DeepSeek"]}`, bearer(mgmtKey))
	key := gjson.Get(recorder.Body.String(), "key").String()
	if recorder.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", recorder.Code, recorder.Body.String())
	}
	select {
	case errReload := <-reloads:
		if errReload != nil {
			t.Fatalf("watch reload after admin write: %v", errReload)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the admin write did not trigger a watcher reload")
	}
	if friendNames(rt.Friends()) != "alice,bob,kim" || rt.Friends().ByName("kim").key != key || rt.Friends().ByName("alice").key != aliceKey {
		t.Fatalf("friends after watcher reload = %s", friendNames(rt.Friends()))
	}
}

// TestIntegrationAdminFriendLifecycle drives the admin API through the real router: a
// created key works at once, PATCH disables it, DELETE revokes it, and the request log
// (enabled here) never records the key.
func TestIntegrationAdminFriendLifecycle(t *testing.T) {
	env := newServerEnv(t, defaultChannels(), "", func(cfg *config.Config) { cfg.RequestLog = true })
	mgmt := bearer(mgmtKey)

	channels := env.do(http.MethodGet, "/z10/channels", nil, mgmt)
	if channels.Code != http.StatusOK || gjson.Get(channels.Body.String(), "channels.0.name").String() != "deepseek" ||
		gjson.Get(channels.Body.String(), "channels.0.models").Raw != `["deepseek-v4-flash","deepseek-v4-pro"]` {
		t.Fatalf("/z10/channels: %d %s", channels.Code, channels.Body.String())
	}

	created := env.do(http.MethodPost, "/z10/friends", []byte(`{"name":"ivy","channels":["deepseek"],"models":["deepseek-v4-*"]}`), mgmt)
	key := gjson.Get(created.Body.String(), "key").String()
	if created.Code != http.StatusCreated || key == "" {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	chat := func(model string) int {
		return env.do(http.MethodPost, "/v1/chat/completions", chatBody(model), bearer(key)).Code
	}
	if code := chat("deepseek-v4-flash"); code != http.StatusOK {
		t.Fatalf("new key: %d", code)
	}
	if hits := env.Hits(); len(hits) != 1 || hits[0].Channel != channelKey("deepseek") {
		t.Fatalf("upstream hits = %+v", hits)
	}
	if code := chat("claude-sonnet-4-6"); code != http.StatusForbidden {
		t.Fatalf("new key, other channel: %d", code)
	}

	if recorder := env.do(http.MethodPatch, "/z10/friends/ivy", []byte(`{"enabled":false}`), mgmt); recorder.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", recorder.Code, recorder.Body.String())
	}
	if code := chat("deepseek-v4-flash"); code != http.StatusUnauthorized {
		t.Fatalf("disabled key: %d", code)
	}
	if recorder := env.do(http.MethodPatch, "/z10/friends/ivy", []byte(`{"enabled":true}`), mgmt); recorder.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", recorder.Code, recorder.Body.String())
	}
	if code := chat("deepseek-v4-flash"); code != http.StatusOK {
		t.Fatalf("re-enabled key: %d", code)
	}
	list := env.do(http.MethodGet, "/z10/friends", nil, mgmt).Body.String()
	// Requests rejected by the gate (other channel, disabled key) are not counted.
	if gjson.Get(list, "friends.0.requests").Int() != 2 || gjson.Get(list, "friends.0.failed_requests").Int() != 0 {
		t.Fatalf("/z10/friends usage = %s", list)
	}

	// Unicode names round-trip through the URL path.
	if recorder := env.do(http.MethodPost, "/z10/friends", []byte(`{"name":"小明","channels":["deepseek"]}`), mgmt); recorder.Code != http.StatusCreated {
		t.Fatalf("create 小明: %d %s", recorder.Code, recorder.Body.String())
	}
	if recorder := env.do(http.MethodPatch, "/z10/friends/%E5%B0%8F%E6%98%8E", []byte(`{"enabled":false}`), mgmt); recorder.Code != http.StatusOK {
		t.Fatalf("patch 小明: %d %s", recorder.Code, recorder.Body.String())
	}

	if recorder := env.do(http.MethodDelete, "/z10/friends/ivy", nil, mgmt); recorder.Code != http.StatusNoContent {
		t.Fatalf("delete: %d %s", recorder.Code, recorder.Body.String())
	}
	if code := chat("deepseek-v4-flash"); code != http.StatusUnauthorized {
		t.Fatalf("deleted key: %d", code)
	}

	// The key is gone from friends.yaml, and no other file (request logs, usage) ever had it.
	var logged []string
	errWalk := filepath.WalkDir(env.dir, func(path string, entry fs.DirEntry, errEntry error) error {
		if errEntry != nil || entry.IsDir() {
			return errEntry
		}
		content := readFile(t, path)
		if strings.Contains(content, key) {
			t.Errorf("%s contains the friend key", path)
		}
		if strings.Contains(content, "/z10/friends") {
			logged = append(logged, content)
		}
		return nil
	})
	if errWalk != nil {
		t.Fatal(errWalk)
	}
	// Control: the request log did record the create call, with the key redacted.
	if !strings.Contains(strings.Join(logged, "\n"), `"key":"<redacted>"`) {
		t.Fatalf("request log of the create call not found (%d /z10/friends logs)", len(logged))
	}
}
