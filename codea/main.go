package main

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/json"
	"encoding/pem"
	"flag"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
)

//go:embed login.html logout.html logout.vsc.js
var staticFS embed.FS

// mustAsset reads an embedded asset by name, failing fast at startup if missing.
func mustAsset(name string) []byte {
	b, err := staticFS.ReadFile(name)
	if err != nil {
		log.Fatalf("embed asset %q: %v", name, err)
	}
	return b
}

// Config holds proxy configuration.
type Config struct {
	Backends     []Backend
	ServiceCmd   string // optional shell command to run as the backend
	ProxyPort    string
	TokenCookie  string            // cookie name, default "vscode-tkn"
	ProxyUseSSL  bool              // enable HTTPS with a self-signed cert
	ProxyHeaders map[string]string // PROXY_HEADER_Xxx=Val → set/override; PROXY_HEADER_Xxx= → delete
}

// Backend describes a single proxy target with its routing prefix.
type Backend struct {
	Prefix string // routing prefix, "/" for root
	Scheme string // http, https, unix, file, text
	Target string // host:port, socket path, dir path, or literal text
	RawURL string // original URL for logging
}

func loadInitConfig() Config {
	backendURL := os.Getenv("BACKEND_URL")
	serviceCmd := os.Getenv("SERVICE_CMD")
	proxyPort := os.Getenv("VSC_PORT")
	if proxyPort == "" {
		proxyPort = "7080"
	}
	cookie := os.Getenv("TOKEN_COOKIE")
	if cookie == "" {
		cookie = "vscode-tkn"
	}
	useSSL := os.Getenv("PROXY_USE_SSL")
	useSSLFlag := useSSL == "1" || strings.ToLower(useSSL) == "true"

	flag.StringVar(&backendURL, "backend", backendURL, "Backend service URL(s)")
	flag.StringVar(&serviceCmd, "service", serviceCmd, "Backend service Cmd")
	flag.StringVar(&proxyPort, "port", proxyPort, "Proxy listen port")
	flag.StringVar(&cookie, "cookie", cookie, "Token cookie name")
	flag.BoolVar(&useSSLFlag, "ssl", useSSLFlag, "Enable HTTPS with self-signed cert")
	flag.Parse()

	if backendURL == "" {
		log.Fatal("BACKEND_URL is required (set via env or -backend flag)")
	}

	backends := parseBackends(backendURL)
	if len(backends) == 0 {
		log.Fatal("BACKEND_URL parsed to zero backends")
	}

	return Config{
		Backends:     backends,
		ServiceCmd:   serviceCmd,
		ProxyPort:    proxyPort,
		TokenCookie:  cookie,
		ProxyUseSSL:  useSSLFlag,
		ProxyHeaders: parseProxyHeaders(),
	}
}

// parseBackends parses BACKEND_URL into a list of Backend entries.
//
// Single backend:
//
//	http://host:port, unix:///path/sock, file:///path/to/dir, text://content
//
// Multi backend (order-preserving, "/prefix/=scheme://..." separated by ";"):
//
//	/api/=http://api:8080;/cdn/=file:///var/www;/x/=text://hello
func parseBackends(raw string) []Backend {
	// Multi-backend detection: a multi-backend string has the form
	// "/prefix=scheme://..." (the "=" precedes the first "://"). This holds
	// even for a single segment like "/=unix:///path", which has no ";".
	if isMultiBackend(raw) {
		segs := splitBackendSegments(raw)
		if backends := parseMultiBackends(segs); len(backends) > 0 {
			return backends
		}
	}

	// Single backend: root prefix "/".
	if b := newBackend("/", raw); b != nil {
		return []Backend{*b}
	}
	return nil
}

// isMultiBackend reports whether raw uses the "/prefix=scheme://..." form.
// It returns true when an "=" appears before the first "://" (i.e. there is a
// routing prefix in front of the backend URL). This correctly classifies both
// "/=unix:///path" (single multi-backend segment) and
// "/a/=http://x;/b/=https://y" (multiple segments), while rejecting plain
// single-backend URLs like "http://h?a=b" where "=" is only in the query.
func isMultiBackend(raw string) bool {
	eqIdx := strings.Index(raw, "=")
	if eqIdx < 0 {
		return false
	}
	schemeIdx := strings.Index(raw, "://")
	// "=" must come before the scheme separator (and a scheme must exist).
	return schemeIdx > eqIdx
}

// parseMultiBackends parses already-split segments into backends.
func parseMultiBackends(segments []string) []Backend {
	var backends []Backend
	for _, seg := range segments {
		prefix, urlStr, ok := strings.Cut(seg, "=")
		if !ok {
			log.Printf("WARNING: backend segment missing '=': %q, skipping", seg)
			continue
		}
		prefix = strings.TrimSpace(prefix)
		urlStr = strings.TrimSpace(urlStr)
		if prefix == "" || urlStr == "" {
			log.Printf("WARNING: empty prefix or url in segment: %q", seg)
			continue
		}
		if !strings.HasPrefix(prefix, "/") {
			prefix = "/" + prefix
		}
		if b := newBackend(prefix, urlStr); b != nil {
			backends = append(backends, *b)
		}
	}
	return backends
}

// splitBackendSegments splits a multi-backend string on ";" outside of the URL portion.
// Format: /a/=http://x;/b/=https://y
func splitBackendSegments(raw string) []string {
	// ";/" is the natural delimiter: semicolon followed by a new prefix starting with "/"
	parts := strings.Split(raw, ";/")
	result := make([]string, 0, len(parts))
	for i, p := range parts {
		if i == 0 {
			result = append(result, p)
		} else {
			result = append(result, "/"+p)
		}
	}
	return result
}

