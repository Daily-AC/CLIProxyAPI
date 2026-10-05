package z10

import (
	"bytes"
	"compress/gzip"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/klauspost/compress/zstd"
	configaccess "github.com/router-for-me/CLIProxyAPI/v8/internal/access/config_access"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/api"
	sdkaccess "github.com/router-for-me/CLIProxyAPI/v8/sdk/access"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
	sdkconfig "github.com/router-for-me/CLIProxyAPI/v8/sdk/config"
	"github.com/tidwall/gjson"
)

// echoResult is what the fake downstream handlers saw.
type echoResult struct {
	Model     string `json:"model"`
	BodySHA   string `json:"body_sha"`
	Principal string `json:"principal"`
}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// ownerProvider returns the built-in config API key provider for ownerKey.
func ownerProvider(t *testing.T) sdkaccess.Provider {
	t.Helper()
	configaccess.Register(&sdkconfig.SDKConfig{APIKeys: []string{ownerKey}})
	for _, provider := range sdkaccess.RegisteredProviders() {
		if provider.Identifier() == sdkaccess.DefaultAccessProviderName {
			return provider
		}
	}
	t.Fatal("config access provider not registered")
	return nil
}

type testEngine struct {
	engine *gin.Engine
	calls  int
}

// newMiddlewareEngine mirrors the upstream layout: the friend middleware is global and
// runs before the route-group auth middleware, and handlers read bodies like upstream.
func newMiddlewareEngine(t *testing.T, rt *Runtime) *testEngine {
	t.Helper()
	gin.SetMode(gin.TestMode)
	manager := sdkaccess.NewManager()
	manager.SetProviders([]sdkaccess.Provider{ownerProvider(t), &accessProvider{rt: rt}})
	te := &testEngine{engine: gin.New()}
	auth := api.AuthMiddleware(manager)

	respond := func(c *gin.Context, raw []byte, model string) {
		te.calls++
		c.JSON(http.StatusOK, echoResult{Model: model, BodySHA: sha(raw), Principal: c.GetString("userApiKey")})
	}
	// OpenAI handlers decode the body with handlers.ReadRequestBody.
	openaiEcho := func(c *gin.Context) {
		raw, _ := c.GetRawData()
		c.Request.Body = io.NopCloser(bytes.NewReader(raw))
		decoded, errDecode := handlers.ReadRequestBody(c)
		if errDecode != nil {
			te.calls++
			c.JSON(http.StatusBadRequest, gin.H{"error": errDecode.Error()})
			return
		}
		respond(c, raw, gjson.GetBytes(decoded, "model").String())
	}
	// Claude handlers parse the raw bytes.
	claudeEcho := func(c *gin.Context) {
		raw, _ := c.GetRawData()
		respond(c, raw, gjson.GetBytes(raw, "model").String())
	}
	geminiEcho := func(c *gin.Context) {
		raw, _ := c.GetRawData()
		respond(c, raw, strings.Split(strings.TrimPrefix(c.Param("action"), "/"), ":")[0])
	}
	list := func(c *gin.Context) {
		te.calls++
		var payload any = modelListShapes[c.Query("shape")]
		(&handlers.BaseAPIHandler{}).WriteModelListResponse(c, "openai", payload)
	}
	detail := func(c *gin.Context) {
		respond(c, nil, strings.TrimPrefix(c.Param("model")+c.Param("action"), "/"))
	}

	engine := te.engine
	engine.Use(rt.Middleware())
	v1 := engine.Group("/v1", auth)
	v1.GET("/models", list)
	v1.GET("/models/*model", detail)
	v1.POST("/chat/completions", openaiEcho)
	v1.POST("/completions", openaiEcho)
	v1.POST("/images/generations", openaiEcho)
	v1.POST("/messages", claudeEcho)
	v1.POST("/messages/count_tokens", claudeEcho)
	v1.GET("/responses", func(c *gin.Context) { respond(c, nil, "websocket") })
	v1.POST("/responses", openaiEcho)
	v1.POST("/responses/compact", openaiEcho)
	engine.POST("/v1/realtime/client_secrets", auth, openaiEcho)
	engine.Group("/backend-api/codex", auth).POST("/responses", openaiEcho)
	v1beta := engine.Group("/v1beta", auth)
	v1beta.GET("/models", list)
	v1beta.POST("/interactions", openaiEcho)
	v1beta.POST("/models/*action", geminiEcho)
	v1beta.GET("/models/*action", detail)
	engine.GET("/healthz", func(c *gin.Context) { c.JSON(http.StatusOK, gin.H{"status": "ok"}) })
	return te
}

