package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
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
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed favicon.ico loading.html login.html logout.html logout.vsc.js
var staticFS embed.FS

// mustAsset reads an embedded asset by name, failing fast at startup if missing.
// embed.FS is an in-memory read-only map; ReadFile just returns a slice over it,
// so there is no I/O and no benefit to pre-caching assets into package vars.
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
	ServiceWsc   string // SERVICE_WSC — working directory for the service
	ServiceUrl   string // SERVICE_URL — download URL for the backend service
	ServiceVer   string // SERVICE_VER — download cache file path ({ext} resolved at runtime)
	ServicePxy   string // SERVICE_PXY — proxy cache root directory
	ServiceDir   string // SERVICE_DIR — install/extract directory
	ServicePre   string // SERVICE_PRE — shell command or file:// script run before start
	ServiceCmd   string // SERVICE_CMD — optional shell command to run as the backend
	ProxyPort    string
	TokenCookie  string            // cookie name, default "vscode-tkn"
	ProxyUseSSL  bool              // enable HTTPS with a self-signed cert
	ProxyHeaders map[string]string // PROXY_HEADER_Xxx=Val → set/override; PROXY_HEADER_Xxx= → delete
}

// Backend describes a single proxy target with its routing prefix.
type Backend struct {
	Prefix    string // routing prefix, "/" for root
	Scheme    string // http, https, unix, file, text
	Target    string // host:port, socket path, dir path, or literal text
	RawURL    string // original URL for logging
	IsService bool   // this backend is the one managed by Codea (auto-deploy, etc.)
}

// GetEnvDef returns the value of env key, falling back to def when unset or
// empty. The result is then expanded with os.ExpandEnv so values may reference
// other env vars (e.g. "${SERVICE_WSC}/.vserve"). The expanded value is written
// back to the environment so child processes see the resolved form.
func GetEnvDef(key, val_def string) string {
	val_old := os.Getenv(key)
	val_new := val_old
	if val_new == "" {
		val_new = val_def
	}
	val_new = os.ExpandEnv(val_new)
	if val_new != val_old {
		log.Printf("SetEnv: %s=%s", key, val_new)
		_ = os.Setenv(key, val_new)
	}
	return val_new
}

// resolveVscodeHash resolves VSCODE_HASH when it is set to the magic value
// "vscode:latest": it fetches the VS Code update API for the latest stable
// server-linux-x64-web release and writes the returned commit version (the
// "version" field, e.g. "7e7950df...") into VSCODE_HASH. When VSCODE_HASH is
// unset or any other value, it is left untouched.
func resolveVscodeHash() {
	const magic = "vscode:latest"
	if os.Getenv("VSCODE_HASH") != magic {
		return
	}
	const apiURL = "https://update.code.visualstudio.com/api/latest/server-linux-x64-web/stable"
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		log.Fatalf("resolve VSCODE_HASH: fetch %s: %v", apiURL, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Fatalf("resolve VSCODE_HASH: %s returned HTTP %d", apiURL, resp.StatusCode)
	}
	var info struct {
		Name    string `json:"name"`
		Version string `json:"version"` // commit hash, e.g. 7e7950df89d055b5a378379db9ee14290772148a
	}
	if err := json.NewDecoder(resp.Body).Decode(&info); err != nil {
		log.Fatalf("resolve VSCODE_HASH: decode response: %v", err)
	}
	if info.Version == "" {
		log.Fatalf("resolve VSCODE_HASH: empty version in response")
	}
	_ = os.Setenv("VSCODE_HASH", info.Version)
	log.Printf("check vscode latest version: VSCODE_HASH=%s VERSION=%s", info.Version, info.Name)
}

