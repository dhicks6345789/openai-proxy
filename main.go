package main

import (
	"embed"
	"flag"
	"fmt"
	"io/fs"
	"log"
	"mime"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"runtime"
	"strings"
	"time"
)

//go:embed all:ui
var uiFS embed.FS

//go:embed all:docs
var docsFS embed.FS

// registerMimeTypes pins deterministic Content-Type headers for embedded
// assets so the UI and docs work even on hosts without a system MIME database.
func registerMimeTypes() {
	mime.AddExtensionType(".html", "text/html; charset=utf-8")
	mime.AddExtensionType(".htm", "text/html; charset=utf-8")
	mime.AddExtensionType(".css", "text/css; charset=utf-8")
	mime.AddExtensionType(".js", "text/javascript; charset=utf-8")
	mime.AddExtensionType(".mjs", "text/javascript; charset=utf-8")
	mime.AddExtensionType(".json", "application/json")
	mime.AddExtensionType(".map", "application/json")
	mime.AddExtensionType(".svg", "image/svg+xml")
	mime.AddExtensionType(".png", "image/png")
	mime.AddExtensionType(".jpg", "image/jpeg")
	mime.AddExtensionType(".jpeg", "image/jpeg")
	mime.AddExtensionType(".gif", "image/gif")
	mime.AddExtensionType(".webp", "image/webp")
	mime.AddExtensionType(".ico", "image/x-icon")
	mime.AddExtensionType(".woff", "font/woff")
	mime.AddExtensionType(".woff2", "font/woff2")
	mime.AddExtensionType(".ttf", "font/ttf")
	mime.AddExtensionType(".eot", "application/vnd.ms-fontobject")
	mime.AddExtensionType(".txt", "text/plain; charset=utf-8")
	mime.AddExtensionType(".xml", "text/xml; charset=utf-8")
	mime.AddExtensionType(".wasm", "application/wasm")
	mime.AddExtensionType(".pdf", "application/pdf")
}

// defaultUpstream is the upstream endpoint used when neither the -upstream
// flag nor the OPENAI_BASE_URL environment variable is set. It can be pointed
// at any OpenAI-compatible service, including a local one.
const defaultUpstream = "https://api.openai.com/v1"

