package main

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"io"
	"mime"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisteredMimeTypes(t *testing.T) {
	registerMimeTypes()

	want := map[string]string{
		".html": "text/html; charset=utf-8",
		".css":  "text/css; charset=utf-8",
		".js":   "text/javascript; charset=utf-8",
		".json": "application/json",
	}
	for ext, wantType := range want {
		if got := mime.TypeByExtension(ext); got != wantType {
			t.Errorf("TypeByExtension(%q) = %q, want %q", ext, got, wantType)
		}
	}
}

func TestHealthEndpoint(t *testing.T) {
	a := newAPI(false, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/api/v1/health", nil)
	rr := httptest.NewRecorder()

	a.health(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var resp map[string]string
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if resp["status"] != "ok" {
		t.Errorf("expected status 'ok', got %s", resp["status"])
	}
}

func TestUserEndpointDesktop(t *testing.T) {
	t.Setenv("USER", "tester")
	a := newAPI(false, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/api/v1/user", nil)
	rr := httptest.NewRecorder()

	a.user(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}

	var resp UserInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if resp.Mode != "desktop" {
		t.Errorf("expected mode 'desktop', got %s", resp.Mode)
	}
	if resp.Username != "tester" {
		t.Errorf("expected username 'tester', got %s", resp.Username)
	}
}

func TestUserEndpointServerProxyHeader(t *testing.T) {
	a := newAPI(true, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/api/v1/user", nil)
	req.Header.Set("Remote-User", "alice_proxy")
	rr := httptest.NewRecorder()

	a.user(rr, req)

	var resp UserInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if resp.Username != "alice_proxy" {
		t.Errorf("expected username 'alice_proxy', got %s", resp.Username)
	}
	if resp.Mode != "server" {
		t.Errorf("expected mode 'server', got %s", resp.Mode)
	}
}

func TestUserEndpointServerNoHeader(t *testing.T) {
	a := newAPI(true, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/api/v1/user", nil)
	rr := httptest.NewRecorder()

	a.user(rr, req)

	var resp UserInfo
	if err := json.Unmarshal(rr.Body.Bytes(), &resp); err != nil {
		t.Fatalf("failed to parse json response: %v", err)
	}

	if resp.Username != "anonymous" {
		t.Errorf("expected username 'anonymous', got %s", resp.Username)
	}
}

func TestUsageAndLogsEndpoints(t *testing.T) {
	t.Setenv("USER", "tester")
	a := newAPI(false, docsFS, ProxyConfig{})

	// Seed some activity directly through the recorder path.
	req := httptest.NewRequest("GET", "/api/v1/usage", nil)
	rr := httptest.NewRecorder()
	a.usageHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /usage returned %d", rr.Code)
	}

	var usage UsageResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &usage); err != nil {
		t.Fatalf("failed to parse usage json: %v", err)
	}
	if len(usage.Users) != 0 {
		t.Errorf("expected no usage entries initially, got %d", len(usage.Users))
	}

	req = httptest.NewRequest("GET", "/api/v1/logs", nil)
	rr = httptest.NewRecorder()
	a.logsHandler(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("GET /logs returned %d", rr.Code)
	}

	var logs LogResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &logs); err != nil {
		t.Fatalf("failed to parse logs json: %v", err)
	}
	if len(logs.Entries) != 0 {
		t.Errorf("expected no log entries initially, got %d", len(logs.Entries))
	}
}

// newTestUpstream starts a fake OpenAI-compatible upstream that records the
// requests it receives and replies with the given body.
func newTestUpstream(t *testing.T, handler http.HandlerFunc) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return srv
}

func TestProxyPassthroughAndUsage(t *testing.T) {
	t.Setenv("USER", "tester")

	var gotPath, gotAuth string
	upstream := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		body, _ := io.ReadAll(r.Body)
		if string(body) != `{"model":"gpt-4o","messages":[]}` {
			t.Errorf("upstream got unexpected body: %s", body)
		}
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"id":"chatcmpl-1","object":"chat.completion","model":"gpt-4o","usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`))
	})

	a := newAPI(false, docsFS, ProxyConfig{
		Upstream: upstream.URL + "/v1",
		APIKey:   "sk-upstream-test",
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","messages":[]}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.proxyOpenAI(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("proxy returned %d, body: %s", rr.Code, rr.Body.String())
	}
	if !strings.Contains(rr.Body.String(), `"total_tokens":15`) {
		t.Errorf("client should receive the upstream body verbatim, got: %s", rr.Body.String())
	}
	if gotPath != "/v1/chat/completions" {
		t.Errorf("upstream path = %q, want /v1/chat/completions (stripped /v1 joined to upstream base)", gotPath)
	}
	if gotAuth != "Bearer sk-upstream-test" {
		t.Errorf("upstream Authorization = %q, want configured key", gotAuth)
	}

	var usage UsageResponse
	req = httptest.NewRequest("GET", "/api/v1/usage", nil)
	rr = httptest.NewRecorder()
	a.usageHandler(rr, req)
	if err := json.Unmarshal(rr.Body.Bytes(), &usage); err != nil {
		t.Fatalf("failed to parse usage json: %v", err)
	}
	if len(usage.Users) != 1 {
		t.Fatalf("expected 1 user entry, got %d", len(usage.Users))
	}
	u := usage.Users[0]
	if u.User != "tester" || u.Requests != 1 || u.TotalTokens != 15 || u.PromptTokens != 10 || u.CompletionTokens != 5 {
		t.Errorf("unexpected usage aggregate: %+v", u)
	}
	if u.Models["gpt-4o"] != 1 {
		t.Errorf("expected model counter gpt-4o=1, got %v", u.Models)
	}
	if usage.Total.TotalTokens != 15 {
		t.Errorf("expected total tokens 15, got %d", usage.Total.TotalTokens)
	}
}

