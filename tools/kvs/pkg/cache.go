package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cache — handles proxy_path endpoints
// =============================================================================

// allowedCacheSchemes restricts the cache to web schemes to mitigate SSRF.
var allowedCacheSchemes = map[string]bool{"http": true, "https": true}

// cacheTransport is a shared transport for the cache with a response
// header timeout so a slow/hung upstream cannot hold connections indefinitely.
var cacheTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   10,
}

// =============================================================================
// Proxy Cache
// =============================================================================

// --- cache cache init (sync.Once, disk-only cache) ---

var (
	cacheOnce       sync.Once
	cacheRoot       string // resolved cache root at first cache request; route only registered when non-empty
	cacheOverride   string // set from SvcCacheDir in main()
	proxyPathPrefix string // normalized proxy_path prefix (e.g. "/__cache/"), set in main()
)

// SetCacheConfig configures the cache root and the proxy_path route prefix
// from main(). Call before the HTTP routes are registered.
func SetCacheConfig(cacheDir, proxyPath string) {
	cacheOverride = filepath.Join(cacheDir, "ccproxy")
	proxyPathPrefix = NormalizeProxyPath(proxyPath)
}

// CachePathPrefix reports the registered proxy_path prefix (empty = disabled).
func CachePathPrefix() string { return proxyPathPrefix }

// initCache resolves the cache root from the cache_dir config. The proxy_path route
// is only registered when cache_dir is set, so the root is always non-empty
// here; this just stores it for the cache handlers.
func initCache() {
	cacheRoot = cacheOverride
	log.Printf("[cache] cache root: %s", cacheRoot)
}

// redirectLimit is a CheckRedirect policy shared by cache and download clients.
func redirectLimit(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
}

// cacheClient is used for manual upstream fetches when caching.
var cacheClient = &http.Client{
	Transport:     cacheTransport,
	Timeout:       5 * time.Minute,
	CheckRedirect: redirectLimit(10),
}

// cacheHeaderWhitelist lists headers preserved in cache metadata.
var cacheHeaderWhitelist = map[string]bool{
	"content-type":        true,
	"content-length":      true,
	"content-encoding":    true,
	"cache-control":       true,
	"etag":                true,
	"last-modified":       true,
	"content-disposition": true,
}

// cacheMeta holds the cached HTTP status and a subset of response headers.
type cacheMeta struct {
	Status  int
	Headers map[string][]string
}

// cachePaths returns the body and meta file paths for a cache entry.
//
// Cache layout mirrors the URL structure:
//
//	{KVS_SVC_PROXYPATH}/{scheme}:{host}/path/to/file.js       → body
//	{KVS_SVC_PROXYPATH}/{scheme}:{host}/path/to/file.js_.json  → metadata
//
// The rest path is cleaned and stripped of leading "/" to stay within
// the cache root.  Requests for the root path use "__index" as filename.
func cachePaths(scheme, host, rest string) (bodyPath, metaPath string) {
	// Clean and make relative to prevent directory traversal.
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(cacheRoot, scheme+":"+host, p)
	return base, base + "_.json"
}

// readCacheMeta reads and parses a cache metadata file from disk.
func readCacheMeta(path string) (*cacheMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m cacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeCacheMeta atomically writes cache metadata as JSON.
func writeCacheMeta(path string, m *cacheMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o644)
}

// =============================================================================
// Cache Handler
// =============================================================================

// handleCache proxies {proxy_path}/[...] → target URL.
//
// Format:  {proxy_path}/[cc~]{scheme}:{host}[/path][?query]
//
//	cc~              — optional cache marker: check cache, write on MISS
//	{scheme}:        — optional scheme (http, https); defaults to https
//	{host}           — upstream host[:port]
//
// HandleCache serves the proxy cache route: {prefix}/{scheme}:{host}/path.
func HandleCache(w http.ResponseWriter, r *http.Request) {
	cacheOnce.Do(initCache)

	p := strings.TrimPrefix(r.URL.Path, proxyPathPrefix)
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
	scheme, host, rest, err := parseCachePath(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	targetURL := buildTargetURL(scheme, host, rest, r.URL.RawQuery)

	log.Printf("[cache] %s %s → %s (cacheable=%v)", r.Method, r.URL.Path, targetURL, cacheable)

	if !cacheable {
		// Plain passthrough — no cache.
		handlePassThroughCache(w, r, targetURL)
		return
	}

	// Try cache lookup; on MISS fetch, stream and store.
	bodyPath, metaPath := cachePaths(scheme, host, rest)
	if serveFromCache(w, bodyPath, metaPath, targetURL) {
		return
	}
	handleCachedCache(w, r, targetURL, bodyPath, metaPath)
}

// parseCachePath extracts scheme, host and rest from the cache path segment.
// Format: scheme:host/rest or host/rest.  Returns an error string for bad input.
func parseCachePath(p string) (scheme, host, rest string, err error) {
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
	if !allowedCacheSchemes[scheme] {
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

// serveFromCache tries to serve a response from cache. Returns true on HIT.
func serveFromCache(w http.ResponseWriter, bodyPath, metaPath, targetURL string) bool {
	meta, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return false
	}
	log.Printf("[cache] cache HIT  %s ← %s (%d bytes)", targetURL, bodyPath, len(body))
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

// handleCachedCache fetches the upstream, streams the response to both the
// client and a cache file, and writes cache metadata on success.
func handleCachedCache(w http.ResponseWriter, r *http.Request, targetURL, bodyPath, metaPath string) {
	log.Printf("[cache] cache MISS (will cache) %s", targetURL)

	// cacheClient already enforces a 5m timeout; reuse the request context
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

	resp, err := cacheClient.Do(req)
	if err != nil {
		log.Printf("[cache] fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Only 2xx (200-299) and 404 responses are cached; all other status codes
	// (3xx, 5xx, etc.) stream through without caching so transient errors are
	// not stuck in cache.
	if !((resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == 404) {
		log.Printf("[cache] upstream returned %d, not caching", resp.StatusCode)
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
	meta := &cacheMeta{
		Status:  resp.StatusCode,
		Headers: make(map[string][]string),
	}
	for k, vs := range resp.Header {
		if cacheHeaderWhitelist[strings.ToLower(k)] {
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
		log.Printf("[cache] mkdir cache FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Stream to client + temp file simultaneously.
	tmp, err := os.CreateTemp(filepath.Dir(bodyPath), ".tmp-*")
	if err != nil {
		log.Printf("[cache] create temp FAIL: %v", err)
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
		log.Printf("[cache] copy error: %v", copyErr)
		return
	}

	// Atomic rename temp → final body.
	if err := os.Rename(tmpName, bodyPath); err != nil {
		log.Printf("[cache] rename cache FAIL: %s → %v", bodyPath, err)
		return
	}
	committed = true

	// Write metadata.
	if err := writeCacheMeta(metaPath, meta); err != nil {
		log.Printf("[cache] write meta FAIL: %s → %v", metaPath, err)
	} else {
		log.Printf("[cache] CACHED %s → %s (%d bytes)", targetURL, bodyPath, n)
	}
}

// handlePassThroughCache proxies the request to the upstream without caching.
func handlePassThroughCache(w http.ResponseWriter, r *http.Request, targetURL string) {
	log.Printf("[cache] passthrough %s", targetURL)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "bad target URL", http.StatusBadRequest)
		return
	}
	// Forward original headers; net/http strips hop-by-hop on the wire.
	req.Header = r.Header.Clone()

	resp, err := cacheClient.Do(req)
	if err != nil {
		log.Printf("[cache] fetch error: %v", err)
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
