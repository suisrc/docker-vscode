package pkg

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Service Preparation — download, extract, fixup before starting the backend command
// =============================================================================

// ServiceState tracks lazy preparation of the service backend. Preparation is
// triggered on the first request to the service prefix (not at startup).
// `preparing` covers the whole lifecycle (from trigger to finish) so requests
// never proxy to the backend before it is fully ready (avoids 502).
// On failure, the state is reset so the next request retries.
type ServiceState struct {
	mu        sync.Mutex
	preparing bool   // true from first trigger until Finish() — gates proxying
	status    string // current status message while preparing; "" when idle
	done      bool   // preparation finished (successfully or not)
	err       error  // preparation error; nil on success
}

// Begin marks preparation as in progress (called once at trigger time).
// Returns false if preparation is already in progress or already succeeded.
func (s *ServiceState) Begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preparing || (s.done && s.err == nil) {
		return false // already preparing or already succeeded
	}
	s.preparing = true
	s.done = false
	s.err = nil
	s.status = ""
	return true
}

// SetPreparing updates the human-readable status while preparing.
func (s *ServiceState) SetPreparing(status string) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// Finish marks preparation done, storing err (nil on success).
func (s *ServiceState) Finish(err error) {
	s.mu.Lock()
	s.preparing = false
	s.status = ""
	s.done = true
	s.err = err
	s.mu.Unlock()
}

// Active reports whether preparation is in progress.
func (s *ServiceState) Active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preparing
}

// GetStatus returns the current status message (empty when idle).
func (s *ServiceState) GetStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// Result returns (done, err) — whether preparation finished and any error.
func (s *ServiceState) Result() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.err
}

// Reset clears the preparation state so the next request re-triggers
// download/extract/start. Used by /__restart.
func (s *ServiceState) Reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preparing = false
	s.done = false
	s.err = nil
	s.status = ""
}

// ServeLoadingPage writes the preparation loading page (loading.html), injecting
// the current status into the __STATUS__ placeholder.
func ServeLoadingPage(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusServiceUnavailable)
	msg := status
	if msg == "" {
		msg = "Preparing Application"
	}
	html := strings.Replace(string(MustAsset("loading.html")), "__STATUS__", msg, 1)
	_, _ = w.Write([]byte(html))
}

