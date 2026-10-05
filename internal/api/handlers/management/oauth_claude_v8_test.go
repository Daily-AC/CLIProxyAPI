package management

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/auth/claude"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
)

type claudeCodeExchange struct{ code, state, redirectURI string }

// fakeClaudeOAuthService keeps the real authorization URL builders and only
// replaces the network-bound token exchange.
type fakeClaudeOAuthService struct {
	*claude.ClaudeAuth
	exchanged chan claudeCodeExchange
}

func (f *fakeClaudeOAuthService) ExchangeCodeForTokensWithRedirect(_ context.Context, code, state, redirectURI string, _ *claude.PKCECodes) (*claude.ClaudeAuthBundle, error) {
	f.exchanged <- claudeCodeExchange{code: code, state: state, redirectURI: redirectURI}
	return &claude.ClaudeAuthBundle{
		TokenData: claude.ClaudeTokenData{
			AccessToken:  "access-" + code,
			RefreshToken: "refresh-" + code,
			Email:        "claude-user@example.test",
			AccountUUID:  "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa",
			Expire:       time.Now().Add(time.Hour).Format(time.RFC3339),
		},
		LastRefresh: time.Now().Format(time.RFC3339),
	}, nil
}

func anthropicForwarderActive() bool {
	callbackForwardersMu.Lock()
	defer callbackForwardersMu.Unlock()
	return callbackForwarders[anthropicCallbackPort] != nil
}

