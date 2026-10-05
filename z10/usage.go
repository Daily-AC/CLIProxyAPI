package z10

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	coreusage "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/usage"
	log "github.com/sirupsen/logrus"
)

const (
	usageFileVersion = 1
	// maxModelBuckets caps distinct per-friend model buckets; further models share
	// otherModelBucket.
	maxModelBuckets  = 200
	otherModelBucket = "_other"
	// maxDayBuckets keeps this many most recent days per friend.
	maxDayBuckets = 400
)

// Counters is one usage bucket.
type Counters struct {
	Requests        int64 `json:"requests"`
	FailedRequests  int64 `json:"failed_requests"`
	InputTokens     int64 `json:"input_tokens"`
	OutputTokens    int64 `json:"output_tokens"`
	CachedTokens    int64 `json:"cached_tokens"`
	ReasoningTokens int64 `json:"reasoning_tokens"`
	TotalTokens     int64 `json:"total_tokens"`
}

func (c *Counters) addTokens(detail coreusage.Detail) {
	c.InputTokens += detail.InputTokens
	c.OutputTokens += detail.OutputTokens
	c.ReasoningTokens += detail.ReasoningTokens
	c.CachedTokens += max(detail.CachedTokens, detail.CacheReadTokens)
	total := detail.TotalTokens
	if total == 0 {
		total = detail.InputTokens + detail.OutputTokens
	}
	c.TotalTokens += total
}

// FriendUsage aggregates one friend's usage; days are UTC+8 dates (YYYY-MM-DD).
type FriendUsage struct {
	Counters
	LastUsed *time.Time           `json:"last_used,omitempty"`
	Models   map[string]*Counters `json:"models"`
	Days     map[string]*Counters `json:"days"`
}

// UsageReport is the persisted file and the /z10/usage response body.
type UsageReport struct {
	Version   int                     `json:"version"`
	UpdatedAt time.Time               `json:"updated_at"`
	Friends   map[string]*FriendUsage `json:"friends"`
}

// UsageStore aggregates friend usage in memory and persists it atomically. Changes are
// written at most FlushDelay after they happen, and immediately once shutdown begins.
type UsageStore struct {
	path       string
	now        func() time.Time
	flushDelay time.Duration

	mu        sync.Mutex
	friends   map[string]*FriendUsage
	updatedAt time.Time
	dirty     bool
	timer     *time.Timer

	writeMu   sync.Mutex
	immediate atomic.Bool
}

// NewUsageStore creates an empty store backed by path.
func NewUsageStore(path string, now func() time.Time, flushDelay time.Duration) *UsageStore {
	if now == nil {
		now = time.Now
	}
	if flushDelay <= 0 {
		flushDelay = defaultFlushDelay
	}
	return &UsageStore{path: path, now: now, flushDelay: flushDelay, friends: map[string]*FriendUsage{}}
}

