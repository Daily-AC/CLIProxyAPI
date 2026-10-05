package management

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

func TestParseManualCodePair(t *testing.T) {
	tests := []struct {
		name      string
		raw       string
		wantCode  string
		wantState string
		wantOK    bool
	}{
		{
			name:   "bare code without state",
			raw:    "ac_01ABCdef-123",
			wantOK: false,
		},
		{
			name:      "code and state",
			raw:       "ac_01ABCdef-123#z0lAE_ru8ATNwBc1inRpt40wFm8axKj3-8uQSQdIq5Q",
			wantCode:  "ac_01ABCdef-123",
			wantState: "z0lAE_ru8ATNwBc1inRpt40wFm8axKj3-8uQSQdIq5Q",
			wantOK:    true,
		},
		{
			name:   "callback url is not a manual pair",
			raw:    "http://localhost:54545/callback?code=abc&state=def",
			wantOK: false,
		},
		{
			name:   "callback url with fragment is not a manual pair",
			raw:    "http://localhost:54545/callback?code=abc#def",
			wantOK: false,
		},
		{
			name:   "empty state",
			raw:    "abc#",
			wantOK: false,
		},
		{
			name:   "empty code",
			raw:    "#def",
			wantOK: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			code, state, ok := parseManualCodePair(tt.raw)
			if ok != tt.wantOK {
				t.Fatalf("ok = %v, want %v", ok, tt.wantOK)
			}
			if !tt.wantOK {
				return
			}
			if code != tt.wantCode {
				t.Fatalf("code = %q, want %q", code, tt.wantCode)
			}
			if state != tt.wantState {
				t.Fatalf("state = %q, want %q", state, tt.wantState)
			}
		})
	}
}

// The custom management panel drives the manual Claude login through these
// endpoint shapes, so they are pinned here: the auth-url response carries
// status/url/state/manual, and "manual" selects the authorize endpoint and
// redirect_uri (query override first, then claude-code.manual-oauth).
func TestRequestAnthropicTokenManualMode(t *testing.T) {
	tests := []struct {
		name       string
		configured bool
		path       string
		wantManual bool
	}{
		{name: "default local flow", path: "/v0/management/anthropic-auth-url", wantManual: false},
		{name: "config enables manual", configured: true, path: "/v0/management/anthropic-auth-url", wantManual: true},
		{name: "query enables manual", path: "/v0/management/anthropic-auth-url?manual=1", wantManual: true},
		{name: "query true enables manual", path: "/v0/management/anthropic-auth-url?manual=true", wantManual: true},
		{name: "query disables configured manual", configured: true, path: "/v0/management/anthropic-auth-url?manual=0", wantManual: false},
		{name: "v8 manual=1", path: "/v8/management/oauth/auth-url?provider=claude&manual=1", wantManual: true},
		{name: "v8 manual=true with web UI", path: "/v8/management/oauth/auth-url?provider=claude&is_webui=true&manual=true", wantManual: true},
		{name: "v8 manual=false overrides config", configured: true, path: "/v8/management/oauth/auth-url?provider=claude&manual=false", wantManual: false},
		{name: "v8 manual=0 overrides config", configured: true, path: "/v8/management/oauth/auth-url?provider=claude&manual=0", wantManual: false},
		{name: "v8 falls back to config", configured: true, path: "/v8/management/oauth/auth-url?provider=claude&is_webui=true", wantManual: true},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := &config.Config{AuthDir: t.TempDir()}
			cfg.ClaudeCode.ManualOAuth = tt.configured
			handler := NewHandlerWithoutConfigFilePath(cfg, nil)
			router := gin.New()
			router.GET("/v0/management/anthropic-auth-url", handler.RequestAnthropicToken)
			router.GET("/v8/management/oauth/auth-url", handler.StartOAuthV8)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, tt.path, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}
			var payload struct {
				Status string `json:"status"`
				URL    string `json:"url"`
				State  string `json:"state"`
				Manual *bool  `json:"manual"`
			}
			if errDecode := json.Unmarshal(w.Body.Bytes(), &payload); errDecode != nil {
				t.Fatalf("decode response: %v", errDecode)
			}
			defer CompleteOAuthSession(payload.State)

			if payload.Status != "ok" || payload.State == "" || payload.Manual == nil {
				t.Fatalf("response = %s, want status ok with state and manual", w.Body.String())
			}
			if *payload.Manual != tt.wantManual {
				t.Fatalf("manual = %v, want %v", *payload.Manual, tt.wantManual)
			}
			wantEndpoint, wantRedirect := claude.AuthURL, claude.RedirectURI
			if tt.wantManual {
				wantEndpoint, wantRedirect = claude.ManualAuthURL, claude.ManualRedirectURI
			}
			if !strings.HasPrefix(payload.URL, wantEndpoint+"?") {
				t.Fatalf("url = %s, want endpoint %s", payload.URL, wantEndpoint)
			}
			parsed, errParse := url.Parse(payload.URL)
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := parsed.Query().Get("redirect_uri"); got != wantRedirect {
				t.Fatalf("redirect_uri = %q, want %q", got, wantRedirect)
			}
			if got := parsed.Query().Get("state"); got != payload.State {
				t.Fatalf("url state = %q, want %q", got, payload.State)
			}
		})
	}
}

// The panel completes the manual login by posting the pasted "<code>#<state>"
// pair in redirect_url; the full localhost callback URL keeps working.
func TestPostOAuthCallbackAcceptsManualCodePair(t *testing.T) {
	tests := []struct {
		name     string
		state    string
		redirect string
	}{
		{name: "manual pair", state: "manual-pair-state", redirect: "ac_manual-code#manual-pair-state"},
		{name: "localhost callback url", state: "callback-url-state", redirect: "http://localhost:54545/callback?code=ac_manual-code&state=callback-url-state"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			authDir := t.TempDir()
			handler := NewHandlerWithoutConfigFilePath(&config.Config{AuthDir: authDir}, nil)
			router := gin.New()
			router.POST("/v0/management/oauth-callback", handler.PostOAuthCallback)

			state := tt.state
			RegisterOAuthSession(state, "anthropic")
			defer CompleteOAuthSession(state)

			body, errMarshal := json.Marshal(map[string]string{"provider": "anthropic", "redirect_url": tt.redirect})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			req := httptest.NewRequest(http.MethodPost, "/v0/management/oauth-callback", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("status = %d, body = %s", w.Code, w.Body.String())
			}

			data, errRead := os.ReadFile(filepath.Join(authDir, ".oauth-anthropic-"+state+".oauth"))
			if errRead != nil {
				t.Fatalf("callback file not written: %v", errRead)
			}
			var callback map[string]string
			if errDecode := json.Unmarshal(data, &callback); errDecode != nil {
				t.Fatal(errDecode)
			}
			if callback["code"] != "ac_manual-code" || callback["state"] != state {
				t.Fatalf("callback = %v, want code ac_manual-code and state %s", callback, state)
			}
		})
	}
}