var cloakedDeepSeek = "claude-fable-5-dd-" + reverse("deepseek-v4-flash")

var modelListShapes = map[string]any{
	"openai": map[string]any{"object": "list", "data": []map[string]any{
		{"id": "deepseek-v4-flash", "object": "model"}, {"id": "claude-sonnet-4-6", "object": "model"}, {"id": "gemini-3-flash", "object": "model"},
	}},
	"claude": map[string]any{"has_more": false, "first_id": "claude-sonnet-4-6", "last_id": cloakedDeepSeek, "data": []map[string]any{
		{"id": "claude-sonnet-4-6", "type": "model"}, {"id": cloakedDeepSeek, "type": "model"},
	}},
	"codex": map[string]any{"models": []map[string]any{{"slug": "deepseek-v4-pro"}, {"slug": "gpt-5.6"}}},
	"gemini": map[string]any{"models": []map[string]any{
		{"name": "models/deepseek-v4-flash"}, {"name": "models/gemini-3-flash"},
	}},
}

type request struct {
	method   string
	path     string
	body     []byte
	encoding string
	headers  map[string]string
}

func (te *testEngine) do(req request) *httptest.ResponseRecorder {
	httpReq := httptest.NewRequest(req.method, req.path, bytes.NewReader(req.body))
	if req.body != nil {
		httpReq.Header.Set("Content-Type", "application/json")
	}
	if req.encoding != "" {
		httpReq.Header.Set("Content-Encoding", req.encoding)
	}
	for key, value := range req.headers {
		httpReq.Header.Set(key, value)
	}
	recorder := httptest.NewRecorder()
	te.engine.ServeHTTP(recorder, httpReq)
	return recorder
}

func bearer(key string) map[string]string { return map[string]string{"Authorization": "Bearer " + key} }

func chatBody(model string) []byte {
	return []byte(`{"model":` + strconv.Quote(model) + `,"messages":[{"role":"user","content":"hi"}]}`)
}

func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	encoder, errEncoder := zstd.NewWriter(nil)
	if errEncoder != nil {
		t.Fatal(errEncoder)
	}
	defer func() { _ = encoder.Close() }()
	return encoder.EncodeAll(data, nil)
}

func gzipCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := gzip.NewWriter(&buffer)
	if _, errWrite := writer.Write(data); errWrite != nil {
		t.Fatal(errWrite)
	}
	if errClose := writer.Close(); errClose != nil {
		t.Fatal(errClose)
	}
	return buffer.Bytes()
}

func decodeEcho(t *testing.T, recorder *httptest.ResponseRecorder) echoResult {
	t.Helper()
	var result echoResult
	if errDecode := json.Unmarshal(recorder.Body.Bytes(), &result); errDecode != nil {
		t.Fatalf("decode echo %q: %v", recorder.Body.String(), errDecode)
	}
	return result
}

func errorCode(recorder *httptest.ResponseRecorder) string {
	return gjson.Get(recorder.Body.String(), "error.code").String()
}

func TestMiddlewareAllowsListedModelsWithBodyIntact(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	cases := []struct {
		name, path, model string
		body              []byte
	}{
		{"chat", "/v1/chat/completions", "deepseek-v4-flash", nil},
		{"chat with suffix", "/v1/chat/completions", "deepseek-v4-flash(high)", nil},
		{"completions", "/v1/completions", "deepseek-v4-pro", []byte(`{"model":"deepseek-v4-pro","prompt":"hi"}`)},
		{"responses", "/v1/responses", "cline-pass/anthropic/claude-sonnet-4-6", []byte(`{"model":"cline-pass/anthropic/claude-sonnet-4-6","input":"hi","stream":true}`)},
		{"messages", "/v1/messages", "deepseek-v4-flash", nil},
		{"messages cloaked id", "/v1/messages", cloakedDeepSeek, nil},
		{"count tokens", "/v1/messages/count_tokens", "deepseek-v4-flash", nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			body := tc.body
			if body == nil {
				body = chatBody(tc.model)
			}
			recorder := te.do(request{method: http.MethodPost, path: tc.path, body: body, headers: bearer(aliceKey)})
			if recorder.Code != http.StatusOK {
				t.Fatalf("status = %d body=%s", recorder.Code, recorder.Body.String())
			}
			echo := decodeEcho(t, recorder)
			if echo.Model != tc.model || echo.BodySHA != sha(body) || echo.Principal != "friend:alice" {
				t.Fatalf("downstream saw %+v", echo)
			}
		})
	}
	usage := rt.Usage().Report().Friends["alice"]
	if usage == nil || usage.Requests != int64(len(cases)) || usage.Models["deepseek-v4-flash"].Requests != 5 {
		t.Fatalf("request accounting = %+v", usage)
	}
}