func TestProxyStreamingUsageCapture(t *testing.T) {
	t.Setenv("USER", "tester")

	upstream := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		flusher := w.(http.Flusher)
		for _, chunk := range []string{
			"data: {\"id\":\"chatcmpl-2\",\"choices\":[{\"delta\":{\"content\":\"Hi\"}}]}\n\n",
			"data: {\"id\":\"chatcmpl-2\",\"choices\":[],\"usage\":{\"prompt_tokens\":20,\"completion_tokens\":8,\"total_tokens\":28}}\n\n",
			"data: [DONE]\n\n",
		} {
			w.Write([]byte(chunk))
			flusher.Flush()
		}
	})

	a := newAPI(false, docsFS, ProxyConfig{Upstream: upstream.URL + "/v1"})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o","stream":true}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.proxyOpenAI(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("proxy returned %d", rr.Code)
	}
	body := rr.Body.String()
	if !strings.Contains(body, `"content":"Hi"`) || !strings.Contains(body, "data: [DONE]") {
		t.Errorf("stream should be forwarded verbatim, got: %s", body)
	}

	a.mu.RLock()
	agg := a.usage["tester"]
	a.mu.RUnlock()
	if agg == nil || agg.TotalTokens != 28 || agg.PromptTokens != 20 || agg.CompletionTokens != 8 {
		t.Errorf("streaming usage not captured: %+v", agg)
	}
}