func loadInitConfig() Config {
	resolveVscodeHash()

	// Flags provide defaults; environment variables take precedence over flags.
	// Bind flags with hard-coded defaults first, parse, then let env vars override.
	backendURL := ""
	serviceWsc := "/wsc"
	serviceUrl := "https://update.code.visualstudio.com/commit:${VSCODE_HASH}/server-linux-x64-web/stable"
	serviceVer := "${SERVICE_WSC}/.vsc/cache/version/${VSCODE_HASH}.{ext}"
	servicePxy := "${SERVICE_WSC}/.vsc/cache/proxies/"
	serviceDir := "${SERVICE_WSC}/.vsc/serve/${VSCODE_HASH}/"
	servicePre := ""
	serviceCmd := ""
	proxyPort := "7080"
	cookie := "vscode-tkn"
	useSSLFlag := false

	flag.StringVar(&backendURL, "backend", backendURL, "Backend service URL(s)")
	flag.StringVar(&serviceWsc, "svc-wsc", serviceWsc, "Service working directory")
	flag.StringVar(&serviceUrl, "svc-url", serviceUrl, "Backend service download URL")
	flag.StringVar(&serviceVer, "svc-ver", serviceVer, "Download cache file path")
	flag.StringVar(&servicePxy, "svc-pxy", servicePxy, "Proxy cache root directory")
	flag.StringVar(&serviceDir, "svc-dir", serviceDir, "Install/extract directory")
	flag.StringVar(&servicePre, "svc-pre", servicePre, "Pre-start script (file:// or shell)")
	flag.StringVar(&serviceCmd, "svc-cmd", serviceCmd, "Backend service command")
	flag.StringVar(&proxyPort, "port", proxyPort, "Proxy listen port")
	flag.StringVar(&cookie, "cookie", cookie, "Token cookie name")
	flag.BoolVar(&useSSLFlag, "use-ssl", useSSLFlag, "Enable HTTPS with self-signed cert")
	flag.Parse()

	// Environment variables override flag values (env-first precedence).
	backendURL = GetEnvDef("BACKEND_URL", backendURL)
	serviceWsc = GetEnvDef("SERVICE_WSC", serviceWsc)
	serviceUrl = GetEnvDef("SERVICE_URL", serviceUrl)
	serviceVer = GetEnvDef("SERVICE_VER", serviceVer)
	servicePxy = GetEnvDef("SERVICE_PXY", servicePxy)
	serviceDir = GetEnvDef("SERVICE_DIR", serviceDir)
	servicePre = GetEnvDef("SERVICE_PRE", servicePre)
	serviceCmd = GetEnvDef("SERVICE_CMD", serviceCmd)
	proxyPort = GetEnvDef("VSCODE_PORT", proxyPort) // VSCODE_PORT 兼容
	proxyPort = GetEnvDef("PROXY_PORT", proxyPort)  // PROXY_PORT 最优先

	cookie = GetEnvDef("TOKEN_COOKIE", cookie)
	useSSLEnv := GetEnvDef("PROXY_USE_SSL", "")
	if useSSLEnv != "" {
		useSSLFlag = useSSLEnv == "1" || strings.EqualFold(useSSLEnv, "true")
	}

	if backendURL == "" {
		log.Fatal("BACKEND_URL is required (set via env or -backend flag)")
	}

	backends := parseBackends(backendURL)
	if len(backends) == 0 {
		log.Fatal("BACKEND_URL parsed to zero backends")
	}

	return Config{
		Backends:     backends,
		ServiceWsc:   serviceWsc,
		ServiceUrl:   serviceUrl,
		ServiceVer:   serviceVer,
		ServicePxy:   servicePxy,
		ServiceDir:   serviceDir,
		ServicePre:   servicePre,
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

	// Single backend: root prefix "/".  When there's only one backend it is
	// implicitly the service backend managed by Codea.
	if b := newBackend("/", raw); b != nil {
		b.IsService = true
		return []Backend{*b}
	}
	return nil
}

// isMultiBackend reports whether raw uses the "/prefix=scheme://..." form.
// It returns true only when an "=" appears before the first "://", i.e. there
// is a routing prefix in front of a real scheme. This classifies both
// "/=unix:///path" (single multi-backend segment) and
// "/a/=http://x;/b/=https://y" (multiple segments), while rejecting plain
// single-backend URLs like "http://h?a=b" where "=" is only in the query.
func isMultiBackend(raw string) bool {
	eqIdx := strings.Index(raw, "=")
	if eqIdx < 0 {
		return false
	}
	schemeIdx := strings.Index(raw, "://")
	return schemeIdx > 0 && schemeIdx > eqIdx
}

// parseMultiBackends parses already-split segments into backends.
// A prefix starting with "^" marks the backend as the service backend managed by
// Codea (auto-deploy, fixup, etc.). The "^" is stripped for routing purposes.
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

		// Detect service marker ("^" prefix) and strip it for routing.
		isService := strings.HasPrefix(prefix, "^")
		if isService {
			prefix = strings.TrimPrefix(prefix, "^")
		}
		if !strings.HasPrefix(prefix, "/") {
			prefix = "/" + prefix
		}

		if b := newBackend(prefix, urlStr); b != nil {
			b.IsService = isService
			backends = append(backends, *b)
		}
	}
	return backends
}

