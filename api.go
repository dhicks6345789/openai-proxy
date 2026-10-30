package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// UserInfo describes the currently authenticated user.
type UserInfo struct {
	Username string `json:"username" example:"alice"`
	AuthType string `json:"auth_type" example:"local_env"`
	Mode     string `json:"mode" example:"desktop"`
}

// HealthResponse describes the service health status.
type HealthResponse struct {
	Status    string `json:"status" example:"ok"`
	Timestamp string `json:"timestamp" example:"2026-08-04T12:00:00Z"`
}

// SystemInfoResponse describes system information.
type SystemInfoResponse struct {
	Mode             string `json:"mode" example:"desktop"`
	GoVersion        string `json:"go_version" example:"go1.24.4"`
	OS               string `json:"os" example:"linux"`
	Arch             string `json:"arch" example:"amd64"`
	Uptime           string `json:"uptime" example:"12m30s"`
	Upstream         string `json:"upstream" example:"https://api.openai.com/v1"`
	APIKeyConfigured bool   `json:"api_key_configured" example:"true"`
	AllowedModels    int    `json:"allowed_models" example:"0"`
	BlockedModels    int    `json:"blocked_models" example:"1"`
}

// OpenAIError mirrors the error object format used by the OpenAI API.
type OpenAIError struct {
	Message string `json:"message" example:"Model 'gpt-4' is not permitted by this proxy's model policy."`
	Type    string `json:"type" example:"invalid_request_error"`
	Param   string `json:"param,omitempty" example:"model"`
	Code    string `json:"code,omitempty" example:"model_not_allowed"`
}

// ErrorResponse describes an error payload in the OpenAI API error format.
type ErrorResponse struct {
	Error OpenAIError `json:"error"`
}

// UserUsage aggregates proxied API activity for a single user.
type UserUsage struct {
	User             string           `json:"user" example:"alice"`
	Requests         int64            `json:"requests" example:"42"`
	PromptTokens     int64            `json:"prompt_tokens" example:"15320"`
	CompletionTokens int64            `json:"completion_tokens" example:"4210"`
	TotalTokens      int64            `json:"total_tokens" example:"19530"`
	LastActivity     time.Time        `json:"last_activity" example:"2026-08-04T12:00:00Z"`
	Models           map[string]int64 `json:"models"`
}

// UsageResponse is the payload returned by the usage endpoint.
type UsageResponse struct {
	Total UserUsage   `json:"total"`
	Users []UserUsage `json:"users"`
}

// RequestLogEntry records a single proxied API request.
type RequestLogEntry struct {
	Time             string `json:"time" example:"2026-08-04T12:00:00Z"`
	User             string `json:"user" example:"alice"`
	Method           string `json:"method" example:"POST"`
	Path             string `json:"path" example:"/v1/chat/completions"`
	Model            string `json:"model" example:"gpt-4o"`
	Stream           bool   `json:"stream" example:"false"`
	Status           int    `json:"status" example:"200"`
	DurationMs       int64  `json:"duration_ms" example:"1830"`
	PromptTokens     int    `json:"prompt_tokens" example:"512"`
	CompletionTokens int    `json:"completion_tokens" example:"128"`
	TotalTokens      int    `json:"total_tokens" example:"640"`
}

// LogResponse is the payload returned by the logs endpoint.
type LogResponse struct {
	Entries []RequestLogEntry `json:"entries"`
}

// proxyHeaders lists headers set by authenticating reverse proxies
// (Pangolin, Traefik, Cloudflare Tunnel, Authelia, Tailscale, etc.).
var proxyHeaders = []string{
	"X-Forwarded-User",
	"Remote-User",
	"X-User",
	"CF-Access-Authenticated-User-Email",
	"X-Auth-Request-User",
	"Pangolin-User",
}