func TestMiddlewareDeniesUnlistedModels(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	for _, tc := range []struct{ path, model string }{
		{"/v1/chat/completions", "gemini-3-flash"},
		{"/v1/chat/completions", "claude-sonnet-4-6"},
		{"/v1/chat/completions", "gpt-5.6"},
		{"/v1/chat/completions", "auto"},
		{"/v1/responses", "gpt-5.6(high)"},
		{"/v1/messages", "claude-sonnet-4-6"},
		{"/v1/messages", "claude-fable-5-dd-" + reverse("gemini-3-flash")},
		{"/v1/messages/count_tokens", "claude-sonnet-4-6"},
		{"/v1/completions", "gpt-5.6"},
	} {
		recorder := te.do(request{method: http.MethodPost, path: tc.path, body: chatBody(tc.model), headers: bearer(aliceKey)})
		if recorder.Code != http.StatusForbidden || errorCode(recorder) != "model_not_allowed" {
			t.Fatalf("%s %s: status=%d body=%s", tc.path, tc.model, recorder.Code, recorder.Body.String())
		}
		if !strings.Contains(recorder.Body.String(), tc.model) {
			t.Fatalf("error must name the model: %s", recorder.Body.String())
		}
	}
	if te.calls != 0 {
		t.Fatalf("denied requests reached handlers %d times", te.calls)
	}

	missing := te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: []byte(`{"messages":[]}`), headers: bearer(aliceKey)})
	if missing.Code != http.StatusBadRequest || errorCode(missing) != "model_required" {
		t.Fatalf("missing model: %d %s", missing.Code, missing.Body.String())
	}
}

func TestMiddlewareGeminiPathModels(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	body := []byte(`{"contents":[{"role":"user","parts":[{"text":"hi"}]}]}`)
	for _, method := range []string{"generateContent", "streamGenerateContent", "countTokens"} {
		recorder := te.do(request{method: http.MethodPost, path: "/v1beta/models/deepseek-v4-flash:" + method, body: body, headers: map[string]string{"X-Goog-Api-Key": aliceKey}})
		if recorder.Code != http.StatusOK || decodeEcho(t, recorder).Model != "deepseek-v4-flash" {
			t.Fatalf("%s: %d %s", method, recorder.Code, recorder.Body.String())
		}
	}
	for path, code := range map[string]string{
		"/v1beta/models/gemini-3-flash:generateContent":                          "model_not_allowed",
		"/v1beta/models/claude-sonnet-4-6:streamGenerateContent?alt=sse":         "model_not_allowed",
		"/v1beta/models/deepseek-v4-flash:embedContent":                          "endpoint_not_allowed",
		"/v1beta/models/deepseek-v4-flash":                                       "endpoint_not_allowed",
		"/v1beta/models/deepseek-v4-flash:generateContent:x":                     "endpoint_not_allowed",
		"/v1beta/models/models%2Fgemini-3-flash:generateContent":                 "model_not_allowed",
		"/v1beta/models/deepseek-v4-flash:generateContent?key=" + aliceKey + "x": "",
	} {
		recorder := te.do(request{method: http.MethodPost, path: path, body: body, headers: map[string]string{"X-Goog-Api-Key": aliceKey}})
		if code == "" {
			continue
		}
		if recorder.Code != http.StatusForbidden || errorCode(recorder) != code {
			t.Fatalf("%s: status=%d body=%s", path, recorder.Code, recorder.Body.String())
		}
	}
	// A top-level body model must be allowed as well.
	smuggled := te.do(request{method: http.MethodPost, path: "/v1beta/models/deepseek-v4-flash:generateContent", body: []byte(`{"model":"gemini-3-flash","contents":[]}`), headers: bearer(aliceKey)})
	if smuggled.Code != http.StatusForbidden {
		t.Fatalf("body model in Gemini request: %d %s", smuggled.Code, smuggled.Body.String())
	}
}