func main() {
	registerMimeTypes()

	defaultPort := getEnv("PORT", "8080")
	defaultMode := getEnv("APP_MODE", "desktop")
	defaultHost := getEnv("HOST", "")

	port := flag.String("port", defaultPort, "Port to listen on")
	mode := flag.String("mode", defaultMode, "Operation mode: 'desktop' or 'server'")
	host := flag.String("host", defaultHost, "Host IP to bind to (defaults to 127.0.0.1 for desktop, 0.0.0.0 for server)")
	basePath := flag.String("base-path", getEnv("BASE_PATH", ""), "Comma-separated URL prefixes to serve under when mounted behind a reverse proxy at sub-paths (e.g. /magooify); the app is always also served from the site root")
	noBrowser := flag.Bool("no-browser", false, "Disable automatic browser launch in desktop mode")
	genDocs := flag.String("gen-docs", "", "Write the Swagger UI documentation page to the given path and exit")
	upstream := flag.String("upstream", getEnv("OPENAI_BASE_URL", defaultUpstream), "Base URL of the OpenAI-compatible endpoint to proxy to, including the API version prefix (e.g. https://api.openai.com/v1)")
	apiKey := flag.String("api-key", getEnv("OPENAI_API_KEY", ""), "API key sent to the upstream endpoint as a Bearer token; if empty the client's own Authorization header is forwarded")
	allowedModels := flag.String("allowed-models", getEnv("ALLOWED_MODELS", ""), "Comma-separated list of allowed model names (a trailing * is a prefix wildcard); empty allows all")
	blockedModels := flag.String("blocked-models", getEnv("BLOCKED_MODELS", ""), "Comma-separated list of blocked model names (a trailing * is a prefix wildcard)")
	flag.Parse()

	// Offline documentation generation (used by the build script to produce docs/api.html).
	if *genDocs != "" {
		if err := os.WriteFile(*genDocs, swaggerUIHTML(), 0o644); err != nil {
			log.Fatalf("Failed to write docs to %s: %v", *genDocs, err)
		}
		log.Printf("Wrote API documentation to %s", *genDocs)
		return
	}

	isServerMode := strings.ToLower(*mode) == "server"

	bindHost := *host
	if bindHost == "" {
		if isServerMode {
			bindHost = "0.0.0.0"
		} else {
			bindHost = "127.0.0.1"
		}
	}

	addr := net.JoinHostPort(bindHost, *port)
	basePaths := parseBasePaths(*basePath)

	cfg := ProxyConfig{
		Upstream:      *upstream,
		APIKey:        *apiKey,
		AllowedModels: splitCSVList(*allowedModels),
		BlockedModels: splitCSVList(*blockedModels),
	}

	a := newAPI(isServerMode, docsFS, cfg)

	handler := buildHandler(a, basePaths)

	user := a.getUser(&http.Request{})

	log.Printf("==================================================")
	log.Printf("OpenAI Proxy Starting")
	log.Printf("Mode      : %s", strings.ToUpper(user.Mode))
	log.Printf("User      : %s (%s)", user.Username, user.AuthType)
	log.Printf("Listening : http://%s", addr)
	if a.upstreamURL != nil {
		log.Printf("Upstream  : %s", a.upstreamURL)
		if cfg.APIKey != "" {
			log.Printf("API key   : configured")
		} else {
			log.Printf("API key   : not set (client Authorization headers are forwarded)")
		}
		log.Printf("Proxy     : http://%s/v1/chat/completions", addr)
	} else {
		log.Printf("Upstream  : NOT CONFIGURED (proxy endpoints will return 503; use -upstream)")
	}
	if len(cfg.AllowedModels) > 0 {
		log.Printf("Allowed   : %s", strings.Join(cfg.AllowedModels, ", "))
	}
	if len(cfg.BlockedModels) > 0 {
		log.Printf("Blocked   : %s", strings.Join(cfg.BlockedModels, ", "))
	}
	for _, bp := range basePaths {
		log.Printf("Base path : %s", bp)
		log.Printf("UI        : http://%s%s/", addr, bp)
		log.Printf("OpenAPI   : http://%s%s/docs/api", addr, bp)
	}
	if len(basePaths) == 0 {
		log.Printf("UI        : http://%s/", addr)
		log.Printf("OpenAPI   : http://%s/docs/api", addr)
	}
	log.Printf("==================================================")

	// Auto-launch web browser in desktop mode
	if !isServerMode && !*noBrowser {
		targetURL := fmt.Sprintf("http://127.0.0.1:%s/", *port)
		go func() {
			log.Printf("Launching browser targeting %s...", targetURL)
			if err := openBrowser(targetURL); err != nil {
				log.Printf("Note: Could not open web browser automatically: %v", err)
			}
		}()
	}

	listener, err := net.Listen("tcp", addr)
	if err != nil {
		log.Fatalf("Failed to listen on %s: %v", addr, err)
	}

	if err := http.Serve(listener, handler); err != nil {
		log.Fatalf("Server stopped with error: %v", err)
	}
}

// parseBasePaths splits a comma-separated list of base paths (from the
// --base-path flag or BASE_PATH env var) and normalizes each entry.
func parseBasePaths(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if bp := normalizeBasePath(part); bp != "" {
			out = append(out, bp)
		}
	}
	return out
}