// The rebased management panel drives Claude logins only through the v8 routes:
// auth-url with is_webui and manual, status polling, and the callback that takes
// either the pasted "<code>#<state>" pair or the full localhost callback URL.
func TestClaudeOAuthV8FlowCompletes(t *testing.T) {
	tests := []struct {
		name         string
		manual       string
		wantEndpoint string
		wantRedirect string
		callback     func(state string) string
	}{
		{
			name:         "manual",
			manual:       "true",
			wantEndpoint: claude.ManualAuthURL,
			wantRedirect: claude.ManualRedirectURI,
			callback:     func(state string) string { return "ac_v8-code#" + state },
		},
		{
			name:         "local",
			manual:       "false",
			wantEndpoint: claude.AuthURL,
			wantRedirect: claude.RedirectURI,
			callback: func(state string) string {
				return "http://localhost:54545/callback?code=ac_v8-code&state=" + state
			},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if tt.manual == "false" {
				// The local flow binds the fixed Claude callback port for the web UI.
				ln, errListen := net.Listen("tcp", "0.0.0.0:54545")
				if errListen != nil {
					t.Skipf("Claude callback port is busy: %v", errListen)
				}
				_ = ln.Close()
			}

			authDir := t.TempDir()
			cfg := &config.Config{AuthDir: authDir, Port: 8317}
			// The query must win over the configured default in both directions.
			cfg.ClaudeCode.ManualOAuth = tt.manual != "true"
			cfg.ClaudeCode.ProxyURL = "socks5://127.0.0.1:17890"
			handler := NewHandlerWithoutConfigFilePath(cfg, nil)
			service := &fakeClaudeOAuthService{exchanged: make(chan claudeCodeExchange, 1)}
			originalFactory := newClaudeOAuthService
			newClaudeOAuthService = func(cfg *config.Config) claudeOAuthService {
				service.ClaudeAuth = claude.NewClaudeAuth(cfg)
				return service
			}
			t.Cleanup(func() { newClaudeOAuthService = originalFactory })

			router := gin.New()
			router.GET("/v8/management/oauth/auth-url", handler.StartOAuthV8)
			router.GET("/v8/management/oauth/status", handler.GetAuthStatus)
			router.POST("/v8/management/oauth/callback", handler.PostOAuthCallback)

			w := httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/auth-url?provider=claude&is_webui=true&manual="+tt.manual, nil))
			if w.Code != http.StatusOK {
				t.Fatalf("auth-url: %d %s", w.Code, w.Body.String())
			}
			var start struct {
				Status string `json:"status"`
				URL    string `json:"url"`
				State  string `json:"state"`
				Manual *bool  `json:"manual"`
			}
			if errDecode := json.Unmarshal(w.Body.Bytes(), &start); errDecode != nil {
				t.Fatal(errDecode)
			}
			t.Cleanup(func() { CancelOAuthSession(start.State) })
			if start.Status != "ok" || start.State == "" || start.Manual == nil || *start.Manual != (tt.manual == "true") {
				t.Fatalf("auth-url response = %s", w.Body.String())
			}
			parsed, errParse := url.Parse(start.URL)
			if errParse != nil {
				t.Fatal(errParse)
			}
			if got := parsed.Scheme + "://" + parsed.Host + parsed.Path; got != tt.wantEndpoint {
				t.Fatalf("authorize endpoint = %q, want %q", got, tt.wantEndpoint)
			}
			if got := parsed.Query().Get("redirect_uri"); got != tt.wantRedirect {
				t.Fatalf("redirect_uri = %q, want %q", got, tt.wantRedirect)
			}
			// Only the local flow needs the callback forwarder on the Claude port.
			if active := anthropicForwarderActive(); active != (tt.manual == "false") {
				t.Fatalf("callback forwarder active = %v for manual=%s", active, tt.manual)
			}

			w = httptest.NewRecorder()
			router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+start.State, nil))
			if !strings.Contains(w.Body.String(), `"status":"wait"`) {
				t.Fatalf("status before callback = %s", w.Body.String())
			}

			body, errMarshal := json.Marshal(map[string]string{"provider": "claude", "redirect_url": tt.callback(start.State)})
			if errMarshal != nil {
				t.Fatal(errMarshal)
			}
			req := httptest.NewRequest(http.MethodPost, "/v8/management/oauth/callback", strings.NewReader(string(body)))
			req.Header.Set("Content-Type", "application/json")
			w = httptest.NewRecorder()
			router.ServeHTTP(w, req)
			if w.Code != http.StatusOK {
				t.Fatalf("callback: %d %s", w.Code, w.Body.String())
			}

			select {
			case exchange := <-service.exchanged:
				want := claudeCodeExchange{code: "ac_v8-code", state: start.State, redirectURI: tt.wantRedirect}
				if exchange != want {
					t.Fatalf("exchange = %+v, want %+v", exchange, want)
				}
			case <-time.After(5 * time.Second):
				t.Fatal("token exchange did not start")
			}

			deadline := time.NewTimer(5 * time.Second)
			defer deadline.Stop()
			ticker := time.NewTicker(10 * time.Millisecond)
			defer ticker.Stop()
			for {
				w = httptest.NewRecorder()
				router.ServeHTTP(w, httptest.NewRequest(http.MethodGet, "/v8/management/oauth/status?state="+start.State, nil))
				var status map[string]string
				if errDecode := json.Unmarshal(w.Body.Bytes(), &status); errDecode != nil {
					t.Fatal(errDecode)
				}
				if status["status"] == "ok" && !anthropicForwarderActive() {
					break
				}
				if status["status"] == "error" {
					t.Fatalf("login failed: %v", status)
				}
				select {
				case <-deadline.C:
					t.Fatalf("login did not complete: %v", status)
				case <-ticker.C:
				}
			}

			fileName := claude.CredentialFileName("claude-user@example.test", "", "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa")
			data, errRead := os.ReadFile(filepath.Join(authDir, fileName))
			if errRead != nil {
				t.Fatalf("credential not saved: %v", errRead)
			}
			var record map[string]any
			if errDecode := json.Unmarshal(data, &record); errDecode != nil {
				t.Fatal(errDecode)
			}
			if record["email"] != "claude-user@example.test" || record["access_token"] != "access-ac_v8-code" {
				t.Fatalf("unexpected credential: %v", record)
			}
			// Credentials minted by the flow keep the OAuth proxy for refresh and inference.
			if record["proxy_url"] != "socks5://127.0.0.1:17890" {
				t.Fatalf("credential proxy_url = %v, want claude-code.proxy-url", record["proxy_url"])
			}
		})
	}
}
