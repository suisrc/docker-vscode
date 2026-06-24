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
	"html/template"
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

//go:embed login.html logout.html
var templateFS embed.FS

var (
	tmplLogin  *template.Template
	tmplLogout *template.Template
)

// Config holds proxy configuration.
type Config struct {
	BackendURL   string
	ServiceCmd   string // optional shell command to run as the backend
	ProxyPort    string
	TokenCookie  string            // cookie name, default "vscode-tkn"
	ProxyUseSSL  bool              // enable HTTPS with a self-signed cert
	ProxyHeaders map[string]string // PROXY_HEADER_Xxx=Val → set/override; PROXY_HEADER_Xxx= → delete
}

func loadConfig() Config {
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

	flag.StringVar(&backendURL, "backend", backendURL, "Backend service URL")
	flag.StringVar(&serviceCmd, "service", serviceCmd, "Backend service Cmd")
	flag.StringVar(&proxyPort, "port", proxyPort, "Proxy listen port")
	flag.StringVar(&cookie, "cookie", cookie, "Token cookie name")
	flag.BoolVar(&useSSLFlag, "ssl", useSSLFlag, "Enable HTTPS with self-signed cert")
	flag.Parse()

	if backendURL == "" {
		log.Fatal("BACKEND_URL is required (set via env or -backend flag)")
	}
	return Config{
		BackendURL:   backendURL,
		ServiceCmd:   serviceCmd,
		ProxyPort:    proxyPort,
		TokenCookie:  cookie,
		ProxyUseSSL:  useSSLFlag,
		ProxyHeaders: parseProxyHeaders(),
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
	cfg := loadConfig()

	// Start backend subprocess if SERVICE_CMD is configured.
	var serviceCmd *exec.Cmd
	if cfg.ServiceCmd != "" {
		serviceCmd = startBackend(cfg.ServiceCmd)
		defer killProcessGroup(serviceCmd)
	}

	// Forward signals to the backend process.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		<-sigCh
		if serviceCmd != nil {
			killProcessGroup(serviceCmd)
			// If the backend is a Unix socket, remove the socket file on exit.
			if strings.HasPrefix(cfg.BackendURL, "unix://") {
				os.Remove(cfg.BackendURL[len("unix://"):])
			}
		}
		os.Exit(0)
	}()

	var err error
	tmplLogin, err = template.ParseFS(templateFS, "login.html")
	if err != nil {
		log.Fatalf("parse login.html: %v", err)
	}
	tmplLogout, err = template.ParseFS(templateFS, "logout.html")
	if err != nil {
		log.Fatalf("parse logout.html: %v", err)
	}

	backendURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Fatalf("invalid BACKEND_URL: %v", err)
	}

	var proxy *httputil.ReverseProxy

	if backendURL.Scheme == "unix" {
		// Unix socket backend: unix:///var/run/vscode.sock
		socketPath := backendURL.Path
		if socketPath == "" {
			// unix:///var/run/vscode.sock → Path = "/var/run/vscode.sock"
			socketPath = backendURL.Host
		}
		log.Printf("using unix socket: %s", socketPath)

		proxy = &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "unix"
				applyProxyHeaders(req, cfg.ProxyHeaders)
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		}
	} else {
		proxy = httputil.NewSingleHostReverseProxy(backendURL)
		origDirector := proxy.Director
		proxy.Director = func(req *http.Request) {
			origDirector(req)
			applyProxyHeaders(req, cfg.ProxyHeaders)
		}
	}

	// Pre-render login page for ModifyResponse (can't stream template there).
	loginHTML := renderLoginHTML()

	// Custom error handler: intercept 401/403 and show login page.
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden {
			if r.Body != nil {
				r.Body.Close()
			}
			r.StatusCode = http.StatusOK
			r.Header = make(http.Header)
			r.Header.Set("Content-Type", "text/html; charset=utf-8")
			r.Body = io.NopCloser(bytes.NewReader(loginHTML))
			r.ContentLength = int64(len(loginHTML))
			return nil
		}
		return nil
	}

	mux := http.NewServeMux()

	// /__login – serves login page (GET) or processes form (POST).
	mux.HandleFunc("/__login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			r.ParseForm()
			tkn := strings.TrimSpace(r.PostFormValue("token"))
			if tkn == "" {
				serveLoginHTML(w)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cfg.TokenCookie,
				Value:    tkn,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			// Reload the page that triggered the login.
			back := r.Referer()
			if back == "" {
				back = "/"
			}
			log.Printf("login ok, cookie %s=%s, reloading: %s", cfg.TokenCookie, tkn, back)
			http.Redirect(w, r, back, http.StatusSeeOther)
		default:
			serveLoginHTML(w)
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
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmplLogout.Execute(w, nil)
	})

	// /__vscode/ – VS Code update API proxy with local cache.
	mux.Handle("/__vscode/", newVscodeUpdateHandler())

	// /__proxy/ – generic external proxy: /__proxy/{scheme}:{host}/path → {scheme}://{host}/path
	mux.HandleFunc("/__proxy/", handleExternalProxy)

	// All other requests go through the reverse proxy.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	addr := ":" + cfg.ProxyPort
	log.Printf("proxy starting on %s → %s", addr, cfg.BackendURL)
	log.Printf("token cookie: %s", cfg.TokenCookie)
	if len(cfg.ProxyHeaders) > 0 {
		log.Printf("proxy headers: %v", cfg.ProxyHeaders)
	}

	if cfg.ProxyUseSSL {
		// HTTP on ProxyPort, HTTPS on ProxyPort+1
		portNum, err := strconv.Atoi(cfg.ProxyPort)
		if err != nil {
			log.Fatalf("invalid VSC_PORT %q: %v", cfg.ProxyPort, err)
		}
		httpsAddr := fmt.Sprintf(":%d", portNum+1)

		cert, err := generateSelfSignedCert()
		if err != nil {
			log.Fatalf("generate self-signed cert: %v", err)
		}
		tlsCfg := &tls.Config{Certificates: []tls.Certificate{cert}}

		// Start HTTP server in background.
		go func() {
			log.Printf("HTTP1 listening on %s", addr)
			if err := http.ListenAndServe(addr, mux); err != nil {
				log.Fatalf("HTTP server: %v", err)
			}
		}()

		log.Printf("HTTPS listening on %s (self-signed certificate)", httpsAddr)
		srv := &http.Server{
			Addr:      httpsAddr,
			Handler:   mux,
			TLSConfig: tlsCfg,
		}
		if err := srv.ListenAndServeTLS("", ""); err != nil {
			log.Fatal(err)
		}
	} else {
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Fatal(err)
		}
	}
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

