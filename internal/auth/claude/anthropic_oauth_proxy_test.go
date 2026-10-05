package claude

import (
	"testing"

	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"golang.org/x/net/proxy"
)

// dialerIsDirect reports whether the auth service ended up dialing without a proxy.
func dialerIsDirect(t *testing.T, auth *ClaudeAuth) bool {
	t.Helper()
	transport, ok := auth.httpClient.Transport.(*utlsRoundTripper)
	if !ok || transport == nil {
		t.Fatalf("expected utlsRoundTripper, got %T", auth.httpClient.Transport)
	}
	return transport.dialer == proxy.Direct
}

// The authorization code exchange runs before any credential exists, so it can only
// pick up claude-code.proxy-url. This is the path that lets a host in a region
// Anthropic blocks complete a login while its global egress stays direct.
func TestNewClaudeAuth_UsesClaudeCodeProxyWhenGlobalIsDirect(t *testing.T) {
	cfg := &config.Config{}
	cfg.ClaudeCode.ProxyURL = "socks5://oauth-proxy.example.com:1080"

	if dialerIsDirect(t, NewClaudeAuth(cfg)) {
		t.Fatal("expected claude-code.proxy-url to be applied, got a direct dialer")
	}
}

func TestNewClaudeAuthWithProxyURL_CredentialDirectBeatsClaudeCodeProxy(t *testing.T) {
	cfg := &config.Config{}
	cfg.ClaudeCode.ProxyURL = "socks5://oauth-proxy.example.com:1080"

	if !dialerIsDirect(t, NewClaudeAuthWithProxyURL(cfg, "direct")) {
		t.Fatal("expected an explicit per-credential direct to win over claude-code.proxy-url")
	}
}

// Without claude-code.proxy-url the resolution order must stay exactly as it was,
// so existing deployments that only set the global proxy keep their behavior.
func TestNewClaudeAuthWithProxyURL_FallsBackToGlobalWhenClaudeCodeProxyUnset(t *testing.T) {
	cfg := &config.Config{SDKConfig: config.SDKConfig{ProxyURL: "socks5://global.example.com:1080"}}

	if dialerIsDirect(t, NewClaudeAuth(cfg)) {
		t.Fatal("expected the global proxy-url to still apply when claude-code.proxy-url is unset")
	}
}