// Limits used by the proxy. They keep request sniffing and usage capture
// bounded so that very large payloads (file uploads, image responses) cannot
// exhaust memory.
const (
	maxInspectBody  = 64 << 20 // largest request body buffered for model sniffing (64 MB)
	maxCaptureBody  = 8 << 20  // largest JSON response buffered for usage capture (8 MB)
	maxSSELineLen   = 1 << 20  // largest single SSE data line scanned (1 MB)
	maxLogEntries   = 1000     // recent request log entries kept in memory
	dialTimeout     = 15 * time.Second
	tlsTimeout      = 10 * time.Second
	respHdrTimeout  = 10 * time.Minute // LLM responses can take a while to start
	idleConnTimeout = 90 * time.Second
)

// ProxyConfig configures the OpenAI-compatible forwarding behaviour.
type ProxyConfig struct {
	// Upstream is the base URL of the OpenAI-compatible endpoint, including
	// the API version prefix (e.g. "https://api.openai.com/v1" or
	// "http://localhost:11434/v1"). Empty disables the proxy.
	Upstream string

	// APIKey is the bearer token sent to the upstream endpoint. When empty
	// the client's own Authorization header is forwarded unchanged.
	APIKey string

	// AllowedModels is a model allowlist (supports a trailing "*" wildcard).
	// Empty means all models are allowed.
	AllowedModels []string

	// BlockedModels is a model denylist checked after the allowlist.
	BlockedModels []string
}

// api holds the state shared by the API handlers.
type api struct {
	startTime      time.Time
	isServerMode   bool
	cfg            ProxyConfig
	upstreamURL    *url.URL
	transport      *http.Transport
	docsFS         fs.FS
	docsFileServer http.Handler

	mu    sync.RWMutex
	usage map[string]*UserUsage // per-user aggregates
	logs  []RequestLogEntry     // recent requests, newest first
}

// @title OpenAI Proxy API
// @description Management API and OpenAI-compatible proxy endpoints for the self-contained OpenAI Proxy application. Requests under /v1 are forwarded to the configured upstream with per-user usage accounting and model filtering.
// @version 1.1.0
// newAPI creates the API and parses the proxy configuration. A configuration
// without a parseable upstream URL leaves the proxy disabled; the proxy
// handler then answers with a 503 error.
func newAPI(isServerMode bool, docsFS fs.FS, cfg ProxyConfig) *api {
	a := &api{
		startTime:    time.Now(),
		isServerMode: isServerMode,
		cfg:          cfg,
		usage:        make(map[string]*UserUsage),
		docsFS:       docsFS,
	}

	if cfg.Upstream != "" {
		u, err := url.Parse(strings.TrimSuffix(cfg.Upstream, "/"))
		switch {
		case err != nil:
			log.Printf("Warning: ignoring unparseable upstream URL %q: %v", cfg.Upstream, err)
		case u.Scheme == "" || u.Host == "":
			log.Printf("Warning: ignoring upstream URL %q: missing scheme or host", cfg.Upstream)
		default:
			a.upstreamURL = u
		}
	}

	a.transport = &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: dialTimeout, KeepAlive: 30 * time.Second}).DialContext,
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          64,
		MaxIdleConnsPerHost:   16,
		IdleConnTimeout:       idleConnTimeout,
		TLSHandshakeTimeout:   tlsTimeout,
		ExpectContinueTimeout: time.Second,
		ResponseHeaderTimeout: respHdrTimeout,
	}

	if sub, err := fs.Sub(docsFS, "docs"); err == nil {
		a.docsFileServer = http.FileServer(http.FS(sub))
	}

	return a
}

// proxyRequestState carries per-request information gathered while the
// response streams back to the client. It is confined to the request's
// goroutine, so no locking is needed.
type proxyRequestState struct {
	user      string
	model     string
	stream    bool
	start     time.Time
	usage     openAIUsage
	haveUsage bool
}

// openAIUsage mirrors the usage object reported by OpenAI-compatible APIs.
type openAIUsage struct {
	PromptTokens     int `json:"prompt_tokens"`
	CompletionTokens int `json:"completion_tokens"`
	TotalTokens      int `json:"total_tokens"`
}

