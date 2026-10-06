package z10

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/fsnotify/fsnotify"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	sdkapi "github.com/router-for-me/CLIProxyAPI/v8/sdk/api"
	log "github.com/sirupsen/logrus"
	"golang.org/x/crypto/bcrypt"
	"gopkg.in/yaml.v3"
)

const (
	// UsageFileName is the persisted friend usage file, resolved next to the main config file.
	UsageFileName = "z10-usage.json"

	defaultReloadDebounce = 250 * time.Millisecond
	defaultFlushDelay     = 5 * time.Second
)

// Options configures a Runtime.
type Options struct {
	// ConfigPath is the main config.yaml path; friends.yaml and z10-usage.json live next to it.
	ConfigPath string
	// Now overrides the clock (tests).
	Now func() time.Time
	// ReloadDebounce delays watcher-triggered reloads so editor write bursts coalesce.
	ReloadDebounce time.Duration
	// FlushDelay bounds how long usage changes may stay unsaved.
	FlushDelay time.Duration
	// OnWatchReload is called after every watcher-triggered reload (tests).
	OnWatchReload func(error)
}

// state is one immutable snapshot of owner settings and friend keys.
type state struct {
	owner *config.Config
	// parsed is the last accepted friends.yaml content.
	parsed *FriendSet
	// friends is parsed minus entries whose key equals an owner key.
	friends *FriendSet
}

// Runtime owns the friend key state, its watcher, and the usage store.
type Runtime struct {
	configPath  string
	friendsPath string
	now         func() time.Time
	debounce    time.Duration
	onReload    func(error)

	state atomic.Pointer[state]
	// mu serializes reloads and admin writes of friends.yaml, so a reload never stores
	// a snapshot older than a concurrent write.
	mu sync.Mutex

	// mgmt authenticates admin routes with the same logic as the management API.
	mgmt  *sdkapi.Handler
	usage *UsageStore

	startOnce     sync.Once
	closeOnce     sync.Once
	done          chan struct{}
	watcher       *fsnotify.Watcher
	flushOnSignal bool
}

// NewRuntime loads the current files and returns a runtime. Load problems are logged
// and leave the feature off (no friend keys) rather than failing startup.
func NewRuntime(opts Options) *Runtime {
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	debounce := opts.ReloadDebounce
	if debounce <= 0 {
		debounce = defaultReloadDebounce
	}
	dir := filepath.Dir(opts.ConfigPath)
	rt := &Runtime{
		configPath:  opts.ConfigPath,
		friendsPath: filepath.Join(dir, FriendsFileName),
		now:         now,
		debounce:    debounce,
		onReload:    opts.OnWatchReload,
		done:        make(chan struct{}),
	}
	rt.state.Store(&state{owner: &config.Config{}, parsed: emptyFriendSet(), friends: emptyFriendSet()})
	rt.mgmt = sdkapi.NewHandlerWithoutConfigFilePath(&config.Config{}, nil)
	rt.usage = NewUsageStore(filepath.Join(dir, UsageFileName), now, opts.FlushDelay)
	if errReload := rt.Reload(); errReload != nil {
		log.WithError(errReload).Error("z10: initial friend key load failed")
	}
	if errLoad := rt.usage.Load(); errLoad != nil {
		log.WithError(errLoad).Error("z10: failed to load friend usage")
	}
	return rt
}

// Friends returns the current friend key snapshot.
func (rt *Runtime) Friends() *FriendSet {
	return rt.state.Load().friends
}

// Usage returns the usage store.
func (rt *Runtime) Usage() *UsageStore {
	return rt.usage
}

// Reload re-reads config.yaml (owner keys and management settings) and friends.yaml.
// On a parse or validation error the previous good snapshot of that file is kept. Friend
// entries whose key equals an owner key are dropped on every reload, so an owner key is
// never restricted; the remaining entries stay active.
func (rt *Runtime) Reload() error {
	rt.mu.Lock()
	defer rt.mu.Unlock()

	previous := rt.state.Load()
	owner, errOwner := loadOwnerConfig(rt.configPath)
	if errOwner != nil {
		log.WithError(errOwner).Error("z10: failed to read owner config; keeping previous owner settings")
		owner = previous.owner
	}

	parsed, errFriends := loadFriends(rt.friendsPath)
	if errFriends != nil {
		log.WithError(errFriends).Error("z10: friends.yaml rejected; keeping last good friend keys")
		parsed = previous.parsed
	}
	errCollision := rt.storeLocked(previous, owner, parsed)
	return errors.Join(errOwner, errFriends, errCollision)
}

// storeLocked activates owner settings and parsed friends.yaml content. Friend entries
// whose key equals an owner key are dropped. rt.mu must be held.
func (rt *Runtime) storeLocked(previous *state, owner *config.Config, parsed *FriendSet) error {
	friends, dropped := parsed.withoutKeys(owner.APIKeys)
	var errCollision error
	if len(dropped) > 0 {
		errCollision = fmt.Errorf("friend keys equal to owner api keys were disabled: %s", strings.Join(dropped, ", "))
		log.WithField("friends", dropped).Error("z10: friend keys equal to owner api keys were disabled")
	}
	warnUnknownChannels(friends, owner)

	rt.state.Store(&state{owner: owner, parsed: parsed, friends: friends})
	if owner != previous.owner {
		rt.mgmt.SetConfig(owner)
	}
	if friendSetSummary(friends) != friendSetSummary(previous.friends) {
		log.WithField("friends", friends.Len()).Info("z10: friend keys loaded")
	}
	return errCollision
}

