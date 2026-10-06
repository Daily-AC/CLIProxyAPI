package z10

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	log "github.com/sirupsen/logrus"
)

// maxAdminBodyBytes caps admin request bodies.
const maxAdminBodyBytes = 1 << 20

// responseBodyOverrideKey is the gin context key whose value the upstream request-log
// middleware (internal/api/middleware) records instead of the response body.
const responseBodyOverrideKey = "RESPONSE_BODY_OVERRIDE"

// adminNamePattern is the name rule for friends created through the admin API. It is
// stricter than the file loader, which also accepts names written by hand.
var adminNamePattern = regexp.MustCompile(`^[\p{L}\p{N}_.-]{1,32}$`)

// friendView is the public description of a friend key. It never includes the key.
type friendView struct {
	Name           string     `json:"name"`
	Models         []string   `json:"models"`
	Channels       []string   `json:"channels"`
	Expires        string     `json:"expires,omitempty"`
	Enabled        bool       `json:"enabled"`
	Active         bool       `json:"active"`
	InactiveReason string     `json:"inactive_reason"`
	LastUsed       *time.Time `json:"last_used,omitempty"`
	Requests       int64      `json:"requests"`
	FailedRequests int64      `json:"failed_requests"`
	TotalTokens    int64      `json:"total_tokens"`
}

// channelView is an openai-compatibility channel a friend key may be granted.
type channelView struct {
	Name   string   `json:"name"`
	Models []string `json:"models"`
}

type createFriendRequest struct {
	Name     string   `json:"name"`
	Channels []string `json:"channels"`
	Models   []string `json:"models"`
	Expires  string   `json:"expires"`
	Enabled  *bool    `json:"enabled"`
}

// updateFriendRequest fields that are absent or null are left unchanged.
type updateFriendRequest struct {
	Enabled  *bool    `json:"enabled"`
	Expires  *string  `json:"expires"`
	Channels []string `json:"channels"`
	Models   []string `json:"models"`
}

// registerAdminRoutes adds the /z10 admin API, guarded by the management key exactly
// like the management API (same handler and config fields).
func (rt *Runtime) registerAdminRoutes(engine *gin.Engine) {
	group := engine.Group("/z10", noStore, rt.managementAvailable, rt.mgmt.Middleware())
	group.GET("/usage", rt.handleUsage)
	group.GET("/channels", rt.handleChannels)
	group.GET("/friends", rt.handleFriends)
	group.POST("/friends", requireJSON, rt.handleCreateFriend)
	group.PATCH("/friends/:name", requireJSON, rt.handleUpdateFriend)
	group.DELETE("/friends/:name", requireJSON, rt.handleDeleteFriend)
}

// managementAvailable mirrors the server's availability gate: the management API is not
// served when the node is controlled by CLIProxyAPIHome.
func (rt *Runtime) managementAvailable(c *gin.Context) {
	if owner := rt.state.Load().owner; owner != nil && owner.Home.Enabled {
		c.AbortWithStatus(http.StatusNotFound)
		return
	}
	c.Next()
}

func noStore(c *gin.Context) {
	c.Header("Cache-Control", "no-store")
	c.Next()
}

// requireJSON rejects writes a plain HTML form could send cross-site. A DELETE carries
// no body and may omit Content-Type.
func requireJSON(c *gin.Context) {
	contentType := c.GetHeader("Content-Type")
	if contentType == "" && c.Request.Method == http.MethodDelete {
		c.Next()
		return
	}
	if mediaType, _, errParse := mime.ParseMediaType(contentType); errParse != nil || mediaType != "application/json" {
		abortAdmin(c, http.StatusUnsupportedMediaType, "Content-Type must be application/json")
		return
	}
	c.Next()
}

func abortAdmin(c *gin.Context, status int, message string) {
	c.AbortWithStatusJSON(status, gin.H{"error": message})
}