// prepareService downloads (if needed), extracts, and runs fixups for the backend
// service. It blocks until preparation is complete. srvState tracks status for the
// loading page.
//
// PrepareService downloads/extracts/fixes the service backend. sc is the
// resolved service config of one [service.<name>] section.
//
// Flow:
//  1. If healthz is set and the backend is already reachable → skip (already running)
//  2. If bin_home exists and is non-empty → skip deploy; run init_shell, plus
//     once_shell only when the {bin_home}/__once__ marker is absent
//  3. Resolve download URL (download has priority; otherwise download_info + field)
//  4. Follow redirects to get the package extension (cache filename suffix)
//  5. Download to {cache_dir}/version/{service}/{version}_{version_hash}.{ext}
//  6. Extract tarball to bin_home
//  7. Run once_shell (once per deployment, guarded by {bin_home}/__once__),
//     then init_shell (every startup)
//
// Returns (managed, error). managed=false means the backend is an external service
// already alive (detected via healthz) — kvs must NOT start/stop it or touch its
// socket. managed=true means kvs owns the backend lifecycle.
func PrepareService(sc *ServiceConfig, srvState *ServiceState) (bool, error) {
	// 0. If config init failed (e.g. version resolve error), fail fast so the
	// loading page shows the error instead of crashing the whole program.
	if sc.InitError != "" {
		return false, fmt.Errorf("%s", sc.InitError)
	}

	// 1. Check if backend is already running (external/system service).
	// healthz uses the "#@<code>" suffix form; a plain URL only requires
	// the backend to be reachable.
	alive := false
	if sc.Healthz != "" {
		alive = isServiceAlive(sc.Healthz)
	}
	if alive {
		log.Printf("[prepare:%s] backend already alive: %s (external service, not managed by kvs)", sc.Name, sc.Healthz)
		return false, nil
	}
	// If the probe/address is unix:// and the socket file exists but is dead,
	// it's a stale socket left by a previous kvs-managed crash — safe to remove
	// so the restarted backend can re-bind it. The healthz may carry the http
	// "#@<code>" suffix and a trailing "/" (healthz = "{ADDR}/#@200"), so the
	// path is normalized first.
	for _, u := range []string{sc.Healthz, sc.Address} {
		if target, ok := unixSocketPath(u); ok {
			if _, err := os.Stat(target); err == nil {
				log.Printf("[prepare:%s] removing stale unix socket: %s", sc.Name, target)
				_ = os.Remove(target)
			}
		}
	}

	// 2. If bin_home exists and is non-empty → already deployed, skip
	// download/extract and just run the scripts.
	if sc.BinHome != "" {
		if installed, _ := isServiceInstalled(sc.BinHome); installed {
			log.Printf("[prepare:%s] bin_home already exists: %s", sc.Name, sc.BinHome)
			return runShellScripts(sc, srvState)
		}
	}

	// 3. Resolve download URL.
	// Build an HTTP client that uses download_proxy if configured.
	dlClient := buildDownloadClient(sc.DownloadProxy)
	if sc.DownloadProxy != "" {
		log.Printf("[prepare:%s] using download proxy: %s", sc.Name, sc.DownloadProxy)
	}

	downloadURL := sc.Download
	if downloadURL == "" && sc.DownloadInfo != "" {
		// Fetch download_info JSON and extract URL field.
		var err error
		downloadURL, err = resolveDownloadInfo(dlClient, sc.DownloadInfo, sc.DownloadFieldURL)
		if err != nil {
			return false, fmt.Errorf("download_info: %w", err)
		}
	}
	if downloadURL == "" {
		// nothing to download → nothing to deploy (but kvs may still start command)
		return runShellScripts(sc, srvState)
	}
	if sc.BinHome == "" {
		return false, fmt.Errorf("bin_home is required when download is set")
	}

	// 4. Follow redirects to resolve the package extension (cache filename
	// suffix).
	ext, err := resolveExtFromURL(dlClient, downloadURL)
	if err != nil {
		return false, fmt.Errorf("resolve extension: %w", err)
	}
	sc.PackageExt = ext
	log.Printf("[prepare:%s] package ext: %s", sc.Name, ext)

	// 5. Compute cache path and download if not cached.
	verName := sc.Version
	if sc.VersionHash != "" {
		verName = sc.Version + "_" + sc.VersionHash
	}
	// Per-service subdirectory so same-version downloads of different
	// services never collide: {cache_dir}/version/{service}/{version}_{verhash}.{ext}
	cachePath := filepath.Join(Services().CacheDir(), "version", sc.Name, verName+"."+ext)

	if _, err := os.Stat(cachePath); err != nil {
		srvState.SetPreparing("Downloading Application: " + downloadURL)
		log.Printf("[prepare:%s] downloading: %s → %s", sc.Name, downloadURL, cachePath)
		// Progress callback updates the loading page status.
		onProgress := func(written, total int64) {
			if total > 0 {
				pct := written * 100 / total
				srvState.SetPreparing(fmt.Sprintf("Downloading Application: %s [%d%%]", downloadURL, pct))
			}
		}
		if err := downloadFile(dlClient, downloadURL, cachePath, onProgress); err != nil {
			return false, fmt.Errorf("download: %w", err)
		}
		log.Printf("[prepare:%s] download complete: %s", sc.Name, cachePath)
	} else {
		log.Printf("[prepare:%s] download cached: %s", sc.Name, cachePath)
	}

	// 6. Extract to bin_home.
	srvState.SetPreparing("Extracting Application: " + sc.BinHome)
	log.Printf("[prepare:%s] extracting: %s → %s", sc.Name, cachePath, sc.BinHome)
	if err := extractTarball(cachePath, sc.BinHome); err != nil {
		return false, fmt.Errorf("extract: %w", err)
	}
	log.Printf("[prepare:%s] extract complete: %s", sc.Name, sc.BinHome)

	return runShellScripts(sc, srvState)
}

// unixSocketPath extracts the socket file path from a unix:// address. The
// scheme is cut manually — url.Parse would eat the first path segment as a
// host ("unix://../../x" → host ".."). A trailing "#…" fragment (the http
// "#@<code>" suffix, which the healthz template appends regardless of scheme)
// and trailing "/" are stripped.
func unixSocketPath(addr string) (string, bool) {
	scheme, target, ok := strings.Cut(addr, "://")
	if !ok || scheme != "unix" {
		return "", false
	}
	if i := strings.LastIndex(target, "#"); i >= 0 {
		target = target[:i]
	}
	target = strings.TrimRight(target, "/")
	if target == "" {
		return "", false
	}
	return target, true
}