// getUser resolves the current user from the local environment (desktop mode)
// or from headers set by the authenticating reverse proxy (server mode).
func (a *api) getUser(r *http.Request) UserInfo {
	if a.isServerMode {
		for _, header := range proxyHeaders {
			if val := r.Header.Get(header); val != "" {
				return UserInfo{
					Username: strings.TrimSpace(val),
					AuthType: "proxy_header (" + header + ")",
					Mode:     "server",
				}
			}
		}
		return UserInfo{
			Username: "anonymous",
			AuthType: "none",
			Mode:     "server",
		}
	}

	username := os.Getenv("USER")
	if username == "" {
		username = os.Getenv("USERNAME")
	}
	if username == "" {
		username = os.Getenv("LOGNAME")
	}
	if username == "" {
		username = "localuser"
	}

	return UserInfo{
		Username: username,
		AuthType: "local_env",
		Mode:     "desktop",
	}
}

// health returns the operational status of the service.
// @Summary Health Check
// @Description Returns operational status of the service.
// @Produce json
// @Success 200 {object} HealthResponse "Healthy response"
// @Router /api/v1/health [get]
func (a *api) health(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(map[string]string{
		"status":    "ok",
		"timestamp": time.Now().Format(time.RFC3339),
	})
}

// user returns the currently authenticated user.
// @Summary Get Current User
// @Description Returns authenticated user info based on environment or proxy headers.
// @Produce json
// @Success 200 {object} UserInfo
// @Router /api/v1/user [get]
func (a *api) user(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(a.getUser(r))
}

// info returns system information such as mode, Go version, OS/Arch, uptime
// and a summary of the proxy configuration.
// @Summary System Info
// @Description Returns system metrics, Go version, OS/Arch, uptime and a proxy configuration summary.
// @Produce json
// @Success 200 {object} SystemInfoResponse
// @Router /api/v1/info [get]
func (a *api) info(w http.ResponseWriter, r *http.Request) {
	mode := "desktop"
	if a.isServerMode {
		mode = "server"
	}

	upstream := ""
	if a.upstreamURL != nil {
		upstream = a.upstreamURL.String()
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(SystemInfoResponse{
		Mode:             mode,
		GoVersion:        runtime.Version(),
		OS:               runtime.GOOS,
		Arch:             runtime.GOARCH,
		Uptime:           time.Since(a.startTime).Truncate(time.Second).String(),
		Upstream:         upstream,
		APIKeyConfigured: a.cfg.APIKey != "",
		AllowedModels:    len(a.cfg.AllowedModels),
		BlockedModels:    len(a.cfg.BlockedModels),
	})
}

// usage returns per-user aggregated token usage and request counts.
// @Summary Usage Summary
// @Description Returns per-user aggregated request counts and token usage recorded by the proxy.
// @Produce json
// @Success 200 {object} UsageResponse
// @Router /api/v1/usage [get]
func (a *api) usageHandler(w http.ResponseWriter, r *http.Request) {
	a.mu.RLock()
	total := UserUsage{User: "total", Models: map[string]int64{}}
	users := make([]UserUsage, 0, len(a.usage))
	for _, u := range a.usage {
		clone := *u
		clone.Models = copyModelCounts(u.Models)
		users = append(users, clone)
		for m, n := range u.Models {
			total.Models[m] += n
		}
		total.Requests += u.Requests
		total.PromptTokens += u.PromptTokens
		total.CompletionTokens += u.CompletionTokens
		total.TotalTokens += u.TotalTokens
		if u.LastActivity.After(total.LastActivity) {
			total.LastActivity = u.LastActivity
		}
	}
	a.mu.RUnlock()

	sort.Slice(users, func(i, j int) bool {
		if users[i].Requests != users[j].Requests {
			return users[i].Requests > users[j].Requests
		}
		return users[i].User < users[j].User
	})
	if users == nil {
		users = []UserUsage{}
	}

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(UsageResponse{Total: total, Users: users})
}

// copyModelCounts clones a per-model request counter map.
func copyModelCounts(src map[string]int64) map[string]int64 {
	out := make(map[string]int64, len(src))
	for m, n := range src {
		out[m] = n
	}
	return out
}

// logs returns the most recent proxied API requests.
// @Summary Recent Requests
// @Description Returns the most recent proxied API requests, newest first.
// @Produce json
// @Param limit query int false "Maximum number of entries to return (default 100)"
// @Success 200 {object} LogResponse
// @Router /api/v1/logs [get]
func (a *api) logsHandler(w http.ResponseWriter, r *http.Request) {
	limit := 100
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}

	a.mu.RLock()
	entries := make([]RequestLogEntry, 0, len(a.logs))
	for i, e := range a.logs {
		if i >= limit {
			break
		}
		entries = append(entries, e)
	}
	a.mu.RUnlock()

	w.Header().Set("Content-Type", "application/json")
	json.NewEncoder(w).Encode(LogResponse{Entries: entries})
}

