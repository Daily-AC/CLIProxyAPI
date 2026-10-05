package z10

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/klauspost/compress/zstd"
	claudemodels "github.com/router-for-me/CLIProxyAPI/v8/internal/client/claude/models"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	"github.com/tidwall/gjson"
)

// maxFriendBodyBytes caps friend request bodies, before and after content decoding.
// A variable so tests can lower it.
var maxFriendBodyBytes int64 = 32 << 20

// gateError is a request rejected by the friend key gate.
type gateError struct {
	status  int
	code    string
	message string
}

func (e *gateError) Error() string { return e.message }

func newGateError(status int, code, format string, args ...any) *gateError {
	return &gateError{status: status, code: code, message: fmt.Sprintf(format, args...)}
}

// quoteModel quotes a client-supplied model name for error messages, truncated.
func quoteModel(model string) string {
	const limit = 120
	if len(model) > limit {
		model = model[:limit] + "..."
	}
	return fmt.Sprintf("%q", model)
}

// routeProviders returns the providers the router would choose from for model. It
// mirrors handlers.getRequestDetailsWithOptions outside Home mode: one thinking suffix is
// parsed off, the base is trimmed, the registry is queried (with its lowercase fallback),
// and the full suffixed name is tried when the base is unknown. "auto" is resolved by
// the router at execution time to any available model, so it never qualifies.
func routeProviders(model string) []string {
	if thinking.ParseSuffix(model).ModelName == "auto" {
		return nil
	}
	base := strings.TrimSpace(thinking.ParseSuffix(model).ModelName)
	providers := util.GetProviderName(base)
	if len(providers) == 0 && base != model {
		providers = util.GetProviderName(model)
	}
	return providers
}

// checkModel decides whether the friend may call routerModel, the exact string the
// router resolves. The name must match a pattern, the model must be registered, and
// every provider that may serve it must belong to the friend's channels; otherwise the
// router could pick another channel registering the same name.
func (f *Friend) checkModel(routerModel string) *gateError {
	if !f.matchesPattern(NormalizeModel(routerModel)) {
		return newGateError(http.StatusForbidden, "model_not_allowed", "model %s is not allowed for this API key", quoteModel(routerModel))
	}
	providers := routeProviders(routerModel)
	if len(providers) == 0 {
		return newGateError(http.StatusForbidden, "model_not_allowed", "model %s is not available for this API key", quoteModel(routerModel))
	}
	for _, provider := range providers {
		if _, allowed := f.providers[strings.ToLower(strings.TrimSpace(provider))]; !allowed {
			return newGateError(http.StatusForbidden, "model_not_allowed", "model %s is served by a channel this API key cannot use", quoteModel(routerModel))
		}
	}
	return nil
}

// listedModelAllowed applies checkModel to a model list or detail ID. Listed IDs may
// carry a "models/" prefix (Gemini) or be Claude list-cloaked; clients send them back in
// the form the matching handler decodes.
func (f *Friend) listedModelAllowed(id string) bool {
	model := claudemodels.ResolveClaudeModelIDPrefix(strings.TrimPrefix(strings.TrimSpace(id), "models/"))
	return model != "" && f.checkModel(model) == nil
}

// readFriendBody reads the raw request body (capped) and returns it together with the
// bytes the handler will parse. Only encodings the target handler decodes itself are
// accepted: identity everywhere, and zstd on routes that use handlers.ReadRequestBody.
func readFriendBody(r *http.Request, handlerDecodes bool) (raw, effective []byte, gateErr *gateError) {
	raw, errRead := io.ReadAll(io.LimitReader(r.Body, maxFriendBodyBytes+1))
	if errRead != nil {
		return nil, nil, newGateError(http.StatusBadRequest, "invalid_body", "failed to read request body")
	}
	if int64(len(raw)) > maxFriendBodyBytes {
		return nil, nil, newGateError(http.StatusRequestEntityTooLarge, "body_too_large", "request body exceeds %d bytes", maxFriendBodyBytes)
	}
	encodings := r.Header.Values("Content-Encoding")
	if len(encodings) > 1 {
		return raw, nil, newGateError(http.StatusUnsupportedMediaType, "unsupported_content_encoding", "multiple Content-Encoding headers are not allowed for this API key")
	}
	encoding := ""
	if len(encodings) == 1 {
		encoding = strings.TrimSpace(encodings[0])
	}
	switch {
	case encoding == "" || strings.EqualFold(encoding, "identity"):
		return raw, raw, nil
	case strings.EqualFold(encoding, "zstd") && handlerDecodes:
		decoded, decodeErr := decodeZstdCapped(raw)
		if decodeErr != nil {
			return raw, nil, decodeErr
		}
		return raw, decoded, nil
	default:
		return raw, nil, newGateError(http.StatusUnsupportedMediaType, "unsupported_content_encoding", "Content-Encoding %q is not supported on this endpoint for this API key", encoding)
	}
}

func decodeZstdCapped(raw []byte) ([]byte, *gateError) {
	decoder, errDecoder := zstd.NewReader(bytes.NewReader(raw), zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(uint64(maxFriendBodyBytes)))
	if errDecoder != nil {
		return nil, newGateError(http.StatusBadRequest, "invalid_body", "invalid zstd request body")
	}
	defer decoder.Close()
	decoded, errRead := io.ReadAll(io.LimitReader(decoder, maxFriendBodyBytes+1))
	if errors.Is(errRead, zstd.ErrDecoderSizeExceeded) || errors.Is(errRead, zstd.ErrWindowSizeExceeded) || int64(len(decoded)) > maxFriendBodyBytes {
		return nil, newGateError(http.StatusRequestEntityTooLarge, "body_too_large", "decoded request body exceeds %d bytes", maxFriendBodyBytes)
	}
	if errRead != nil {
		return nil, newGateError(http.StatusBadRequest, "invalid_body", "invalid zstd request body")
	}
	return decoded, nil
}

// bodyModel validates the parsed body strictly and returns the "model" value as the
// handlers read it (gjson, first match). The body must be exactly one JSON object with
// no BOM or leading bytes, and may contain at most one top-level key that equals
// "model" case-insensitively after unescaping, so no parser can see a second model.
func bodyModel(body []byte) (model string, present bool, gateErr *gateError) {
	trimmed := bytes.TrimLeft(body, " \t\r\n")
	if !json.Valid(body) || len(trimmed) == 0 || trimmed[0] != '{' {
		return "", false, newGateError(http.StatusBadRequest, "invalid_json", "request body must be a single JSON object")
	}
	modelKeys := 0
	gjson.ParseBytes(body).ForEach(func(key, _ gjson.Result) bool {
		if strings.EqualFold(key.String(), "model") {
			modelKeys++
		}
		return true
	})
	if modelKeys > 1 {
		return "", false, newGateError(http.StatusBadRequest, "duplicate_model", "request body must contain a single model field")
	}
	result := gjson.GetBytes(body, "model")
	if !result.Exists() {
		if modelKeys > 0 {
			return "", false, newGateError(http.StatusBadRequest, "invalid_model", "the model field must be spelled \"model\"")
		}
		return "", false, nil
	}
	if result.Type != gjson.String {
		return "", false, newGateError(http.StatusBadRequest, "invalid_model", "model must be a string")
	}
	return result.String(), true, nil
}