func TestMiddlewareDeniesUnlistedEndpointsAndWebSockets(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	body := chatBody("deepseek-v4-flash")
	for _, req := range []request{
		{method: http.MethodPost, path: "/v1/images/generations", body: body},
		{method: http.MethodPost, path: "/v1/responses/compact", body: body},
		{method: http.MethodPost, path: "/backend-api/codex/responses", body: body},
		{method: http.MethodPost, path: "/v1beta/interactions", body: body},
		{method: http.MethodPost, path: "/v1/realtime/client_secrets", body: body},
		{method: http.MethodGet, path: "/v1/responses"},
		{method: http.MethodGet, path: "/healthz"},
		{method: http.MethodGet, path: "/v0/management/config"},
		{method: http.MethodPut, path: "/v1/chat/completions", body: body},
	} {
		req.headers = bearer(aliceKey)
		recorder := te.do(req)
		if recorder.Code != http.StatusForbidden || errorCode(recorder) != "endpoint_not_allowed" {
			t.Fatalf("%s %s: status=%d body=%s", req.method, req.path, recorder.Code, recorder.Body.String())
		}
	}
	for _, req := range []request{
		{method: http.MethodGet, path: "/v1/responses", headers: map[string]string{"Upgrade": "websocket", "Connection": "Upgrade", "Sec-WebSocket-Version": "13", "Sec-WebSocket-Key": "dGhlIHNhbXBsZSBub25jZQ=="}},
		{method: http.MethodPost, path: "/v1/chat/completions", body: body, headers: map[string]string{"Connection": "keep-alive, Upgrade", "Upgrade": "websocket"}},
	} {
		req.headers["Authorization"] = "Bearer " + aliceKey
		recorder := te.do(req)
		if recorder.Code != http.StatusForbidden || errorCode(recorder) != "websocket_not_allowed" {
			t.Fatalf("%s %s: status=%d body=%s", req.method, req.path, recorder.Code, recorder.Body.String())
		}
	}
	if te.calls != 0 {
		t.Fatalf("denied requests reached handlers %d times", te.calls)
	}
}

// zstdSkippableFrame builds a zstd skippable frame whose payload decoders ignore.
func zstdSkippableFrame(payload []byte) []byte {
	frame := make([]byte, 8, 8+len(payload))
	binary.LittleEndian.PutUint32(frame[0:4], 0x184D2A50)
	binary.LittleEndian.PutUint32(frame[4:8], uint32(len(payload)))
	return append(frame, payload...)
}

