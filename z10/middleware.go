package z10

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/router-for-me/CLIProxyAPI/v8/sdk/api/handlers"
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
}

// friendRoutes is the endpoint allowlist for friend keys, keyed by method and the matched
// gin route pattern. Everything else is denied, including routes added upstream later.
var friendRoutes = map[string]friendRoute{
	"POST /v1/chat/completions":      {kind: routeBodyModel},
	"POST /v1/completions":           {kind: routeBodyModel},
	"POST /v1/messages":              {kind: routeBodyModel},
	"POST /v1/messages/count_tokens": {kind: routeBodyModel},
	"POST /v1/responses":             {kind: routeBodyModel},
	"GET /v1/models":                 {kind: routeModelList},
	"GET /v1/models/*model":          {kind: routeModelDetail, param: "model"},
	"GET /v1beta/models":             {kind: routeModelList},
	"GET /v1beta/models/*action":     {kind: routeModelDetail, param: "action"},
	"POST /v1beta/models/*action":    {kind: routeGeminiAction, param: "action"},
}

// geminiMethods mirrors the methods dispatched by the Gemini handler.
var geminiMethods = map[string]bool{"generateContent": true, "streamGenerateContent": true, "countTokens": true}

// maxGzipBodyBytes caps the extra gzip view used only for model inspection.
const maxGzipBodyBytes = 64 << 20

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
		if isUpgradeRequest(c.Request) {
			rt.deny(c, friend, http.StatusForbidden, "websocket_not_allowed", "WebSocket and protocol upgrades are not allowed for this API key")
			return
		}
		route, allowed := friendRoutes[c.Request.Method+" "+c.FullPath()]
		if !allowed {
			rt.deny(c, friend, http.StatusForbidden, "endpoint_not_allowed", fmt.Sprintf("%s %s is not allowed for this API key", c.Request.Method, c.Request.URL.Path))
			return
		}

		switch route.kind {
		case routeModelList:
			serveFilteredModelList(c, friend)
			rt.usage.Touch(friend.Name)
			return
		case routeModelDetail:
			if !friend.Allows(strings.TrimPrefix(c.Param(route.param), "/")) {
				c.AbortWithStatusJSON(http.StatusNotFound, gin.H{"error": gin.H{"message": "Model not found", "type": "invalid_request_error", "code": "model_not_found"}})
				return
			}
			c.Next()
			rt.usage.Touch(friend.Name)
			return
		}

		var pathModel string
		if route.kind == routeGeminiAction {
			parts := strings.Split(strings.TrimPrefix(c.Param(route.param), "/"), ":")
			if len(parts) != 2 || !geminiMethods[parts[1]] {
				rt.deny(c, friend, http.StatusForbidden, "endpoint_not_allowed", fmt.Sprintf("%s %s is not allowed for this API key", c.Request.Method, c.Request.URL.Path))
				return
			}
			pathModel = parts[0]
		}
		primary, models, errRead := inspectRequestModels(c, pathModel)
		if errRead != nil {
			c.AbortWithStatusJSON(http.StatusBadRequest, gin.H{"error": gin.H{"message": "Invalid request: failed to read body", "type": "invalid_request_error"}})
			return
		}
		if primary == "" {
			rt.deny(c, friend, http.StatusBadRequest, "model_required", "request does not specify a model")
			return
		}
		for _, model := range models {
			if !friend.Allows(model) {
				rt.deny(c, friend, http.StatusForbidden, "model_not_allowed", fmt.Sprintf("model %q is not allowed for this API key", model))
				return
			}
		}

		c.Next()
		rt.usage.RecordRequest(friend.Name, NormalizeModel(primary), c.Writer.Status() >= http.StatusBadRequest)
	}
}

func (rt *Runtime) deny(c *gin.Context, friend *Friend, status int, code, message string) {
	log.WithFields(log.Fields{
		"friend": friend.Name,
		"method": c.Request.Method,
		"path":   c.Request.URL.Path,
		"code":   code,
	}).Info("z10: friend key request denied")
	errType := "permission_error"
	if status == http.StatusBadRequest {
		errType = "invalid_request_error"
	}
	c.AbortWithStatusJSON(status, gin.H{"error": gin.H{"message": message, "type": errType, "code": code}})
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

// inspectRequestModels collects every model the request may name and restores the
// original body bytes. The body is inspected as raw bytes (what the Claude and Gemini
// handlers parse), as decoded by handlers.ReadRequestBody (what the OpenAI handlers
// parse), and gunzipped; every model found in any view must be allowed, so a payload
// that decodes differently per parser cannot smuggle a second model past the check.
// primary is the model used for usage accounting.
func inspectRequestModels(c *gin.Context, pathModel string) (primary string, models []string, err error) {
	raw, errRead := c.GetRawData()
	if errRead != nil {
		return "", nil, errRead
	}
	restoreBody(c, raw)
	views := [][]byte{raw}
	if decoded, errDecode := handlers.ReadRequestBody(c); errDecode == nil && !bytes.Equal(decoded, raw) {
		views = append(views, decoded)
	}
	restoreBody(c, raw)
	if gunzipped, ok := gunzipView(raw, c.Request.Header.Get("Content-Encoding")); ok {
		views = append(views, gunzipped)
	}

	if pathModel != "" {
		primary = pathModel
		models = append(models, pathModel)
	}
	// Prefer the decoded view for accounting: it is what the OpenAI handlers route on.
	for index := len(views) - 1; index >= 0; index-- {
		found := modelsInBody(views[index])
		models = append(models, found...)
		if primary == "" {
			for _, model := range found {
				if strings.TrimSpace(model) != "" {
					primary = model
					break
				}
			}
		}
	}
	return primary, models, nil
}

func restoreBody(c *gin.Context, raw []byte) {
	c.Request.Body = io.NopCloser(bytes.NewReader(raw))
}

// modelsInBody returns the top-level "model" value as gjson resolves it (the parser used
// by the handlers) plus every top-level "model" key, which catches duplicate keys that
// other JSON parsers would resolve differently.
func modelsInBody(body []byte) []string {
	var out []string
	if result := gjson.GetBytes(body, "model"); result.Exists() {
		out = append(out, result.String())
	}
	if root := gjson.ParseBytes(body); root.IsObject() {
		root.ForEach(func(key, value gjson.Result) bool {
			if key.String() == "model" {
				out = append(out, value.String())
			}
			return true
		})
	}
	return out
}

func gunzipView(raw []byte, encoding string) ([]byte, bool) {
	if !strings.EqualFold(strings.TrimSpace(encoding), "gzip") {
		return nil, false
	}
	reader, errReader := gzip.NewReader(bytes.NewReader(raw))
	if errReader != nil {
		return nil, false
	}
	defer func() { _ = reader.Close() }()
	data, errRead := io.ReadAll(io.LimitReader(reader, maxGzipBodyBytes+1))
	if errRead != nil || len(data) > maxGzipBodyBytes {
		return nil, false
	}
	return data, true
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
			if friend.Allows(modelEntryID(entry)) {
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