// basePathHandler wraps the application handler so it can also be served from
// one or more reverse-proxy sub-paths (e.g. /magooify) without the proxy
// needing to strip the prefix. Requests arriving with one of those path
// prefixes have the prefix stripped before they reach the application routes.
// A request whose path is exactly a prefix (no trailing slash) is redirected
// to the trailing-slash form so the page's relative links resolve against the
// sub-path rather than the site root. Requests without a known prefix (the
// site root, or a proxy that strips the prefix itself) are passed through
// unchanged, so the app is always also served from its default location.
func basePathHandler(basePaths []string, next http.Handler) http.Handler {
	paths := make([]string, 0, len(basePaths))
	for _, bp := range basePaths {
		if bp = normalizeBasePath(bp); bp != "" {
			paths = append(paths, bp)
		}
	}
	if len(paths) == 0 {
		return next
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		p := r.URL.Path

		for _, basePath := range paths {
			var stripped string

			switch {
			case p == basePath:
				// Redirect to the trailing-slash form so the browser resolves
				// relative URLs (vendor/, style.css, app.js, api/...) against
				// the base path instead of the site root.
				location := basePath + "/"
				if r.URL.RawQuery != "" {
					location += "?" + r.URL.RawQuery
				}
				http.Redirect(w, r, location, http.StatusMovedPermanently)
				return
			case p == basePath+"/":
				stripped = "/"
			case strings.HasPrefix(p, basePath+"/"):
				stripped = strings.TrimPrefix(p, basePath)
			default:
				continue
			}

			r2 := r.Clone(r.Context())
			r2.URL = new(url.URL)
			*r2.URL = *r.URL
			r2.URL.Path = stripped
			r2.URL.RawPath = ""
			next.ServeHTTP(w, r2)
			return
		}

		next.ServeHTTP(w, r)
	})
}

// buildHandler wires up all HTTP routes for the application. The app is always
// served from the site root; the optional basePaths let it additionally be
// served from reverse-proxy sub-paths such as /magooify when the proxy passes
// the full path through.
func buildHandler(a *api, basePaths []string) http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/api/v1/health", a.health)
	mux.HandleFunc("/api/v1/user", a.user)
	mux.HandleFunc("/api/v1/info", a.info)
	mux.HandleFunc("/api/v1/usage", a.usageHandler)
	mux.HandleFunc("/api/v1/logs", a.logsHandler)

	// OpenAI-compatible proxy endpoints under the canonical /v1 prefix.
	mux.HandleFunc("/v1", a.proxyOpenAI)
	mux.HandleFunc("/v1/", a.proxyOpenAI)
	// Bare endpoint paths for clients whose base URL omits the /v1 suffix.
	proxyRoutes := []string{
		"/chat/completions", "/completions", "/responses", "/embeddings",
		"/moderations", "/models", "/models/", "/images/", "/audio/",
		"/files", "/files/", "/batches", "/batches/", "/fine_tuning/",
		"/vector_stores", "/vector_stores/", "/threads", "/threads/",
	}
	for _, p := range proxyRoutes {
		mux.HandleFunc(p, a.proxyOpenAI)
	}

	mux.HandleFunc("/docs/api", a.serveDocs)
	mux.HandleFunc("/docs/", a.serveDocs)

	subFS, err := fs.Sub(uiFS, "ui")
	if err != nil {
		log.Fatalf("Failed to initialize embedded UI filesystem: %v", err)
	}
	fileServer := http.FileServer(http.FS(subFS))

	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api") || strings.HasPrefix(r.URL.Path, "/docs") {
			http.NotFound(w, r)
			return
		}

		path := strings.TrimPrefix(r.URL.Path, "/")
		if path != "" && path != "index.html" {
			f, err := subFS.Open(path)
			if err == nil {
				f.Close()
				fileServer.ServeHTTP(w, r)
				return
			}
		}

		// A stripping proxy forwards a bare prefix such as "/magooify" as an
		// empty path. Redirect to the trailing-slash form so the browser loads
		// the index document from a URL ending in "/". Only this case is
		// redirected: ordinary resource requests like /magooify/app.js are
		// served as-is, never bounced to a trailing slash.
		if r.URL.Path == "" {
			if loc, ok := indexRedirectLocation(r); ok {
				http.Redirect(w, r, loc, http.StatusMovedPermanently)
				return
			}
		}

		// SPA fallback to index.html. The page uses plain relative links, so
		// resources resolve against the document URL automatically, whether the
		// app is served from the site root or a reverse-proxy sub-path.
		serveIndex(w, r, subFS)
	})

	return basePathHandler(basePaths, mux)
}