// splitBackendSegments splits a multi-backend string on ";", then keeps each
// segment. A segment may start with an optional "^" service marker followed by
// a "/prefix=...". Splitting on ";" alone is safe because the URL body never
// contains a ";" in our supported schemes (http/https/unix/file/text).
// Format: /a/=http://x;/b/=https://y  or  ^/a/=http://x;/b/=https://y
func splitBackendSegments(raw string) []string {
	parts := strings.Split(raw, ";")
	segs := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			segs = append(segs, p)
		}
	}
	return segs
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
// PROXY_HEADER_Xxx=    → delete header Xxx.
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

// =============================================================================
// Service Preparation — download, extract, fixup before starting SERVICE_CMD
// =============================================================================

// serviceState tracks lazy preparation of the service backend. Preparation is
// triggered on the first request to the service prefix (not at startup).
// `preparing` covers the whole lifecycle (from trigger to finish) so requests
// never proxy to the backend before it is fully ready (avoids 502).
type serviceState struct {
	once      sync.Once
	mu        sync.RWMutex
	preparing bool   // true from first trigger until finish() — gates proxying
	status    string // current status message while preparing; "" when idle
	done      bool   // preparation finished (successfully or not)
	err       error  // preparation error; nil on success
}

// begin marks preparation as in progress (called once at trigger time).
func (s *serviceState) begin() {
	s.mu.Lock()
	s.preparing = true
	s.mu.Unlock()
}

// setPreparing updates the human-readable status while preparing.
func (s *serviceState) setPreparing(status string) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// finish marks preparation done, storing err (nil on success).
func (s *serviceState) finish(err error) {
	s.mu.Lock()
	s.preparing = false
	s.status = ""
	s.done = true
	s.err = err
	s.mu.Unlock()
}

// active reports whether preparation is in progress.
func (s *serviceState) active() bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.preparing
}

// getStatus returns the current status message (empty when idle).
func (s *serviceState) getStatus() string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.status
}

// result returns (done, err) — whether preparation finished and any error.
func (s *serviceState) result() (bool, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.done, s.err
}

// serveLoadingPage writes the preparation loading page (loading.html), injecting
// the current status into the __STATUS__ placeholder.
func serveLoadingPage(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusServiceUnavailable)
	msg := status
	if msg == "" {
		msg = "Preparing Application"
	}
	html := strings.Replace(string(mustAsset("loading.html")), "__STATUS__", msg, 1)
	_, _ = w.Write([]byte(html))
}

// prepareService downloads (if needed), extracts, and runs fixups for the backend
// service. It blocks until preparation is complete. srvState tracks status for the
// loading page.
func prepareService(cfg Config, srvState *serviceState) error {
	if cfg.ServiceUrl == "" {
		return nil // nothing to download
	}
	if cfg.ServiceDir == "" {
		return fmt.Errorf("SERVICE_DIR is required when SERVICE_URL is set")
	}
	if cfg.ServiceVer == "" {
		return fmt.Errorf("SERVICE_VER is required when SERVICE_URL is set")
	}

	// Already installed? A non-empty SERVICE_DIR means a previous extraction
	// succeeded; skip preparation. An empty/missing dir triggers re-extract.
	if installed, reason := isServiceInstalled(cfg.ServiceDir); installed {
		log.Printf("[prepare] SERVICE_DIR exists and looks valid: %s", cfg.ServiceDir)
		return nil
	} else if reason != "" {
		log.Printf("[prepare] %s, will re-extract: %s", reason, cfg.ServiceDir)
	}

	// Resolve {ext} placeholder in SERVICE_VER.
	verPath, err := resolveVerPath(cfg.ServiceVer, cfg.ServiceUrl)
	if err != nil {
		return err
	}

	// Download if not cached.
	if _, err := os.Stat(verPath); err != nil {
		srvState.setPreparing("Downloading Application: " + cfg.ServiceUrl)
		log.Printf("[prepare] downloading: %s → %s", cfg.ServiceUrl, verPath)
		if err := downloadFile(cfg.ServiceUrl, verPath); err != nil {
			return fmt.Errorf("download SERVICE_URL: %w", err)
		}
		log.Printf("[prepare] download complete: %s", verPath)
	} else {
		log.Printf("[prepare] download cached: %s", verPath)
	}

	// Extract.
	srvState.setPreparing("Extracting Application: " + cfg.ServiceDir)
	log.Printf("[prepare] extracting: %s → %s", verPath, cfg.ServiceDir)
	if err := extractTarball(verPath, cfg.ServiceDir); err != nil {
		return fmt.Errorf("extract: %w", err)
	}
	log.Printf("[prepare] extract complete: %s", cfg.ServiceDir)

	// Run SERVICE_PRE if set.
	if cfg.ServicePre != "" {
		srvState.setPreparing("Running pre-start script: " + cfg.ServicePre)
		log.Printf("[prepare] running pre-start script: %s", cfg.ServicePre)
		if err := runServicePreScript(cfg.ServicePre); err != nil {
			return fmt.Errorf("SERVICE_PRE: %w", err)
		}
		log.Printf("[prepare] pre-start script complete")
	}

	return nil
}