// newBackend creates a Backend by parsing scheme:// from rawURL.
func newBackend(prefix, rawURL string) *Backend {
	scheme, target, ok := strings.Cut(rawURL, "://")
	if !ok {
		log.Printf("WARNING: backend %q has no scheme, skipping", rawURL)
		return nil
	}
	scheme = strings.ToLower(scheme)
	log.Printf("backend %q → prefix=%q scheme=%q target=%q", rawURL, prefix, scheme, target)
	return &Backend{
		Prefix: prefix,
		Scheme: scheme,
		Target: target,
		RawURL: rawURL,
	}
}

// parseProxyHeaders reads PROXY_HEADER_* env vars.
// PROXY_HEADER_Xxx=Val → set/override header Xxx to Val.
// PROXY_HEADER_Xxx=     → delete header Xxx.
func parseProxyHeaders() map[string]string {
	hdrs := make(map[string]string)
	const prefix = "PROXY_HEADER_"
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		name := k[len(prefix):]
		if name == "" {
			continue
		}
		hdrs[name] = v
	}
	return hdrs
}

func main() {
	cfg := loadInitConfig()
	loadCorsConfig() // parse VSC_CORS_* env vars (no-op when none set)

	// Start backend subprocess if SERVICE_CMD is configured.
	var serviceCmd *exec.Cmd
	if cfg.ServiceCmd != "" {
		serviceCmd = startBackend(cfg.ServiceCmd)
	}

	// Build handlers for each backend.
	type backendHandler struct {
		prefix  string
		handler http.Handler
	}
	var handlers []backendHandler
	for _, b := range cfg.Backends {
		h := createBackendHandler(b, cfg.ProxyHeaders)
		handlers = append(handlers, backendHandler{prefix: b.Prefix, handler: h})
	}

	mux := http.NewServeMux()

	// /__login – serves login page (GET) or processes form (POST).
	mux.HandleFunc("/__login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			if err := r.ParseForm(); err != nil {
				http.Error(w, "invalid form", http.StatusBadRequest)
				return
			}
			tkn := strings.TrimSpace(r.PostFormValue("token"))
			if tkn == "" {
				serveStaticAsset(w, "login.html")
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cfg.TokenCookie,
				Value:    tkn,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			back := safeReferer(r.Referer())
			log.Printf("login ok, cookie %s=%s, reloading: %s", cfg.TokenCookie, tkn, back)
			http.Redirect(w, r, back, http.StatusSeeOther)
		default:
			serveStaticAsset(w, "login.html")
		}
	})

	// /__logout – clears the token cookie.
	mux.HandleFunc("/__logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     cfg.TokenCookie,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
		})
		log.Printf("logout: cleared cookie %s", cfg.TokenCookie)
		serveStaticAsset(w, "logout.html")
	})

	// /__vscode/ – VS Code update API proxy with local cache.
	mux.Handle("/__vscode/", newVscodeUpdateHandler())

	// /__proxy/ – generic external proxy: /__proxy/{scheme}:{host}/path → {scheme}://{host}/path
	mux.HandleFunc("/__proxy/", handleExternalProxy)

	// /__logout.vsc.js – VS Code logout-button script injected into proxied HTML.
	// Named .vsc because it targets the VS Code activity-bar toolbar; other apps
	// can get their own script (e.g. /__logout.xxx.js) later.
	mux.HandleFunc("/__logout.vsc.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		_, _ = w.Write(mustAsset("logout.vsc.js"))
	})

	// All other requests: dispatch to backends in config order.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, bh := range handlers {
			if strings.HasPrefix(r.URL.Path, bh.prefix) {
				bh.handler.ServeHTTP(w, r)
				return
			}
		}
		http.Error(w, "no backend matched", http.StatusBadGateway)
	})

	servers := buildServers(cfg.ProxyPort, cfg.ProxyUseSSL, mux)

	// Graceful shutdown on SIGINT/SIGTERM: stop accepting new connections,
	// wait for active ones, then signal the backend subprocess to exit.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	for _, srv := range servers {
		go func(s *http.Server, isTLS bool) {
			var e error
			if isTLS {
				e = s.ListenAndServeTLS("", "")
			} else {
				e = s.ListenAndServe()
			}
			if e != nil && e != http.ErrServerClosed {
				log.Fatalf("server on %s: %v", s.Addr, e)
			}
		}(srv.server, srv.isTLS)
	}

	log.Printf("proxy starting: %s", strings.Join(serverAddrs(servers), ", "))
	log.Printf("token cookie: %s", cfg.TokenCookie)
	log.Printf("backends: %d", len(cfg.Backends))
	if len(cfg.ProxyHeaders) > 0 {
		log.Printf("proxy headers: %v", cfg.ProxyHeaders)
	}

	<-sigCh
	log.Printf("shutdown signal received, draining…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.server.Shutdown(shutdownCtx)
	}
	if serviceCmd != nil {
		killProcessGroup(serviceCmd)
		for _, b := range cfg.Backends {
			if b.Scheme == "unix" {
				_ = os.Remove(b.Target)
			}
		}
	}
}

// serverInstance binds an *http.Server with its TLS flag.
type serverInstance struct {
	server *http.Server
	isTLS  bool
}

// buildServers constructs one (plain HTTP) or two (HTTP + HTTPS) servers with
// sensible timeouts and the self-signed cert when SSL is enabled.
func buildServers(port string, useSSL bool, mux http.Handler) []serverInstance {
	base := &http.Server{
		Addr:              ":" + port,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // streaming downloads may take long
		IdleTimeout:       120 * time.Second,
	}
	if !useSSL {
		return []serverInstance{{server: base, isTLS: false}}
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("invalid VSC_PORT %q: %v", port, err)
	}
	cert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatalf("generate self-signed cert: %v", err)
	}
	httpsSrv := &http.Server{
		Addr:              fmt.Sprintf(":%d", portNum+1),
		Handler:           mux,
		TLSConfig:         &tls.Config{Certificates: []tls.Certificate{cert}, MinVersion: tls.VersionTLS12},
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		IdleTimeout:       120 * time.Second,
	}
	return []serverInstance{
		{server: base, isTLS: false},
		{server: httpsSrv, isTLS: true},
	}
}