// proxyOpenAI forwards OpenAI-compatible API requests to the configured
// upstream endpoint, accounting usage per user and applying the model policy.
//
// @Summary Proxy OpenAI-compatible endpoint
// @Description Forwards the request to the configured OpenAI-compatible upstream, records per-user usage and applies the model allow/deny policy. Supports streaming (SSE) responses. Any path under /v1 is forwarded, with the /v1 prefix stripped against the upstream's base path.
// @Accept json
// @Produce json
// @Success 200 {object} object "OpenAI-compatible response (JSON, or an SSE event stream for streamed requests)"
// @Failure 403 {object} ErrorResponse "Model blocked by policy"
// @Failure 502 {object} ErrorResponse "Upstream request failed"
// @Failure 503 {object} ErrorResponse "No upstream configured"
// @Router /v1/chat/completions [post]
// @Router /v1/completions [post]
// @Router /v1/responses [post]
// @Router /v1/embeddings [post]
// @Router /v1/moderations [post]
// @Router /v1/images/generations [post]
// @Router /v1/audio/speech [post]
// @Router /v1/audio/transcriptions [post]
// @Router /v1/audio/translations [post]
// @Router /v1/models [get]
// @Router /v1/models/{model} [get]
func (a *api) proxyOpenAI(w http.ResponseWriter, r *http.Request) {
	if a.upstreamURL == nil {
		writeOpenAIError(w, http.StatusServiceUnavailable,
			"No upstream endpoint configured. Start the proxy with -upstream (or OPENAI_BASE_URL) pointing at an OpenAI-compatible endpoint.",
			"proxy_error", "", "")
		return
	}

	st := &proxyRequestState{
		user:  a.getUser(r).Username,
		start: time.Now(),
	}

	// Sniff the request body for the target model and streaming flag. The
	// body is reassembled unchanged afterwards, so the proxy stays
	// transparent for multipart uploads and very large payloads.
	st.model, st.stream = a.inspectRequestBody(r)

	if st.model != "" && !a.modelAllowed(st.model) {
		writeOpenAIError(w, http.StatusForbidden,
			fmt.Sprintf("Model %q is not permitted by this proxy's model policy.", st.model),
			"invalid_request_error", "model", "model_not_allowed")
		a.recordRequest(st, http.StatusForbidden, r)
		return
	}

	proxy := &httputil.ReverseProxy{
		Rewrite:       a.rewriteUpstream,
		Transport:     a.transport,
		FlushInterval: -1, // flush after every write so SSE chunks arrive immediately
		ModifyResponse: func(resp *http.Response) error {
			return a.wrapUsageCapture(resp, st)
		},
		ErrorHandler: func(w http.ResponseWriter, r *http.Request, err error) {
			if !errors.Is(err, context.Canceled) {
				log.Printf("[proxy] upstream error: user=%s %s %s: %v", st.user, r.Method, r.URL.Path, err)
			}
			writeOpenAIError(w, http.StatusBadGateway,
				"Upstream request failed: "+err.Error(),
				"proxy_error", "", "")
		},
		ErrorLog: log.New(io.Discard, "", 0), // quiet; the ErrorHandler above logs what matters
	}

	rec := &statusRecorder{ResponseWriter: w}
	proxy.ServeHTTP(rec, r)
	a.recordRequest(st, rec.status, r)
}

