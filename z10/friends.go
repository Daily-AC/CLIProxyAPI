// Package z10 adds restricted "friend keys" to the z10 deployment of CLIProxyAPI.
//
// Friend keys are defined in friends.yaml next to the main config file. They may only
// call allowlisted models on allowlisted endpoints, and their usage is aggregated by
// friend name. Owner keys (api-keys in config.yaml) are not affected.
package z10

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/http"
	"regexp"
	"strings"
	"time"
	"unicode"

	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"gopkg.in/yaml.v3"
)

const (
	// FriendsFileName is the friend key file, resolved next to the main config file.
	FriendsFileName = "friends.yaml"
	// PrincipalPrefix prefixes the access principal of every authenticated friend key.
	PrincipalPrefix = "friend:"
	// minKeyLength rejects short keys, which are easy to guess.
	minKeyLength = 16
)

// usageZone is the time zone used for date-only expiry values and per-day usage buckets.
var usageZone = time.FixedZone("UTC+8", 8*60*60)

var friendNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_.-]{1,64}$`)

// friendNameRule describes friendNamePattern and the dot rule in error messages.
const friendNameRule = "1-64 letters, digits, '_', '.' or '-', not only dots"

// validFriendName reports whether name may name a friend. Names made only of dots are
// rejected because URL normalization removes them from /z10/friends/<name>.
func validFriendName(name string) bool {
	return friendNamePattern.MatchString(name) && strings.Trim(name, ".") != ""
}

// Friend is one validated friend key.
type Friend struct {
	Name string
	// Models holds the model patterns as written in friends.yaml.
	Models []string
	// Channels holds the openai-compatibility channel names as written in friends.yaml.
	Channels []string
	// ExpiresRaw is the configured expiry text; empty means the key never expires.
	ExpiresRaw string
	// ExpiresAt is the first instant at which the key is no longer valid.
	ExpiresAt time.Time
	Enabled   bool

	key      string
	patterns []string
	// providers holds the registry provider keys of Channels.
	providers map[string]struct{}
}

// Inactive states reported by the admin API.
const (
	inactiveDisabled = "disabled"
	inactiveExpired  = "expired"
)

// inactiveState returns inactiveDisabled, inactiveExpired, or "" for an active key.
func (f *Friend) inactiveState(now time.Time) string {
	if !f.Enabled {
		return inactiveDisabled
	}
	if !f.ExpiresAt.IsZero() && !now.Before(f.ExpiresAt) {
		return inactiveExpired
	}
	return ""
}

// inactiveReason returns a non-empty message when the key must be rejected with 401.
func (f *Friend) inactiveReason(now time.Time) string {
	switch f.inactiveState(now) {
	case inactiveDisabled:
		return "API key disabled"
	case inactiveExpired:
		return "API key expired"
	}
	return ""
}

// matchesPattern reports whether a normalized model name matches one of the patterns.
func (f *Friend) matchesPattern(normalized string) bool {
	if normalized == "" {
		return false
	}
	for _, pattern := range f.patterns {
		if globMatch(pattern, normalized) {
			return true
		}
	}
	return false
}

// FriendSet is an immutable snapshot of all friend keys.
type FriendSet struct {
	list   []*Friend
	byKey  map[string]*Friend
	byName map[string]*Friend
}

func emptyFriendSet() *FriendSet {
	return &FriendSet{byKey: map[string]*Friend{}, byName: map[string]*Friend{}}
}

// Len returns the number of friend keys.
func (s *FriendSet) Len() int {
	if s == nil {
		return 0
	}
	return len(s.list)
}

// Friends returns the friends in file order.
func (s *FriendSet) Friends() []*Friend {
	if s == nil {
		return nil
	}
	return append([]*Friend(nil), s.list...)
}

// ByName returns the friend with the given name.
func (s *FriendSet) ByName(name string) *Friend {
	if s == nil {
		return nil
	}
	return s.byName[name]
}

// withoutKeys returns a copy without the friends whose key is in keys, plus the names
// of the dropped friends.
func (s *FriendSet) withoutKeys(keys []string) (*FriendSet, []string) {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		if trimmed := strings.TrimSpace(key); trimmed != "" {
			blocked[trimmed] = struct{}{}
		}
	}
	out := emptyFriendSet()
	var dropped []string
	for _, friend := range s.Friends() {
		if _, isBlocked := blocked[friend.key]; isBlocked {
			dropped = append(dropped, friend.Name)
			continue
		}
		out.list = append(out.list, friend)
		out.byKey[friend.key] = friend
		out.byName[friend.Name] = friend
	}
	return out, dropped
}

// match returns the first credential of the request that is a friend key, using the
// same credential sources and order as the built-in config API key provider.
func (s *FriendSet) match(r *http.Request) (*Friend, string) {
	if s == nil || len(s.byKey) == 0 || r == nil {
		return nil, ""
	}
	for _, candidate := range requestCredentials(r) {
		if friend, ok := s.byKey[candidate.value]; ok {
			return friend, candidate.source
		}
	}
	return nil, ""
}

type credential struct {
	value  string
	source string
}

// requestCredentials mirrors internal/access/config_access so that the middleware and
// the access provider recognize exactly the keys that upstream authentication sees.
func requestCredentials(r *http.Request) []credential {
	queryKey, queryAuthToken := "", ""
	if r.URL != nil {
		query := r.URL.Query()
		queryKey = query.Get("key")
		queryAuthToken = query.Get("auth_token")
	}
	candidates := []credential{
		{extractBearerToken(r.Header.Get("Authorization")), "authorization"},
		{r.Header.Get("X-Goog-Api-Key"), "x-goog-api-key"},
		{r.Header.Get("X-Api-Key"), "x-api-key"},
		{queryKey, "query-key"},
		{queryAuthToken, "query-auth-token"},
	}
	out := candidates[:0]
	for _, candidate := range candidates {
		if candidate.value != "" {
			out = append(out, candidate)
		}
	}
	return out
}

func extractBearerToken(header string) string {
	if header == "" {
		return ""
	}
	parts := strings.SplitN(header, " ", 2)
	if len(parts) != 2 || strings.ToLower(parts[0]) != "bearer" {
		return header
	}
	return strings.TrimSpace(parts[1])
}

type friendEntry struct {
	Name     string   `yaml:"name"`
	Key      string   `yaml:"key"`
	Models   []string `yaml:"models"`
	Channels []string `yaml:"channels"`
	Expires  string   `yaml:"expires,omitempty"`
	Enabled  *bool    `yaml:"enabled"`
}

type friendsDocument struct {
	Keys []friendEntry `yaml:"keys"`
}

// ParseFriends parses and validates friends.yaml content. Error messages never contain
// keys. Collisions with owner API keys are handled by the runtime, which drops only the
// colliding entries.
func ParseFriends(data []byte) (*FriendSet, error) {
	var doc friendsDocument
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	if errDecode := decoder.Decode(&doc); errDecode != nil && !errors.Is(errDecode, io.EOF) {
		return nil, fmt.Errorf("parse %s: %s", FriendsFileName, redactYAMLError(errDecode))
	}

	set := emptyFriendSet()
	var problems []string
	for index, entry := range doc.Keys {
		label := fmt.Sprintf("keys[%d]", index)
		name := strings.TrimSpace(entry.Name)
		if name != "" {
			label = fmt.Sprintf("keys[%d] (%s)", index, name)
		}
		if !validFriendName(name) {
			problems = append(problems, label+": name must be "+friendNameRule)
			continue
		}
		if _, exists := set.byName[name]; exists {
			problems = append(problems, label+": duplicate name")
			continue
		}
		key := entry.Key
		if key == "" {
			problems = append(problems, label+": key is empty")
			continue
		}
		if strings.IndexFunc(key, unicode.IsSpace) >= 0 {
			problems = append(problems, label+": key must not contain whitespace")
			continue
		}
		if len(key) < minKeyLength {
			problems = append(problems, fmt.Sprintf("%s: key must be at least %d characters", label, minKeyLength))
			continue
		}
		if other, exists := set.byKey[key]; exists {
			problems = append(problems, fmt.Sprintf("%s: key duplicates the key of %s", label, other.Name))
			continue
		}
		friend := &Friend{Name: name, key: key, Enabled: entry.Enabled == nil || *entry.Enabled, providers: map[string]struct{}{}}
		if len(entry.Channels) == 0 {
			problems = append(problems, label+": channels is required (openai-compatibility names this key may use)")
			continue
		}
		channelsOK := true
		for _, channel := range entry.Channels {
			channel = strings.TrimSpace(channel)
			if channel == "" {
				problems = append(problems, label+": empty channel name")
				channelsOK = false
				break
			}
			friend.Channels = append(friend.Channels, channel)
			friend.providers[channelProviderKey(channel)] = struct{}{}
		}
		if !channelsOK {
			continue
		}
		modelsOK := true
		for _, model := range entry.Models {
			pattern := strings.ToLower(strings.TrimSpace(model))
			if pattern == "" {
				problems = append(problems, label+": empty model pattern")
				modelsOK = false
				break
			}
			friend.Models = append(friend.Models, strings.TrimSpace(model))
			friend.patterns = append(friend.patterns, pattern)
		}
		if !modelsOK {
			continue
		}
		if raw := strings.TrimSpace(entry.Expires); raw != "" {
			expiresAt, errExpires := parseExpiry(raw)
			if errExpires != nil {
				problems = append(problems, label+": "+errExpires.Error())
				continue
			}
			friend.ExpiresRaw = raw
			friend.ExpiresAt = expiresAt
		}
		set.list = append(set.list, friend)
		set.byKey[key] = friend
		set.byName[name] = friend
	}
	if len(problems) > 0 {
		return nil, fmt.Errorf("invalid %s: %s", FriendsFileName, strings.Join(problems, "; "))
	}
	return set, nil
}

// channelProviderKey maps an openai-compatibility channel name to the provider key under
// which upstream registers its models and schedules its credentials.
func channelProviderKey(channel string) string {
	return util.OpenAICompatibleProviderKey(channel)
}

// parseExpiry accepts a date (valid through the end of that day in UTC+8) or RFC3339.
func parseExpiry(raw string) (time.Time, error) {
	if day, errDate := time.ParseInLocation("2006-01-02", raw, usageZone); errDate == nil {
		return day.AddDate(0, 0, 1), nil
	}
	if instant, errRFC := time.Parse(time.RFC3339, raw); errRFC == nil {
		return instant, nil
	}
	return time.Time{}, fmt.Errorf("expires %q must be YYYY-MM-DD or RFC3339", raw)
}

var yamlQuotedValue = regexp.MustCompile("`[^`]*`")

// redactYAMLError removes quoted scalar values, which could contain a key, from yaml errors.
func redactYAMLError(err error) string {
	return yamlQuotedValue.ReplaceAllString(err.Error(), "`<redacted>`")
}

// NormalizeModel turns a requested or listed model name into the form matched against
// friend patterns: surrounding spaces are trimmed, a leading "models/" is removed, Claude
// list-cloaked IDs (claude-fable-5-dd-<reversed>) are decoded, one thinking suffix
// "name(value)" is stripped, and the result is lowercased. Patterns only narrow names;
// which channel serves a model is checked separately against the model registry.
func NormalizeModel(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "models/")
	model = claudemodels.ResolveClaudeModelIDPrefix(model)
	model = thinking.ParseSuffix(model).ModelName
	return strings.ToLower(strings.TrimSpace(model))
}

// globMatch matches s against pattern where '*' matches any sequence (including '/').
func globMatch(pattern, s string) bool {
	p, i := 0, 0
	star, resume := -1, 0
	for i < len(s) {
		switch {
		case p < len(pattern) && pattern[p] == '*':
			star, resume = p, i
			p++
		case p < len(pattern) && pattern[p] == s[i]:
			p++
			i++
		case star >= 0:
			p = star + 1
			resume++
			i = resume
		default:
			return false
		}
	}
	for p < len(pattern) && pattern[p] == '*' {
		p++
	}
	return p == len(pattern)
}
