package z10

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
	log "github.com/sirupsen/logrus"
	"github.com/tidwall/gjson"
)

type routeKind int

const (
	routeBodyModel routeKind = iota + 1
	routeGeminiAction
	routeModelList
	routeModelDetail
)

type friendRoute struct {
	kind  routeKind
	param string
	// decodes is true when the handler reads the body with handlers.ReadRequestBody,
	// which decodes zstd; other handlers parse the raw bytes.
	decodes bool
	// claudeIDs is true when the handler decodes Claude list-cloaked model IDs.
	claudeIDs bool
}

// friendRoutes is the endpoint allowlist for friend keys, keyed by method and the matched
// gin route pattern. Everything else is denied, including routes added upstream later.
var friendRoutes = map[string]friendRoute{
	"POST /v1/chat/completions":      {kind: routeBodyModel, decodes: true},
	"POST /v1/completions":           {kind: routeBodyModel, decodes: true},
	"POST /v1/responses":             {kind: routeBodyModel, decodes: true},
	"POST /v1/messages":              {kind: routeBodyModel, claudeIDs: true},
	"POST /v1/messages/count_tokens": {kind: routeBodyModel, claudeIDs: true},
	"GET /v1/models":                 {kind: routeModelList},
	"GET /v1/models/*model":          {kind: routeModelDetail, param: "model"},
	"GET /v1beta/models":             {kind: routeModelList},
	"GET /v1beta/models/*action":     {kind: routeModelDetail, param: "action"},
	"POST /v1beta/models/*action":    {kind: routeGeminiAction, param: "action"},
}

// geminiMethods mirrors the methods dispatched by the Gemini handler.
var geminiMethods = map[string]bool{"generateContent": true, "streamGenerateContent": true, "countTokens": true}

// Middleware enforces friend key restrictions. It runs before the route-group auth
// middleware; requests without a friend key pass through untouched.
func (rt *Runtime) Middleware() gin.HandlerFunc {
	return func(c *gin.Context) {
		friend, _ := rt.Friends().match(c.Request)
		// CORS preflight requests are answered by the CORS middleware before any handler.
		if friend == nil || c.Request.Method == http.MethodOptions {
			c.Next()
			return
		}
		if reason := friend.inactiveReason(rt.now()); reason != "" {
			c.AbortWithStatusJSON(http.StatusUnauthorized, gin.H{"error": reason})
			return
		}
		// The access provider accepts the key only with this marker, so a key that
		// becomes a friend key between this check and authentication is rejected.
		c.Request = c.Request.WithContext(withFriendMarker(c.Request.Context(), friend))
		if isUpgradeRequest(c.Request) {
			rt.deny(c, friend, newGateError(http.StatusForbidden, "websocket_not_allowed", "WebSocket and protocol upgrades are not allowed for this API key"))
			return
		}
		route, allowed := friendRoutes[c.Request.Method+" "+c.FullPath()]
		if !allowed {
			rt.deny(c, friend, newGateError(http.StatusForbidden, "endpoint_not_allowed", "%s %s is not allowed for this API key", c.Request.Method, c.Request.URL.Path))
			return
		}

		switch route.kind {
		case routeModelList:
			serveFilteredModelList(c, friend)
			rt.usage.Touch(friend.Name)
			return
		case routeModelDetail:
			if !friend.listedModelAllowed(strings.TrimPrefix(c.Param(route.param), "/")) {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "Model not found", "type": "invalid_request_error", "code": "model_not_found"}})
				return
			}
			c.Next()
			rt.usage.Touch(friend.Name)
			return
		}

		routerModel, gateErr := gateModelRequest(c, friend, route)
		if gateErr != nil {
			rt.deny(c, friend, gateErr)
			return
		}
		c.Next()
		rt.usage.RecordRequest(friend.Name, NormalizeModel(routerModel), c.Writer.Status() >= http.StatusBadRequest)
	}
}

// gateModelRequest validates the body of a model request, restores the original bytes
// for the handler, and checks every model the request names. It returns the model the
// router will resolve.
func gateModelRequest(c *gin.Context, friend *Friend, route friendRoute) (string, *gateError) {
	var pathModel string
	if route.kind == routeGeminiAction {
		parts := strings.Split(strings.TrimPrefix(c.Param(route.param), "/"), ":")
		if len(parts) != 2 || !geminiMethods[parts[1]] {
			return "", newGateError(http.StatusForbidden, "endpoint_not_allowed", "%s %s is not allowed for this API key", c.Request.Method, c.Request.URL.Path)
		}
		pathModel = parts[0]
	}

	raw, effective, gateErr := readFriendBody(c.Request, route.decodes)
	if raw != nil {
		restoreBody(c, raw)
	}
	if gateErr != nil {
		return "", gateErr
	}
	bodyModelName, present, gateErr := bodyModel(effective)
	if gateErr != nil {
		return "", gateErr
	}

	if route.kind == routeGeminiAction {
		// The Gemini handler routes on the path; a body model must still be allowed.
		if present {
			if gateErr = friend.checkModel(bodyModelName); gateErr != nil {
				return "", gateErr
			}
		}
		return pathModel, friend.checkModel(pathModel)
	}

	if !present || bodyModelName == "" {
		return "", newGateError(http.StatusBadRequest, "model_required", "request does not specify a model")
	}
	routerModel := bodyModelName
	if route.claudeIDs {
		routerModel = claudemodels.ResolveClaudeModelIDPrefix(bodyModelName)
	}
	return routerModel, friend.checkModel(routerModel)
}