// rewriteUpstream points the outbound request at the configured upstream. The
// canonical proxy path is "/v1/...": the "/v1" prefix is stripped and the
// remainder appended to the upstream's base path. Requests that arrive at the
// bare endpoint paths (e.g. "/chat/completions") are forwarded unchanged, so
// clients configured without the "/v1" suffix also work.
func (a *api) rewriteUpstream(pr *httputil.ProxyRequest) {
	pr.SetURL(a.upstreamURL)

	remainder := stripV1Prefix(pr.In.URL.Path)
	outPath := path.Join("/", a.upstreamURL.Path, remainder)
	if outPath == "" {
		outPath = "/"
	}
	pr.Out.URL.Path = outPath
	pr.Out.URL.RawPath = ""

	pr.SetXForwarded()

	if a.cfg.APIKey != "" {
		pr.Out.Header.Set("Authorization", "Bearer "+a.cfg.APIKey)
	}
	// Ask for an uncompressed response so usage capture can read the body.
	// Servers that ignore this are handled by the gzip fallback in
	// wrapUsageCapture.
	pr.Out.Header.Set("Accept-Encoding", "identity")
}

// stripV1Prefix removes a leading "/v1" (or "/v1/") from an inbound proxy
// path; anything else is returned unchanged.
func stripV1Prefix(p string) string {
	if p == "/v1" {
		return "/"
	}
	if strings.HasPrefix(p, "/v1/") {
		return strings.TrimPrefix(p, "/v1")
	}
	return p
}

// bodyReassembler is a ReadCloser that replays bytes read earlier and then
// continues with the rest of the original body, closing it on Close.
type bodyReassembler struct {
	io.Reader
	original io.Reader
}