func TestProxyPathRewriteVariants(t *testing.T) {
	t.Setenv("USER", "tester")

	var gotPaths []string
	upstream := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		gotPaths = append(gotPaths, r.URL.Path)
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"object":"list","data":[]}`))
	})

	a := newAPI(false, docsFS, ProxyConfig{Upstream: upstream.URL + "/v1"})

	// Canonical path, bare path and a model detail path all land in the
	// expected upstream locations.
	cases := map[string]string{
		"/v1/models":        "/v1/models",
		"/models":           "/v1/models",
		"/v1/models/gpt-4o": "/v1/models/gpt-4o",
	}
	for inbound := range cases {
		req := httptest.NewRequest("GET", inbound, nil)
		rr := httptest.NewRecorder()
		a.proxyOpenAI(rr, req)
		if rr.Code != http.StatusOK {
			t.Errorf("%s: proxy returned %d", inbound, rr.Code)
		}
	}
	if len(gotPaths) != len(cases) {
		t.Fatalf("upstream saw %d requests, want %d", len(gotPaths), len(cases))
	}
	for _, want := range cases {
		found := false
		for _, got := range gotPaths {
			if got == want {
				found = true
			}
		}
		if !found {
			t.Errorf("upstream never saw %q; saw %v", want, gotPaths)
		}
	}
}

func TestProxyBlockedModel(t *testing.T) {
	t.Setenv("USER", "tester")

	upstreamHit := false
	upstream := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		upstreamHit = true
		w.Write([]byte(`{}`))
	})

	a := newAPI(false, docsFS, ProxyConfig{
		Upstream:      upstream.URL + "/v1",
		BlockedModels: []string{"gpt-4*"},
	})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.proxyOpenAI(rr, req)

	if rr.Code != http.StatusForbidden {
		t.Fatalf("blocked model returned %d, want 403", rr.Code)
	}
	if upstreamHit {
		t.Errorf("blocked request must not reach the upstream")
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error json: %v", err)
	}
	if errResp.Error.Param != "model" || errResp.Error.Message == "" {
		t.Errorf("unexpected error payload: %+v", errResp.Error)
	}
}

func TestProxyAllowedModels(t *testing.T) {
	t.Setenv("USER", "tester")

	upstream := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Write([]byte(`{"ok":true}`))
	})

	a := newAPI(false, docsFS, ProxyConfig{
		Upstream:      upstream.URL + "/v1",
		AllowedModels: []string{"gpt-4o", "claude-*"},
	})

	// A model outside the allowlist is refused.
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"mistral-7b"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()
	a.proxyOpenAI(rr, req)
	if rr.Code != http.StatusForbidden {
		t.Errorf("model outside allowlist returned %d, want 403", rr.Code)
	}

	// An allowlisted exact model passes.
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"gpt-4o"}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	a.proxyOpenAI(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("allowlisted model returned %d, want 200", rr.Code)
	}

	// An allowlisted wildcard model passes.
	req = httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{"model":"claude-3-sonnet"}`))
	req.Header.Set("Content-Type", "application/json")
	rr = httptest.NewRecorder()
	a.proxyOpenAI(rr, req)
	if rr.Code != http.StatusOK {
		t.Errorf("wildcard-allowlisted model returned %d, want 200", rr.Code)
	}
}

func TestProxyNoUpstreamConfigured(t *testing.T) {
	a := newAPI(false, docsFS, ProxyConfig{})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	rr := httptest.NewRecorder()

	a.proxyOpenAI(rr, req)

	if rr.Code != http.StatusServiceUnavailable {
		t.Fatalf("no-upstream returned %d, want 503", rr.Code)
	}
	if !strings.Contains(rr.Body.String(), "No upstream endpoint configured") {
		t.Errorf("expected explanatory error message, got: %s", rr.Body.String())
	}
}

