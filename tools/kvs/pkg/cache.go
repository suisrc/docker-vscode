package pkg

import (
	"bytes"
	"compress/gzip"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
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

// SetCacheSed parses the cache_sed rules ("file|old|new||...") and enables
// cache content rewriting for cc~ cached responses. Empty rules disable it.
func SetCacheSed(rules string) { parseCacheSed(rules) }

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
// Sed is the cache_sed processing plan persisted at write time; serveFromCache
// relies on it alone to turn the stored bytes into a response body.
type cacheMeta struct {
	Status  int
	Headers map[string][]string
	Sed     *sedMeta `json:"sed,omitempty"`
}

// cachePaths returns the body and meta file paths for a cache entry.
//
// Cache layout mirrors the URL structure:
//
//	{root}/{scheme}:{host}/path/to/file.js       → body
//	{root}/{scheme}:{host}/path/to/file.js_.json  → metadata
//
// The rest path is cleaned and stripped of leading "/" to stay within
// the cache root.  Requests for the root path use "__index" as filename.
func cachePaths(root, scheme, host, rest string) (bodyPath, metaPath string) {
	// Clean and make relative to prevent directory traversal.
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(root, scheme+":"+host, p)
	return base, base + "_.json"
}

// cachePathsVersioned is cachePaths with an extra version segment after the
// host: {root}/{scheme}:{host}/{version}/path (body + "_.json" sidecar).
// version is expected to be a single sanitized path segment.
func cachePathsVersioned(root, scheme, host, version, rest string) (bodyPath, metaPath string) {
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(root, scheme+":"+host, version, p)
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
	bodyPath, metaPath := cachePaths(cacheRoot, scheme, host, rest)
	if serveFromCache(w, bodyPath, metaPath, targetURL, r.Host) {
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
// The stored body is transformed per the persisted sedMeta plan (legacy
// entries without one are served as-is).
func serveFromCache(w http.ResponseWriter, bodyPath, metaPath, targetURL string, host string) bool {
	meta, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return false
	}
	out, outGzip, err := sedServe(meta.Sed, body, host)
	if err != nil {
		log.Printf("[cache] cache_sed serve FAIL: %s → %v", bodyPath, err)
		out, outGzip = body, isGzipMeta(meta)
	}
	if meta.Sed == nil {
		// Legacy entry (no persisted plan): the body is the upstream
		// original — keep its framing from the stored headers.
		outGzip = isGzipMeta(meta)
	}
	log.Printf("[cache] cache HIT  %s ← %s (%d bytes)", targetURL, bodyPath, len(out))
	w.Header().Set("X-Cache", "HIT")
	for k, vs := range meta.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	// Framing fixup AFTER meta headers are copied, so stored headers are
	// overridden (never duplicated) with the framing of the body actually
	// sent: mode=each stores plain payload and responds per SrcGzip, so the
	// metadata's original Content-Encoding must not leak through.
	if outGzip {
		w.Header().Set("Content-Encoding", "gzip")
	} else {
		w.Header().Del("Content-Encoding")
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(meta.Status)
	_, _ = w.Write(out)
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

	// Ensure cache directory exists.
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		log.Printf("[cache] mkdir cache FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// cache_sed path: classify the rewrite once at write time (see
	// sedProcess) and persist the mode in the metadata; serve time relies
	// on it alone.
	if sedRules != nil {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[cache] read body FAIL: %v", err)
			return
		}
		name := filepath.Base(bodyPath)
		srcGzip := isGzipMeta(meta)
		out, splan, err := sedProcess(sedMatchingRules(name), name, data, srcGzip, r.Host)
		if err != nil {
			log.Printf("[cache] cache_sed FAIL: %s → %v", bodyPath, err)
			out, splan = data, sedMeta{Mode: sedModeNone, Gzipped: srcGzip}
		}
		if splan.Mode == sedModeOnce {
			// Keep the original bytes once: <file>_.bak1 (never overwritten).
			bakPath := bodyPath + "_.bak1"
			if _, err := os.Stat(bakPath); os.IsNotExist(err) {
				if err := atomicWriteFile(bakPath, data, 0o644); err != nil {
					log.Printf("[cache] cache_sed backup FAIL: %v", err)
				} else {
					log.Printf("[cache] cache_sed: backup %s → %s", name, filepath.Base(bakPath))
				}
			}
			// Content-Length no longer matches the rewritten body.
			delete(meta.Headers, "Content-Length")
			w.Header().Del("Content-Length")
		}
		if splan.Mode == sedModeEach {
			// Stored bytes are plain payload; drop the gzip framing claim.
			delete(meta.Headers, "Content-Encoding")
			w.Header().Del("Content-Encoding")
			delete(meta.Headers, "Content-Length")
			w.Header().Del("Content-Length")
		}
		meta.Sed = &splan
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(out)
		if err := atomicWriteFile(bodyPath, out, 0o644); err != nil {
			log.Printf("[cache] write body FAIL: %s → %v", bodyPath, err)
			return
		}
		if err := writeCacheMeta(metaPath, meta); err != nil {
			log.Printf("[cache] write meta FAIL: %s → %v", metaPath, err)
		} else {
			log.Printf("[cache] CACHED %s → %s (%d bytes, sed=%s)", targetURL, bodyPath, len(out), splan.Mode)
		}
		return
	}

	// Plain path: stream to client + temp file simultaneously.
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

// =============================================================================
// cc~ Backends — static file proxy caching per [proxies] route
// =============================================================================

// defaultCacheDir is the cache root fallback when cache_dir is not
// configured.
const defaultCacheDir = "/cache"

// cacheBase holds the cc~ backend cache root: {cache_dir:-/cache}/ccproxy.
// This is the same directory used by the /__cache/ proxy cache
// ({cache_dir}/ccproxy), so cc~ backends and /__cache/cc~ requests share
// the same on-disk cache content.
var cacheBase string

// SetCacheDir configures the cc~ backend cache root from the cache_dir
// config value (empty → /cache). Call before routes are built.
func SetCacheDir(cacheDir string) {
	base := cacheDir
	if base == "" {
		base = defaultCacheDir
	}
	cacheBase = filepath.Join(base, "ccproxy")
	log.Printf("[cache] cc~ backend cache root: %s", cacheBase)
}

// HandleCCBackend returns the caching proxy handler for a cc~ marked backend
// (http/https only). Proxied responses are cached at
// {cache_dir:-/cache}/ccproxy/{scheme}:{host}/path, mirroring the URL
// structure (body + "_.json" metadata sidecar), reusing the same cache
// machinery as the /__cache/ proxy route:
//   - only GET 2xx/404 responses are cached
//   - cache HITs are served directly from disk
//   - non-GET requests pass through uncached
//
// KVS_CC_VER_REFERER (a query key name, e.g. "app_version"): when set AND the
// request's Referer URL carries that query key, the key's value is inserted
// as a version path segment after the host:
//
//	{cache_dir}/ccproxy/{scheme}:{host}/{version}/{path}
//
// Missing config, missing Referer, or missing key → plain layout (ignored).
func HandleCCBackend(b Backend) http.Handler {
	scheme := b.Scheme
	host := b.Target
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cacheOnce.Do(initCache)

		// Sub-path of the backend: strip the routing prefix (full path for
		// regex backends, where the prefix is the pattern itself).
		rest := r.URL.Path
		if !b.IsRegex {
			rest = strings.TrimPrefix(rest, b.Prefix)
		}
		if rest == "" {
			rest = "/"
		}

		targetURL := buildTargetURL(scheme, host, rest, r.URL.RawQuery)
		log.Printf("[cache] cc~ %s %s → %s", r.Method, r.URL.Path, targetURL)

		if r.Method != http.MethodGet {
			handlePassThroughCache(w, r, targetURL)
			return
		}

		// Version segment from the Referer query (KVS_CC_VER_REFERER):
		// cache layout becomes {cache_dir}/ccproxy/{scheme}:{domain}/{version}/{target-path}/{path}
		// — the version goes right after the domain, before the backend
		// target's own path prefix (e.g. "remote/v4").
		var bodyPath, metaPath string
		if v := refererVersion(r); v != "" {
			log.Printf("[cache] cc~ version segment: %s", v)
			// host may carry a path (e.g. "zcode.z.ai/remote/v4") — split
			// domain from path so the version lands between them.
			domain, targetPath := host, ""
			if i := strings.Index(host, "/"); i >= 0 {
				domain, targetPath = host[:i], host[i:]
			}
			bodyPath, metaPath = cachePathsVersioned(cacheBase, scheme, domain, v, targetPath+rest)
		} else {
			bodyPath, metaPath = cachePaths(cacheBase, scheme, host, rest)
		}
		if serveFromCache(w, bodyPath, metaPath, targetURL, r.Host) {
			return
		}
		handleCachedCache(w, r, targetURL, bodyPath, metaPath)
	})
}

// refererVersion extracts the version segment from the request's Referer.
// KVS_CC_VER_REFERER names a query key (e.g. "app_version"); when the key is
// present in the Referer URL query, its value is sanitized into a single
// safe path segment and returned. Returns "" when disabled or absent.
func refererVersion(r *http.Request) string {
	key := os.Getenv("KVS_CC_VER_REFERER")
	if key == "" || r == nil {
		return ""
	}
	ref := r.Header.Get("Referer")
	if ref == "" {
		return ""
	}
	u, err := url.Parse(ref)
	if err != nil {
		return ""
	}
	v := u.Query().Get(key)
	if v == "" {
		return ""
	}
	// Sanitize into one safe path segment (letters, digits, dot, dash, _).
	v = strings.Map(func(c rune) rune {
		switch {
		case c >= 'a' && c <= 'z', c >= 'A' && c <= 'Z', c >= '0' && c <= '9':
			return c
		case c == '.', c == '-', c == '_':
			return c
		default:
			return '-'
		}
	}, v)
	return v
}

// =============================================================================
// cache_sed — rewrite cc~ cached response bodies on write
// =============================================================================

// sedMode is the cache_sed processing mode decided once at cache-write time
// and persisted in the metadata, so serve time never re-derives it.
type sedMode string

const (
	sedModeNone sedMode = "none" // no rule changed the body
	sedModeOnce sedMode = "once" // host-independent rewrite, applied and stored once
	sedModeEach sedMode = "each" // new value contains ">host<": original stored, rewritten per request
)

// sedMeta is the per-file rewrite plan persisted in the cache metadata.
//   - Mode: how the stored body was processed (see sedMode).
//   - Gzipped: whether the STORED bytes are gzip framed.
//   - SrcGzip: whether the ORIGINAL upstream body was gzip framed. For
//     mode=each it drives the response framing: gzip origins are
//     re-compressed per request, plain origins stay plain.
//   - Each: the request-dependent rules (old/new, new keeps the ">host<"
//     placeholder) that actually matched this file, persisted as an array —
//     one file may carry several each rules; serve time uses this list
//     instead of the runtime config.
type sedMeta struct {
	Mode    sedMode       `json:"mode"`
	Gzipped bool          `json:"gzipped"`
	SrcGzip bool          `json:"src_gzip,omitempty"`
	Each    []sedEachRule `json:"each,omitempty"`
}

// sedEachRule is one persisted old→new pair (new may contain ">host<").
type sedEachRule struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// cacheSedRule is one parsed rewrite group: "<file>|<old>|<new>".
// file supports a single '*' wildcard (prefix*/suffix match); without '*'
// the cache file base name must equal the rule's file field exactly.
type cacheSedRule struct {
	file string // match pattern against the cache file base name
	old  string // literal content to find
	new  string // replacement content
}

// sedRules holds the parsed cache_sed groups; nil = feature disabled.
var sedRules []cacheSedRule

// parseCacheSed parses "file|old|new||file2|old2|new2||..." into sedRules.
// '|' separates fields inside a group; '||' separates groups. A group with
// fewer than 3 fields is skipped with a warning. Empty input disables the
// feature (sedRules = nil).
func parseCacheSed(spec string) {
	spec = strings.TrimSpace(spec)
	if spec == "" {
		sedRules = nil
		return
	}
	var rules []cacheSedRule
	for _, part := range strings.Split(spec, "||") {
		fields := strings.Split(part, "|")
		if len(fields) < 3 {
			log.Printf("[cache] cache_sed: skip invalid group %q (want file|old|new)", part)
			continue
		}
		file := strings.TrimSpace(fields[0])
		if file == "" {
			log.Printf("[cache] cache_sed: skip group with empty file pattern")
			continue
		}
		rules = append(rules, cacheSedRule{file: file, old: fields[1], new: fields[2]})
	}
	if len(rules) == 0 {
		sedRules = nil
		log.Printf("[cache] cache_sed: no valid rules, disabled")
		return
	}
	sedRules = rules
	log.Printf("[cache] cache_sed: %d rule(s) enabled", len(rules))
}

// sedFileMatch reports whether the rule matches the cache file base name.
// A '*' in the pattern splits it into prefix+suffix parts; the name must
// start with the prefix and end with the suffix. Without '*' exact match.
func sedFileMatch(pattern, name string) bool {
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	prefix, suffix, _ := strings.Cut(pattern, "*")
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// isGzipMeta reports whether the cache metadata records a gzip body.
func isGzipMeta(meta *cacheMeta) bool {
	for _, v := range meta.Headers["Content-Encoding"] {
		if strings.EqualFold(strings.TrimSpace(v), "gzip") {
			return true
		}
	}
	return false
}

// sedMatchingRules returns the runtime rules whose file pattern matches name.
func sedMatchingRules(name string) []cacheSedRule {
	var out []cacheSedRule
	for _, rule := range sedRules {
		if sedFileMatch(rule.file, name) {
			out = append(out, rule)
		}
	}
	return out
}

// sedApply expands ">host<" in replacements and substitutes old→new for
// rules whose old value occurs in the body.
func sedApply(rules []cacheSedRule, payload []byte, host string) ([]byte, bool) {
	s := string(payload)
	changed := false
	for _, rule := range rules {
		if !strings.Contains(s, rule.old) {
			continue
		}
		newVal := strings.ReplaceAll(rule.new, ">host<", host)
		s = strings.ReplaceAll(s, rule.old, newVal)
		changed = true
		log.Printf("[cache] cache_sed: apply %q → %q", rule.old, newVal)
	}
	if !changed {
		return payload, false
	}
	return []byte(s), true
}

// gzipEncode compresses payload into a gzip framed byte slice.
func gzipEncode(payload []byte) ([]byte, error) {
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	if _, err := zw.Write(payload); err != nil {
		return nil, fmt.Errorf("gzip write: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("gzip close: %w", err)
	}
	return buf.Bytes(), nil
}

// gzipDecode decompresses a gzip framed body into its raw payload.
func gzipDecode(data []byte) ([]byte, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("gzip open: %w", err)
	}
	payload, err := io.ReadAll(zr)
	zr.Close()
	if err != nil {
		return nil, fmt.Errorf("gzip read: %w", err)
	}
	return payload, nil
}

// sedProcess runs the cache-write pipeline and decides how the body is
// stored and later served. Rules split into once (host-independent, new has
// no ">host<") and each (new contains ">host<"):
//
//  1. once rules are applied first and their result is baked into the
//     stored body;
//  2. the each rules whose old value still occurs in that payload are
//     persisted (as an array — a file may carry several); if any exist the
//     mode is each and the payload is stored PLAIN (gzip framing stripped)
//     so per-request rewrites + re-compression work;
//  3. otherwise mode is once (rewritten body stored, gzip re-applied) or
//     none (body stored as fetched).
//
// Returns the bytes to store and the plan to persist.
func sedProcess(rules []cacheSedRule, name string, data []byte, srcGzip bool, host string) (store []byte, sm sedMeta, err error) {
	if len(rules) == 0 {
		return data, sedMeta{Mode: sedModeNone, Gzipped: srcGzip}, nil
	}
	payload := data
	if srcGzip {
		if payload, err = gzipDecode(data); err != nil {
			return nil, sedMeta{}, err
		}
	}

	var onceRules, eachRules []cacheSedRule
	for _, r := range rules {
		if strings.Contains(r.new, ">host<") {
			eachRules = append(eachRules, r)
		} else {
			onceRules = append(onceRules, r)
		}
	}

	// Host-independent rules first; the result is baked into the body.
	payload, onceChanged := sedApply(onceRules, payload, host)

	// Persist every each rule that still matches (post-once payload).
	var matched []sedEachRule
	for _, r := range eachRules {
		if strings.Contains(string(payload), r.old) {
			matched = append(matched, sedEachRule{Old: r.old, New: r.new})
		}
	}
	if len(matched) > 0 {
		log.Printf("[cache] cache_sed: %s: mode=each (%d rule(s), once=%v), stored plain (%d bytes)", name, len(matched), onceChanged, len(payload))
		return payload, sedMeta{Mode: sedModeEach, SrcGzip: srcGzip, Each: matched}, nil
	}
	if onceChanged {
		stored := payload
		if srcGzip {
			if stored, err = gzipEncode(payload); err != nil {
				return nil, sedMeta{}, err
			}
		}
		log.Printf("[cache] cache_sed: %s: mode=once, rewritten (%d → %d bytes)", name, len(data), len(stored))
		return stored, sedMeta{Mode: sedModeOnce, Gzipped: srcGzip}, nil
	}
	// Rules matched the file name but nothing in the body.
	return data, sedMeta{Mode: sedModeNone, Gzipped: srcGzip}, nil
}

// sedServe transforms a stored cache body into the response body for the
// current request, driven solely by the persisted sedMeta:
//
//   - mode=none / once: stored body is final — returned unchanged.
//   - mode=each: apply the persisted Each rules with the current request
//     Host; the response framing follows SrcGzip (gzip origins are
//     re-compressed per request, plain origins stay plain — compression
//     is never forced).
//
// Returns the bytes to send and whether the response is gzip framed.
func sedServe(sm *sedMeta, body []byte, host string) ([]byte, bool, error) {
	if sm == nil || sm.Mode != sedModeEach || len(sm.Each) == 0 {
		return body, sm != nil && sm.Gzipped, nil
	}
	rules := make([]cacheSedRule, len(sm.Each))
	for i, r := range sm.Each {
		rules[i] = cacheSedRule{old: r.Old, new: r.New}
	}
	out, _ := sedApply(rules, body, host)
	if sm.SrcGzip {
		gz, err := gzipEncode(out)
		if err != nil {
			return nil, false, err
		}
		return gz, true, nil
	}
	// Plain source: never compress — echo the framing the origin used.
	return out, false, nil
}