func serverAddrs(servers []serverInstance) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		scheme := "http"
		if s.isTLS {
			scheme = "https"
		}
		out = append(out, scheme+"://"+s.server.Addr)
	}
	return out
}

// safeReferer returns a same-origin redirect target, falling back to "/".
func safeReferer(ref string) string {
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host != "" {
		// Only allow relative refs to avoid open redirect.
		return "/"
	}
	return ref
}

// applyProxyHeaders rewrites request headers according to PROXY_HEADER_* env vars.
// PROXY_HEADER_Xxx=Val → set/override header Xxx; PROXY_HEADER_Xxx= → delete header Xxx.
func applyProxyHeaders(req *http.Request, hdrs map[string]string) {
	if len(hdrs) == 0 {
		return
	}
	for name, val := range hdrs {
		if val == "" {
			req.Header.Del(name)
		} else {
			req.Header.Set(name, val)
		}
	}
}

// =============================================================================
// External Proxy — handles /__proxy/ endpoints
// =============================================================================

// allowedExtProxySchemes restricts the external proxy to web schemes to mitigate SSRF.
var allowedExtProxySchemes = map[string]bool{"http": true, "https": true}

// extProxyTransport is a shared transport for the external proxy with a response
// header timeout so a slow/hung upstream cannot hold connections indefinitely.
var extProxyTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       90 * time.Second,
}

// =============================================================================
// External Proxy Cache
// =============================================================================

// proxyCacheRoot is the filesystem root for external proxy caches.
var proxyCacheRoot string

func initProxyCache() {
	dir := os.Getenv("VSC_CACHE")
	if dir == "" {
		dir = "/app/.vscode"
	}
	proxyCacheRoot = filepath.Join(dir, "proxy")
	log.Printf("[ext-proxy] cache root: %s", proxyCacheRoot)
}

// proxyCacheClient is used for manual upstream fetches when caching.
var proxyCacheClient = &http.Client{
	Transport: extProxyTransport,
	Timeout:   5 * time.Minute,
	CheckRedirect: func(req *http.Request, via []*http.Request) error {
		if len(via) >= 10 {
			return fmt.Errorf("too many redirects")
		}
		return nil
	},
}

// proxyCacheHeaderWhitelist lists headers preserved in cache metadata.
var proxyCacheHeaderWhitelist = map[string]bool{
	"content-type":        true,
	"content-length":      true,
	"content-encoding":    true,
	"cache-control":       true,
	"etag":                true,
	"last-modified":       true,
	"content-disposition": true,
}

// proxyCacheMeta holds the cached HTTP status and a subset of response headers.
type proxyCacheMeta struct {
	Status  int
	Headers map[string][]string
}

// proxyCachePaths returns the body and meta file paths for a cache entry.
//
// Cache layout mirrors the URL structure:
//
//	{VSC_CACHE}/proxy/{scheme}:{host}/path/to/file.js       → body
//	{VSC_CACHE}/proxy/{scheme}:{host}/path/to/file.js.meta  → metadata
//
// The rest path is cleaned and stripped of leading "/" to stay within
// the cache root.  Requests for the root path use "__index" as filename.
func proxyCachePaths(scheme, host, rest string) (bodyPath, metaPath string) {
	// Clean and make relative to prevent directory traversal.
	p := filepath.Clean(rest)
	p = strings.TrimPrefix(p, "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(proxyCacheRoot, scheme+":"+host, p)
	return base, base + ".meta"
}

// readProxyMeta reads and parses a cache metadata file.
func readProxyMeta(path string) (*proxyCacheMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m proxyCacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeProxyMeta atomically writes cache metadata as JSON.
func writeProxyMeta(path string, m *proxyCacheMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o644)
}

// =============================================================================
// External Proxy Handler
// =============================================================================

// handleExternalProxy proxies /__proxy/[...] → target URL.
//
// Format:  /__proxy/[cc+]{scheme}:{host}[/path][?query]
//
//	cc+              — optional cache marker: check cache, write on MISS
//	{scheme}:        — optional scheme (http, https); defaults to https
//	{host}           — upstream host[:port]
//
// Examples:
//
//	/__proxy/cc+https:cdn.example.com/lib.js → cache+proxy https://cdn.example.com/lib.js
//	/__proxy/cc+cdn.example.com/lib.js       → cache+proxy (https default)
//	/__proxy/https:cdn.example.com/lib.js    → proxy only, check cache first
//	/__proxy/cdn.example.com/lib.js           → proxy only (https default)
func handleExternalProxy(w http.ResponseWriter, r *http.Request) {
	if proxyCacheRoot == "" {
		initProxyCache()
	}

	p := strings.TrimPrefix(r.URL.Path, "/__proxy/")
	if p == "" || p == "/" {
		http.Error(w, "missing domain/path", http.StatusBadRequest)
		return
	}

	// 1. Detect cc+ cache prefix.
	cacheable := strings.HasPrefix(p, "cc+")
	if cacheable {
		p = p[3:]
	}
	// Only GET requests qualify for cache write.
	if r.Method != "GET" {
		cacheable = false
	}

	// 2. Parse scheme:host from the first segment.
	//    Format: scheme:host/rest or host/rest
	var scheme, host, rest string
	slashIdx := strings.Index(p, "/")
	if slashIdx >= 0 {
		rest = p[slashIdx:]
		p = p[:slashIdx]
	} else {
		rest = "/"
	}

	if idx := strings.Index(p, ":"); idx >= 0 {
		scheme = strings.ToLower(p[:idx])
		host = p[idx+1:]
	} else {
		scheme = "https"
		host = p
	}

	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}
	if !allowedExtProxySchemes[scheme] {
		http.Error(w, "unsupported scheme", http.StatusBadRequest)
		return
	}

	// 3. Build full target URL for cache key and upstream fetch.
	targetURL := fmt.Sprintf("%s://%s%s", scheme, host, rest)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	log.Printf("[ext-proxy] %s %s → %s (cacheable=%v)", r.Method, r.URL.Path, targetURL, cacheable)

	// 4. Try cache lookup (both cacheable and non-cacheable paths).
	bodyPath, metaPath := proxyCachePaths(scheme, host, rest)

	if meta, err := readProxyMeta(metaPath); err == nil {
		body, err := os.ReadFile(bodyPath)
		if err == nil {
			log.Printf("[ext-proxy] cache HIT  %s ← %s (%d bytes)", targetURL, bodyPath, len(body))
			w.Header().Set("X-Cache", "HIT")
			for k, vs := range meta.Headers {
				for _, v := range vs {
					w.Header().Add(k, v)
				}
			}
			w.WriteHeader(meta.Status)
			_, _ = w.Write(body)
			return
		}
	}

	// 5. Cache MISS — branch by cacheable.
	if cacheable {
		handleCachedProxy(w, r, targetURL, scheme, host, bodyPath, metaPath)
	} else {
		handlePassThroughProxy(w, r, targetURL)
	}
}