// Load replaces the in-memory state with the persisted file. A corrupt file is moved
// aside instead of being overwritten by the next flush.
func (s *UsageStore) Load() error {
	data, errRead := os.ReadFile(s.path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return nil
	}
	if errRead != nil {
		return fmt.Errorf("read usage file: %w", errRead)
	}
	var report UsageReport
	if errParse := json.Unmarshal(data, &report); errParse != nil || report.Version != usageFileVersion {
		aside := fmt.Sprintf("%s.corrupt-%d", s.path, s.now().Unix())
		if errRename := os.Rename(s.path, aside); errRename != nil {
			return fmt.Errorf("unreadable usage file and failed to move it aside: %w", errRename)
		}
		if errParse == nil {
			errParse = fmt.Errorf("unsupported version %d", report.Version)
		}
		return fmt.Errorf("unreadable usage file moved to %s: %w", aside, errParse)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.friends = map[string]*FriendUsage{}
	for name, entry := range report.Friends {
		if entry == nil {
			continue
		}
		if entry.Models == nil {
			entry.Models = map[string]*Counters{}
		}
		if entry.Days == nil {
			entry.Days = map[string]*Counters{}
		}
		s.friends[name] = entry
	}
	s.updatedAt = report.UpdatedAt
	return nil
}

// RecordRequest counts one inbound model request of a friend.
func (s *UsageStore) RecordRequest(name, model string, failed bool) {
	s.update(name, model, true, func(c *Counters) {
		c.Requests++
		if failed {
			c.FailedRequests++
		}
	})
}

// RecordTokens adds the token usage of one upstream attempt.
func (s *UsageStore) RecordTokens(name, model string, detail coreusage.Detail) {
	s.update(name, model, false, func(c *Counters) { c.addTokens(detail) })
}

// Touch updates last_used for requests that carry no model (model lists).
func (s *UsageStore) Touch(name string) {
	s.update(name, "", true, nil)
}

func (s *UsageStore) update(name, model string, used bool, apply func(*Counters)) {
	now := s.now()
	s.mu.Lock()
	entry := s.friends[name]
	if entry == nil {
		entry = &FriendUsage{Models: map[string]*Counters{}, Days: map[string]*Counters{}}
		s.friends[name] = entry
	}
	if used {
		stamp := now.In(usageZone)
		entry.LastUsed = &stamp
	}
	if apply != nil {
		apply(&entry.Counters)
		apply(bucket(entry.Models, modelBucketKey(entry.Models, model)))
		apply(bucket(entry.Days, now.In(usageZone).Format("2006-01-02")))
		trimDays(entry.Days)
	}
	s.updatedAt = now
	s.dirty = true
	scheduleFlush := !s.immediate.Load() && s.timer == nil
	if scheduleFlush {
		s.timer = time.AfterFunc(s.flushDelay, s.flushFromTimer)
	}
	s.mu.Unlock()
	if s.immediate.Load() {
		if errFlush := s.Flush(); errFlush != nil {
			log.WithError(errFlush).Error("z10: failed to flush friend usage")
		}
	}
}

// modelBucketKey returns the bucket for model, folding new models into otherModelBucket
// once maxModelBuckets distinct models exist.
func modelBucketKey(models map[string]*Counters, model string) string {
	if model == "" {
		model = "unknown"
	}
	if _, exists := models[model]; exists {
		return model
	}
	distinct := len(models)
	if _, hasOther := models[otherModelBucket]; hasOther {
		distinct--
	}
	if distinct >= maxModelBuckets {
		return otherModelBucket
	}
	return model
}

// trimDays drops the oldest day buckets beyond maxDayBuckets (keys are YYYY-MM-DD).
func trimDays(days map[string]*Counters) {
	if len(days) <= maxDayBuckets {
		return
	}
	keys := make([]string, 0, len(days))
	for key := range days {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys[:len(keys)-maxDayBuckets] {
		delete(days, key)
	}
}

func bucket(buckets map[string]*Counters, key string) *Counters {
	counters := buckets[key]
	if counters == nil {
		counters = &Counters{}
		buckets[key] = counters
	}
	return counters
}

func (s *UsageStore) flushFromTimer() {
	if errFlush := s.Flush(); errFlush != nil {
		log.WithError(errFlush).Error("z10: failed to flush friend usage")
	}
}

// FlushOnEveryChange switches to synchronous writes; used once shutdown begins.
func (s *UsageStore) FlushOnEveryChange() {
	s.immediate.Store(true)
}

// Report returns a deep copy of the current usage.
func (s *UsageStore) Report() UsageReport {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.reportLocked()
}

func (s *UsageStore) reportLocked() UsageReport {
	report := UsageReport{Version: usageFileVersion, UpdatedAt: s.updatedAt, Friends: make(map[string]*FriendUsage, len(s.friends))}
	for name, entry := range s.friends {
		clone := &FriendUsage{Counters: entry.Counters, Models: cloneBuckets(entry.Models), Days: cloneBuckets(entry.Days)}
		if entry.LastUsed != nil {
			lastUsed := *entry.LastUsed
			clone.LastUsed = &lastUsed
		}
		report.Friends[name] = clone
	}
	return report
}

func cloneBuckets(in map[string]*Counters) map[string]*Counters {
	out := make(map[string]*Counters, len(in))
	for key, counters := range in {
		copied := *counters
		out[key] = &copied
	}
	return out
}

// Flush writes pending changes atomically (temp file, fsync, rename).
func (s *UsageStore) Flush() error {
	s.writeMu.Lock()
	defer s.writeMu.Unlock()

	s.mu.Lock()
	if s.timer != nil {
		s.timer.Stop()
		s.timer = nil
	}
	if !s.dirty {
		s.mu.Unlock()
		return nil
	}
	data, errMarshal := json.MarshalIndent(s.reportLocked(), "", "  ")
	s.dirty = false
	s.mu.Unlock()
	if errMarshal != nil {
		return fmt.Errorf("encode usage: %w", errMarshal)
	}
	if errWrite := writeFileAtomic(s.path, data); errWrite != nil {
		s.mu.Lock()
		s.dirty = true
		if s.timer == nil && !s.immediate.Load() {
			s.timer = time.AfterFunc(s.flushDelay, s.flushFromTimer)
		}
		s.mu.Unlock()
		return errWrite
	}
	return nil
}

func writeFileAtomic(path string, data []byte) (err error) {
	temp, errCreate := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".tmp-*")
	if errCreate != nil {
		return fmt.Errorf("create temp usage file: %w", errCreate)
	}
	tempPath := temp.Name()
	defer func() {
		if err != nil {
			_ = os.Remove(tempPath)
		}
	}()
	if errChmod := temp.Chmod(0o600); errChmod != nil {
		_ = temp.Close()
		return fmt.Errorf("chmod temp usage file: %w", errChmod)
	}
	if _, errWrite := temp.Write(append(data, '\n')); errWrite != nil {
		_ = temp.Close()
		return fmt.Errorf("write temp usage file: %w", errWrite)
	}
	if errSync := temp.Sync(); errSync != nil {
		_ = temp.Close()
		return fmt.Errorf("sync temp usage file: %w", errSync)
	}
	if errClose := temp.Close(); errClose != nil {
		return fmt.Errorf("close temp usage file: %w", errClose)
	}
	if errRename := os.Rename(tempPath, path); errRename != nil {
		return fmt.Errorf("replace usage file: %w", errRename)
	}
	return nil
}

// usagePlugin receives upstream usage records. It only aggregates principals of the
// friend provider; owner principals are raw API keys and are never stored or logged.
type usagePlugin struct {
	rt *Runtime
}

func (p *usagePlugin) HandleUsage(_ context.Context, record coreusage.Record) {
	name, isFriend := strings.CutPrefix(record.APIKey, PrincipalPrefix)
	if !isFriend || p.rt.Friends().ByName(name) == nil {
		return
	}
	model := record.Alias
	if strings.TrimSpace(model) == "" {
		model = record.Model
	}
	p.rt.usage.RecordTokens(name, NormalizeModel(model), record.Detail)
}