// handleExternalProxy proxies /__proxy/{scheme}:{host}/path → {scheme}://{host}/path
// If no scheme is given, defaults to https.
// Examples:
//
//	/__proxy/https:main.vscode-cdn.net/path → https://main.vscode-cdn.net/path
//	/__proxy/main.vscode-cdn.net/path       → https://main.vscode-cdn.net/path
func handleExternalProxy(w http.ResponseWriter, r *http.Request) {
	p := strings.TrimPrefix(r.URL.Path, "/__proxy/")
	if p == "" || p == "/" {
		http.Error(w, "missing domain/path", http.StatusBadRequest)
		return
	}

	// Extract scheme:host from the first segment.
	// Format: scheme:host/rest or host/rest
	var scheme, host, rest string
	slashIdx := strings.Index(p, "/")
	if slashIdx >= 0 {
		rest = p[slashIdx:]
		p = p[:slashIdx]
	} else {
		rest = "/"
	}

	if idx := strings.Index(p, ":"); idx >= 0 {
		scheme = p[:idx]
		host = p[idx+1:]
	} else {
		scheme = "https"
		host = p
	}

	if host == "" {
		http.Error(w, "missing host", http.StatusBadRequest)
		return
	}

	targetURL := fmt.Sprintf("%s://%s%s", scheme, host, rest)
	if r.URL.RawQuery != "" {
		targetURL += "?" + r.URL.RawQuery
	}

	log.Printf("[ext-proxy] %s %s → %s", r.Method, r.URL.Path, targetURL)

	target, err := url.Parse(targetURL)
	if err != nil {
		http.Error(w, "invalid target URL", http.StatusBadRequest)
		return
	}

	rp := httputil.NewSingleHostReverseProxy(target)
	rp.Director = func(req *http.Request) {
		req.URL.Scheme = target.Scheme
		req.URL.Host = target.Host
		req.URL.Path = target.Path
		req.URL.RawQuery = target.RawQuery
		req.Host = target.Host
		// Strip /__proxy/... prefix headers.
		req.Header.Set("X-Forwarded-For", r.RemoteAddr)
	}
	rp.ErrorHandler = func(w http.ResponseWriter, r *http.Request, err error) {
		log.Printf("[ext-proxy] error proxying %s: %v", targetURL, err)
		http.Error(w, "proxy error", http.StatusBadGateway)
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
		client: &http.Client{
			Timeout: 10 * time.Minute,
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
		w.Write(data)
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

	var js json.RawMessage
	if err := json.Unmarshal(body, &js); err != nil {
		log.Printf("[vscode-update] latest invalid JSON from upstream: %v", err)
		http.Error(w, "invalid upstream response", http.StatusBadGateway)
		return
	}

	_ = os.MkdirAll(filepath.Dir(cachePath), 0755)
	if err := os.WriteFile(cachePath, body, 0644); err != nil {
		log.Printf("[vscode-update] latest write cache FAIL: %s → %v", cachePath, err)
	} else {
		log.Printf("[vscode-update] latest CACHED %s/%s → %s (%d bytes)", platform, quality, cachePath, len(body))
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "MISS")
	w.Write(body)
}

func (h *vscodeUpdateHandler) handleCommit(w http.ResponseWriter, r *http.Request, commit, platform, quality string) {
	cacheDir := filepath.Join(h.cacheDir, platform, quality)

	// 1. 查找缓存（扩展名未知，按 commit 前缀匹配）
	if cachedPath, ok := findCachedFile(cacheDir, commit); ok {
		fi, _ := os.Stat(cachedPath)
		log.Printf("[vscode-update] commit HIT  %s/%s/%s ← %s (%d bytes)", platform, quality, commit, cachedPath, fi.Size())
		ext := extractExtFromPath(cachedPath)
		redirectDownload(w, r, quality, commit, platform, ext)
		return
	}

	log.Printf("[vscode-update] commit MISS %s/%s/%s (not found in: %s)", platform, quality, commit, cacheDir)

	// 2. 从上游获取（跟随所有重定向，记录最终 URL）
	upstreamURL := fmt.Sprintf("%s/commit:%s/%s/%s", h.upstream, commit, platform, quality)
	log.Printf("[vscode-update] commit fetch: %s", upstreamURL)

	var finalURL string
	client := &http.Client{
		Timeout: 10 * time.Minute,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			finalURL = req.URL.String()
			if len(via) >= 10 {
				return fmt.Errorf("too many redirects")
			}
			return nil
		},
	}

	resp, err := client.Get(upstreamURL)
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

	// 如果 CheckRedirect 没触发（无重定向），用最终 resp 的 URL
	if finalURL == "" {
		finalURL = resp.Request.URL.String()
	}
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

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		log.Printf("[vscode-update] commit read body error: %v", err)
		http.Error(w, "read upstream body failed", http.StatusBadGateway)
		return
	}

	_ = os.MkdirAll(cacheDir, 0755)
	if err := os.WriteFile(cachePath, body, 0644); err != nil {
		log.Printf("[vscode-update] commit write cache FAIL: %s → %v", cachePath, err)
	} else {
		log.Printf("[vscode-update] commit CACHED %s/%s/%s → %s (%d bytes)", platform, quality, commit, cachePath, len(body))
	}

	// 4. 缓存完成后重定向到 download 接口，由它用正确文件名提供下载
	redirectDownload(w, r, quality, commit, platform, ext)
}

