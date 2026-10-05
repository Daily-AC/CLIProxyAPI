package config

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// Deployments configured before v8 keep the Claude login settings under the
// legacy claude-code section; v8 maps that section to upstream.claude.
const (
	legacyClaudeCodeOAuthYAML = "claude-code:\n  manual-oauth: true\n  proxy-url: \"socks5://127.0.0.1:17890\"\n"
	v8ClaudeCodeOAuthYAML     = "config-version: 8\nupstream:\n  claude:\n    manual-oauth: true\n    proxy-url: \"socks5://127.0.0.1:17890\"\n"
)

// v8ClaudeCodeOAuthPaths lists the v8 locations the legacy settings must land on.
var v8ClaudeCodeOAuthPaths = map[string]string{
	"upstream.claude.manual-oauth": "true",
	"upstream.claude.proxy-url":    "socks5://127.0.0.1:17890",
}

func assertClaudeCodeOAuthConfig(t *testing.T, cfg *Config) {
	t.Helper()
	if !cfg.ClaudeCode.ManualOAuth {
		t.Fatal("ClaudeCode.ManualOAuth = false, want true")
	}
	if got := cfg.ClaudeCode.ProxyURL; got != "socks5://127.0.0.1:17890" {
		t.Fatalf("ClaudeCode.ProxyURL = %q, want socks5://127.0.0.1:17890", got)
	}
	if cfg.ProxyURL != "" {
		t.Fatalf("global proxy-url = %q, want it untouched", cfg.ProxyURL)
	}
}

func assertV8ClaudeCodeOAuthLayout(t *testing.T, data []byte) {
	t.Helper()
	var doc yaml.Node
	if err := yaml.Unmarshal(data, &doc); err != nil {
		t.Fatal(err)
	}
	root := doc.Content[0]
	if yamlPath(root, "claude-code") != nil {
		t.Fatalf("legacy claude-code section remained after migration:\n%s", data)
	}
	for path, want := range v8ClaudeCodeOAuthPaths {
		value := yamlPath(root, path)
		if value == nil || value.Value != want {
			t.Fatalf("%s missing or changed after migration, want %q:\n%s", path, want, data)
		}
	}
	if strings.Contains(string(data), "# claude-code") || strings.Contains(string(data), "# upstream.claude") {
		t.Fatalf("Claude login settings were commented out as unknown fields:\n%s", data)
	}
	if err := ValidateV8Config(data); err != nil {
		t.Fatalf("migrated config is not a valid v8 document: %v", err)
	}
}

func TestClaudeCodeOAuthLoadsFromBothLayouts(t *testing.T) {
	for name, raw := range map[string]string{"legacy": legacyClaudeCodeOAuthYAML, "v8": v8ClaudeCodeOAuthYAML} {
		t.Run(name, func(t *testing.T) {
			cfg, err := ParseConfigBytes([]byte(raw))
			if err != nil {
				t.Fatalf("ParseConfigBytes() error = %v", err)
			}
			assertClaudeCodeOAuthConfig(t, cfg)
		})
	}
}

func TestClaudeCodeOAuthSurvivesV8Migration(t *testing.T) {
	migrated, changed, err := NormalizeConfigLayout([]byte(legacyClaudeCodeOAuthYAML), true)
	if err != nil || !changed {
		t.Fatalf("NormalizeConfigLayout() changed = %v, error = %v", changed, err)
	}
	assertV8ClaudeCodeOAuthLayout(t, migrated)

	cfg, err := ParseConfigBytes(migrated)
	if err != nil {
		t.Fatalf("ParseConfigBytes(migrated) error = %v", err)
	}
	assertClaudeCodeOAuthConfig(t, cfg)
}

// Saving through the config writer must keep the settings: a v0 save leaves a
// legacy file legacy, and a v8 save migrates them to upstream.claude.
func TestClaudeCodeOAuthSurvivesConfigSave(t *testing.T) {
	for _, migrate := range []bool{false, true} {
		name := "v0 save"
		if migrate {
			name = "v8 save"
		}
		t.Run(name, func(t *testing.T) {
			file := filepath.Join(t.TempDir(), "config.yaml")
			if err := os.WriteFile(file, []byte(legacyClaudeCodeOAuthYAML), 0600); err != nil {
				t.Fatal(err)
			}
			cfg, err := LoadConfig(file)
			if err != nil {
				t.Fatalf("LoadConfig() error = %v", err)
			}
			assertClaudeCodeOAuthConfig(t, cfg)

			cfg.Port = 8318
			if err = SaveConfigPreserveComments(file, cfg, migrate); err != nil {
				t.Fatalf("SaveConfigPreserveComments() error = %v", err)
			}
			data, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			if migrate {
				assertV8ClaudeCodeOAuthLayout(t, data)
			} else {
				var doc yaml.Node
				if err = yaml.Unmarshal(data, &doc); err != nil {
					t.Fatal(err)
				}
				for path, want := range map[string]string{"claude-code.manual-oauth": "true", "claude-code.proxy-url": "socks5://127.0.0.1:17890"} {
					if value := yamlPath(doc.Content[0], path); value == nil || value.Value != want {
						t.Fatalf("v0 save dropped %s:\n%s", path, data)
					}
				}
			}

			reloaded, err := LoadConfig(file)
			if err != nil {
				t.Fatalf("LoadConfig(saved) error = %v", err)
			}
			assertClaudeCodeOAuthConfig(t, reloaded)
		})
	}
}

func TestParseConfigBytesClaudeCodeProxyURL(t *testing.T) {
	tests := []struct {
		name string
		yaml string
		want string
	}{
		{
			name: "defaults to empty so the global proxy-url keeps deciding",
			yaml: "port: 8317\n",
			want: "",
		},
		{
			name: "reads a dedicated oauth proxy",
			yaml: "claude-code:\n  proxy-url: \"http://127.0.0.1:32450\"\n",
			want: "http://127.0.0.1:32450",
		},
		{
			name: "coexists with manual-oauth",
			yaml: "claude-code:\n  manual-oauth: true\n  proxy-url: \"socks5://127.0.0.1:1080\"\n",
			want: "socks5://127.0.0.1:1080",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg, errParse := ParseConfigBytes([]byte(tt.yaml))
			if errParse != nil {
				t.Fatalf("ParseConfigBytes() error = %v", errParse)
			}
			if got := cfg.ClaudeCode.ProxyURL; got != tt.want {
				t.Fatalf("ClaudeCode.ProxyURL = %q, want %q", got, tt.want)
			}
		})
	}
}
