package executor

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/config"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/misc"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/registry"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/runtime/executor/helps"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/thinking"
	"github.com/router-for-me/CLIProxyAPI/v8/internal/util"
	cliproxyauth "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/auth"
	cliproxyexecutor "github.com/router-for-me/CLIProxyAPI/v8/sdk/cliproxy/executor"
	sdktranslator "github.com/router-for-me/CLIProxyAPI/v8/sdk/translator"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const (
	codexUserAgent             = "codex-tui/0.154.0 (Mac OS 26.5.2; arm64) iTerm.app/3.6.11 (codex-tui; 0.154.0)"
	codexOriginator            = "codex-tui"
	codexDefaultImageToolModel = "gpt-image-2"
	codexResponsesLiteHeader   = "X-OpenAI-Internal-Codex-Responses-Lite"
)

var dataTag = []byte("data:")

func translateCodexRequestPair(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte) {
	original, body, _ := translateCodexRequestPairWithUpdateIntent(from, to, model, originalPayload, payload, stream, preserveEmptyThinkingBlocks...)
	return original, body
}

func translateCodexRequestPairWithUpdateIntent(from, to sdktranslator.Format, model string, originalPayload, payload []byte, stream bool, preserveEmptyThinkingBlocks ...bool) ([]byte, []byte, bool) {
	isCompat := len(preserveEmptyThinkingBlocks) > 0 && preserveEmptyThinkingBlocks[0]
	ctx := context.Background()
	translate := func(raw []byte) ([]byte, bool) {
		if isCompat && from == sdktranslator.FormatClaude && to == sdktranslator.FormatCodex {
			return helps.TranslateRequestWithAPIKeyModelCompatibility(ctx, nil, nil, from, to, model, raw, stream, true), false
		}
		translated := sdktranslator.TranslateRequestEnvelope(ctx, from, to, sdktranslator.RequestEnvelope{Format: from, Model: model, Stream: stream, Body: raw})
		return translated.Body, translated.ConfigurationUpdatesChanged
	}
	if bytes.Equal(originalPayload, payload) {
		body, changed := translate(payload)
		return body, body, changed
	}
	originalTranslated, _ := translate(originalPayload)
	body, changed := translate(payload)
	return originalTranslated, body, changed
}

// PrepareRequest injects Codex credentials into the outgoing HTTP request.
func (e *CodexExecutor) PrepareRequest(req *http.Request, auth *cliproxyauth.Auth) error {
	if req == nil {
		return nil
	}
	apiKey, _ := codexCreds(auth)
	if strings.TrimSpace(apiKey) != "" {
		req.Header.Set("Authorization", "Bearer "+apiKey)
	} else {
		req.Header.Del("Authorization")
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(req, attrs)
	return nil
}

// HttpRequest injects Codex credentials into the request and executes it.
func (e *CodexExecutor) HttpRequest(ctx context.Context, auth *cliproxyauth.Auth, req *http.Request) (*http.Response, error) {
	if req == nil {
		return nil, fmt.Errorf("codex executor: request is nil")
	}
	if ctx == nil {
		ctx = req.Context()
	}
	httpReq := req.WithContext(ctx)
	if err := e.PrepareRequest(httpReq, auth); err != nil {
		return nil, err
	}
	httpClient := helps.NewUtlsHTTPClient(ctx, e.cfg, auth, 0)
	return httpClient.Do(httpReq)
}

func (e *CodexExecutor) cacheHelper(ctx context.Context, from sdktranslator.Format, url string, req cliproxyexecutor.Request, rawJSON []byte, headerSets ...http.Header) (*http.Request, []byte, error) {
	var headers http.Header
	if len(headerSets) > 0 {
		headers = headerSets[0]
	}
	var cache helps.CodexCache
	if sourceFormatEqual(from, sdktranslator.FormatClaude) {
		modelName := strings.TrimSpace(gjson.GetBytes(rawJSON, "model").String())
		if modelName == "" {
			modelName = thinking.ParseSuffix(req.Model).ModelName
		}
		cached, ok, errCache := helps.ClaudeCodePromptCache(ctx, modelName, req.Payload, headers)
		if errCache != nil {
			return nil, nil, errCache
		}
		if ok {
			cache = cached
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAIResponse) {
		promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key")
		if promptCacheKey.Exists() {
			cache.ID = promptCacheKey.String()
		}
	} else if sourceFormatEqual(from, sdktranslator.FormatOpenAI) {
		if promptCacheKey := gjson.GetBytes(req.Payload, "prompt_cache_key"); promptCacheKey.Exists() {
			cache.ID = strings.TrimSpace(promptCacheKey.String())
		}
		if cache.ID == "" {
			cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
		}
		if cache.ID == "" {
			if apiKey := strings.TrimSpace(helps.APIKeyFromContext(ctx)); apiKey != "" {
				cache.ID = uuid.NewSHA1(uuid.NameSpaceOID, []byte("cli-proxy-api:codex:prompt-cache:"+apiKey)).String()
			}
		}
	}
	if cache.ID == "" {
		cache.ID = helps.ProviderSessionUUID("codex", req.Metadata)
	}

	if cache.ID != "" {
		rawJSON = helps.SetStringIfDifferent(rawJSON, "prompt_cache_key", cache.ID)
	}
	rawJSON = helps.SanitizeCodexInputLogprobs(rawJSON)
	rawJSON = helps.SanitizeCodexInputItemIDs(rawJSON)
	rawJSON = helps.FinalizePayload(ctx, rawJSON)
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, url, bytes.NewReader(rawJSON))
	if err != nil {
		return nil, nil, err
	}
	if cache.ID != "" {
		httpReq.Header.Set("Session-Id", cache.ID)
	}
	return httpReq, rawJSON, nil
}

func applyCodexHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0]
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
}