// handleCachedProxy fetches the upstream, streams the response to both the
// client and a cache file, and writes cache metadata on success.
func handleCachedProxy(w http.ResponseWriter, r *http.Request, targetURL, scheme, host, bodyPath, metaPath string) {
	log.Printf("[ext-proxy] cache MISS (will cache) %s", targetURL)

	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "bad target URL", http.StatusBadRequest)
		return
	}
	// Forward a safe subset of request headers.
	for k, vs := range r.Header {
		switch strings.ToLower(k) {
		case "accept", "accept-encoding", "accept-language", "user-agent":
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}

	resp, err := proxyCacheClient.Do(req)
	if err != nil {
		log.Printf("[ext-proxy] fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Only cache successful (2xx) responses.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[ext-proxy] upstream returned %d, not caching", resp.StatusCode)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Collect whitelisted headers for cache metadata.
	meta := &proxyCacheMeta{
		Status:  resp.StatusCode,
		Headers: make(map[string][]string),
	}
	for k, vs := range resp.Header {
		if proxyCacheHeaderWhitelist[strings.ToLower(k)] {
			meta.Headers[k] = vs
		}
	}

	// Set response headers before writing body.
	for k, vs := range meta.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)

	// Ensure cache directory exists.
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		log.Printf("[ext-proxy] mkdir cache FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Stream to client + temp file simultaneously.
	tmp, err := os.CreateTemp(filepath.Dir(bodyPath), ".tmp-*")
	if err != nil {
		log.Printf("[ext-proxy] create temp FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmpName := tmp.Name()

	n, copyErr := io.Copy(io.MultiWriter(w, tmp), resp.Body)
	_ = tmp.Close()

	if copyErr != nil {
		log.Printf("[ext-proxy] copy error: %v", copyErr)
		_ = os.Remove(tmpName)
		return
	}

	// Atomic rename temp → final body.
	if err := os.Rename(tmpName, bodyPath); err != nil {
		log.Printf("[ext-proxy] rename cache FAIL: %s → %v", bodyPath, err)
		_ = os.Remove(tmpName)
		return
	}

	// Write metadata.
	if err := writeProxyMeta(metaPath, meta); err != nil {
		log.Printf("[ext-proxy] write meta FAIL: %s → %v", metaPath, err)
	} else {
		log.Printf("[ext-proxy] CACHED %s → %s (%d bytes)", targetURL, bodyPath, n)
	}
}

// handlePassThroughProxy proxies the request directly to the upstream without
// caching. It checks the cache first (done by the caller), and only reaches
// here on a cache MISS for non-cacheable paths.
func handlePassThroughProxy(w http.ResponseWriter, r *http.Request, targetURL string) {
	log.Printf("[ext-proxy] passthrough %s", targetURL)

	target, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "invalid target URL", http.StatusBadRequest)
		return
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Transport = extProxyTransport
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
		req.URL.RawQuery = target.RawQuery
		req.Host = target.Host
		req.Header.Del("X-Forwarded-For")
		req.Header.Set("X-Forwarded-For", req.RemoteAddr)
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[ext-proxy] error proxying %s: %v", targetURL, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
	}
	rp.ModifyResponse = func(resp *http.Response) error {
		resp.Header.Set("X-Cache", "MISS")
		return nil
	}

	rp.ServeHTTP(w, r)
}

// =============================================================================
// VS Code Update Proxy — handles /__vscode/ endpoints
// =============================================================================

// vscodeUpdateHandler caches and proxies VS Code update API requests.
// Two endpoints:
//
//	GET /__vscode/api/latest/{platform}/{quality}      → latest version JSON
//	GET /__vscode/commit:{commit}/{platform}/{quality}  → download archive
type vscodeUpdateHandler struct {
	cacheDir string
	upstream string
	client   *http.Client
	// redirectClient follows redirects and records the final URL; reused across requests.
	redirectClient *http.Client
}

func newVscodeUpdateHandler() *vscodeUpdateHandler {
	dir := os.Getenv("VSC_CACHE")
	if dir == "" {
		dir = "/app/.vscode"
	}
	log.Printf("[vscode-update] cache dir: %s", dir)
	return &vscodeUpdateHandler{
		cacheDir: dir,
		upstream: "https://update.code.visualstudio.com",
		client:   &http.Client{Timeout: 10 * time.Minute},
		redirectClient: &http.Client{
			Timeout: 10 * time.Minute,
			CheckRedirect: func(req *http.Request, via []*http.Request) error {
				if len(via) >= 10 {
					return fmt.Errorf("too many redirects")
				}
				return nil
			},
		},
	}
}

func (h *vscodeUpdateHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	// Go ServeMux 不会自动剥离匹配前缀，r.URL.Path 仍是完整路径。
	// 移除 /__vscode/ 前缀得到相对路径。
	p := strings.TrimPrefix(r.URL.Path, "/__vscode/")

	if strings.HasPrefix(p, "api/latest/") {
		rest := strings.TrimPrefix(p, "api/latest/")
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "bad route: api/latest/{platform}/{quality}", http.StatusBadRequest)
			return
		}
		h.handleLatest(w, r, parts[0], parts[1])
		return
	}

	if strings.HasPrefix(p, "commit:") {
		idx := strings.Index(p, "/")
		if idx < 0 {
			http.Error(w, "bad route: commit:{commit}/{platform}/{quality}", http.StatusBadRequest)
			return
		}
		commit := p[len("commit:"):idx]
		rest := p[idx+1:]
		parts := strings.SplitN(rest, "/", 2)
		if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
			http.Error(w, "bad route: commit:{commit}/{platform}/{quality}", http.StatusBadRequest)
			return
		}
		h.handleCommit(w, r, commit, parts[0], parts[1])
		return
	}

	if strings.HasPrefix(p, "download/") {
		// download/{quality}/{commit}/vscode-{platform}.{ext}
		rest := strings.TrimPrefix(p, "download/")
		parts := strings.SplitN(rest, "/", 3)
		if len(parts) != 3 || parts[0] == "" || parts[1] == "" || parts[2] == "" {
			http.Error(w, "bad route: download/{quality}/{commit}/vscode-{platform}.{ext}", http.StatusBadRequest)
			return
		}
		quality, commit, filename := parts[0], parts[1], parts[2]
		platform, ext := parseDownloadFilename(filename)
		if platform == "" {
			http.Error(w, "bad filename: want vscode-{platform}.{ext}", http.StatusBadRequest)
			return
		}
		h.handleDownload(w, r, commit, quality, platform, ext)
		return
	}

	http.NotFound(w, r)
}