// Close closes the original body once the reassembled body is consumed.
func (b *bodyReassembler) Close() error {
	if c, ok := b.original.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// inspectRequestBody reads a JSON request body (within limits), extracts the
// model and streaming flag, and reassembles the body unchanged for forwarding.
// Requests it cannot handle (multipart uploads, oversized or unreadable
// bodies) are passed through untouched with no model information.
func (a *api) inspectRequestBody(r *http.Request) (model string, stream bool) {
	if r.Body == nil {
		return "", false
	}
	switch r.Method {
	case http.MethodPost, http.MethodPut, http.MethodPatch:
	default:
		return "", false
	}

	ct := r.Header.Get("Content-Type")
	if ct != "" && !strings.HasPrefix(ct, "application/json") {
		return "", false // multipart uploads (audio, files) stream through uninspected
	}

	data, err := io.ReadAll(io.LimitReader(r.Body, maxInspectBody+1))
	if err != nil {
		// Cannot read the whole body; forward the part we did read.
		r.Body = &bodyReassembler{Reader: io.MultiReader(bytes.NewReader(data), r.Body), original: r.Body}
		log.Printf("[proxy] request body read error from %s: %v", r.URL.Path, err)
		return "", false
	}
	// Replay what was read, then continue with the remainder of the body,
	// so the request is forwarded byte-for-byte unchanged.
	r.Body = &bodyReassembler{Reader: io.MultiReader(bytes.NewReader(data), r.Body), original: r.Body}

	if len(data) > maxInspectBody {
		return "", false // oversized: forward without sniffing
	}

	var probe struct {
		Model  string `json:"model"`
		Stream bool   `json:"stream"`
	}
	if err := json.Unmarshal(data, &probe); err != nil {
		return "", false // not JSON (or empty): nothing to sniff
	}
	return strings.TrimSpace(probe.Model), probe.Stream
}

// modelAllowed applies the configured model policy: a non-empty allowlist
// must match, and the blocklist must not match. Patterns are
// case-insensitive and support a trailing "*" wildcard.
func (a *api) modelAllowed(model string) bool {
	if model == "" {
		return true
	}
	if len(a.cfg.AllowedModels) > 0 && !matchAnyPattern(a.cfg.AllowedModels, model) {
		return false
	}
	if len(a.cfg.BlockedModels) > 0 && matchAnyPattern(a.cfg.BlockedModels, model) {
		return false
	}
	return true
}

// matchAnyPattern reports whether model matches any of the patterns.
func matchAnyPattern(patterns []string, model string) bool {
	for _, p := range patterns {
		if matchModelPattern(p, model) {
			return true
		}
	}
	return false
}

// matchModelPattern matches a model against a case-insensitive pattern that
// may end in "*" as a prefix wildcard.
func matchModelPattern(pattern, model string) bool {
	pattern = strings.ToLower(strings.TrimSpace(pattern))
	model = strings.ToLower(strings.TrimSpace(model))
	if pattern == "" {
		return false
	}
	if suffix, wild := strings.CutSuffix(pattern, "*"); wild {
		return strings.HasPrefix(model, suffix)
	}
	return pattern == model
}

// wrapUsageCapture wraps the upstream response body so token usage is
// captured as the data streams to the client. JSON responses are scanned at
// EOF; SSE streams are scanned line by line, which is how streamed
// chat completions report usage in their final chunk.
func (a *api) wrapUsageCapture(resp *http.Response, st *proxyRequestState) error {
	if resp.Body == nil {
		return nil
	}

	ct := resp.Header.Get("Content-Type")
	isSSE := strings.HasPrefix(ct, "text/event-stream")
	isJSON := strings.HasPrefix(ct, "application/json")
	if !isSSE && !isJSON {
		return nil
	}

	src := io.Reader(resp.Body)
	if resp.Header.Get("Content-Encoding") == "gzip" {
		if gr, err := gzip.NewReader(resp.Body); err == nil {
			// The body is decompressed from here on, so the encoding
			// headers no longer describe it.
			resp.Header.Del("Content-Encoding")
			resp.Header.Del("Content-Length")
			src = gr
		}
	}

	resp.Body = &usageCaptureReader{
		src:     src,
		sse:     isSSE,
		limit:   maxCaptureBody,
		onUsage: func(u openAIUsage) { st.usage = u; st.haveUsage = true },
	}
	return nil
}

// usageCaptureReader passes bytes through to the client unchanged while
// eavesdropping on the stream to extract token usage.
type usageCaptureReader struct {
	src      io.Reader
	sse      bool
	limit    int
	captured []byte // JSON response prefix, bounded by limit
	overflow bool   // captured buffer filled; JSON capture abandoned
	lineBuf  []byte // partial SSE line awaiting its newline
	lineOver bool   // current SSE line exceeded maxSSELineLen
	done     bool   // EOF seen; final scan performed
	onUsage  func(openAIUsage)
}

// Read forwards bytes to the client and scans them for usage information.
func (u *usageCaptureReader) Read(p []byte) (int, error) {
	n, err := u.src.Read(p)
	if n > 0 {
		if u.sse {
			u.scanSSE(p[:n], false)
		} else {
			u.captureJSON(p[:n])
		}
	}
	if err != nil && !u.done {
		u.done = true
		if u.sse {
			// The final line may not end with a newline.
			u.scanSSE(nil, true)
		} else if !u.overflow && len(u.captured) > 0 {
			u.parseJSONUsage(u.captured)
		}
	}
	return n, err
}

// Close closes the underlying source once the response has been fully
// delivered, satisfying io.ReadCloser for http.Response.Body.
func (u *usageCaptureReader) Close() error {
	if c, ok := u.src.(io.Closer); ok {
		return c.Close()
	}
	return nil
}

// captureJSON accumulates a bounded copy of a JSON response body.
func (u *usageCaptureReader) captureJSON(chunk []byte) {
	if u.overflow {
		return
	}
	if len(chunk) > u.limit-len(u.captured) {
		u.overflow = true
		u.captured = nil
		return
	}
	u.captured = append(u.captured, chunk...)
}

// scanSSE feeds a chunk (plus an end-of-stream marker) through the line
// splitter and inspects each complete "data:" line for usage.
func (u *usageCaptureReader) scanSSE(chunk []byte, end bool) {
	if len(chunk) > 0 {
		for len(chunk) > 0 {
			idx := bytes.IndexByte(chunk, '\n')
			if idx < 0 {
				u.appendLine(chunk)
				return
			}
			u.appendLine(chunk[:idx])
			u.flushSSELine()
			chunk = chunk[idx+1:]
		}
	}
	if end {
		u.flushSSELine()
	}
}

// appendLine buffers partial SSE line bytes, giving up on overly long lines.
func (u *usageCaptureReader) appendLine(b []byte) {
	if u.lineOver {
		return
	}
	if len(u.lineBuf)+len(b) > maxSSELineLen {
		u.lineOver = true
		u.lineBuf = nil
		return
	}
	u.lineBuf = append(u.lineBuf, b...)
}

// flushSSELine inspects the buffered line, then resets the line buffer.
func (u *usageCaptureReader) flushSSELine() {
	line := strings.TrimSpace(string(u.lineBuf))
	u.lineBuf = nil
	u.lineOver = false
	if !strings.HasPrefix(line, "data:") {
		return
	}
	payload := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
	if payload == "" || payload == "[DONE]" {
		return
	}
	u.parseJSONUsage([]byte(payload))
}

// parseJSONUsage unmarshals a JSON snippet and reports any usage object found.
func (u *usageCaptureReader) parseJSONUsage(data []byte) {
	var probe struct {
		Usage *openAIUsage `json:"usage"`
	}
	if err := json.Unmarshal(data, &probe); err != nil || probe.Usage == nil {
		return
	}
	if probe.Usage.PromptTokens == 0 && probe.Usage.CompletionTokens == 0 && probe.Usage.TotalTokens == 0 {
		return
	}
	u.onUsage(*probe.Usage)
}

// recordRequest updates the per-user aggregates and the recent request log.
func (a *api) recordRequest(st *proxyRequestState, status int, r *http.Request) {
	now := time.Now()
	entry := RequestLogEntry{
		Time:             st.start.Format(time.RFC3339),
		User:             st.user,
		Method:           r.Method,
		Path:             r.URL.Path,
		Model:            st.model,
		Stream:           st.stream,
		Status:           status,
		DurationMs:       now.Sub(st.start).Milliseconds(),
		PromptTokens:     st.usage.PromptTokens,
		CompletionTokens: st.usage.CompletionTokens,
		TotalTokens:      st.usage.TotalTokens,
	}

	a.mu.Lock()
	agg := a.usage[st.user]
	if agg == nil {
		agg = &UserUsage{User: st.user, Models: map[string]int64{}}
		a.usage[st.user] = agg
	}
	agg.Requests++
	if st.model != "" {
		agg.Models[st.model]++
	}
	if st.haveUsage {
		agg.PromptTokens += int64(st.usage.PromptTokens)
		agg.CompletionTokens += int64(st.usage.CompletionTokens)
		agg.TotalTokens += int64(st.usage.TotalTokens)
	}
	agg.LastActivity = now
	a.logs = append([]RequestLogEntry{entry}, a.logs...)
	if len(a.logs) > maxLogEntries {
		a.logs = a.logs[:maxLogEntries]
	}
	a.mu.Unlock()

	log.Printf("[proxy] %s %s user=%s model=%s status=%d duration=%dms prompt_tokens=%d completion_tokens=%d total_tokens=%d",
		entry.Method, entry.Path, entry.User, entry.Model, entry.Status, entry.DurationMs,
		entry.PromptTokens, entry.CompletionTokens, entry.TotalTokens)
}

// writeOpenAIError writes an error in the OpenAI API error format.
func writeOpenAIError(w http.ResponseWriter, status int, message, errType, param, code string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(ErrorResponse{Error: OpenAIError{
		Message: message,
		Type:    errType,
		Param:   param,
		Code:    code,
	}})
}

// statusRecorder captures the status code written to the client so the proxy
// can log it. Unwrap lets http.ResponseController reach the underlying
// writer's Flusher, which keeps SSE streaming working.
type statusRecorder struct {
	http.ResponseWriter
	status int
}

func (s *statusRecorder) WriteHeader(code int) {
	s.status = code
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	return s.ResponseWriter.Write(b)
}

// Unwrap exposes the wrapped ResponseWriter to http.ResponseController.
func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}