// handleDownload serves a cached commit file with the correct vscode-{platform}.{ext} filename.
func (h *vscodeUpdateHandler) handleDownload(w http.ResponseWriter, r *http.Request, commit, quality, platform, ext string) {
	cacheDir := filepath.Join(h.cacheDir, platform, quality)

	cachedPath, ok := findCachedFile(cacheDir, commit)
	if !ok {
		log.Printf("[vscode-update] download MISS: %s/%s/%s not found in %s", platform, quality, commit, cacheDir)
		http.Error(w, "file not found in cache", http.StatusNotFound)
		return
	}

	fi, _ := os.Stat(cachedPath)
	log.Printf("[vscode-update] download SERVE %s/%s/%s ← %s (%d bytes)", platform, quality, commit, cachedPath, fi.Size())

	downloadName := fmt.Sprintf("vscode-%s%s", platform, ext)
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, downloadName))
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

// ---- helpers ----
// (serveCommitFile is removed; handleDownload replaces it)

func findCachedFile(dir, commit string) (string, bool) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", false
	}
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		if strings.HasPrefix(e.Name(), commit) {
			return filepath.Join(dir, e.Name()), true
		}
	}
	return "", false
}

func extractExtFromURL(rawURL string) string {
	if idx := strings.Index(rawURL, "?"); idx >= 0 {
		rawURL = rawURL[:idx]
	}
	base := path.Base(rawURL)
	if base == "" || base == "." || base == "/" {
		return ""
	}
	dotIdx := strings.Index(base, ".")
	if dotIdx < 0 {
		return ""
	}
	return base[dotIdx:]
}

func extractExtFromPath(filePath string) string {
	base := filepath.Base(filePath)
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

// =============================================================================

func renderLoginHTML() []byte {
	var buf bytes.Buffer
	if err := tmplLogin.Execute(&buf, nil); err != nil {
		panic("render login: " + err.Error())
	}
	return buf.Bytes()
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

	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "CodeAuth",
			Organization: []string{"Self-Signed CodeAuth"},
		},
		DNSNames:              []string{"self.ca"},
		NotBefore:             time.Now(),
		NotAfter:              time.Now().Add(10 * 365 * 24 * time.Hour),
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

func serveLoginHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmplLogin.Execute(w, nil)
}