// isServiceAlive probes a backend URL. Any response proves reachability,
// unless the URL carries a "#@<code>" suffix, which requires exactly that
// status. unix probes dial the socket, file probes stat the path.
func isServiceAlive(probeURL string) bool {
	scheme, target, ok := strings.Cut(probeURL, "://")
	if !ok {
		return false
	}
	switch scheme {
	case "http", "https":
		// "#@<code>" suffix: require that exact status.
		required := 0
		if idx := strings.LastIndex(probeURL, "#@"); idx >= 0 {
			if n, err := strconv.Atoi(probeURL[idx+2:]); err == nil {
				required = n
				probeURL = probeURL[:idx]
			}
		}
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(probeURL)
		if err != nil {
			return false
		}
		resp.Body.Close()
		if required != 0 {
			return resp.StatusCode == required
		}
		return true // reachable is enough
	case "unix":
		sock, ok := unixSocketPath(probeURL)
		if !ok {
			return false
		}
		conn, err := net.Dial("unix", sock)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	case "file":
		_, err := os.Stat(target)
		return err == nil
	}
	return false
}

// onceMarkerName is the sentinel file written into bin_home after once_shell has
// run successfully for the current deployment. Its content is the RFC3339
// timestamp of that first successful run. Its presence makes later kvs startups
// (and /__restart cycles) skip once_shell — that is what turns once_shell into a
// true one-shot script. Delete the file to force once_shell to run again.
const onceMarkerName = "__once__"

// runShellScripts runs once_shell and init_shell in order and returns
// prepareService's result. once_shell runs only once per deployment, tracked by
// the {bin_home}/__once__ sentinel file; init_shell runs on every kvs startup.
// Both support "file://" prefix (script executed directly, shebang respected);
// any other value is run via "sh -c". Empty values are skipped.
func runShellScripts(sc *ServiceConfig, srvState *ServiceState) (bool, error) {
	// Run once_shell only when it has not already succeeded for this
	// deployment. The marker is written after a successful run only, so a
	// failed run leaves no marker and is retried on the next preparation cycle.
	if sc.OnceShell != "" {
		markerPath := ""
		if sc.BinHome != "" {
			markerPath = filepath.Join(sc.BinHome, onceMarkerName)
		}
		if ts, done := readOnceMarker(markerPath); done {
			log.Printf("[prepare:%s] once script already ran at %s, skipping: %s", sc.Name, ts, sc.OnceShell)
		} else {
			srvState.SetPreparing("Running once script: " + sc.OnceShell)
			log.Printf("[prepare:%s] running once script: %s", sc.Name, sc.OnceShell)
			if err := RunServiceStartup(sc.OnceShell); err != nil {
				return false, fmt.Errorf("once_shell: %w", err)
			}
			log.Printf("[prepare:%s] once script complete", sc.Name)
			// Record completion; the content is the run timestamp (RFC3339).
			if markerPath == "" {
				log.Printf("[prepare:%s] WARNING: bin_home not set, once_shell result cannot be recorded and may run again", sc.Name)
			} else if err := os.WriteFile(markerPath, []byte(time.Now().Format(time.RFC3339)+"\n"), 0o644); err != nil {
				log.Printf("[prepare:%s] WARNING: failed to write once marker %s: %v", sc.Name, markerPath, err)
			} else {
				log.Printf("[prepare:%s] once marker written: %s", sc.Name, markerPath)
			}
		}
	}

	// Run init_shell if set (every kvs startup).
	if sc.InitShell != "" {
		srvState.SetPreparing("Running startup script: " + sc.InitShell)
		log.Printf("[prepare:%s] running startup script: %s", sc.Name, sc.InitShell)
		if err := RunServiceStartup(sc.InitShell); err != nil {
			return false, fmt.Errorf("init_shell: %w", err)
		}
		log.Printf("[prepare:%s] startup script complete", sc.Name)
	}

	return true, nil
}

// readOnceMarker reports whether the once_shell sentinel file exists, returning
// its recorded timestamp. An empty path, or an unreadable file, yields
// ("", false) — meaning once_shell has not run yet.
func readOnceMarker(path string) (string, bool) {
	if path == "" {
		return "", false
	}
	b, err := os.ReadFile(path)
	if err != nil {
		return "", false
	}
	return strings.TrimSpace(string(b)), true
}

// buildDownloadClient creates an *http.Client for service downloads.
// If proxyURL is non-empty, it supports http://, https://, and socks5:// proxies.
// Uses http.DefaultTransport as the base (optimal default settings for CDN),
// only overriding the proxy. A generous timeout allows large tarballs.
func buildDownloadClient(proxyURL string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			log.Printf("[prepare] invalid download_proxy %q: %v, ignoring", proxyURL, err)
		} else {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       30 * time.Minute,
		CheckRedirect: redirectLimit(10),
	}
}