// serveDocs serves the Swagger UI documentation page, the raw OpenAPI
// specification and the Swagger UI assets. The embedded "docs" directory is
// exposed under /docs so that the page, spec and assets resolve consistently
// with a copy of the "docs" folder hosted on a static web site.
func (a *api) serveDocs(w http.ResponseWriter, r *http.Request) {
	if a.docsFileServer == nil {
		http.NotFound(w, r)
		return
	}

	path := strings.TrimPrefix(r.URL.Path, "/docs")
	switch path {
	case "/api", "/api/":
		path = "/api.html"
	case "/api/swagger.json", "/api/openapi.json":
		path = "/swagger.json"
	}

	req := r.Clone(r.Context())
	req.URL.Path = path
	a.docsFileServer.ServeHTTP(w, req)
}

// swaggerUIHTML returns the Swagger UI documentation page. It references the
// Swagger UI assets and the OpenAPI specification with relative URLs so that
// the same page works both inside the application (at /docs/api) and when the
// contents of the "docs" folder are copied to a static web site.
func swaggerUIHTML() []byte {
	return []byte(`<!DOCTYPE html>
<html lang="en">
<head>
<meta charset="utf-8"/>
<meta name="viewport" content="width=device-width, initial-scale=1"/>
<title>OpenAI Proxy - API Documentation</title>
<link rel="stylesheet" href="swagger-ui/swagger-ui.css"/>
<style>
body { margin: 0; font-family: -apple-system, BlinkMacSystemFont, 'Segoe UI', Roboto, sans-serif; }
.topbar { background: #1e293b; border-bottom: 1px solid #334155; padding: 0.75rem 1.5rem; display: flex; align-items: center; gap: 1rem; flex-wrap: wrap; }
.topbar h1 { font-size: 1.1rem; font-weight: 600; color: #38bdf8; margin: 0; }
.topbar a { color: #94a3b8; font-size: 0.9rem; text-decoration: none; margin-left: auto; }
.topbar a:hover { color: #38bdf8; }
</style>
</head>
<body>
<div class="topbar">
  <h1>OpenAI Proxy - API Documentation</h1>
  <a href="../">Back to Home</a>
</div>
<div id="swagger-ui"></div>
<script src="swagger-ui/swagger-ui-bundle.js"></script>
<script src="swagger-ui/swagger-ui-standalone-preset.js"></script>
<script>
window.onload = function () {
  window.ui = SwaggerUIBundle({
    url: "swagger.json",
    dom_id: "#swagger-ui",
    deepLinking: true,
    presets: [SwaggerUIBundle.presets.apis, SwaggerUIStandalonePreset],
    layout: "StandaloneLayout",
    docExpansion: "list",
    displayRequestDuration: true,
    filter: true,
  });
};
</script>
</body>
</html>`)
}