func TestMiddlewareCompressedBodies(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)

	allowed := zstdCompress(t, chatBody("deepseek-v4-flash"))
	recorder := te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: allowed, encoding: "zstd", headers: bearer(aliceKey)})
	if recorder.Code != http.StatusOK {
		t.Fatalf("zstd allowed: %d %s", recorder.Code, recorder.Body.String())
	}
	if echo := decodeEcho(t, recorder); echo.Model != "deepseek-v4-flash" || echo.BodySHA != sha(allowed) {
		t.Fatalf("zstd body not passed through unchanged: %+v", echo)
	}

	forbidden := zstdCompress(t, chatBody("claude-sonnet-4-6"))
	if recorder = te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: forbidden, encoding: "zstd", headers: bearer(aliceKey)}); recorder.Code != http.StatusForbidden {
		t.Fatalf("zstd forbidden: %d %s", recorder.Code, recorder.Body.String())
	}

	gzipForbidden := gzipCompress(t, chatBody("claude-sonnet-4-6"))
	if recorder = te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: gzipForbidden, encoding: "gzip", headers: bearer(aliceKey)}); recorder.Code != http.StatusForbidden {
		t.Fatalf("gzip forbidden: %d %s", recorder.Code, recorder.Body.String())
	}
	// Upstream handlers do not decode gzip; an allowed gzip body passes the check and
	// fails downstream exactly as it would without friend keys.
	gzipAllowed := gzipCompress(t, chatBody("deepseek-v4-flash"))
	if recorder = te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: gzipAllowed, encoding: "gzip", headers: bearer(aliceKey)}); recorder.Code != http.StatusBadRequest || te.calls == 0 {
		t.Fatalf("gzip allowed: %d %s", recorder.Code, recorder.Body.String())
	}

	// Smuggling: a skippable frame carries a forbidden model that the Claude handler
	// (which parses raw bytes) would route on, while the decoded body looks allowed.
	smuggled := append(zstdSkippableFrame(chatBody("claude-sonnet-4-6")), allowed...)
	ownerView := te.do(request{method: http.MethodPost, path: "/v1/messages", body: smuggled, encoding: "zstd", headers: bearer(ownerKey)})
	if ownerView.Code != http.StatusOK || decodeEcho(t, ownerView).Model != "claude-sonnet-4-6" {
		t.Fatalf("fixture must route to the smuggled model downstream: %d %s", ownerView.Code, ownerView.Body.String())
	}
	if recorder = te.do(request{method: http.MethodPost, path: "/v1/messages", body: smuggled, encoding: "zstd", headers: bearer(aliceKey)}); recorder.Code != http.StatusForbidden {
		t.Fatalf("smuggled zstd model: %d %s", recorder.Code, recorder.Body.String())
	}

	duplicate := []byte(`{"model":"deepseek-v4-flash","model":"claude-sonnet-4-6","messages":[]}`)
	if recorder = te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: duplicate, headers: bearer(aliceKey)}); recorder.Code != http.StatusForbidden {
		t.Fatalf("duplicate model keys: %d %s", recorder.Code, recorder.Body.String())
	}
	escaped := []byte(`{"model":"deepseek-v4-flash","model":"claude-sonnet-4-6","messages":[]}`)
	if recorder = te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: escaped, headers: bearer(aliceKey)}); recorder.Code != http.StatusForbidden {
		t.Fatalf("escaped duplicate model key: %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestMiddlewareInactiveKeys(t *testing.T) {
	clock := newFakeClock(testNow())
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, clock)
	te := newMiddlewareEngine(t, rt)
	for _, path := range []string{"/v1/chat/completions", "/v1/images/generations"} {
		recorder := te.do(request{method: http.MethodPost, path: path, body: chatBody("deepseek-v4-flash"), headers: bearer(bobKey)})
		if recorder.Code != http.StatusUnauthorized || gjson.Get(recorder.Body.String(), "error").String() != "API key disabled" {
			t.Fatalf("disabled %s: %d %s", path, recorder.Code, recorder.Body.String())
		}
	}
	clock.Set(time.Date(2026, 12, 31, 16, 0, 0, 0, time.UTC)) // 2027-01-01 00:00 UTC+8
	recorder := te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: chatBody("deepseek-v4-flash"), headers: bearer(aliceKey)})
	if recorder.Code != http.StatusUnauthorized || gjson.Get(recorder.Body.String(), "error").String() != "API key expired" {
		t.Fatalf("expired: %d %s", recorder.Code, recorder.Body.String())
	}
	// The access provider rejects inactive keys even without the middleware.
	if _, authErr := (&accessProvider{rt: rt}).Authenticate(t.Context(), httptest.NewRequest(http.MethodGet, "/v1/models?key="+aliceKey, nil)); authErr == nil || authErr.StatusCode != http.StatusUnauthorized || authErr.Message != "API key expired" {
		t.Fatalf("provider: %+v", authErr)
	}
}

func TestMiddlewareCredentialSources(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	for name, req := range map[string]request{
		"x-api-key":      {path: "/v1/messages", headers: map[string]string{"X-Api-Key": aliceKey}},
		"x-goog-api-key": {path: "/v1/chat/completions", headers: map[string]string{"X-Goog-Api-Key": aliceKey}},
		"query key":      {path: "/v1/chat/completions?key=" + aliceKey},
		"query token":    {path: "/v1/chat/completions?auth_token=" + aliceKey},
		"raw auth":       {path: "/v1/chat/completions", headers: map[string]string{"Authorization": aliceKey}},
		// A request that also carries an owner key is still restricted.
		"mixed": {path: "/v1/chat/completions", headers: map[string]string{"Authorization": "Bearer " + ownerKey, "X-Api-Key": aliceKey}},
	} {
		req.method = http.MethodPost
		req.body = chatBody("claude-sonnet-4-6")
		if recorder := te.do(req); recorder.Code != http.StatusForbidden {
			t.Fatalf("%s: %d %s", name, recorder.Code, recorder.Body.String())
		}
		req.body = chatBody("deepseek-v4-flash")
		recorder := te.do(req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s allowed: %d %s", name, recorder.Code, recorder.Body.String())
		}
	}
}