func TestProxyUpstreamError(t *testing.T) {
	t.Setenv("USER", "tester")

	// Close the listener without serving: connections are refused.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	upstreamURL := srv.URL
	srv.Close()

	a := newAPI(false, docsFS, ProxyConfig{Upstream: upstreamURL + "/v1"})

	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(`{}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.proxyOpenAI(rr, req)

	if rr.Code != http.StatusBadGateway {
		t.Fatalf("upstream failure returned %d, want 502", rr.Code)
	}
	var errResp ErrorResponse
	if err := json.Unmarshal(rr.Body.Bytes(), &errResp); err != nil {
		t.Fatalf("failed to parse error json: %v", err)
	}
	if errResp.Error.Type != "proxy_error" {
		t.Errorf("unexpected error type %q", errResp.Error.Type)
	}
}

func TestProxyGzipResponseDecodedForCapture(t *testing.T) {
	t.Setenv("USER", "tester")

	upstream := newTestUpstream(t, func(w http.ResponseWriter, r *http.Request) {
		// Pretend the upstream ignored our Accept-Encoding: identity and
		// gzipped the response anyway.
		var buf bytes.Buffer
		zw := gzip.NewWriter(&buf)
		zw.Write([]byte(`{"usage":{"prompt_tokens":3,"completion_tokens":4,"total_tokens":7}}`))
		zw.Close()
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Encoding", "gzip")
		w.Write(buf.Bytes())
	})

	a := newAPI(false, docsFS, ProxyConfig{Upstream: upstream.URL + "/v1"})

	req := httptest.NewRequest("POST", "/v1/embeddings", strings.NewReader(`{"model":"text-embedding-3-small"}`))
	req.Header.Set("Content-Type", "application/json")
	rr := httptest.NewRecorder()

	a.proxyOpenAI(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("proxy returned %d", rr.Code)
	}
	if enc := rr.Header().Get("Content-Encoding"); enc != "" {
		t.Errorf("response should be delivered decompressed, got Content-Encoding %q", enc)
	}
	if !strings.Contains(rr.Body.String(), `"total_tokens":7`) {
		t.Errorf("client should receive the decoded JSON, got: %s", rr.Body.String())
	}

	a.mu.RLock()
	agg := a.usage["tester"]
	a.mu.RUnlock()
	if agg == nil || agg.TotalTokens != 7 {
		t.Errorf("usage not captured from gzipped response: %+v", agg)
	}
}

func TestModelFilterMatching(t *testing.T) {
	cases := []struct {
		pattern string
		model   string
		want    bool
	}{
		{"gpt-4o", "gpt-4o", true},
		{"gpt-4o", "GPT-4O", true},
		{"gpt-4*", "gpt-4o", true},
		{"gpt-4*", "gpt-3.5-turbo", false},
		{"*", "anything", true},
		{"gpt-4o", "gpt-4o-2024", false},
		{"", "gpt-4o", false},
	}
	for _, tc := range cases {
		if got := matchModelPattern(tc.pattern, tc.model); got != tc.want {
			t.Errorf("matchModelPattern(%q, %q) = %v, want %v", tc.pattern, tc.model, got, tc.want)
		}
	}
}

func TestStripV1Prefix(t *testing.T) {
	cases := map[string]string{
		"/v1":                  "/",
		"/v1/chat/completions": "/chat/completions",
		"/chat/completions":    "/chat/completions",
		"/":                    "/",
	}
	for in, want := range cases {
		if got := stripV1Prefix(in); got != want {
			t.Errorf("stripV1Prefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeDocs(t *testing.T) {
	a := newAPI(false, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/docs/api", nil)
	rr := httptest.NewRecorder()

	a.serveDocs(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("docs handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("expected text/html content type, got %q", ct)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("swagger-ui")) {
		t.Errorf("expected docs page to load Swagger UI")
	}
}

func TestServeOpenAPISpec(t *testing.T) {
	a := newAPI(false, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/docs/swagger.json", nil)
	rr := httptest.NewRecorder()

	a.serveDocs(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("spec handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "application/json" {
		t.Errorf("expected application/json content type, got %q", ct)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte(`"swagger": "2.0"`)) {
		t.Errorf("expected OpenAPI specification to be served")
	}
}

func TestServeSwaggerUIAsset(t *testing.T) {
	a := newAPI(false, docsFS, ProxyConfig{})
	req := httptest.NewRequest("GET", "/docs/swagger-ui/swagger-ui.css", nil)
	rr := httptest.NewRecorder()

	a.serveDocs(rr, req)

	if status := rr.Code; status != http.StatusOK {
		t.Errorf("asset handler returned wrong status code: got %v want %v", status, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/css; charset=utf-8" {
		t.Errorf("expected text/css content type, got %q", ct)
	}
	if !bytes.Contains(rr.Body.Bytes(), []byte("swagger-ui")) {
		t.Errorf("expected Swagger UI stylesheet to be served")
	}
}

func TestNormalizeBasePath(t *testing.T) {
	cases := map[string]string{
		"":           "",
		"   ":        "",
		"/":          "",
		"magooify":   "/magooify",
		"/magooify":  "/magooify",
		"/magooify/": "/magooify",
	}
	for in, want := range cases {
		if got := normalizeBasePath(in); got != want {
			t.Errorf("normalizeBasePath(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeAllRoutes(t *testing.T) {
	registerMimeTypes()
	a := newAPI(true, docsFS, ProxyConfig{})
	// A stripping proxy (Pangolin) removes the /magooify prefix before
	// forwarding, so the app sees the site-relative paths below.
	handler := buildHandler(a, nil)

	cases := []struct {
		path        string
		wantCode    int
		wantContent string
		wantBody    string
	}{
		{"/", http.StatusOK, "text/html", "OpenAI Proxy"},
		{"/style.css", http.StatusOK, "text/css", ".brand-icon"},
		{"/vendor/bootstrap/bootstrap.min.css", http.StatusOK, "text/css", "bootstrap"},
		{"/app.js", http.StatusOK, "text/javascript", "DOMContentLoaded"},
		{"/api/v1/user", http.StatusOK, "application/json", `"auth_type"`},
		{"/api/v1/health", http.StatusOK, "application/json", `"ok"`},
		{"/api/v1/usage", http.StatusOK, "application/json", `"total"`},
		{"/api/v1/logs", http.StatusOK, "application/json", `"entries"`},
		{"/docs/api", http.StatusOK, "text/html", "swagger-ui"},
		{"/docs/swagger.json", http.StatusOK, "application/json", `"swagger"`},
		{"/docs/swagger-ui/swagger-ui.css", http.StatusOK, "text/css", "swagger-ui"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.path, rr.Code, tc.wantCode)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantContent) {
			t.Errorf("%s: content-type = %q, want %q", tc.path, ct, tc.wantContent)
		}
		if tc.wantBody != "" && !bytes.Contains(rr.Body.Bytes(), []byte(tc.wantBody)) {
			t.Errorf("%s: body does not contain %q", tc.path, tc.wantBody)
		}
	}
}

func TestServeIndexPlainRelativeLinks(t *testing.T) {
	registerMimeTypes()
	a := newAPI(true, docsFS, ProxyConfig{})
	handler := buildHandler(a, nil)

	req := httptest.NewRequest("GET", "/", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); ct != "text/html; charset=utf-8" {
		t.Errorf("content-type = %q, want text/html", ct)
	}
	body := rr.Body.String()
	if strings.Contains(body, "<base") {
		t.Errorf("index.html must not contain an injected <base> tag")
	}
	if !strings.Contains(body, `href="vendor/bootstrap/bootstrap.min.css"`) {
		t.Errorf("index.html must keep plain relative asset links")
	}
	if !strings.Contains(body, `src="app.js"`) {
		t.Errorf("index.html must keep plain relative script links")
	}
}

func TestIndexRedirectAddsTrailingSlash(t *testing.T) {
	registerMimeTypes()
	a := newAPI(true, docsFS, ProxyConfig{})

	// The app is served from the site root; a proxy that passes paths through
	// unchanged (or direct access) reaches the SPA fallback. Only a stripping
	// proxy's empty path is redirected (see TestIndexRedirectWithStrippedPrefix);
	// files and SPA-fallback paths are served without a trailing-slash redirect.
	handler := buildHandler(a, nil)

	cases := []struct {
		path     string
		wantCode int
		wantHTML bool
		wantCT   string
	}{
		// Real files are served, never redirected.
		{"/app.js", http.StatusOK, false, "text/javascript"},
		{"/style.css", http.StatusOK, false, "text/css"},
		{"/vendor/bootstrap/bootstrap.min.css", http.StatusOK, false, "text/css"},
		// The index document and SPA-fallback paths are served as-is.
		{"/", http.StatusOK, true, "text/html"},
		{"/index.html", http.StatusOK, true, "text/html"},
		{"/magooify", http.StatusOK, true, "text/html"},
		{"/magooify?x=1", http.StatusOK, true, "text/html"},
		{"/foo", http.StatusOK, true, "text/html"},
	}
	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.path, rr.Code, tc.wantCode)
		}
		if loc := rr.Header().Get("Location"); loc != "" {
			t.Errorf("%s: unexpected Location %q", tc.path, loc)
		}
		if tc.wantHTML && !strings.Contains(rr.Body.String(), "OpenAI Proxy") {
			t.Errorf("%s: expected index.html body", tc.path)
		}
		if tc.wantCT != "" {
			if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantCT) {
				t.Errorf("%s: content-type = %q, want %q", tc.path, ct, tc.wantCT)
			}
		}
	}
}

func TestIndexRedirectWithStrippedPrefix(t *testing.T) {
	// Traefik/Pangolin stripPrefix on a bare "/magooify" forwards an empty
	// path with X-Forwarded-Prefix; the redirect must point at the external
	// URL with a trailing slash.
	req := httptest.NewRequest("GET", "/", nil)
	req.URL.Path = ""
	req.Header.Set("X-Forwarded-Prefix", "/magooify")
	if loc, ok := indexRedirectLocation(req); !ok || loc != "/magooify/" {
		t.Errorf("empty path: loc = %q, ok = %v, want %q, true", loc, ok, "/magooify/")
	}

	// An unsafe X-Forwarded-Prefix must not turn the redirect into an open
	// redirect; it falls back to the site root.
	req = httptest.NewRequest("GET", "/", nil)
	req.URL.Path = ""
	req.Header.Set("X-Forwarded-Prefix", "//evil.com")
	if loc, ok := indexRedirectLocation(req); !ok || loc != "/" {
		t.Errorf("unsafe prefix: loc = %q, ok = %v, want %q, true", loc, ok, "/")
	}

	// The root path already ends in "/" and needs no redirect, even through a
	// strip proxy.
	req = httptest.NewRequest("GET", "/", nil)
	req.Header.Set("X-Forwarded-Prefix", "/magooify")
	if _, ok := indexRedirectLocation(req); ok {
		t.Errorf("root path: expected no redirect")
	}
}

func TestSafeForwardedPrefix(t *testing.T) {
	cases := map[string]string{
		"":             "",
		"/magooify":    "/magooify",
		"/magooify/":   "/magooify",
		"magooify":     "",
		"//evil.com":   "",
		"/magooify/..": "",
		"/magooify?x":  "",
		"/magooify#x":  "",
		"/magooify\\x": "",
		" /sub ":       "/sub",
	}
	for in, want := range cases {
		if got := safeForwardedPrefix(in); got != want {
			t.Errorf("safeForwardedPrefix(%q) = %q, want %q", in, got, want)
		}
	}
}

func TestServeWithoutBasePathStillWorks(t *testing.T) {
	registerMimeTypes()
	a := newAPI(true, docsFS, ProxyConfig{})
	handler := buildHandler(a, nil)

	req := httptest.NewRequest("GET", "/style.css", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/css") {
		t.Errorf("content-type = %q, want text/css", ct)
	}

	req = httptest.NewRequest("GET", "/", nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)

	if rr.Code != http.StatusOK {
		t.Errorf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, "text/html") {
		t.Errorf("content-type = %q, want text/html", ct)
	}
}

func TestServeUnderBasePath(t *testing.T) {
	registerMimeTypes()
	a := newAPI(true, docsFS, ProxyConfig{})
	// With --base-path=/magooify the app serves the sub-path (a proxy passes
	// the full path through) while continuing to serve the site root.
	handler := buildHandler(a, []string{"/magooify"})

	cases := []struct {
		path        string
		wantCode    int
		wantContent string
		wantBody    string
	}{
		// Under the base path.
		{"/magooify/", http.StatusOK, "text/html", "OpenAI Proxy"},
		{"/magooify/style.css", http.StatusOK, "text/css", ".brand-icon"},
		{"/magooify/vendor/bootstrap/bootstrap.min.css", http.StatusOK, "text/css", "bootstrap"},
		{"/magooify/app.js", http.StatusOK, "text/javascript", "DOMContentLoaded"},
		{"/magooify/api/v1/user", http.StatusOK, "application/json", `"auth_type"`},
		{"/magooify/api/v1/health", http.StatusOK, "application/json", `"ok"`},
		{"/magooify/docs/api", http.StatusOK, "text/html", "swagger-ui"},
		{"/magooify/docs/swagger.json", http.StatusOK, "application/json", `"swagger"`},
		{"/magooify/docs/swagger-ui/swagger-ui.css", http.StatusOK, "text/css", "swagger-ui"},
		// The site root keeps working alongside the base path.
		{"/", http.StatusOK, "text/html", "OpenAI Proxy"},
		{"/style.css", http.StatusOK, "text/css", ".brand-icon"},
		{"/app.js", http.StatusOK, "text/javascript", "DOMContentLoaded"},
		{"/api/v1/health", http.StatusOK, "application/json", `"ok"`},
		{"/docs/api", http.StatusOK, "text/html", "swagger-ui"},
	}

	for _, tc := range cases {
		req := httptest.NewRequest("GET", tc.path, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)

		if rr.Code != tc.wantCode {
			t.Errorf("%s: status = %d, want %d", tc.path, rr.Code, tc.wantCode)
		}
		if ct := rr.Header().Get("Content-Type"); !strings.Contains(ct, tc.wantContent) {
			t.Errorf("%s: content-type = %q, want %q", tc.path, ct, tc.wantContent)
		}
		if tc.wantBody != "" && !bytes.Contains(rr.Body.Bytes(), []byte(tc.wantBody)) {
			t.Errorf("%s: body does not contain %q", tc.path, tc.wantBody)
		}
	}

	// A bare base path (no trailing slash) must 301 to the slash form so
	// relative links in index.html resolve under the base path.
	req := httptest.NewRequest("GET", "/magooify", nil)
	rr := httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMovedPermanently {
		t.Errorf("/magooify: status = %d, want %d (redirect)", rr.Code, http.StatusMovedPermanently)
	}
	if loc := rr.Header().Get("Location"); loc != "/magooify/" {
		t.Errorf("/magooify: Location = %q, want %q", loc, "/magooify/")
	}

	// The query string survives the redirect.
	req = httptest.NewRequest("GET", "/magooify?x=1", nil)
	rr = httptest.NewRecorder()
	handler.ServeHTTP(rr, req)
	if rr.Code != http.StatusMovedPermanently {
		t.Errorf("/magooify?x=1: status = %d, want %d (redirect)", rr.Code, http.StatusMovedPermanently)
	}
	if loc := rr.Header().Get("Location"); loc != "/magooify/?x=1" {
		t.Errorf("/magooify?x=1: Location = %q, want %q", loc, "/magooify/?x=1")
	}
}

func TestServeMultipleBasePaths(t *testing.T) {
	registerMimeTypes()
	a := newAPI(true, docsFS, ProxyConfig{})
	handler := buildHandler(a, []string{"/magooify", "/other"})

	for _, prefix := range []string{"/magooify", "/other"} {
		req := httptest.NewRequest("GET", prefix, nil)
		rr := httptest.NewRecorder()
		handler.ServeHTTP(rr, req)
		if rr.Code != http.StatusMovedPermanently {
			t.Errorf("%s: status = %d, want 301", prefix, rr.Code)
		}
		if loc := rr.Header().Get("Location"); loc != prefix+"/" {
			t.Errorf("%s: Location = %q, want %q", prefix, loc, prefix+"/")
		}
	}
}

func TestParseBasePaths(t *testing.T) {
	cases := map[string][]string{
		"":                 nil,
		"/magooify":        {"/magooify"},
		"/magooify,/foo":   {"/magooify", "/foo"},
		" magooify , /x/ ": {"/magooify", "/x"},
		"/,":               nil,
	}
	for in, want := range cases {
		got := parseBasePaths(in)
		if len(got) != len(want) {
			t.Errorf("parseBasePaths(%q) = %v, want %v", in, got, want)
			continue
		}
		for i := range want {
			if got[i] != want[i] {
				t.Errorf("parseBasePaths(%q) = %v, want %v", in, got, want)
				break
			}
		}
	}
}