// warnUnknownChannels logs channels that name no openai-compatibility entry. Such a
// channel grants nothing, so this is a hint for typos, not an error.
func warnUnknownChannels(friends *FriendSet, owner *config.Config) {
	known := make(map[string]struct{}, len(owner.OpenAICompatibility))
	for _, compat := range owner.OpenAICompatibility {
		known[channelProviderKey(compat.Name)] = struct{}{}
	}
	for _, friend := range friends.Friends() {
		for _, channel := range friend.Channels {
			if _, ok := known[channelProviderKey(channel)]; !ok {
				log.WithFields(log.Fields{"friend": friend.Name, "channel": channel}).Warn("z10: channel matches no openai-compatibility entry")
			}
		}
	}
}

func friendSetSummary(set *FriendSet) string {
	var b strings.Builder
	for _, friend := range set.Friends() {
		fmt.Fprintf(&b, "%s|%v|%s|%s|%s;", friend.Name, friend.Enabled, friend.ExpiresRaw, strings.Join(friend.Models, ","), strings.Join(friend.Channels, ","))
	}
	return b.String()
}

func loadFriends(path string) (*FriendSet, error) {
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return emptyFriendSet(), nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read %s: %w", FriendsFileName, errRead)
	}
	return ParseFriends(data)
}

// loadOwnerConfig parses config.yaml (legacy or v8 layout) without the side effects of
// config.LoadConfig, which rewrites the file when it finds a plaintext management key.
func loadOwnerConfig(path string) (*config.Config, error) {
	cfg := &config.Config{}
	data, errRead := os.ReadFile(path)
	if errors.Is(errRead, fs.ErrNotExist) {
		return cfg, nil
	}
	if errRead != nil {
		return nil, fmt.Errorf("read config: %w", errRead)
	}
	if errParse := yaml.Unmarshal(data, cfg); errParse != nil {
		return nil, fmt.Errorf("parse config: %w", errParse)
	}
	// The management handler compares against a bcrypt hash. Upstream hashes a plaintext
	// secret on load; do the same in memory so the same secret is accepted.
	secret := cfg.RemoteManagement.SecretKey
	if secret != "" && !looksLikeBcrypt(secret) {
		hashed, errHash := bcrypt.GenerateFromPassword([]byte(secret), bcrypt.DefaultCost)
		if errHash != nil {
			return nil, fmt.Errorf("hash management key: %w", errHash)
		}
		cfg.RemoteManagement.SecretKey = string(hashed)
	}
	return cfg, nil
}

func looksLikeBcrypt(value string) bool {
	return len(value) > 4 && (value[:4] == "$2a$" || value[:4] == "$2b$" || value[:4] == "$2y$")
}

// Start launches the file watcher. It is idempotent.
func (rt *Runtime) Start() {
	rt.startOnce.Do(func() {
		if errWatch := rt.startWatcher(); errWatch != nil {
			log.WithError(errWatch).Error("z10: friends.yaml hot reload disabled")
		}
	})
}

// Close stops the watcher and flushes usage.
func (rt *Runtime) Close() {
	rt.closeOnce.Do(func() {
		close(rt.done)
		if rt.watcher != nil {
			if errClose := rt.watcher.Close(); errClose != nil {
				log.WithError(errClose).Warn("z10: failed to close watcher")
			}
		}
		if errFlush := rt.usage.Flush(); errFlush != nil {
			log.WithError(errFlush).Error("z10: failed to flush friend usage")
		}
	})
}

func (rt *Runtime) startWatcher() error {
	watcher, errNew := fsnotify.NewWatcher()
	if errNew != nil {
		return fmt.Errorf("create watcher: %w", errNew)
	}
	// Watch the directory, not the files: editors replace files by rename, which
	// would silently detach a file watch.
	if errAdd := watcher.Add(filepath.Dir(rt.friendsPath)); errAdd != nil {
		_ = watcher.Close()
		return fmt.Errorf("watch config directory: %w", errAdd)
	}
	rt.watcher = watcher
	go rt.watchLoop(watcher)
	return nil
}

func (rt *Runtime) watchLoop(watcher *fsnotify.Watcher) {
	friendsBase := filepath.Base(rt.friendsPath)
	configBase := filepath.Base(rt.configPath)
	var timer *time.Timer
	for {
		select {
		case <-rt.done:
			if timer != nil {
				timer.Stop()
			}
			return
		case event, ok := <-watcher.Events:
			if !ok {
				return
			}
			base := filepath.Base(event.Name)
			if base != friendsBase && base != configBase {
				continue
			}
			if timer == nil {
				timer = time.AfterFunc(rt.debounce, rt.watchReload)
			} else {
				timer.Reset(rt.debounce)
			}
		case errWatch, ok := <-watcher.Errors:
			if !ok {
				return
			}
			log.WithError(errWatch).Warn("z10: config directory watcher error")
		}
	}
}

func (rt *Runtime) watchReload() {
	select {
	case <-rt.done:
		return
	default:
	}
	errReload := rt.Reload()
	if rt.onReload != nil {
		rt.onReload(errReload)
	}
}
