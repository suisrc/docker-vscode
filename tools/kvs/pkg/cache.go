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
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// =============================================================================
// Cache — handles proxy_path endpoints
//
// cache_sed 总逻辑（写入与命中都以此为准）:
//  1. 文件名未匹配规则 → 透传，数据不做任何处理，怎么来怎么走。
//  2. 文件名匹配 → 按上游 Content-Encoding 类型解压：gzip 用 gzip 解压， br 用 brotli CLI 解压，其他编码不处理（等同透传）。
//  3. 解压后的内容未命中替换串 → 跳过，处理同 1（原始字节原样存/回）。
//  4. 命中且只有 once 规则 → 替换后压缩为 gzip 存储，直接回给前端。
//  5. once 之外还有 each 匹配 → 原文（plain，不再 gzip）存储；每次请求替换后，上游是 gzip/br 的统一用 gzip 压缩回给前端，否则原文返回。
// =============================================================================

// allowedCacheSchemes restricts the cache to web schemes (SSRF mitigation).
var allowedCacheSchemes = map[string]bool{"http": true, "https": true}

// cacheTransport: shared transport with a response header timeout so a slow
// upstream cannot hold connections indefinitely.
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

// initCache stores the cache root (route only registered when cache_dir
// is set, so cacheOverride is never empty here).
func initCache() {
	cacheRoot = cacheOverride
	log.Printf("[cache] cache root: %s", cacheRoot)
}

// redirectLimit caps upstream redirects (shared by cache clients).
func redirectLimit(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
}