// isServiceInstalled reports whether dir looks like a valid service install —
// i.e. it exists, is a directory, and is non-empty. The returned reason is
// non-empty when the directory exists but is empty (incomplete extraction).
// This is application-agnostic: codea does not assume any specific binary name.
func isServiceInstalled(dir string) (ok bool, reason string) {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false, ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, "SERVICE_DIR exists but is not readable"
	}
	if len(entries) == 0 {
		return false, "SERVICE_DIR exists but is empty"
	}
	return true, ""
}

// resolveVerPath resolves the {ext} placeholder in verPattern using the file
// extension inferred from urlStr's final (redirected) URL.
func resolveVerPath(verPattern, urlStr string) (string, error) {
	if !strings.Contains(verPattern, "{ext}") {
		return verPattern, nil
	}
	ext, err := resolveExtFromURL(urlStr)
	if err != nil {
		return "", fmt.Errorf("resolve extension from SERVICE_URL: %w", err)
	}
	verPath := strings.Replace(verPattern, "{ext}", ext, 1)
	log.Printf("[prepare] SERVICE_VER resolved: %s", verPath)
	return verPath, nil
}

// resolveExtFromURL follows redirects on urlStr and extracts the file extension
// from the final URL's basename (e.g. ".tar.gz"). Only the final URL is needed,
// so a HEAD request suffices; a non-2xx status is logged but not fatal.
func resolveExtFromURL(urlStr string) (string, error) {
	client := &http.Client{
		Timeout:       30 * time.Second,
		CheckRedirect: redirectLimit(10),
	}
	resp, err := client.Head(urlStr)
	if err != nil {
		return "", err
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[prepare] HEAD %s returned %d (using final URL for extension)", urlStr, resp.StatusCode)
	}
	finalURL := ""
	if resp.Request != nil {
		finalURL = resp.Request.URL.String()
	}
	return extractExt(finalURL), nil
}

// extractExt returns the extension portion of a basename without the leading
// dot, e.g. "x.tar.gz" → "tar.gz". The leading dot is intentionally omitted so
// callers control it in the template (e.g. "${HASH}.{ext}" → "hash.tar.gz").
func extractExt(base string) string {
	base = filepath.Base(base)
	// Strip query/fragment.
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	dotIdx := strings.Index(base, ".")
	if dotIdx < 0 {
		log.Printf("[prepare] no extension found in %q, defaulting to tar.gz", base)
		return "tar.gz" // sensible default for compressed tarballs
	}
	return base[dotIdx+1:]
}

// downloadClient is used for SERVICE_URL downloads with a generous timeout so
// large tarballs don't hang forever, but slow mirrors still work.
var downloadClient = &http.Client{Timeout: 30 * time.Minute}

// downloadFile downloads urlStr to destPath using an atomic temp+rename strategy.
func downloadFile(urlStr, destPath string) error {
	resp, err := downloadClient.Get(urlStr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	written, err := io.Copy(tmp, resp.Body)
	if err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Verify downloaded size against Content-Length (when provided).
	if resp.ContentLength > 0 && written != resp.ContentLength {
		return fmt.Errorf("download truncated: got %d bytes, expected %d", written, resp.ContentLength)
	}

	if err := os.Rename(tmpName, destPath); err != nil {
		return err
	}
	success = true
	return nil
}

// extractTarball extracts a .tar.gz (or .tgz) archive into destDir, creating
// destDir if needed. It strips the top-level directory from archive entries
// so the contents land directly in destDir.
func extractTarball(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	var stripPrefix string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		// Determine common prefix to strip from the first entry.
		// When the first entry has no '/' (rare: bare file), stripPrefix stays
		// empty and no stripping occurs — all names are used as-is.
		if stripPrefix == "" {
			if idx := strings.Index(hdr.Name, "/"); idx >= 0 {
				stripPrefix = hdr.Name[:idx+1]
			}
		}

		rel := strings.TrimPrefix(hdr.Name, stripPrefix)
		if rel == "" || rel == "." {
			continue
		}
		// Skip macOS metadata.
		if strings.HasPrefix(filepath.Base(rel), "._") {
			continue
		}

		target := filepath.Join(destDir, rel)
		// Safety: ensure target stays within destDir (reject path traversal).
		if !isPathWithin(target, destDir) {
			log.Printf("[prepare] WARNING: skipping path traversal attempt: %s", hdr.Name)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Create symlink if supported; skip on error.
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				log.Printf("[prepare] symlink skipped: %s → %s (%v)", target, hdr.Linkname, err)
			}
		}
	}
	return nil
}

