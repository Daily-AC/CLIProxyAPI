package z10

import (
	"strings"
	"testing"
	"time"
)

func reverse(s string) string {
	runes := []rune(s)
	for i, j := 0, len(runes)-1; i < j; i, j = i+1, j-1 {
		runes[i], runes[j] = runes[j], runes[i]
	}
	return string(runes)
}

func TestParseFriends(t *testing.T) {
	set, err := ParseFriends([]byte(testFriendsYAML + `  - name: carol
    key: sk-z10-carol-0123456789abcdef
    models: ["deepseek-v4-flash"]
    channels: ["deepseek"]
    expires: "2026-10-06T12:00:00Z"
`))
	if err != nil {
		t.Fatalf("ParseFriends: %v", err)
	}
	if set.Len() != 3 {
		t.Fatalf("friends = %d, want 3", set.Len())
	}
	alice := set.ByName("alice")
	if !alice.Enabled {
		t.Fatal("enabled must default to true")
	}
	wantAliceExpiry := time.Date(2027, 1, 1, 0, 0, 0, 0, usageZone)
	if !alice.ExpiresAt.Equal(wantAliceExpiry) || alice.ExpiresRaw != "2026-12-31" {
		t.Fatalf("alice expiry = %v (%q), want %v", alice.ExpiresAt, alice.ExpiresRaw, wantAliceExpiry)
	}
	if got := strings.Join(alice.Models, ","); got != "deepseek-v4-*,Cline-Pass/*" {
		t.Fatalf("alice models = %s", got)
	}
	if _, ok := alice.providers[deepseekProvider]; !ok || len(alice.providers) != 2 || strings.Join(alice.Channels, ",") != "DeepSeek,cline-pass" {
		t.Fatalf("alice channels = %v providers = %v", alice.Channels, alice.providers)
	}
	if _, ok := alice.providers[clinePassProvider]; !ok {
		t.Fatalf("alice providers = %v", alice.providers)
	}
	if set.ByName("bob").Enabled {
		t.Fatal("bob must be disabled")
	}
	if want := time.Date(2026, 10, 6, 12, 0, 0, 0, time.UTC); !set.ByName("carol").ExpiresAt.Equal(want) {
		t.Fatalf("carol expiry = %v, want %v", set.ByName("carol").ExpiresAt, want)
	}

	empty, errEmpty := ParseFriends(nil)
	if errEmpty != nil || empty.Len() != 0 {
		t.Fatalf("empty file: %v %d", errEmpty, empty.Len())
	}
}

func TestParseFriendsValidation(t *testing.T) {
	entry := func(name, key, extra string) string {
		return "  - name: " + name + "\n    key: " + key + "\n    models: [\"deepseek-v4-*\"]\n    channels: [deepseek]\n" + extra
	}
	cases := []struct {
		name string
		yaml string
		want string
	}{
		{"duplicate name", "keys:\n" + entry("alice", aliceKey, "") + entry("alice", bobKey, ""), "duplicate name"},
		{"duplicate key", "keys:\n" + entry("alice", aliceKey, "") + entry("bob", aliceKey, ""), "duplicates the key of alice"},
		{"empty key", "keys:\n" + entry("alice", `""`, ""), "key is empty"},
		{"short key", "keys:\n" + entry("alice", "short", ""), "at least 16"},
		{"whitespace key", "keys:\n" + entry("alice", `"sk-z10 alice 0123456789"`, ""), "whitespace"},
		{"missing channels", "keys:\n  - name: alice\n    key: " + aliceKey + "\n    models: [\"x\"]\n", "channels is required"},
		{"empty channel", "keys:\n  - name: alice\n    key: " + aliceKey + "\n    models: [\"x\"]\n    channels: [\" \"]\n", "empty channel name"},
		{"bad name", "keys:\n" + entry("al ice", aliceKey, ""), "name must match"},
		{"empty name", "keys:\n" + entry(`""`, aliceKey, ""), "name must match"},
		{"bad expiry", "keys:\n" + entry("alice", aliceKey, "    expires: next-week\n"), "must be YYYY-MM-DD or RFC3339"},
		{"empty pattern", "keys:\n  - name: alice\n    key: " + aliceKey + "\n    models: [\"\"]\n    channels: [deepseek]\n", "empty model pattern"},
		{"unknown field", "keys:\n" + entry("alice", aliceKey, "    enabeld: false\n"), "enabeld"},
		{"type error", "keys:\n  - name: alice\n    key: " + aliceKey + "\n    enabled: " + bobKey + "\n", "parse"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := ParseFriends([]byte(tc.yaml))
			if err == nil {
				t.Fatalf("expected error containing %q", tc.want)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("error = %q, want it to contain %q", err, tc.want)
			}
			for _, secret := range []string{aliceKey, bobKey, ownerKey} {
				if strings.Contains(err.Error(), secret) {
					t.Fatalf("error leaks a key: %q", err)
				}
			}
		})
	}
}