// resolveDownloadInfo fetches a JSON API and extracts the URL field.
func resolveDownloadInfo(client *http.Client, infoURL, field string) (string, error) {
	resp, err := client.Get(infoURL)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result map[string]any
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode JSON: %w", err)
	}
	if field == "" {
		field = "url"
	}
	urlStr, _ := result[field].(string)
	if urlStr == "" {
		return "", fmt.Errorf("field %q not found or empty in response", field)
	}
	return urlStr, nil
}

// isServiceInstalled reports whether dir exists, is a directory, and is non-empty.
func isServiceInstalled(dir string) (ok bool, reason string) {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false, ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, "dir exists but is not readable"
	}
	if len(entries) == 0 {
		return false, "dir exists but is empty"
	}
	return true, ""
}

// resolveExtFromURL follows redirects on urlStr and extracts the file extension
// from the final URL's basename (e.g. .tar.gz).
//
// First tries to extract the extension from the URL path directly (no network
// request). If the URL path has no recognizable extension (e.g. a short
// redirect URL), falls back to a HEAD request with a short timeout.
func resolveExtFromURL(client *http.Client, urlStr string) (string, error) {
	// Fast path: extract extension from the URL path without a network request.
	if ext := extractExt(urlStr); ext != "tar.gz" || strings.Contains(filepath.Base(urlStr), ".tar.gz") {
		log.Printf("[prepare] ext from URL path: %s", ext)
		return ext, nil
	}

	// Slow path: HEAD request to follow redirects and get the final URL.
	// Use a short timeout so a slow proxy/server doesn't block for minutes.
	headClient := *client
	headClient.Timeout = 30 * time.Second
	resp, err := headClient.Head(urlStr)
	if err != nil {
		// HEAD failed — default to tar.gz rather than failing the whole prepare.
		log.Printf("[prepare] HEAD %s failed: %v, defaulting ext to tar.gz", urlStr, err)
		return "tar.gz", nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[prepare] HEAD %s returned %d (using final URL for extension)", urlStr, resp.StatusCode)
	}
	finalURL := ""
	if resp.Request != nil {
		finalURL = resp.Request.URL.String()
	}
	if finalURL == "" {
		finalURL = urlStr
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
	if dotIdx < 0 || strings.HasSuffix(base[dotIdx+1:], ".tar.gz") {
		log.Printf("[prepare] no extension found in %q, defaulting to tar.gz", base)
		return "tar.gz" // sensible default for compressed tarballs
	}
	return base[dotIdx+1:]
}

// writerFunc adapts a function to the io.Writer interface.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// downloadFile downloads urlStr to destPath using an atomic temp+rename strategy.
// onProgress (if non-nil) is called periodically with bytes written and total.
func downloadFile(client *http.Client, urlStr, destPath string, onProgress func(written, total int64)) error {
	resp, err := client.Get(urlStr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
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

	// Use io.TeeReader + a counting writer for progress, with a 64KB buffer.
	var written int64
	var lastUpdate time.Time
	counter := writerFunc(func(p []byte) (int, error) {
		written += int64(len(p))
		if onProgress != nil && time.Since(lastUpdate) >= time.Second {
			onProgress(written, total)
			lastUpdate = time.Now()
		}
		return tmp.Write(p)
	})
	buf := make([]byte, 64*1024)
	_, err = io.CopyBuffer(counter, resp.Body, buf)
	if err != nil {
		tmp.Close()
		return err
	}
	// Final progress update.
	if onProgress != nil {
		onProgress(written, total)
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

// runServiceStartup executes a shell entry (once_shell/init_shell/stop_shell).
// If it starts with "file://", the referenced file is made executable and run
// directly (the kernel reads its #! shebang). Otherwise, the string is run via
// sh -c.
func RunServiceStartup(cmd string) error {
	var c *exec.Cmd
	if filePath, ok := strings.CutPrefix(cmd, "file://"); ok {
		if err := os.Chmod(filePath, 0o755); err != nil {
			return fmt.Errorf("chmod startup script file: %w", err)
		}
		c = exec.Command(filePath)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// NormalizeProxyPath ensures p starts and ends with '/'.
func NormalizeProxyPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "" // empty = cache proxy disabled
	}
	if p == "/" {
		return "/__cache/" // bare "/" is ambiguous, use default
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p = p + "/"
	}
	return p
}

// =============================================================================
// Service Manager — lifecycle state for every hosted (svc://) service
// =============================================================================

// managedService couples one service config with its lazy-preparation state
// and the kvs-owned backend subprocess. One instance per svc:// service.
type managedService struct {
	cfg   *ServiceConfig
	state ServiceState
	proc  Process
}

// ServiceManager tracks every named service of the current config. A config
// reload (via /__restart) only replaces the cfg pointers — preparation state
// and subprocesses survive.
type ServiceManager struct {
	mu       sync.Mutex
	svcs     map[string]*managedService
	cacheDir string // download cache root ({cache_dir}/version/…)
}

var gServices = &ServiceManager{svcs: map[string]*managedService{}}

// Services returns the process-wide service manager.
func Services() *ServiceManager { return gServices }

// SetCacheDir records the download cache root used by PrepareService.
func (m *ServiceManager) SetCacheDir(dir string) {
	m.mu.Lock()
	m.cacheDir = dir
	m.mu.Unlock()
}

// CacheDir returns the download cache root (empty → /cache, matching cc~).
func (m *ServiceManager) CacheDir() string {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cacheDir == "" {
		return defaultCacheDir
	}
	return m.cacheDir
}

// Register syncs the manager with a freshly loaded config: missing services
// are added, existing ones only get their cfg pointer refreshed (state and
// subprocess are kept). Configs are shared pointers — callers must treat
// them as read-only after registration.
func (m *ServiceManager) Register(cfgs map[string]*ServiceConfig) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for name, sc := range cfgs {
		if ms, ok := m.svcs[name]; ok {
			ms.cfg = sc
			continue
		}
		m.svcs[name] = &managedService{cfg: sc}
	}
}

// Get returns the current config of a registered service (nil if unknown).
func (m *ServiceManager) Get(name string) *ServiceConfig {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ms, ok := m.svcs[name]; ok {
		return ms.cfg
	}
	return nil
}

// State returns the preparation state of a service (nil if unknown).
func (m *ServiceManager) State(name string) *ServiceState {
	m.mu.Lock()
	defer m.mu.Unlock()
	if ms, ok := m.svcs[name]; ok {
		return &ms.state
	}
	return nil
}

// StartProc launches the service command in its own process group. The
// command (and the optional agent bridge args) were already expanded at
// config-load time; the subprocess additionally inherits the whole kvs
// environment, including every reverse-assigned KVS_SVC_<NAME>_* variable.
// No-op when the command is empty or the previous child is still running.
func (m *ServiceManager) StartProc(sc *ServiceConfig) error {
	m.mu.Lock()
	ms, ok := m.svcs[sc.Name]
	m.mu.Unlock()
	if !ok {
		return fmt.Errorf("service %q not registered", sc.Name)
	}
	if sc.Command == "" {
		return fmt.Errorf("service %q has no command", sc.Name)
	}
	if ms.proc.Running() {
		return fmt.Errorf("service %q process is running", sc.Name)
	}
	return ms.proc.Start(sc.Name, sc.Command+sc.VscAgentArgs)
}

// StopOne terminates a kvs-managed service subprocess, runs stop_shell and
// cleans up a unix address socket. External services (never started by kvs,
// proc not running) are not touched. Returns whether a managed subprocess
// was stopped.
func (m *ServiceManager) StopOne(name string, runStopShell bool) bool {
	m.mu.Lock()
	ms, ok := m.svcs[name]
	m.mu.Unlock()
	if !ok {
		return false
	}
	stopped := false
	if ms.proc.Running() {
		ms.proc.Stop()
		stopped = true
	}
	sc := ms.cfg
	if stopped && runStopShell && sc != nil && sc.StopShell != "" {
		log.Printf("[service %s] running stop_shell: %s", name, sc.StopShell)
		if err := RunServiceStartup(sc.StopShell); err != nil {
			log.Printf("[service %s] stop_shell error: %v", name, err)
		}
	}
	// Clean up the address socket of a kvs-managed unix backend.
	if stopped && sc != nil {
		if scheme, target, ok := strings.Cut(sc.Address, "://"); ok && scheme == "unix" {
			_ = os.Remove(target)
		}
	}
	return stopped
}

// StopAll terminates every kvs-managed service subprocess (with stop_shell).
// External services detected via check are never started by kvs and are left
// untouched.
func (m *ServiceManager) StopAll() {
	m.mu.Lock()
	names := make([]string, 0, len(m.svcs))
	for name := range m.svcs {
		names = append(names, name)
	}
	m.mu.Unlock()
	for _, name := range names {
		m.StopOne(name, true)
	}
}