// decodeAdminJSON decodes a body that must be exactly one JSON object with known fields.
func decodeAdminJSON(c *gin.Context, target any) error {
	body, errRead := io.ReadAll(http.MaxBytesReader(c.Writer, c.Request.Body, maxAdminBodyBytes))
	if errRead != nil {
		return fmt.Errorf("failed to read request body (limit %d bytes)", maxAdminBodyBytes)
	}
	if trimmed := bytes.TrimSpace(body); len(trimmed) == 0 || trimmed[0] != '{' {
		return fmt.Errorf("request body must be a JSON object")
	}
	decoder := json.NewDecoder(bytes.NewReader(body))
	decoder.DisallowUnknownFields()
	if errDecode := decoder.Decode(target); errDecode != nil {
		return fmt.Errorf("invalid request body: %v", errDecode)
	}
	if _, errExtra := decoder.Token(); errExtra != io.EOF {
		return fmt.Errorf("request body must contain a single JSON object")
	}
	return nil
}

func (rt *Runtime) handleUsage(c *gin.Context) {
	c.JSON(http.StatusOK, rt.usage.Report())
}

func (rt *Runtime) handleChannels(c *gin.Context) {
	names := enabledChannels(rt.state.Load().owner)
	views := make([]channelView, 0, len(names))
	for _, name := range names {
		models := []string{}
		for _, info := range registry.GetGlobalRegistry().GetAvailableModelsByProvider(channelProviderKey(name)) {
			if info != nil && info.ID != "" {
				models = append(models, info.ID)
			}
		}
		sort.Strings(models)
		views = append(views, channelView{Name: name, Models: models})
	}
	c.JSON(http.StatusOK, gin.H{"channels": views})
}

func (rt *Runtime) handleFriends(c *gin.Context) {
	now := rt.now()
	report := rt.usage.Report()
	friends := rt.Friends().Friends()
	views := make([]friendView, 0, len(friends))
	for _, friend := range friends {
		views = append(views, newFriendView(friend, now, report.Friends[friend.Name]))
	}
	c.JSON(http.StatusOK, gin.H{"friends": views})
}

func (rt *Runtime) handleCreateFriend(c *gin.Context) {
	var req createFriendRequest
	if errDecode := decodeAdminJSON(c, &req); errDecode != nil {
		abortAdmin(c, http.StatusBadRequest, errDecode.Error())
		return
	}
	if !adminNamePattern.MatchString(req.Name) || !validFriendName(req.Name) {
		abortAdmin(c, http.StatusBadRequest, "name must be 1-32 letters, digits, '_', '.' or '-', not only dots")
		return
	}
	if req.Models == nil {
		req.Models = []string{"*"}
	}
	models, errModels := cleanModels(req.Models)
	if errModels != nil {
		abortAdmin(c, http.StatusBadRequest, errModels.Error())
		return
	}
	expires, errExpires := cleanExpires(req.Expires)
	if errExpires != nil {
		abortAdmin(c, http.StatusBadRequest, errExpires.Error())
		return
	}
	enabled := req.Enabled == nil || *req.Enabled
	key, errKey := newFriendKey()
	if errKey != nil {
		log.WithError(errKey).Error("z10: failed to generate a friend key")
		abortAdmin(c, http.StatusInternalServerError, "failed to generate a key")
		return
	}

	set, errEdit := rt.editFriends(req.Name, func(entries []friendEntry, owner *config.Config) ([]friendEntry, *adminError) {
		if findEntry(entries, req.Name) >= 0 {
			return nil, &adminError{http.StatusConflict, fmt.Sprintf("friend %q already exists", req.Name)}
		}
		channels, errChannels := resolveChannels(owner, req.Channels)
		if errChannels != nil {
			return nil, &adminError{http.StatusBadRequest, errChannels.Error()}
		}
		return append(entries, friendEntry{Name: req.Name, Key: key, Models: models, Channels: channels, Expires: expires, Enabled: &enabled}), nil
	})
	if errEdit != nil {
		abortAdmin(c, errEdit.status, errEdit.message)
		return
	}
	log.WithField("name", req.Name).Info("z10: friend created")
	view := rt.viewOf(set.ByName(req.Name))
	// This is the only response that carries a key; keep it out of request logs.
	c.Set(responseBodyOverrideKey, mustMarshal(gin.H{"friend": view, "key": "<redacted>"}))
	c.JSON(http.StatusCreated, gin.H{"friend": view, "key": key})
}

