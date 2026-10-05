package z10

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
)

func TestUsageAggregationAndPersistence(t *testing.T) {
	// 16:30 UTC is already the next day in UTC+8.
	clock := newFakeClock(time.Date(2026, 10, 5, 16, 30, 0, 0, time.UTC))
	rt := newTestRuntime(t, testConfigYAML, testFriendsYAML, clock)
	plugin := &usagePlugin{rt: rt}
	store := rt.Usage()

	store.RecordRequest("alice", "deepseek-v4-flash", false)
	store.RecordRequest("alice", "deepseek-v4-flash", true)
	plugin.HandleUsage(t.Context(), coreusage.Record{
		APIKey: "friend:alice", Alias: "DeepSeek-V4-Flash(high)", Model: "deepseek-chat",
		Detail: coreusage.Detail{InputTokens: 100, OutputTokens: 20, CachedTokens: 30, ReasoningTokens: 5, TotalTokens: 120},
	})
	plugin.HandleUsage(t.Context(), coreusage.Record{
		APIKey: "friend:alice", Model: "deepseek-v4-pro",
		Detail: coreusage.Detail{InputTokens: 10, OutputTokens: 2, CacheReadTokens: 4},
	})
	// Owner principals are raw keys and unknown friends are ignored.
	plugin.HandleUsage(t.Context(), coreusage.Record{APIKey: ownerKey, Model: "claude-sonnet-4-6", Detail: coreusage.Detail{InputTokens: 999}})
	plugin.HandleUsage(t.Context(), coreusage.Record{APIKey: "friend:mallory", Model: "x", Detail: coreusage.Detail{InputTokens: 999}})

	report := store.Report()
	if len(report.Friends) != 1 {
		t.Fatalf("friends in report = %v", report.Friends)
	}
	alice := report.Friends["alice"]
	want := Counters{Requests: 2, FailedRequests: 1, InputTokens: 110, OutputTokens: 22, CachedTokens: 34, ReasoningTokens: 5, TotalTokens: 132}
	if alice.Counters != want {
		t.Fatalf("alice totals = %+v, want %+v", alice.Counters, want)
	}
	if flash := alice.Models["deepseek-v4-flash"]; flash.Requests != 2 || flash.InputTokens != 100 {
		t.Fatalf("per-model = %+v", flash)
	}
	if pro := alice.Models["deepseek-v4-pro"]; pro.TotalTokens != 12 {
		t.Fatalf("total falls back to input+output: %+v", pro)
	}
	if day := alice.Days["2026-10-06"]; day == nil || day.Requests != 2 || day.TotalTokens != 132 {
		t.Fatalf("per-day (UTC+8) = %+v", alice.Days)
	}
	if alice.LastUsed == nil || !alice.LastUsed.Equal(clock.Now()) {
		t.Fatalf("last_used = %v", alice.LastUsed)
	}

	if errFlush := store.Flush(); errFlush != nil {
		t.Fatal(errFlush)
	}
	data, errRead := os.ReadFile(store.path)
	if errRead != nil {
		t.Fatal(errRead)
	}
	if strings.Contains(string(data), ownerKey) || strings.Contains(string(data), aliceKey) {
		t.Fatal("usage file must not contain keys")
	}
	info, errStat := os.Stat(store.path)
	if errStat != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("usage file mode = %v (%v)", info.Mode(), errStat)
	}

	reloaded := NewUsageStore(store.path, clock.Now, time.Hour)
	if errLoad := reloaded.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	before, _ := json.Marshal(report)
	after, _ := json.Marshal(reloaded.Report())
	if string(before) != string(after) {
		t.Fatalf("round trip mismatch:\n%s\n%s", before, after)
	}
}

func TestUsageCorruptFileMovedAside(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, UsageFileName)
	writeTestFile(t, path, "{not json")
	store := NewUsageStore(path, newFakeClock(testNow()).Now, time.Hour)
	if errLoad := store.Load(); errLoad == nil {
		t.Fatal("expected error for corrupt usage file")
	}
	if _, errStat := os.Stat(path); !os.IsNotExist(errStat) {
		t.Fatal("corrupt file must be moved aside")
	}
	matches, _ := filepath.Glob(path + ".corrupt-*")
	if len(matches) != 1 {
		t.Fatalf("aside files = %v", matches)
	}
}

func TestUsageFlushOnEveryChange(t *testing.T) {
	dir := t.TempDir()
	store := NewUsageStore(filepath.Join(dir, UsageFileName), newFakeClock(testNow()).Now, time.Hour)
	store.FlushOnEveryChange()
	store.RecordRequest("alice", "deepseek-v4-flash", false)
	reloaded := NewUsageStore(store.path, time.Now, time.Hour)
	if errLoad := reloaded.Load(); errLoad != nil {
		t.Fatal(errLoad)
	}
	if reloaded.Report().Friends["alice"].Requests != 1 {
		t.Fatal("change was not written synchronously")
	}
}