// applyModelHeaderOverrides forces models.json config.override_header onto upstream headers.
func applyModelHeaderOverrides(headers http.Header, modelName string) {
	if headers == nil {
		return
	}
	overrides := registry.ModelOverrideHeaders(modelName)
	if len(overrides) == 0 {
		return
	}
	for key, value := range overrides {
		headers.Set(key, value)
	}
	if strings.Contains(headers.Get("User-Agent"), "Mac OS") && codexSessionHeaderValue(headers) == "" {
		headers.Set("Session_id", uuid.NewString())
	}
}

// applyCodexDirectImageHeaders sets Codex upstream headers for direct /images/* calls.
// Downstream client User-Agent values are not forwarded to reduce Cloudflare 1010 blocks.
func applyCodexDirectImageHeaders(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, clientHeaders ...http.Header) {
	var ginHeaders http.Header
	if len(clientHeaders) > 0 && clientHeaders[0] != nil {
		ginHeaders = clientHeaders[0].Clone()
		ginHeaders.Del("User-Agent")
	} else if ginCtx, ok := r.Context().Value("gin").(*gin.Context); ok && ginCtx != nil && ginCtx.Request != nil {
		ginHeaders = ginCtx.Request.Header.Clone()
		ginHeaders.Del("User-Agent")
	}
	applyCodexHeadersFromSources(r, auth, token, stream, cfg, ginHeaders)
}

func applyCodexHeadersFromSources(r *http.Request, auth *cliproxyauth.Auth, token string, stream bool, cfg *config.Config, ginHeaders http.Header) {
	r.Header.Set("Content-Type", "application/json")
	if strings.TrimSpace(token) != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	} else {
		r.Header.Del("Authorization")
	}

	if ginHeaders != nil && ginHeaders.Get("X-Codex-Beta-Features") != "" {
		r.Header.Set("X-Codex-Beta-Features", ginHeaders.Get("X-Codex-Beta-Features"))
	}
	misc.EnsureHeader(r.Header, ginHeaders, "Version", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-Metadata", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Turn-State", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Client-Request-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Codex-Window-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Thread-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "Session-Id", "")
	misc.EnsureHeader(r.Header, ginHeaders, "X-Openai-Internal-Codex-Responses-Lite", "")

	cfgUserAgent, _ := codexHeaderDefaults(cfg, auth)
	ensureHeaderWithConfigPrecedence(r.Header, ginHeaders, "User-Agent", cfgUserAgent, codexUserAgent)

	if stream {
		r.Header.Set("Accept", "text/event-stream")
	} else {
		r.Header.Set("Accept", "application/json")
	}
	r.Header.Set("Connection", "Keep-Alive")

	isAPIKey := codexAuthUsesAPIKey(auth)
	if originator := strings.TrimSpace(ginHeaders.Get("Originator")); originator != "" {
		r.Header.Set("Originator", originator)
	} else if !isAPIKey {
		r.Header.Set("Originator", codexOriginator)
	}
	if !isAPIKey {
		if auth != nil && auth.Metadata != nil {
			if accountID, ok := auth.Metadata["account_id"].(string); ok {
				r.Header.Set("Chatgpt-Account-Id", accountID)
			}
		}
	}
	var attrs map[string]string
	if auth != nil {
		attrs = auth.Attributes
	}
	util.ApplyCustomHeadersFromAttrs(r, attrs, ginHeaders)
	applyCodexCloakingHeaders(r.Header, cfg, auth)
}

const codexRoutingHintHeader = "X-Codex-Routing-Hint"