func (h *vscodeUpdateHandler) handleLatest(w http.ResponseWriter, r *http.Request, platform, quality string) {
	cachePath := filepath.Join(h.cacheDir, platform, quality, "latest.json")

	if data, err := os.ReadFile(cachePath); err == nil {
		log.Printf("[vscode-update] latest HIT  %s/%s ← %s (%d bytes)", platform, quality, cachePath, len(data))
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("X-Cache", "HIT")
		_, _ = w.Write(data)
		return
	}

	log.Printf("[vscode-update] latest MISS %s/%s (not found: %s)", platform, quality, cachePath)
	upstreamURL := fmt.Sprintf("%s/api/latest/%s/%s", h.upstream, platform, quality)
	log.Printf("[vscode-update] latest fetch: %s", upstreamURL)

	resp, err := h.client.Get(upstreamURL)
	if err != nil {
		log.Printf("[vscode-update] latest fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[vscode-update] latest upstream returned %d", resp.StatusCode)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		log.Printf("[vscode-update] latest read error: %v", err)
		http.Error(w, "read upstream failed", http.StatusBadGateway)
		return
	}

	if !json.Valid(body) {
		log.Printf("[vscode-update] latest invalid JSON from upstream")
		http.Error(w, "invalid upstream response", http.StatusBadGateway)
		return
	}

	_ = os.MkdirAll(filepath.Dir(cachePath), 0o755)
	if err := atomicWriteFile(cachePath, body, 0o644); err != nil {
		log.Printf("[vscode-update] latest write cache FAIL: %s → %v", cachePath, err)
	} else {
		log.Printf("[vscode-update] latest CACHED %s/%s → %s (%d bytes)", platform, quality, cachePath, len(body))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	_, _ = w.Write(body)
}

func (h *vscodeUpdateHandler) handleCommit(w http.ResponseWriter, r *http.Request, commit, platform, quality string) {
	cacheDir := filepath.Join(h.cacheDir, platform, quality)

	// 1. 查找缓存（扩展名未知，按 commit 前缀匹配）
	if cachedPath, ok := findCachedFile(cacheDir, commit, ""); ok {
		fi, _ := os.Stat(cachedPath)
		log.Printf("[vscode-update] commit HIT  %s/%s/%s ← %s (%d bytes)", platform, quality, commit, cachedPath, fileSize(fi))
		ext := extractExtFromPath(cachedPath)
		redirectDownload(w, r, quality, commit, platform, ext)
		return
	}

	log.Printf("[vscode-update] commit MISS %s/%s/%s (not found in: %s)", platform, quality, commit, cacheDir)

	// 2. 从上游获取（跟随所有重定向，记录最终 URL）
	upstreamURL := fmt.Sprintf("%s/commit:%s/%s/%s", h.upstream, commit, platform, quality)
	log.Printf("[vscode-update] commit fetch: %s", upstreamURL)

	resp, err := h.redirectClient.Get(upstreamURL)
	if err != nil {
		log.Printf("[vscode-update] commit fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		log.Printf("[vscode-update] commit upstream returned %d", resp.StatusCode)
		http.Error(w, "upstream error", http.StatusBadGateway)
		return
	}

	finalURL := resp.Request.URL.String()
	log.Printf("[vscode-update] commit redirect → %s", finalURL)

	// 3. 确定扩展名：优先从最终 URL 提取，其次 Content-Type
	ext := extractExtFromURL(finalURL)
	if ext == "" {
		ext = inferExtFromContentType(resp.Header.Get("Content-Type"))
	}
	if ext == "" {
		ext = ".tar.gz"
	}
	cachePath := filepath.Join(cacheDir, commit+ext)

	// 4. 流式落盘，避免将大文件（数百 MB）读入内存。
	if err := os.MkdirAll(cacheDir, 0o755); err != nil {
		log.Printf("[vscode-update] commit mkdir FAIL: %v", err)
		http.Error(w, "cache write failed", http.StatusBadGateway)
		return
	}
	n, err := streamToAtomicFile(cachePath, resp.Body)
	if err != nil {
		log.Printf("[vscode-update] commit write cache FAIL: %s → %v", cachePath, err)
		http.Error(w, "cache write failed", http.StatusBadGateway)
		return
	}
	log.Printf("[vscode-update] commit CACHED %s/%s/%s → %s (%d bytes)", platform, quality, commit, cachePath, n)

	// 5. 缓存完成后重定向到 download 接口，由它用正确文件名提供下载
	redirectDownload(w, r, quality, commit, platform, ext)
}

// handleDownload serves a cached commit file with the correct vscode-{platform}.{ext} filename.
func (h *vscodeUpdateHandler) handleDownload(w http.ResponseWriter, r *http.Request, commit, quality, platform, ext string) {
	cacheDir := filepath.Join(h.cacheDir, platform, quality)

	// Prefer the exact expected path so a stale file with a different ext
	// cannot be served under the wrong filename.
	cachedPath, ok := findCachedFile(cacheDir, commit, ext)
	if !ok {
		log.Printf("[vscode-update] download MISS: %s/%s/%s not found in %s", platform, quality, commit, cacheDir)
		http.Error(w, "file not found in cache", http.StatusNotFound)
		return
	}

	fi, _ := os.Stat(cachedPath)
	log.Printf("[vscode-update] download SERVE %s/%s/%s ← %s (%d bytes)", platform, quality, commit, cachedPath, fileSize(fi))

	downloadName := fmt.Sprintf("vscode-%s%s", platform, ext)
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, downloadName))
	// Let http.ServeFile set Content-Type (application/octet-stream) and handle ranges.
	http.ServeFile(w, r, cachedPath)
}