func (rt *Runtime) deny(c *gin.Context, friend *Friend, gateErr *gateError) {
	log.WithFields(log.Fields{
		"friend": friend.Name,
		"method": c.Request.Method,
		"path":   c.Request.URL.Path,
		"code":   gateErr.code,
	}).Info("z10: friend key request denied")
	errType := "permission_error"
	switch gateErr.status {
	case http.StatusBadRequest, http.StatusRequestEntityTooLarge, http.StatusUnsupportedMediaType:
		errType = "invalid_request_error"
	}
	c.AbortWithStatusJSON(gateErr.status, gin.H{"error": gin.H{"message": gateErr.message, "type": errType, "code": gateErr.code}})
}

// isUpgradeRequest detects WebSocket and other protocol upgrades. The model of a
// WebSocket session is chosen per message, so it cannot be checked here.
func isUpgradeRequest(r *http.Request) bool {
	if r.Method == http.MethodConnect {
		return true
	}
	if r.Header.Get("Upgrade") != "" || r.Header.Get("Sec-WebSocket-Key") != "" || r.Header.Get("Sec-WebSocket-Version") != "" {
		return true
	}
	for _, value := range r.Header.Values("Connection") {
		for _, token := range strings.Split(value, ",") {
			if strings.EqualFold(strings.TrimSpace(token), "upgrade") {
				return true
			}
		}
	}
	return false
}

func restoreBody(c *gin.Context, raw []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
}

// serveFilteredModelList buffers the model list written by the handler and rewrites it
// so that only models the friend may call are listed.
func serveFilteredModelList(c *gin.Context, friend *Friend) {
	original := c.Writer
	buffer := &bufferedWriter{ResponseWriter: original, status: http.StatusOK, size: -1}
	c.Writer = buffer
	c.Next()
	c.Writer = original

	if !buffer.Written() {
		if buffer.status != http.StatusOK {
			original.WriteHeader(buffer.status)
		}
		return
	}
	body := buffer.body.Bytes()
	if buffer.status == http.StatusOK {
		body = filterModelList(body, friend)
	}
	original.Header().Set("Content-Length", strconv.Itoa(len(body)))
	original.WriteHeader(buffer.status)
	if _, errWrite := original.Write(body); errWrite != nil {
		log.WithError(errWrite).Debug("z10: failed to write filtered model list")
	}
}

var emptyModelList = []byte(`{"data":[],"object":"list"}`)

// filterModelList keeps allowed entries of the "data" (OpenAI, Claude, Grok) and
// "models" (Gemini, Codex client catalog) arrays. Unknown shapes become an empty list.
func filterModelList(body []byte, friend *Friend) []byte {
	var document map[string]json.RawMessage
	if errUnmarshal := json.Unmarshal(body, &document); errUnmarshal != nil || document == nil {
		return emptyModelList
	}
	found := false
	for _, field := range []string{"data", "models"} {
		rawEntries, exists := document[field]
		if !exists {
			continue
		}
		found = true
		var entries []json.RawMessage
		_ = json.Unmarshal(rawEntries, &entries)
		kept := make([]json.RawMessage, 0, len(entries))
		for _, entry := range entries {
			if friend.listedModelAllowed(modelEntryID(entry)) {
				kept = append(kept, entry)
			}
		}
		document[field] = mustMarshal(kept)
		if field == "data" {
			// Anthropic lists expose pagination cursors that would otherwise name hidden models.
			for _, cursor := range []struct {
				key   string
				index int
			}{{"first_id", 0}, {"last_id", len(kept) - 1}} {
				if _, hasCursor := document[cursor.key]; !hasCursor {
					continue
				}
				id := ""
				if cursor.index >= 0 && cursor.index < len(kept) {
					id = gjson.GetBytes(kept[cursor.index], "id").String()
				}
				document[cursor.key] = mustMarshal(id)
			}
		}
	}
	if !found {
		return emptyModelList
	}
	return mustMarshal(document)
}

// modelEntryID returns the routable name of a model list entry.
func modelEntryID(entry json.RawMessage) string {
	for _, field := range []string{"id", "slug", "name"} {
		if value := gjson.GetBytes(entry, field); value.Type == gjson.String && value.String() != "" {
			return value.String()
		}
	}
	return ""
}

func mustMarshal(value any) []byte {
	var buffer bytes.Buffer
	encoder := json.NewEncoder(&buffer)
	encoder.SetEscapeHTML(false)
	if errEncode := encoder.Encode(value); errEncode != nil {
		return []byte("null")
	}
	return bytes.TrimRight(buffer.Bytes(), "\n")
}

// bufferedWriter captures a handler response so it can be rewritten before sending.
type bufferedWriter struct {
	gin.ResponseWriter
	body   bytes.Buffer
	status int
	size   int
}

func (w *bufferedWriter) WriteHeader(code int) {
	if code > 0 && w.size < 0 {
		w.status = code
	}
}

func (w *bufferedWriter) WriteHeaderNow() {
	if w.size < 0 {
		w.size = 0
	}
}

func (w *bufferedWriter) Write(data []byte) (int, error) {
	w.WriteHeaderNow()
	n, errWrite := w.body.Write(data)
	w.size += n
	return n, errWrite
}

func (w *bufferedWriter) WriteString(s string) (int, error) {
	return w.Write([]byte(s))
}

func (w *bufferedWriter) Status() int { return w.status }

func (w *bufferedWriter) Size() int { return w.size }

func (w *bufferedWriter) Written() bool { return w.size >= 0 }

// Flush is a no-op: the response is sent after filtering.
func (w *bufferedWriter) Flush() {}