func TestFriendExpiryAndEnabled(t *testing.T) {
	set, err := ParseFriends([]byte(testFriendsYAML))
	if err != nil {
		t.Fatal(err)
	}
	alice := set.ByName("alice")
	lastValid := time.Date(2026, 12, 31, 23, 59, 59, 0, usageZone)
	if reason := alice.inactiveReason(lastValid); reason != "" {
		t.Fatalf("alice inactive at %v: %s", lastValid, reason)
	}
	if reason := alice.inactiveReason(lastValid.Add(time.Second)); reason != "API key expired" {
		t.Fatalf("alice at expiry: %q", reason)
	}
	if reason := set.ByName("bob").inactiveReason(testNow()); reason != "API key disabled" {
		t.Fatalf("bob: %q", reason)
	}
}

func TestNormalizeModel(t *testing.T) {
	cases := map[string]string{
		"deepseek-v4-flash":                                      "deepseek-v4-flash",
		"  DeepSeek-V4-Flash(high) ":                             "deepseek-v4-flash",
		"deepseek-v4-flash(8192)":                                "deepseek-v4-flash",
		"deepseek-v4-flash(high)(low)":                           "deepseek-v4-flash(high)",
		"models/gemini-3-flash":                                  "gemini-3-flash",
		"claude-fable-5-dd-" + reverse("deepseek-v4-flash"):      "deepseek-v4-flash",
		"claude-fable-5-dd-" + reverse("gemini-3-flash") + "(1)": "gemini-3-flash",
		"cline-pass/x-ai/grok-4(high)":                           "cline-pass/x-ai/grok-4",
		"claude-sonnet-4-6":                                      "claude-sonnet-4-6",
		"   ":                                                    "",
	}
	for input, want := range cases {
		if got := NormalizeModel(input); got != want {
			t.Errorf("NormalizeModel(%q) = %q, want %q", input, got, want)
		}
	}
}

func TestGlobMatch(t *testing.T) {
	cases := []struct {
		pattern, value string
		want           bool
	}{
		{"deepseek-v4-*", "deepseek-v4-flash", true},
		{"deepseek-v4-*", "deepseek-v4-", true},
		{"deepseek-v4-*", "deepseek-v3", false},
		{"deepseek-v4-*", "xdeepseek-v4-flash", false},
		{"cline-pass/*", "cline-pass/anthropic/claude-sonnet", true},
		{"*-flash", "gemini-3-flash", true},
		{"a*b*c", "axxbyyc", true},
		{"a*b*c", "axxbyy", false},
		{"exact", "exact", true},
		{"exact", "exact2", false},
		{"*", "", true},
		{"", "", true},
		{"", "x", false},
	}
	for _, tc := range cases {
		if got := globMatch(tc.pattern, tc.value); got != tc.want {
			t.Errorf("globMatch(%q, %q) = %v, want %v", tc.pattern, tc.value, got, tc.want)
		}
	}
}

func TestCheckModelUsesRegistryChannels(t *testing.T) {
	registerDefaultModels(t)
	registerTestModels(t, "openai-compatible-subscription", "deepseek-v4-1-flash")
	set, err := ParseFriends([]byte(testFriendsYAML))
	if err != nil {
		t.Fatal(err)
	}
	alice := set.ByName("alice")
	for _, model := range []string{"deepseek-v4-flash", "DEEPSEEK-V4-PRO(high)", "deepseek-v4-pro(8192)", "cline-pass/anthropic/claude-sonnet-4-6"} {
		if gateErr := alice.checkModel(model); gateErr != nil {
			t.Errorf("alice must be allowed %q: %v", model, gateErr)
		}
	}
	for model, reason := range map[string]string{
		"claude-sonnet-4-6":        "not allowed",
		"gemini-3-flash":           "not allowed",
		"xdeepseek-v4-flash":       "not allowed",
		"":                         "not allowed",
		"deepseek-v4-unknown":      "not available",
		"deepseek-v4-1-flash":      "channel this API key cannot use",
		"auto":                     "not allowed",
		"models/deepseek-v4-flash": "not available",
		"claude-fable-5-dd-" + reverse("deepseek-v4-flash"): "not available",
	} {
		gateErr := alice.checkModel(model)
		if gateErr == nil || !strings.Contains(gateErr.message, reason) {
			t.Errorf("alice %q: %v, want %q", model, gateErr, reason)
		}
	}
	// A list entry is shown in the form its handler decodes.
	for id, want := range map[string]bool{
		"models/deepseek-v4-flash":                          true,
		"claude-fable-5-dd-" + reverse("deepseek-v4-flash"): true,
		"claude-sonnet-4-6":                                 false,
		"deepseek-v4-1-flash":                               false,
	} {
		if got := alice.listedModelAllowed(id); got != want {
			t.Errorf("listedModelAllowed(%q) = %v, want %v", id, got, want)
		}
	}

	// A second channel registering an allowed name takes the model away (fail closed).
	registerTestModels(t, "openai-compatible-subscription-2", "deepseek-v4-flash")
	if gateErr := alice.checkModel("deepseek-v4-flash"); gateErr == nil || gateErr.code != "model_not_allowed" {
		t.Fatalf("shared model name must be denied: %v", gateErr)
	}
	if gateErr := alice.checkModel("DeepSeek-V4-Flash(high)"); gateErr == nil {
		t.Fatal("case and suffix variants must resolve like the router")
	}
}