// redirectDownload sends a 302 to the download endpoint with the correct filename.
func redirectDownload(w http.ResponseWriter, r *http.Request, quality, commit, platform, ext string) {
	target := fmt.Sprintf("/__vscode/download/%s/%s/vscode-%s%s", quality, commit, platform, ext)
	log.Printf("[vscode-update] redirect → %s", target)
	http.Redirect(w, r, target, http.StatusFound)
}

// parseDownloadFilename extracts platform and extension from "vscode-{platform}.{ext}".
func parseDownloadFilename(name string) (platform, ext string) {
	// name like: vscode-server-linux-x64-web.tar.gz
	if !strings.HasPrefix(name, "vscode-") {
		return "", ""
	}
	rest := name[len("vscode-"):]
	dotIdx := strings.Index(rest, ".")
	if dotIdx < 0 {
		return "", ""
	}
	return rest[:dotIdx], rest[dotIdx:]
}

// findCachedFile returns a cached file for the given commit.
// If extHint is non-empty it first tries the exact "{commit}{extHint}" path;
// otherwise (or on miss) it scans the directory for any "{commit}.*" match.
// This avoids false matches when one commit is a prefix of another.
func findCachedFile(dir, commit, extHint string) (string, bool) {
	if extHint != "" {
		p := filepath.Join(dir, commit+extHint)
		if fi, err := os.Stat(p); err == nil && !fi.IsDir() {
			return p, true
		}
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if strings.HasPrefix(name, commit+".") || name == commit {
			return filepath.Join(dir, name), true
		}
	}
	return "", false
}

func extractExtFromURL(rawURL string) string {
	// Strip query and fragment before taking the basename.
	for _, sep := range []string{"?", "#"} {
		if idx := strings.Index(rawURL, sep); idx >= 0 {
			rawURL = rawURL[:idx]
		}
	}
	return extractExt(path.Base(rawURL))
}

func extractExtFromPath(filePath string) string {
	return extractExt(filepath.Base(filePath))
}

// extractExt returns the extension portion (including the leading ".") of a
// basename, e.g. "x.tar.gz" → ".tar.gz". Returns "" if there is no ".".
func extractExt(base string) string {
	dotIdx := strings.Index(base, ".")
	if dotIdx < 0 {
		return ""
	}
	return base[dotIdx:]
}

func inferExtFromContentType(ct string) string {
	ct = strings.TrimSpace(strings.SplitN(ct, ";", 2)[0])
	switch ct {
	case "application/gzip", "application/x-gzip":
		return ".tar.gz"
	case "application/zip":
		return ".zip"
	case "application/x-xz":
		return ".tar.xz"
	case "application/x-bzip2":
		return ".tar.bz2"
	case "application/x-compressed-tar", "application/x-tar":
		return ".tar"
	}
	return ""
}

// fileSize safely reports a file's size for logging.
func fileSize(fi os.FileInfo) int64 {
	if fi == nil {
		return -1
	}
	return fi.Size()
}

// atomicWriteFile writes data to path via a temp file + rename for crash safety.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return err
	}
	return os.Rename(tmpName, path)
}

// streamToAtomicFile streams r into path via a temp file + rename, returning bytes written.
func streamToAtomicFile(path string, r io.Reader) (int64, error) {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return 0, err
	}
	tmpName := tmp.Name()

	n, copyErr := io.Copy(tmp, r)
	closeErr := tmp.Close()

	// Prefer the copy error; if copy succeeded, surface the close error.
	if copyErr != nil {
		_ = os.Remove(tmpName)
		return n, copyErr
	}
	if closeErr != nil {
		_ = os.Remove(tmpName)
		return n, closeErr
	}

	if err := os.Chmod(tmpName, 0o644); err != nil {
		_ = os.Remove(tmpName)
		return n, err
	}
	return n, os.Rename(tmpName, path)
}

// =============================================================================

// =============================================================================
// Backend Handlers — createBackendHandler dispatches to the right handler type.
// =============================================================================