// applyCodexRoutingHint sends the routing hint native Codex attaches to every
// ChatGPT-backend Responses request: "model=<slug>" plus ";tier=<service_tier>"
// when the body requests a tier (openai/codex rust-v0.155.0,
// codex-rs/core/src/client.rs build_routing_hint_header). Without it, a
// translated request carries service_tier=priority only in the body. Whether
// the backend needs the header to grant priority is undocumented.
//
// The model is the resolved model written to the upstream body, while the tier
// is read from the final body so payload rules cannot make the hint stale. A
// hint forwarded by a native client names its original model and is replaced.
// Operator configuration keeps precedence: when an auth "header:" rule for the
// hint resolves to a value (static, or a "$Header" reference the request
// carries), that value is sent, and callers apply models.json override_header
// afterwards. A rule that resolves to nothing falls back to the derived hint.
// API-key requests are not touched, matching native Codex, which sends no hint
// to API-key providers.
func applyCodexRoutingHint(ctx context.Context, headers http.Header, auth *cliproxyauth.Auth, baseModel string, upstreamBody []byte, clientHeaders http.Header) {
	if codexAuthUsesAPIKey(auth) {
		return
	}
	deleteHeaderCaseInsensitive(headers, codexRoutingHintHeader)
	if operatorHint := codexOperatorHeaderValue(ctx, auth, clientHeaders, codexRoutingHintHeader); operatorHint != "" {
		headers.Set(codexRoutingHintHeader, operatorHint)
		return
	}
	model := strings.TrimSpace(baseModel)
	if model == "" {
		return
	}
	hint := "model=" + model
	if tier := gjson.GetBytes(upstreamBody, "service_tier"); tier.Type == gjson.String {
		if value := strings.TrimSpace(tier.String()); value != "" {
			hint += ";tier=" + value
		}
	}
	headers.Set(codexRoutingHintHeader, hint)
}

// codexOperatorHeaderValue returns the value the auth's "header:" rules
// resolve to for name, using the same resolver that applied them to the
// request, so dynamic references that resolve to nothing report "".
func codexOperatorHeaderValue(ctx context.Context, auth *cliproxyauth.Auth, clientHeaders http.Header, name string) string {
	if auth == nil || len(auth.Attributes) == 0 {
		return ""
	}
	resolved := (&http.Request{Header: http.Header{}}).WithContext(ctx)
	util.ApplyCustomHeadersFromAttrs(resolved, auth.Attributes, clientHeaders)
	return strings.TrimSpace(resolved.Header.Get(name))
}

func isCodexCloakingDisabled(cfg *config.Config, auth *cliproxyauth.Auth) bool {
	if auth != nil && auth.AuthKind() == cliproxyauth.AuthKindAPIKey {
		return true
	}
	return cfg != nil && cfg.Codex.DisableCloaking
}

func applyCodexCloakingHeaders(headers http.Header, cfg *config.Config, auth *cliproxyauth.Auth) {
	if headers == nil || isCodexCloakingDisabled(cfg, auth) {
		return
	}
	headers.Del("X-Cpa-Trace-Id")
	headers.Del("X-Cpa-Version")
	headers.Del("X-Cpa-Commit")
	headers.Del("X-Cpa-Build-Date")
	headers.Del("X-Cpa-Support-Plugin")
}

func codexHeaderDefaults(cfg *config.Config, auth *cliproxyauth.Auth) (userAgent string, originator string) {
	userAgent = codexUserAgent
	originator = codexOriginator
	if cfg == nil {
		return userAgent, originator
	}
	if cfg.Codex.UserAgent != "" {
		userAgent = cfg.Codex.UserAgent
	}
	if cfg.Codex.Originator != "" {
		originator = cfg.Codex.Originator
	}
	return userAgent, originator
}

func ensureHeaderWithConfigPrecedence(dst, src http.Header, name, cfgValue, fallback string) {
	if dst == nil {
		return
	}
	if cfgValue != "" {
		dst.Set(name, cfgValue)
		return
	}
	misc.EnsureHeader(dst, src, name, fallback)
}

func codexSessionHeaderValue(headers http.Header) string {
	if headers == nil {
		return ""
	}
	if v := headers.Get("Session_id"); v != "" {
		return v
	}
	return headers.Get("Session-Id")
}

func deleteHeaderCaseInsensitive(headers http.Header, name string) {
	if headers == nil {
		return
	}
	headers.Del(name)
}

func parseCodexResponsesLiteHeader(headers http.Header) (bool, bool) {
	if headers == nil {
		return false, false
	}
	value := strings.TrimSpace(headers.Get(codexResponsesLiteHeader))
	if value == "" {
		return false, false
	}
	parsed, err := strconv.ParseBool(value)
	if err != nil {
		return false, false
	}
	return parsed, true
}