// isPathWithin reports whether target is destDir itself or nested inside it,
// guarding against path traversal in extracted archives.
func isPathWithin(target, destDir string) bool {
	cleanDest := filepath.Clean(destDir)
	cleanTarget := filepath.Clean(target)
	return cleanTarget == cleanDest ||
		strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator))
}

// runServicePreScript executes SERVICE_PRE. If it starts with "file://", the referenced file
// is made executable and run directly (the kernel reads its #! shebang). Otherwise,
// the string is run via sh -c.
func runServicePreScript(cmd string) error {
	var c *exec.Cmd
	if filePath, ok := strings.CutPrefix(cmd, "file://"); ok {
		if err := os.Chmod(filePath, 0o755); err != nil {
			return fmt.Errorf("chmod pre-start script file: %w", err)
		}
		c = exec.Command(filePath)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

func main() {
	cfg := loadInitConfig()

	// Apply SERVICE_PXY as the external proxy cache root.
	if cfg.ServicePxy != "" {
		proxyCacheOverride = cfg.ServicePxy
	}

	// Service preparation state (for showing loading page during download/extract).
	srvState := &serviceState{}

	// Build (prefix → handler) routes for each backend. The service backend's
	// prefix is recorded separately so the loading page is only shown for it
	// during preparation (non-service backends stay reachable).
	type route struct {
		prefix    string
		handler   http.Handler
		isService bool
	}
	routes := make([]route, len(cfg.Backends))
	servicePrefix := "" // prefix of the Codea-managed service backend, "" if none
	for i, b := range cfg.Backends {
		routes[i] = route{prefix: b.Prefix, handler: createBackendHandler(b, cfg.ProxyHeaders), isService: b.IsService}
		if b.IsService {
			servicePrefix = b.Prefix
		}
	}

	// serviceCmd holds the backend subprocess; started either eagerly (no
	// service backend) or lazily after preparation completes (service backend).
	var serviceCmd *exec.Cmd

	// Cookie helpers shared by the login/logout handlers.
	setTokenCookie := func(w http.ResponseWriter, value string, maxAge int) {
		http.SetCookie(w, &http.Cookie{
			Name:     cfg.TokenCookie,
			Value:    value,
			Path:     "/",
			MaxAge:   maxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	mux := http.NewServeMux()

	// /__login – serves login page (GET) or processes form (POST).
	mux.HandleFunc("/__login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			serveStaticAsset(w, "login.html")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		tkn := strings.TrimSpace(r.PostFormValue("token"))
		if tkn == "" {
			serveStaticAsset(w, "login.html")
			return
		}
		setTokenCookie(w, tkn, 0)
		back := safeReferer(r.Referer())
		log.Printf("login ok, cookie %s=%s, reloading: %s", cfg.TokenCookie, tkn, back)
		http.Redirect(w, r, back, http.StatusSeeOther)
	})

	// /__logout – clears the token cookie.
	mux.HandleFunc("/__logout", func(w http.ResponseWriter, r *http.Request) {
		setTokenCookie(w, "", -1)
		log.Printf("logout: cleared cookie %s", cfg.TokenCookie)
		serveStaticAsset(w, "logout.html")
	})

	// /favicon.ico – a minimal inline SVG favicon (blue rounded square with "C")
	// so browsers don't log 404s for it. Modern browsers accept image/svg+xml.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		serveStaticAsset(w, "favicon.ico")
	})

	// /__proxy/ – generic external proxy: /__proxy/{scheme}:{host}/path → {scheme}://{host}/path
	// Only registered when SERVICE_PXY is configured (caching requires a root).
	if cfg.ServicePxy != "" {
		mux.HandleFunc("/__proxy/", handleExternalProxy)
	}

	// /__logout.vsc.js – VS Code logout-button script injected into proxied HTML.
	// Named .vsc because it targets the VS Code activity-bar toolbar; other apps
	// can get their own script (e.g. /__logout.xxx.js) later.
	mux.HandleFunc("/__logout.vsc.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(mustAsset("logout.vsc.js"))
	})

	// All other requests: dispatch to backends. The service backend is
	// prepared lazily on first access: while preparing, its prefix returns the
	// loading page; once done (success), it proxies normally; on error, 500.
	// Non-service backends are always reachable.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			if !strings.HasPrefix(r.URL.Path, rt.prefix) {
				continue
			}
			if rt.isService {
				// Trigger preparation on first hit (once, in a goroutine).
				srvState.once.Do(func() {
					srvState.begin() // mark preparing immediately, before the goroutine runs
					go func() {
						err := prepareService(cfg, srvState)
						if err == nil && cfg.ServiceCmd != "" {
							// Clean up stale unix socket files left by a previous crash.
							for _, b := range cfg.Backends {
								if b.IsService && b.Scheme == "unix" {
									_ = os.Remove(b.Target)
								}
							}
							// Start the backend subprocess after a successful prepare.
							if c := startBackend(cfg.ServiceCmd); c != nil {
								serviceCmd = c
							}
						}
						srvState.finish(err)
						if err != nil {
							log.Printf("[prepare] service preparation failed: %v", err)
						}
					}()
				})
				// Preparing? Show loading page.
				if srvState.active() {
					serveLoadingPage(w, srvState.getStatus())
					return
				}
				// Finished: proxy on success, 500 on error.
				if done, err := srvState.result(); done {
					if err != nil {
						http.Error(w, "service preparation failed: "+err.Error(), http.StatusInternalServerError)
						return
					}
					rt.handler.ServeHTTP(w, r)
					return
				}
				// not yet active (goroutine just scheduled) — loading page.
				serveLoadingPage(w, srvState.getStatus())
				return
			}
			rt.handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "no backend matched", http.StatusBadGateway)
	})

	servers := buildServers(cfg.ProxyPort, cfg.ProxyUseSSL, mux)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP servers first so the loading page is available during preparation.
	for _, srv := range servers {
		go func(s *http.Server, isTLS bool) {
			var err error
			if isTLS {
				err = s.ListenAndServeTLS("", "")
			} else {
				err = s.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				log.Fatalf("server on %s: %v", s.Addr, err)
			}
		}(srv.server, srv.isTLS)
	}

	log.Printf("proxy starting: %s", strings.Join(serverAddrs(servers), ", "))
	log.Printf("token cookie: %s", cfg.TokenCookie)
	log.Printf("backends: %d", len(cfg.Backends))
	for i, b := range cfg.Backends {
		marker := ""
		if b.IsService {
			marker = " (service)"
		}
		log.Printf("  route[%d] %s → %s://%s%s", i, b.Prefix, b.Scheme, b.Target, marker)
	}
	if len(cfg.ProxyHeaders) > 0 {
		log.Printf("proxy headers: %v", cfg.ProxyHeaders)
	}

	// Start backend subprocess if SERVICE_CMD is configured. When a service
	// backend exists, the subprocess is started lazily after preparation
	// completes (see the service route handler above); otherwise it starts now.
	if cfg.ServiceCmd != "" && servicePrefix == "" {
		serviceCmd = startBackend(cfg.ServiceCmd)
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
			if b.IsService && b.Scheme == "unix" {
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
	base := newProxyServer(":"+port, mux, nil)
	if !useSSL {
		return []serverInstance{{server: base, isTLS: false}}
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("invalid proxy port %q: %v", port, err)
	}
	cert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatalf("generate self-signed cert: %v", err)
	}
	httpsSrv := newProxyServer(fmt.Sprintf(":%d", portNum+1), mux, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	return []serverInstance{
		{server: base, isTLS: false},
		{server: httpsSrv, isTLS: true},
	}
}

// newProxyServer returns an *http.Server with the proxy's standard timeouts.
// A nil tlsCfg yields a plain HTTP server.
func newProxyServer(addr string, mux http.Handler, tlsCfg *tls.Config) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // streaming (proxying, downloads) may take long; bounded by IdleTimeout + client disconnect
		IdleTimeout:       120 * time.Second,
	}
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
	}
	return srv
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
// Only relative paths are allowed to prevent open redirect attacks.
// url.Parse may fail on refs containing spaces; those safely fall back to "/".
func safeReferer(ref string) string {
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil || u.Host != "" {
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
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   10,
}

// =============================================================================
// External Proxy Cache
// =============================================================================

// --- proxy cache init (sync.Once, disk-only cache) ---

var (
	proxyCacheOnce     sync.Once
	proxyCacheRoot     string // resolved from SERVICE_PXY at first /__proxy/ request; route only registered when non-empty
	proxyCacheOverride string // set from SERVICE_PXY in main()
)

// initProxyCache resolves the cache root from SERVICE_PXY. The /__proxy/ route
// is only registered when SERVICE_PXY is set, so the root is always non-empty
// here; this just stores it for the cache handlers.
func initProxyCache() {
	proxyCacheRoot = proxyCacheOverride
	log.Printf("[ext-proxy] cache root (SERVICE_PXY): %s", proxyCacheRoot)
}

// redirectLimit is a CheckRedirect policy shared by proxy and download clients.
func redirectLimit(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
}

// proxyCacheClient is used for manual upstream fetches when caching.
var proxyCacheClient = &http.Client{
	Transport:     extProxyTransport,
	Timeout:       5 * time.Minute,
	CheckRedirect: redirectLimit(10),
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
//	{SERVICE_PXY}/{scheme}:{host}/path/to/file.js       → body
//	{SERVICE_PXY}/{scheme}:{host}/path/to/file.js_.json  → metadata
//
// The rest path is cleaned and stripped of leading "/" to stay within
// the cache root.  Requests for the root path use "__index" as filename.
func proxyCachePaths(scheme, host, rest string) (bodyPath, metaPath string) {
	// Clean and make relative to prevent directory traversal.
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(proxyCacheRoot, scheme+":"+host, p)
	return base, base + "_.json"
}

// readProxyMeta reads and parses a cache metadata file from disk.
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
// Format:  /__proxy/[cc~]{scheme}:{host}[/path][?query]
//
//	cc~              — optional cache marker: check cache, write on MISS
//	{scheme}:        — optional scheme (http, https); defaults to https
//	{host}           — upstream host[:port]
func handleExternalProxy(w http.ResponseWriter, r *http.Request) {
	proxyCacheOnce.Do(initProxyCache)

	p := strings.TrimPrefix(r.URL.Path, "/__proxy/")
	if p == "" || p == "/" {
		http.Error(w, "missing domain/path", http.StatusBadRequest)
		return
	}

	// Detect cc~ cache prefix.
	cacheable := strings.HasPrefix(p, "cc~")
	if cacheable {
		p = p[3:]
	}
	// Only cache safe GET responses.
	if r.Method != http.MethodGet {
		cacheable = false
	}

	// Parse scheme:host/rest from the path.
	scheme, host, rest, err := parseProxyPath(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	targetURL := buildTargetURL(scheme, host, rest, r.URL.RawQuery)

	log.Printf("[ext-proxy] %s %s → %s (cacheable=%v)", r.Method, r.URL.Path, targetURL, cacheable)

	if !cacheable {
		// Plain passthrough — no cache.
		handlePassThroughProxy(w, r, targetURL)
		return
	}

	// Try cache lookup; on MISS fetch, stream and store.
	bodyPath, metaPath := proxyCachePaths(scheme, host, rest)
	if serveFromProxyCache(w, bodyPath, metaPath, targetURL) {
		return
	}
	handleCachedProxy(w, r, targetURL, bodyPath, metaPath)
}

// parseProxyPath extracts scheme, host and rest from the proxy path segment.
// Format: scheme:host/rest or host/rest.  Returns an error string for bad input.
func parseProxyPath(p string) (scheme, host, rest string, err error) {
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
		return "", "", "", fmt.Errorf("missing host")
	}
	if !allowedExtProxySchemes[scheme] {
		return "", "", "", fmt.Errorf("unsupported scheme")
	}
	return scheme, host, rest, nil
}

// buildTargetURL constructs the full upstream URL with a strings.Builder.
func buildTargetURL(scheme, host, rest, rawQuery string) string {
	var b strings.Builder
	b.Grow(len(scheme) + 3 + len(host) + len(rest) + len(rawQuery) + 1)
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	b.WriteString(rest)
	if rawQuery != "" {
		b.WriteByte('?')
		b.WriteString(rawQuery)
	}
	return b.String()
}

// serveFromProxyCache tries to serve a response from cache. Returns true on HIT.
func serveFromProxyCache(w http.ResponseWriter, bodyPath, metaPath, targetURL string) bool {
	meta, err := readProxyMeta(metaPath)
	if err != nil {
		return false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return false
	}
	log.Printf("[ext-proxy] cache HIT  %s ← %s (%d bytes)", targetURL, bodyPath, len(body))
	w.Header().Set("X-Cache", "HIT")
	for k, vs := range meta.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(meta.Status)
	_, _ = w.Write(body)
	return true
}

// handleCachedProxy fetches the upstream, streams the response to both the
// client and a cache file, and writes cache metadata on success.
func handleCachedProxy(w http.ResponseWriter, r *http.Request, targetURL, bodyPath, metaPath string) {
	log.Printf("[ext-proxy] cache MISS (will cache) %s", targetURL)

	// proxyCacheClient already enforces a 5m timeout; reuse the request context
	// so client disconnects cancel the upstream fetch.
	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "bad target URL", http.StatusBadRequest)
		return
	}
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

	// Non-2xx: stream through without caching.
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
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	n, copyErr := io.Copy(io.MultiWriter(w, tmp), resp.Body)
	_ = tmp.Close()
	if copyErr != nil {
		log.Printf("[ext-proxy] copy error: %v", copyErr)
		return
	}

	// Atomic rename temp → final body.
	if err := os.Rename(tmpName, bodyPath); err != nil {
		log.Printf("[ext-proxy] rename cache FAIL: %s → %v", bodyPath, err)
		return
	}
	committed = true

	// Write metadata.
	if err := writeProxyMeta(metaPath, meta); err != nil {
		log.Printf("[ext-proxy] write meta FAIL: %s → %v", metaPath, err)
	} else {
		log.Printf("[ext-proxy] CACHED %s → %s (%d bytes)", targetURL, bodyPath, n)
	}
}