func TestOwnerKeyUntouched(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	zstdBody := zstdCompress(t, chatBody("claude-sonnet-4-6"))
	for _, req := range []request{
		{method: http.MethodPost, path: "/v1/chat/completions", body: chatBody("claude-sonnet-4-6")},
		{method: http.MethodPost, path: "/v1/chat/completions", body: zstdBody, encoding: "zstd"},
		{method: http.MethodPost, path: "/v1/images/generations", body: chatBody("gpt-image-2")},
		{method: http.MethodPost, path: "/v1/responses/compact", body: chatBody("gpt-5.6")},
		{method: http.MethodPost, path: "/v1beta/models/gemini-3-flash:generateContent", body: []byte(`{"contents":[]}`)},
		{method: http.MethodGet, path: "/v1/responses", headers: map[string]string{"Upgrade": "websocket", "Connection": "Upgrade"}},
	} {
		if req.headers == nil {
			req.headers = map[string]string{}
		}
		req.headers["Authorization"] = "Bearer " + ownerKey
		recorder := te.do(req)
		if recorder.Code != http.StatusOK {
			t.Fatalf("owner %s %s: %d %s", req.method, req.path, recorder.Code, recorder.Body.String())
		}
		echo := decodeEcho(t, recorder)
		if echo.Principal != ownerKey || (req.body != nil && echo.BodySHA != sha(req.body)) {
			t.Fatalf("owner request changed: %+v", echo)
		}
	}
	listing := te.do(request{method: http.MethodGet, path: "/v1/models?shape=openai", headers: bearer(ownerKey)})
	if got := len(gjson.Get(listing.Body.String(), "data").Array()); got != 3 {
		t.Fatalf("owner model list filtered: %s", listing.Body.String())
	}
	if invalid := te.do(request{method: http.MethodPost, path: "/v1/chat/completions", body: chatBody("x"), headers: bearer("not-a-key")}); invalid.Code != http.StatusUnauthorized {
		t.Fatalf("unknown key: %d", invalid.Code)
	}
	if len(rt.Usage().Report().Friends) != 0 {
		t.Fatal("owner requests must not be recorded")
	}
}

func TestModelListFiltering(t *testing.T) {
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, newFakeClock(testNow()))
	te := newMiddlewareEngine(t, rt)
	cases := []struct {
		path, field string
		want        []string
	}{
		{"/v1/models?shape=openai", "data.#.id", []string{"deepseek-v4-flash"}},
		{"/v1/models?shape=claude", "data.#.id", []string{cloakedDeepSeek}},
		{"/v1/models?shape=codex", "models.#.slug", []string{"deepseek-v4-pro"}},
		{"/v1beta/models?shape=gemini", "models.#.name", []string{"models/deepseek-v4-flash"}},
		{"/v1/models?shape=unknown", "data.#.id", nil},
	}
	for _, tc := range cases {
		recorder := te.do(request{method: http.MethodGet, path: tc.path, headers: bearer(aliceKey)})
		if recorder.Code != http.StatusOK {
			t.Fatalf("%s: %d %s", tc.path, recorder.Code, recorder.Body.String())
		}
		var got []string
		for _, value := range gjson.Get(recorder.Body.String(), tc.field).Array() {
			got = append(got, value.String())
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") {
			t.Fatalf("%s: got %v want %v (%s)", tc.path, got, tc.want, recorder.Body.String())
		}
		if length := recorder.Header().Get("Content-Length"); length != strconv.Itoa(recorder.Body.Len()) {
			t.Fatalf("%s: Content-Length %s, body %d", tc.path, length, recorder.Body.Len())
		}
	}
	claude := te.do(request{method: http.MethodGet, path: "/v1/models?shape=claude", headers: bearer(aliceKey)})
	for _, cursor := range []string{"first_id", "last_id"} {
		if gjson.Get(claude.Body.String(), cursor).String() != cloakedDeepSeek {
			t.Fatalf("%s not recomputed: %s", cursor, claude.Body.String())
		}
	}

	for path, status := range map[string]int{
		"/v1/models/deepseek-v4-flash":     http.StatusOK,
		"/v1/models/claude-sonnet-4-6":     http.StatusNotFound,
		"/v1beta/models/deepseek-v4-flash": http.StatusOK,
		"/v1beta/models/gemini-3-flash":    http.StatusNotFound,
	} {
		if recorder := te.do(request{method: http.MethodGet, path: path, headers: bearer(aliceKey)}); recorder.Code != status {
			t.Fatalf("%s: %d want %d", path, recorder.Code, status)
		}
	}
	if usage := rt.Usage().Report().Friends["alice"]; usage == nil || usage.LastUsed == nil || usage.Requests != 0 {
		t.Fatalf("model listing must update last_used only: %+v", usage)
	}
}