// cacheClient fetches upstream responses when caching.
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
// Cache layout mirrors the URL structure (rest is cleaned and stripped of
// its leading "/" to stay within the root; root requests use "__index"):
//
//	{root}/{scheme}:{host}/path/to/file.js        → body
//	{root}/{scheme}:{host}/path/to/file.js_.json  → metadata
func cachePaths(root, scheme, host, rest string) (bodyPath, metaPath string) {
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(root, scheme+":"+host, p)
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

// HandleCache serves the proxy cache route:
// {proxy_path}/[cc~]{scheme}:{host}[/path][?query] — cc~ marks cacheable
// requests (GET only), {scheme}: defaults to https.
func HandleCache(w http.ResponseWriter, r *http.Request) {
	cacheOnce.Do(initCache)

	p := strings.TrimPrefix(r.URL.Path, proxyPathPrefix)
	if p == "" || p == "/" {
		http.Error(w, "missing domain/path", http.StatusBadRequest)
		return
	}

	cacheable := strings.HasPrefix(p, "cc~") && r.Method == http.MethodGet
	if cacheable {
		p = p[3:]
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
	// Framing fixup AFTER meta headers are copied — always clear first, then
	// set once, so the header is never duplicated (a repeated Content-Encoding
	// makes browsers reject the whole response):
	//   mode=each → rewrite per request, gzip framed when SrcGzip (rule 5);
	//   otherwise → stored bytes are final (none = upstream original with
	//                its recorded framing header; once = our gzip).
	out, outGzip := sedServeFromMeta(meta, body, host)
	log.Printf("[cache] cache HIT  %s ← %s (%d bytes)", targetURL, bodyPath, len(out))
	w.Header().Set("X-Cache", "HIT")
	for k, vs := range meta.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Del("Content-Encoding")
	if outGzip {
		w.Header().Set("Content-Encoding", "gzip")
	} else if enc := cacheContentEncoding(meta); enc != "" {
		// Non-gzip encoded stored body (e.g. br passthrough): keep the
		// original encoding claim — the stored bytes are NOT plain and must
		// never be re-framed or delivered bare.
		w.Header().Set("Content-Encoding", enc)
	}
	w.Header().Del("Content-Length")
	w.WriteHeader(meta.Status)
	_, _ = w.Write(out)
	return true
}

// copyHeaders adds every header from src into dst (used to mirror cached
// or upstream headers onto a response).
func copyHeaders(dst http.Header, src map[string][]string) {
	for k, vs := range src {
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// handleCachedCache fetches the upstream on a MISS, mirrors it to the
// client and writes the cache entry on success.
func handleCachedCache(w http.ResponseWriter, r *http.Request, targetURL, bodyPath, metaPath string) {
	log.Printf("[cache] cache MISS (will cache) %s", targetURL)

	// Reuse the request context so client disconnects cancel the upstream
	// fetch (cacheClient caps the total at 5m).
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
	// Rule 2 only: files matching a cache_sed rule need a pipeline-
	// controllable encoding (gzip, decodable in-process; br via brotli CLI).
	// Force gzip upstream for those. Unmatched files (rule 1: pass through
	// untouched) keep the client's original Accept-Encoding so upstream
	// responses — including br — are stored and served exactly as-is.
	if name := filepath.Base(bodyPath); len(sedMatchingRules(name)) > 0 {
		req.Header.Set("Accept-Encoding", "gzip")
	}

	resp, err := cacheClient.Do(req)
	if err != nil {
		log.Printf("[cache] fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Only 2xx and 404 responses are cached; everything else streams
	// through uncached so transient errors never stick.
	cacheableStatus := (resp.StatusCode >= 200 && resp.StatusCode < 300) || resp.StatusCode == 404
	if !cacheableStatus {
		log.Printf("[cache] upstream returned %d, not caching", resp.StatusCode)
		copyHeaders(w.Header(), resp.Header)
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Collect whitelisted headers for the cache metadata and the response.
	meta := &cacheMeta{Status: resp.StatusCode, Headers: map[string][]string{}}
	for k, vs := range resp.Header {
		if cacheHeaderWhitelist[strings.ToLower(k)] {
			meta.Headers[k] = vs
		}
	}
	copyHeaders(w.Header(), meta.Headers)
	w.Header().Set("X-Cache", "MISS")

	// Ensure cache directory exists.
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		log.Printf("[cache] mkdir cache FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// cache_sed path: sedPipeline decides once what is stored, sent and
	// persisted (see its rules 1-5 comment).
	if sedRules != nil {
		data, err := io.ReadAll(resp.Body)
		if err != nil {
			log.Printf("[cache] read body FAIL: %v", err)
			return
		}
		name := filepath.Base(bodyPath)
		store, send, splan, sendGzip := sedPipeline(sedMatchingRules(name), name, data, cacheContentEncoding(meta), r.Host)
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
		}
		// Response framing per the plan — header driven, bytes never sniffed.
		switch splan.Mode {
		case sedModeOnce: // stored & served gzip; Content-Length no longer matches
			meta.Headers["Content-Encoding"] = []string{"gzip"}
			delete(meta.Headers, "Content-Length")
			w.Header().Set("Content-Encoding", "gzip")
			w.Header().Del("Content-Length")
		case sedModeEach: // stored plain — the original framing claim must go
			delete(meta.Headers, "Content-Encoding")
			delete(meta.Headers, "Content-Length")
			w.Header().Del("Content-Length")
			if sendGzip {
				w.Header().Set("Content-Encoding", "gzip")
			} else {
				w.Header().Del("Content-Encoding")
			}
		}
		// Passthrough (rules 1/2/3): upstream headers stay exactly as recorded
		// — body untouched, so Content-Length still matches.
		if splan.Mode != sedModeNone {
			meta.Sed = &splan
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(send)
		if err := atomicWriteFile(bodyPath, store, 0o644); err != nil {
			log.Printf("[cache] write body FAIL: %s → %v", bodyPath, err)
			return
		}
		if err := writeCacheMeta(metaPath, meta); err != nil {
			log.Printf("[cache] write meta FAIL: %s → %v", metaPath, err)
		} else {
			log.Printf("[cache] CACHED %s → %s (store %d, send %d, sed=%s)", targetURL, bodyPath, len(store), len(send), splan.Mode)
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

// defaultCacheDir is the cache root fallback when cache_dir is not configured.
const defaultCacheDir = "/cache"

// cacheBase is the cc~ backend cache root — the same {cache_dir}/ccproxy
// directory the /__cache/ route uses, so both share on-disk content.
var cacheBase string

// SetCacheDir configures the cc~ backend cache root (empty → /cache).
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
// {cache_dir:-/cache}/ccproxy/{scheme}:{host}/path — same layout and
// machinery as the /__cache/ proxy route (only GET 2xx/404 are cached;
// HITs serve from disk; non-GET passes through uncached).
func HandleCCBackend(b Backend) http.Handler {
	scheme := b.Scheme
	host := b.Target
	// Version-marker target: "[v=key]" anywhere in the target (e.g.
	// "zcode.z.ai/[v=app_version]remote/v4"). The marker is removed from
	// the upstream URL; for the cache path it is replaced per request with
	// the value of the named query key (or "0.0.0" when absent), so each
	// app version gets its own cache namespace.
	upstreamHost := host
	var verPrefix, verKey, verSuffix string
	if i := strings.Index(host, "[v="); i >= 0 {
		if j := strings.IndexByte(host[i:], ']'); j > 3 { // j > 3 → non-empty key
			verPrefix, verKey, verSuffix = host[:i], host[i+3:i+j], host[i+j+1:]
			upstreamHost = verPrefix + verSuffix
		}
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		cacheOnce.Do(initCache)

		// Sub-path of the backend: strip the routing prefix (full path for
		// regex backends, where the prefix is the pattern itself).
		rest := r.URL.Path
		if !b.IsRegex {
			rest = strings.TrimPrefix(rest, b.Source)
		}
		if rest == "" {
			rest = "/"
		}

		// Cache namespace host: with a [v=key] marker, embed the resolved
		// version so different versions cache apart. The version segment
		// gets its own "/" (the marker carried none).
		cacheHost := upstreamHost
		if verKey != "" {
			cacheHost = verPrefix + GetReqestParam(r, verKey, "0.0.0") + "/" + verSuffix
		}

		targetURL := buildTargetURL(scheme, upstreamHost, rest, r.URL.RawQuery)
		log.Printf("[cache] cc~ %s %s → %s (cache=%s)", r.Method, r.URL.Path, targetURL, cacheHost)

		if r.Method != http.MethodGet {
			handlePassThroughCache(w, r, targetURL)
			return
		}

		bodyPath, metaPath := cachePaths(cacheBase, scheme, cacheHost, rest)
		if serveFromCache(w, bodyPath, metaPath, targetURL, r.Host) {
			return
		}
		handleCachedCache(w, r, targetURL, bodyPath, metaPath)
	})
}

func GetReqestParam(r *http.Request, key, def string) string {
	if v := r.URL.Query().Get(key); v != "" {
		return v
	}
	if ref := r.Header.Get("referer"); ref != "" {
		if u, err := url.Parse(ref); err == nil {
			if v := u.Query().Get(key); v != "" {
				return v
			}
		}
	}
	return def
}

// =============================================================================
// cache_sed — rewrite cc~ cached response bodies on write
// =============================================================================

// sedMode is the cache_sed processing mode decided once at cache-write time
// and persisted in the metadata, so serve time never re-derives it.
type sedMode string

const (
	sedModeNone sedMode = "none" // rules did not touch the body — upstream bytes stored & served as-is (rules 1/2/3)
	sedModeOnce sedMode = "once" // once-only rewrite, stored gzip, served as-is (rule 4)
	sedModeEach sedMode = "each" // once baked in + each rewrites per request; stored plain (rule 5)
)

// sedMeta is the per-file rewrite plan persisted in the cache metadata.
//   - Mode: how the stored body was processed (see sedMode).
//   - SrcGzip: only for mode=each — the upstream origin was gzip/br
//     encoded, so every response is re-compressed as gzip after the
//     per-request rewrite (rule 5); plain origins stay plain.
//   - Each: the request-dependent rules (old→new, new keeps the ">host<"
//     placeholder) that matched this file at write time; serve time uses
//     this list instead of the runtime config.
type sedMeta struct {
	Mode    sedMode       `json:"mode"`
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
	return strings.EqualFold(cacheContentEncoding(meta), "gzip")
}

// cacheContentEncoding returns the first Content-Encoding value recorded in
// the cache metadata (lowercased, trimmed; empty = plain body).
func cacheContentEncoding(meta *cacheMeta) string {
	for _, v := range meta.Headers["Content-Encoding"] {
		if v = strings.ToLower(strings.TrimSpace(v)); v != "" && v != "identity" {
			return v
		}
	}
	return ""
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

// brotliOnce guards the one-time brotli binary detection.
var brotliOnce sync.Once

// brotliFound caches the result of the brotli binary lookup.
var brotliFound bool

// brotliAvailable reports whether the `brotli` CLI is installed. Detected
// once; a missing binary disables br decoding for the process lifetime.
func brotliAvailable() bool {
	brotliOnce.Do(func() {
		if _, err := exec.LookPath("brotli"); err == nil {
			brotliFound = true
			return
		}
		log.Printf("[cache] 未安装 brotli 软件，不支持 br 解压，按原样返回")
	})
	return brotliFound
}

// brotliDecompress decompresses a brotli framed body via the `brotli` CLI
// (no Go br dependency): -d decompress, -c write to stdout.
func brotliDecompress(compressed []byte) ([]byte, error) {
	cmd := exec.Command("brotli", "-d", "-c")
	cmd.Stdin = bytes.NewReader(compressed)
	var out bytes.Buffer
	cmd.Stdout = &out
	if err := cmd.Run(); err != nil {
		return nil, fmt.Errorf("brotli 解压失败: %w", err)
	}
	return out.Bytes(), nil
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

// sedPipeline implements the cache_sed rules 1-5 for a cache MISS in one
// pass. It returns the bytes to STORE on disk, the bytes to SEND to the
// client, the plan to persist in the metadata (Mode=none ⇒ Sed stays nil)
// and whether the response is gzip framed.
//
//	rules 1/3, unsupported encoding, decode/encode failure:
//		passthrough — store=send=original bytes, headers untouched;
//	rule 4 (once only): store=send=gzip(rewritten) — HIT serves as-is;
//	rule 5 (once + each): store=rewritten plain; send is the each-rewritten
//		copy, gzip framed when the origin was gzip/br (HIT replays this).
func sedPipeline(rules []cacheSedRule, name string, data []byte, srcEnc string, host string) (store, send []byte, plan sedMeta, sendGzip bool) {
	// Rule 1: file name not matched — pass through untouched.
	if len(rules) == 0 {
		return data, data, sedMeta{Mode: sedModeNone}, false
	}

	// Rule 2: decode per upstream Content-Encoding. Unsupported encodings
	// and decode failures degrade to passthrough.
	var payload []byte
	encoded := false // origin was gzip/br — rule 5 response framing
	switch srcEnc {
	case "":
		payload = data
	case "gzip":
		p, err := gzipDecode(data)
		if err != nil {
			log.Printf("[cache] cache_sed: %s gzip decode FAIL: %v (passthrough)", name, err)
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		payload, encoded = p, true
	case "br":
		if !brotliAvailable() {
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		p, err := brotliDecompress(data)
		if err != nil {
			log.Printf("[cache] cache_sed: %s brotli decode FAIL: %v (passthrough)", name, err)
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		payload, encoded = p, true
	default:
		// Other encodings are not handled — equivalent to passthrough.
		log.Printf("[cache] cache_sed: %s upstream encoding %q unsupported (passthrough)", name, srcEnc)
		return data, data, sedMeta{Mode: sedModeNone}, false
	}

	// Split rules into once (host-independent) and each (">host<" placeholder).
	var onceRules, eachRules []cacheSedRule
	for _, r := range rules {
		if strings.Contains(r.new, ">host<") {
			eachRules = append(eachRules, r)
		} else {
			onceRules = append(onceRules, r)
		}
	}

	// Rule 3: no rule content occurs in the payload — passthrough.
	s := string(payload)
	anyHit := false
	for _, r := range onceRules {
		if strings.Contains(s, r.old) {
			anyHit = true
			break
		}
	}
	if !anyHit {
		for _, r := range eachRules {
			if strings.Contains(s, r.old) {
				anyHit = true
				break
			}
		}
	}
	if !anyHit {
		log.Printf("[cache] cache_sed: %s: no rule content matched (passthrough)", name)
		return data, data, sedMeta{Mode: sedModeNone}, false
	}

	// Rule 4: apply ALL once rules first — the result is baked into the
	// stored body for both the once and each paths.
	payload, onceChanged := sedApply(onceRules, payload, host)

	// Rule 5 selection: each rules still matching the post-once payload.
	var matched []sedEachRule
	for _, r := range eachRules {
		if strings.Contains(string(payload), r.old) {
			matched = append(matched, sedEachRule{Old: r.old, New: r.new})
		}
	}
	if len(matched) == 0 {
		// Rule 4: once only — compress to gzip, store and serve as-is.
		gz, err := gzipEncode(payload)
		if err != nil {
			log.Printf("[cache] cache_sed: %s gzip encode FAIL: %v (passthrough)", name, err)
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		log.Printf("[cache] cache_sed: %s: mode=once, stored gzip (%d → %d bytes)", name, len(data), len(gz))
		return gz, gz, sedMeta{Mode: sedModeOnce}, true
	}

	// Rule 5: store plain (once result baked in); the response is rewritten
	// per request and gzip framed when the origin was gzip/br.
	log.Printf("[cache] cache_sed: %s: mode=each (%d rule(s), once=%v), stored plain (%d bytes)", name, len(matched), onceChanged, len(payload))
	plan = sedMeta{Mode: sedModeEach, SrcGzip: encoded, Each: matched}
	send = sedApplyEach(matched, payload, host)
	if encoded {
		gz, gerr := gzipEncode(send)
		if gerr == nil {
			return payload, gz, plan, true
		}
		log.Printf("[cache] cache_sed: %s gzip encode FAIL: %v (serving plain)", name, gerr)
		plan.SrcGzip = false // keep MISS and HIT framing in agreement
	}
	return payload, send, plan, false
}

// sedApplyEach applies persisted each rules (old→new, ">host<" expanded
// with the current request Host) against a plain stored body.
func sedApplyEach(each []sedEachRule, payload []byte, host string) []byte {
	rules := make([]cacheSedRule, len(each))
	for i, r := range each {
		rules[i] = cacheSedRule{old: r.Old, new: r.New}
	}
	out, _ := sedApply(rules, payload, host)
	return out
}

// sedServeFromMeta builds the HIT response body from the stored bytes,
// driven solely by the persisted plan and the metadata headers (never by
// sniffing bytes):
//
//	mode=each  → apply the persisted Each rules with the current request
//	             Host; gzip framed when SrcGzip (rule 5 replay);
//	otherwise  → stored bytes are final; framing is whatever the metadata
//	             headers record (none = upstream original, once = our gzip).
func sedServeFromMeta(meta *cacheMeta, body []byte, host string) ([]byte, bool) {
	sm := meta.Sed
	if sm == nil || sm.Mode != sedModeEach || len(sm.Each) == 0 {
		return body, isGzipMeta(meta)
	}
	out := sedApplyEach(sm.Each, body, host)
	if sm.SrcGzip {
		gz, err := gzipEncode(out)
		if err != nil {
			log.Printf("[cache] cache_sed gzip encode FAIL: %v (serving plain)", err)
			return out, false
		}
		return gz, true
	}
	// Plain origin: never compress — echo the framing the origin used.
	return out, false
}