// handlePassThroughProxy proxies the request to the upstream without caching.
func handlePassThroughProxy(w http.ResponseWriter, r *http.Request, targetURL string) {
	log.Printf("[ext-proxy] passthrough %s", targetURL)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "bad target URL", http.StatusBadRequest)
		return
	}
	// Forward original headers; net/http strips hop-by-hop on the wire.
	req.Header = r.Header.Clone()

	resp, err := proxyCacheClient.Do(req)
	if err != nil {
		log.Printf("[ext-proxy] fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
		rp.ModifyResponse = proxyResponseModifier()
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
		rp.ModifyResponse = proxyResponseModifier()
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
			// {now} is replaced with the current time on every request.
			body := strings.ReplaceAll(content, "{now}", time.Now().Format(time.RFC3339))
			_, _ = w.Write([]byte(body))
		})

	default:
		log.Fatalf("unknown backend scheme %q in %q", b.Scheme, b.RawURL)
		return nil
	}
}

// proxyResponseModifier returns the composed response modifier applied to all
// reverse-proxy backends: replace 401/403 with the login page, then inject the
// logout-button script into recognised HTML apps.
func proxyResponseModifier() func(*http.Response) error {
	return chainModifiers(authRedirectModifier(), injectLogoutButton)
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
		r.StatusCode = http.StatusOK
		r.Header = make(http.Header)
		r.Header.Set("Content-Type", "text/html; charset=utf-8")
		body := mustAsset("login.html")
		r.Body = io.NopCloser(bytes.NewReader(body))
		r.ContentLength = int64(len(body))
		return nil
	}
}