// indexRedirectLocation returns the Location header for a 301 that adds a
// trailing slash when a stripping proxy (Traefik/Pangolin) forwards a bare
// prefix such as "/magooify" as an empty path. ok is false when the request
// does not need a redirect. The prefix the proxy reports via
// X-Forwarded-Prefix is re-attached so the redirect points at the external
// URL the browser sees; otherwise it falls back to the site root.
func indexRedirectLocation(r *http.Request) (string, bool) {
	if r.URL.Path != "" {
		return "", false
	}

	prefix := safeForwardedPrefix(r.Header.Get("X-Forwarded-Prefix"))
	if prefix == "" {
		return "/", true
	}
	return prefix + "/", true
}

// safeForwardedPrefix validates a proxy-supplied X-Forwarded-Prefix header
// before it is trusted for building a redirect URL. The header is attacker
// controllable on misconfigured proxy chains, so only a safe path-absolute
// value such as "/magooify" is accepted; anything that could turn the redirect
// into an open redirect (protocol-relative, dot segments, query/fragment
// characters) is rejected.
func safeForwardedPrefix(val string) string {
	val = strings.TrimSpace(val)
	if val == "" || !strings.HasPrefix(val, "/") || strings.HasPrefix(val, "//") {
		return ""
	}
	if strings.ContainsAny(val, "\\?#\r\n") || strings.Contains(val, "..") {
		return ""
	}
	return normalizeBasePath(val)
}

// normalizeBasePath cleans a reported reverse-proxy prefix so it can be used
// in a redirect URL. An empty value (or "/") means the site root.
func normalizeBasePath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" || p == "/" {
		return ""
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	return strings.TrimSuffix(p, "/")
}

// serveIndex writes the embedded index.html. The page uses only plain
// relative links, so no <base> tag is needed: resources resolve against the
// document URL, which keeps them working under any reverse-proxy sub-path as
// long as the request URL carries a trailing slash.
func serveIndex(w http.ResponseWriter, r *http.Request, subFS fs.FS) {
	data, err := fs.ReadFile(subFS, "index.html")
	if err != nil {
		http.NotFound(w, r)
		return
	}

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	// Never cache the page so a stale copy can't linger after redeploys.
	w.Header().Set("Cache-Control", "no-store")
	w.Write(data)
}

func getEnv(key, defaultValue string) string {
	if val := os.Getenv(key); val != "" {
		return val
	}
	return defaultValue
}

// splitCSVList splits a comma-separated list of values, trimming whitespace
// and discarding empty entries.
func splitCSVList(raw string) []string {
	var out []string
	for _, part := range strings.Split(raw, ",") {
		if v := strings.TrimSpace(part); v != "" {
			out = append(out, v)
		}
	}
	return out
}

// openBrowser launches the system's default web browser at the given URL.
func openBrowser(url string) error {
	// Give the server a moment to start listening.
	time.Sleep(100 * time.Millisecond)

	var cmd *exec.Cmd

	switch runtime.GOOS {
	case "linux":
		cmd = exec.Command("xdg-open", url)
	case "windows":
		cmd = exec.Command("cmd", "/c", "start", url)
	case "darwin":
		cmd = exec.Command("open", url)
	default:
		return fmt.Errorf("unsupported platform: %s", runtime.GOOS)
	}

	return cmd.Start()
}