// createBackendHandler builds an http.Handler for the given backend.
// Supported schemes: http, https, unix (reverse proxy), file (directory), text (literal).
func createBackendHandler(b Backend, proxyHeaders map[string]string) http.Handler {
	switch b.Scheme {
	case "http", "https":
		targetURL, err := url.Parse(b.RawURL)
		if err != nil {
			log.Fatalf("invalid backend URL %q: %v", b.RawURL, err)
		}
		rp := httputil.NewSingleHostReverseProxy(targetURL)
		origDirector := rp.Director
		rp.Director = func(req *http.Request) {
			origDirector(req)
			applyProxyHeaders(req, proxyHeaders)
		}
		rp.ModifyResponse = chainModifiers(authRedirectModifier(), corsModifier(), injectLogoutButton)
		return rp

	case "unix":
		socketPath := b.Target
		log.Printf("backend unix socket: %s", socketPath)
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "unix"
				applyProxyHeaders(req, proxyHeaders)
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		}
		rp.ModifyResponse = chainModifiers(authRedirectModifier(), corsModifier(), injectLogoutButton)
		return rp

	case "file":
		dir := b.Target
		log.Printf("backend file server: %s", dir)
		return http.StripPrefix(b.Prefix, http.FileServer(http.Dir(dir)))

	case "text":
		content := b.Target
		log.Printf("backend text: %d bytes", len(content))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			_, _ = w.Write([]byte(content))
		})

	default:
		log.Fatalf("unknown backend scheme %q in %q", b.Scheme, b.RawURL)
		return nil
	}
}

// authRedirectModifier returns a ModifyResponse that replaces 401/403 from the
// upstream with the pre-rendered login page, so the browser shows login instead
// of an error. Shared by http/https and unix reverse proxies.
func authRedirectModifier() func(*http.Response) error {
	return func(r *http.Response) error {
		if r.StatusCode != http.StatusUnauthorized && r.StatusCode != http.StatusForbidden {
			return nil
		}
		if r.Body != nil {
			r.Body.Close()
		}
		loginHTML := mustAsset("login.html")
		r.StatusCode = http.StatusOK
		r.Header = make(http.Header)
		r.Header.Set("Content-Type", "text/html; charset=utf-8")
		r.Body = io.NopCloser(bytes.NewReader(loginHTML))
		r.ContentLength = int64(len(loginHTML))
		return nil
	}
}

// chainModifiers runs the given response modifiers in order, stopping at the
// first error. This lets us compose auth-redirect and logout-button injection.
func chainModifiers(mods ...func(*http.Response) error) func(*http.Response) error {
	return func(r *http.Response) error {
		for _, m := range mods {
			if m == nil {
				continue
			}
			if err := m(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// appDetector describes how to recognise a proxied app and which logout script
// to inject into its HTML.
type appDetector struct {
	fingerprint []byte // substring searched for in the response body
	scriptTag   []byte // <script src="..."> appended when the fingerprint matches
}

// appDetector pairs a body-content fingerprint with the logout-script tag to
// inject when the fingerprint is found. Adding support for a new proxied app is
// just a matter of appending an entry here (plus the script + its route).
var appDetectors = []appDetector{
	{
		// VS Code / code-server workbench boot page.
		fingerprint: []byte(`<meta id="vscode-workbench-web-configuration"`),
		scriptTag:   []byte(`<script src="/__logout.vsc.js"></script>`),
	},
}

// injectLogoutButton inspects proxied HTML document responses and, when the body
// matches a known app fingerprint, appends that app's logout-button script tag.
// Unrecognised pages pass through untouched, so non-VS-Code backends are not
// polluted with a useless script.
//
// Only top-level HTML documents (Content-Type: text/html, GET) are inspected,
// so static assets, API calls and Server-Sent-Events are unaffected.
func injectLogoutButton(r *http.Response) error {
	if r.Request == nil || r.Request.Method != http.MethodGet {
		return nil
	}
	// Only inspect top-level navigations; skip iframes / fetches.
	if dest := r.Request.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		return nil
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		return nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}
	if r.Body != nil {
		r.Body.Close()
	}

	// Detect the app by fingerprint; inject only on a match.
	tag := matchAppScript(body)
	if tag == nil {
		// Unknown app — restore the original body unchanged.
		setResponseBody(r, body)
		return nil
	}

	setResponseBody(r, append(body, tag...))
	return nil
}

// matchAppScript returns the logout-script tag for the first app whose
// fingerprint is found in body, or nil if no app matches.
func matchAppScript(body []byte) []byte {
	for _, d := range appDetectors {
		if bytes.Contains(body, d.fingerprint) {
			return d.scriptTag
		}
	}
	return nil
}

// setResponseBody replaces the response body with b, fixing Content-Length and
// clearing Transfer-Encoding / Uncompressed so clients see a consistent payload.
func setResponseBody(r *http.Response, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	r.Header.Del("Transfer-Encoding")
	r.Uncompressed = false
}

// generateSelfSignedCert creates a self-signed TLS certificate valid for 10 years.
func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	// Allow a small clock-skew window so the cert is valid immediately.
	notBefore := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "CodeAuth",
			Organization: []string{"Self-Signed CodeAuth"},
		},
		DNSNames:              []string{"self.ca"},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// startBackend runs a command directly as a subprocess in its own process group.
// Note: command is split with strings.Fields, so arguments cannot contain spaces.
func startBackend(cmdStr string) *exec.Cmd {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("start backend command: %v", err)
	}
	log.Printf("backend command started (pid %d): %s", cmd.Process.Pid, cmdStr)
	return cmd
}

// killProcessGroup sends SIGTERM to the process group of the given command.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		log.Printf("get pgid: %v", err)
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		log.Printf("kill backend pgid %d: %v", pgid, err)
	} else {
		log.Printf("sent SIGTERM to backend process group %d", pgid)
	}
}