// chainModifiers runs the given response modifiers in order, stopping at the
// first error. This lets us compose auth-redirect and logout-button injection.
//
// NOTE: authRedirectModifier must run before injectLogoutButton — when the
// upstream returns 401/403, authRedirectModifier replaces the body with
// login.html (which contains no app fingerprints), so injectLogoutButton is
// a no-op. Running in the reverse order would inject into the error page before
// it gets replaced.
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
// to inject into its HTML. Adding support for a new proxied app is just a matter
// of appending an entry here (plus the script + its route).
type appDetector struct {
	fingerprint []byte // substring searched for in the response body
	scriptTag   []byte // <script src="..."> appended when the fingerprint matches
}

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

	// Detect the app by fingerprint; inject only on a match.
	if tag := matchAppScript(body); tag != nil {
		setResponseBody(r, injectScript(body, tag))
		return nil
	}
	// No match — restore body as-is.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

// injectScript inserts tag into body before the last </body> (or appends if none).
func injectScript(body, tag []byte) []byte {
	const closeBody = "</body>"
	if idx := bytes.LastIndex(body, []byte(closeBody)); idx >= 0 {
		return bytes.Join([][]byte{body[:idx], tag, body[idx:]}, nil)
	}
	return append(body, tag...)
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
// clearing Transfer-Encoding so clients see a consistent payload.
func setResponseBody(r *http.Response, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	r.Header.Del("Transfer-Encoding")
	// If the transport already decompressed, drop Content-Encoding so the
	// client doesn't try to decompress an already-decompressed body.
	if r.Uncompressed {
		r.Header.Del("Content-Encoding")
	}
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
	log.Printf("backend command started (pid %d)", cmd.Process.Pid)
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

// serveStaticAsset writes an embedded asset with the given content type.
func serveStaticAsset(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mustAsset(name))
}