func (rt *Runtime) handleUpdateFriend(c *gin.Context) {
	name := c.Param("name")
	var req updateFriendRequest
	if errDecode := decodeAdminJSON(c, &req); errDecode != nil {
		abortAdmin(c, http.StatusBadRequest, errDecode.Error())
		return
	}
	var models []string
	if req.Models != nil {
		var errModels error
		if models, errModels = cleanModels(req.Models); errModels != nil {
			abortAdmin(c, http.StatusBadRequest, errModels.Error())
			return
		}
	}
	var expires string
	if req.Expires != nil {
		var errExpires error
		if expires, errExpires = cleanExpires(*req.Expires); errExpires != nil {
			abortAdmin(c, http.StatusBadRequest, errExpires.Error())
			return
		}
	}

	set, errEdit := rt.editFriends(name, func(entries []friendEntry, owner *config.Config) ([]friendEntry, *adminError) {
		index := findEntry(entries, name)
		if index < 0 {
			return nil, &adminError{http.StatusNotFound, fmt.Sprintf("friend %q not found", name)}
		}
		entry := &entries[index]
		if req.Channels != nil {
			channels, errChannels := resolveChannels(owner, req.Channels)
			if errChannels != nil {
				return nil, &adminError{http.StatusBadRequest, errChannels.Error()}
			}
			entry.Channels = channels
		}
		if req.Enabled != nil {
			enabled := *req.Enabled
			entry.Enabled = &enabled
		}
		if req.Expires != nil {
			entry.Expires = expires
		}
		if req.Models != nil {
			entry.Models = models
		}
		return entries, nil
	})
	if errEdit != nil {
		abortAdmin(c, errEdit.status, errEdit.message)
		return
	}
	log.WithField("name", name).Info("z10: friend updated")
	c.JSON(http.StatusOK, gin.H{"friend": rt.viewOf(set.ByName(name))})
}

func (rt *Runtime) handleDeleteFriend(c *gin.Context) {
	name := c.Param("name")
	_, errEdit := rt.editFriends("", func(entries []friendEntry, _ *config.Config) ([]friendEntry, *adminError) {
		index := findEntry(entries, name)
		if index < 0 {
			return nil, &adminError{http.StatusNotFound, fmt.Sprintf("friend %q not found", name)}
		}
		return slices.Delete(entries, index, index+1), nil
	})
	if errEdit != nil {
		abortAdmin(c, errEdit.status, errEdit.message)
		return
	}
	log.WithField("name", name).Info("z10: friend deleted")
	c.Status(http.StatusNoContent)
}

// viewOf describes one friend with its current usage totals.
func (rt *Runtime) viewOf(friend *Friend) friendView {
	return newFriendView(friend, rt.now(), rt.usage.Report().Friends[friend.Name])
}

func newFriendView(friend *Friend, now time.Time, usage *FriendUsage) friendView {
	state := friend.inactiveState(now)
	view := friendView{
		Name:           friend.Name,
		Models:         append([]string{}, friend.Models...),
		Channels:       append([]string{}, friend.Channels...),
		Expires:        friend.ExpiresRaw,
		Enabled:        friend.Enabled,
		Active:         state == "",
		InactiveReason: state,
	}
	if usage != nil {
		view.LastUsed = usage.LastUsed
		view.Requests = usage.Requests
		view.FailedRequests = usage.FailedRequests
		view.TotalTokens = usage.TotalTokens
	}
	return view
}