// =============================================================================
// CORS Body Rewriting — vscode private-deployment URL rewriting
// =============================================================================
//
// Controlled by VSC_CORS_* env vars:
//
//	VSC_CORS_IDX   = from->to,from->to             index page only
//	VSC_CORS_SUF_* = from->to,...                   suffix match on normalized path
//	VSC_CORS_PRE_* = from->to,...                   prefix match on normalized path
//
// Path normalization: replace . - / with _, then lowercase.
// Only .js / .html / .json responses are rewritten (index page is treated as .html).

// corsCfg is set by loadCorsConfig. nil means no rules → corsModifier returns nil.
var corsCfg *corsConfig

type corsConfig struct {
	idxRules  []corsPair     // VSC_CORS_IDX
	pathRules []corsPathRule // VSC_CORS_SUF_* / VSC_CORS_PRE_*
}

type corsPathRule struct {
	pattern string // normalized path substring
	suffix  bool   // true=suffix match, false=prefix match
	pairs   []corsPair
}

type corsPair struct {
	from string
	to   string
}

func (c *corsConfig) isEmpty() bool {
	return c == nil || (len(c.idxRules) == 0 && len(c.pathRules) == 0)
}

// loadCorsConfig reads VSC_CORS_* env vars. Call once at startup.
func loadCorsConfig() {
	cfg := &corsConfig{}
	const prefix = "VSC_CORS_"
	for _, e := range os.Environ() {
		k, v, _ := strings.Cut(e, "=")
		if !strings.HasPrefix(k, prefix) {
			continue
		}
		rest := k[len(prefix):]
		if rest == "IDX" {
			cfg.idxRules = parseCorsPairs(v)
			continue
		}
		if after, ok := strings.CutPrefix(rest, "SUF_"); ok && after != "" {
			if pairs := parseCorsPairs(v); len(pairs) > 0 {
				cfg.pathRules = append(cfg.pathRules, corsPathRule{pattern: after, suffix: true, pairs: pairs})
			}
			continue
		}
		if after, ok := strings.CutPrefix(rest, "PRE_"); ok && after != "" {
			if pairs := parseCorsPairs(v); len(pairs) > 0 {
				cfg.pathRules = append(cfg.pathRules, corsPathRule{pattern: after, suffix: false, pairs: pairs})
			}
			continue
		}
	}
	if !cfg.isEmpty() {
		corsCfg = cfg
	}
}

// parseCorsPairs parses "from->to,from->to" into a []corsPair.
func parseCorsPairs(raw string) []corsPair {
	var pairs []corsPair
	for _, seg := range strings.Split(raw, ",") {
		seg = strings.TrimSpace(seg)
		if seg == "" {
			continue
		}
		f, t, ok := strings.Cut(seg, "->")
		if !ok {
			continue
		}
		f, t = strings.TrimSpace(f), strings.TrimSpace(t)
		if f == "" || t == "" {
			continue
		}
		pairs = append(pairs, corsPair{from: f, to: t})
	}
	return pairs
}

// corsModifier returns a ModifyResponse that rewrites response bodies according
// to the loaded VSC_CORS_* rules. Returns nil when no rules are configured.
func corsModifier() func(*http.Response) error {
	if corsCfg.isEmpty() {
		return nil
	}
	return func(r *http.Response) error {
		reqPath := r.Request.URL.Path
		ext := strings.ToLower(path.Ext(reqPath))

		// Only rewrite .js / .html / .json; index page is treated as .html.
		var pairs []corsPair
		switch {
		case isIndexRequest(r):
			pairs = corsCfg.idxRules
		case ext == ".js" || ext == ".html" || ext == ".json":
			pairs = matchCorsPath(reqPath, corsCfg.pathRules)
		default:
			return nil
		}
		if len(pairs) == 0 {
			return nil
		}

		body, err := io.ReadAll(r.Body)
		if err != nil {
			return err
		}
		if r.Body != nil {
			r.Body.Close()
		}

		for _, p := range pairs {
			body = bytes.ReplaceAll(body, []byte(p.from), []byte(p.to))
		}
		setResponseBody(r, body)
		return nil
	}
}

// isIndexRequest returns true for a top-level navigation to the root path
// (the page that bootstraps the SPA — treated as .html by CORS rules).
func isIndexRequest(r *http.Response) bool {
	if r.Request == nil || r.Request.Method != http.MethodGet {
		return false
	}
	// Only top-level navigations, not fetches inside the SPA.
	if d := r.Request.Header.Get("Sec-Fetch-Dest"); d != "" && d != "document" {
		return false
	}
	p := r.Request.URL.Path
	return p == "/" || p == "" || strings.HasSuffix(p, "/index.html")
}

// matchCorsPath normalises the request path (. - / → _, lowercase) and walks
// pathRules in order; the first SUF/PRE rule to match returns its pairs.
func matchCorsPath(reqPath string, rules []corsPathRule) []corsPair {
	norm := corsNormalizePath(reqPath)
	for _, r := range rules {
		if r.suffix && strings.HasSuffix(norm, r.pattern) {
			return r.pairs
		}
		if !r.suffix && strings.HasPrefix(norm, r.pattern) {
			return r.pairs
		}
	}
	return nil
}

// corsNormalizePath replaces . - / with _ and lowercases.
func corsNormalizePath(p string) string {
	b := make([]byte, 0, len(p))
	for i := 0; i < len(p); i++ {
		c := p[i]
		switch c {
		case '.', '-', '/':
			b = append(b, '_')
		default:
			if c >= 'A' && c <= 'Z' {
				c += 'a' - 'A'
			}
			b = append(b, c)
		}
	}
	return string(b)
}

// serveStaticAsset writes an embedded asset with the given content type.
func serveStaticAsset(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mustAsset(name))
}
