package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// =============================================================================
// Cache — cc~ 缓存核心：缓存根与元数据、两条入口（/__cache/{scheme}:{host}/…
// 与路由前缀 cc~ 的 HandleCCBackend）、命中直出与 MISS 写入、透传。
//
// 规则（都来自环境变量，启动时安装、配置重载后重扫）分别在同包另几个文件：
//   - cachesed.go  KVS_CC_SED_<名称>  正文改写（once/each 计划写入元数据）
//   - cacherec.go  KVS_CC_REC_<名称>  命中即短路（不发上游、不写缓存）
//   - cachezip.go  正文处理：gzip/brotli 编解码、原文备份、替换落盘与响应头
//   - cachehdr.go  响应头：上游头复制、[cache_headers] 附加头与 OPTIONS 预检
// 两者共用本文件末尾的匹配小工具（hostMatch/globMatch/targetHost/splitList…）。
//
// cache_sed 总逻辑（写入与命中都以此为准）:
//  0. 规则来自环境变量 KVS_CC_SED_<名称> = old|new|file[,files…][|host[,hosts…]]
//     （一个变量一条规则，不再有 cache_sed 固定配置）。
//     缓存键（域名与全路径）统一小写，见 cachePaths。
//  1. 文件名未匹配规则，或来源主机（代理后端的 host）不在规则的 host 列表内 →
//     透传，数据不做任何处理，怎么来怎么走。文件名列表按 , 分隔（如
//     __index,*.js,*.json）：**无扩展名的请求**（目录首页 /、/0.42.0/ 以及
//     …/0.42.0/extension 这类）统一存为 __index（见 cacheIndexName）。
//     host 列表比对的是**内容来源**（vscode.dev 这类被代理主机），与访问域名无关；
//     请求 Host 只用于展开替换内容里的 >host<（浏览器可见的地址）。
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
	cacheOverride   string // set from CacheDir in main()
	cachePathPrefix string // normalized cache_path prefix (e.g. "/__cache/"), set in main()
)

// SetCacheConfig configures the cache root, the cache_path route prefix and
// the extra response headers ([cache_headers], see cachehdr.go) from main().
// Call before the HTTP routes are registered.
func SetCacheConfig(cacheDir, cachePath string, headers map[string]string) {
	cacheOverride = filepath.Join(cacheDir, "ccproxy")
	cachePathPrefix = NormalizeCachePath(cachePath)
	cacheHeaders = headers
}

// CachePathPrefix reports the registered cache_path prefix (empty = disabled).
func CachePathPrefix() string { return cachePathPrefix }

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

// cacheIndexName is the cache entry base name of a request without a file
// extension — the directory homepage ("/", "/0.42.0/") and endpoints such as
// "/GitHub/remotehub/0.42.0/extension". cache_sed rules target them all with
// the single token "__index" (see cachePaths).
const cacheIndexName = "__index"

// cachePaths returns the body and meta file paths for a cache entry.
// Cache layout mirrors the URL structure (rest is cleaned and stripped of
// its leading "/" to stay within the root):
//
//	{root}/{scheme}:{host}/path/to/file.js        → body
//	{root}/{scheme}:{host}/path/to/file.js_.json  → metadata
//
// A path whose base name has no extension — not only "/" — is stored as
// cacheIndexName in its own directory, so every 无后缀资源 shares one rule
// token (`__index`) instead of each name needing its own rule:
//
//	/                 → __index
//	/0.42.0/          → 0.42.0/__index     (trailing "/" = directory request)
//	/0.42.0/extension → 0.42.0/__index     (extensionless endpoint)
//
// The cache KEY is lower-cased as a whole ({scheme}:{host} and the full path):
// the same resource requested as `/GitHub/remotehub/...` and
// `/github/RemoteHub/...` is one entry, so letter-case differences in the
// domain or the path can never split the cache or cause a spurious MISS.
// This folds the DISK KEY ONLY — callers pass the original host/rest to
// buildTargetURL, so the upstream request keeps its exact letter case (paths
// are case-sensitive on many origins).
func cachePaths(root, scheme, host, rest string) (bodyPath, metaPath string) {
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	switch {
	case p == "" || p == ".":
		p = cacheIndexName
	case strings.HasSuffix(rest, "/"):
		// Directory request: Clean dropped the trailing "/", so p is the
		// directory — its entry is that directory's cacheIndexName. Checked
		// before the extension test because a directory name may contain a
		// dot (e.g. /0.42.0/).
		p = filepath.Join(p, cacheIndexName)
	case !strings.Contains(filepath.Base(p), "."):
		p = filepath.Join(filepath.Dir(p), cacheIndexName)
	}
	base := filepath.Join(root, strings.ToLower(scheme+":"+host), strings.ToLower(p))
	return base, base + "_.json"
}

// readCacheMeta reads and parses a cache metadata file from disk, with an
// in-memory cache: hits serve from parsed state, validated by (size, mtime,
// inode). The inode is part of the key because cache writes go through
// "temp file + rename" — rename always creates a new inode, so a rewrite
// within the same 1-second mtime granularity still invalidates.
func readCacheMeta(path string) (*cacheMeta, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return nil, err
	}
	ino := uint64(0)
	if st, ok := fi.Sys().(*syscall.Stat_t); ok {
		ino = st.Ino
	}

	ccMetaCache.mu.Lock()
	e, ok := ccMetaCache.m[path]
	ccMetaCache.mu.Unlock()
	if ok && e.size == fi.Size() && e.modTime.Equal(fi.ModTime()) && e.ino == ino {
		m := e.meta
		return &m, nil
	}

	data, err := io.ReadAll(f)
	if err != nil {
		return nil, err
	}
	var m cacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}

	ccMetaCache.mu.Lock()
	if len(ccMetaCache.m) >= ccMetaCacheMax {
		ccMetaCache.m = map[string]ccMetaCacheEntry{} // 整表丢弃：命中集随版本/热点自然重建
	}
	ccMetaCache.m[path] = ccMetaCacheEntry{size: fi.Size(), modTime: fi.ModTime(), ino: ino, meta: m}
	ccMetaCache.mu.Unlock()
	return &m, nil
}

// ccMetaCacheEntry is one parsed metadata file kept in memory.
type ccMetaCacheEntry struct {
	size    int64
	modTime time.Time
	ino     uint64
	meta    cacheMeta
}

// ccMetaCacheMax caps the in-memory metadata cache (per-entry cost is small:
// headers + sed plan; 4096 entries ≪ 10MB).
const ccMetaCacheMax = 4096

var ccMetaCache = struct {
	mu sync.Mutex
	m  map[string]ccMetaCacheEntry
}{m: map[string]ccMetaCacheEntry{}}

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
// {cache_path}/[cc~]{scheme}:{host}[/path][?query] — cc~ marks cacheable
// requests (GET only), {scheme}: defaults to https.
func HandleCache(w http.ResponseWriter, r *http.Request) {
	cacheOnce.Do(initCache)
	applyCacheResponseHeaders(w, r)
	if cachePreflight(w, r) {
		return
	}

	p := strings.TrimPrefix(r.URL.Path, cachePathPrefix)
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

	// cache_rec 短路规则：命中就不发上游、直接回规则指定的响应。
	if rule := recMatchingRule(host, rest); rule != nil {
		serveCacheRec(w, r, rule, host, rest)
		return
	}

	log.Printf("[cache] %s %s → %s (cacheable=%v)", r.Method, r.URL.Path, targetURL, cacheable)

	if !cacheable {
		// Plain passthrough — no cache.
		handlePassThroughCache(w, r, targetURL)
		return
	}

	// Try cache lookup; on MISS fetch, stream and store.
	// 注意：host/rest 原样进 targetURL（上游大小写敏感），只有 cachePaths
	// 内部的磁盘键会小写——不要把这里的小写化扩散到 targetURL。
	bodyPath, metaPath := cachePaths(cacheRoot, scheme, host, rest)
	if serveFromCache(w, r, bodyPath, metaPath, targetURL, r.Host) {
		return
	}
	handleCachedCache(w, r, targetURL, bodyPath, metaPath)
}

// buildTargetURL constructs the full upstream URL with a strings.Builder.
// host may carry a base path (a cc~ route's b.Target, e.g. "example.com/base/"
// — the trailing "/" of a root-mounted route), while rest is the request
// sub-path, which has no leading slash except the root default "/". Collapse
// that one boundary: otherwise a root request becomes "https://host//" and the
// upstream really receives "GET // HTTP/1.1" on the wire.
func buildTargetURL(scheme, host, rest, rawQuery string) string {
	if strings.HasSuffix(host, "/") && strings.HasPrefix(rest, "/") {
		rest = rest[1:]
	}
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
// The stored body is transformed per the persisted sedMeta plan (absent = no
// rewrite was recorded for this entry, so the stored bytes are final).
//
// Body delivery depends on the persisted plan:
//
//   - mode=each → the body must be rewritten per request, so it is read into
//     memory (the only case that still does);
//   - everything else (no plan / none / once) → the stored bytes are final and
//     are served straight from the file: http.ServeContent sends them via
//     kernel sendfile (zero-copy, no heap allocation) and adds Range /
//     If-Modified-Since support. Non-200 cached statuses (404 is cached)
//     fall back to the in-memory path so the status code survives.
func serveFromCache(w http.ResponseWriter, r *http.Request, bodyPath, metaPath, targetURL, host string) bool {
	meta, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	needsEach := meta.Sed != nil && meta.Sed.Mode == sedModeEach && len(meta.Sed.Each) > 0

	// 响应头要等正文确认可读之后才写：正文缺失（元数据在、文件被清）时要
	// 干净地回退 MISS，不能留下 X-Cache: HIT 或元数据首部让 MISS 路径重复叠加。
	hit := func() {
		w.Header().Set("X-Cache", "HIT")
		copyHeadersExcept(w.Header(), meta.Headers, cacheHeadersFor(r.URL.Path))
	}

	if needsEach {
		body, err := os.ReadFile(bodyPath)
		if err != nil {
			return false
		}
		out, outGzip := sedServeFromMeta(meta, body, host)
		log.Printf("[cache] cache HIT  %s ← %s (%d bytes, sed=each)", targetURL, bodyPath, len(out))
		hit()
		setCachedFraming(w, meta, outGzip)
		// 正文已在手上，直接给出 Content-Length（旧实现走 chunked）。
		w.Header().Del("Content-Length")
		w.Header().Set("Content-Length", strconv.Itoa(len(out)))
		w.WriteHeader(meta.Status)
		_, _ = w.Write(out)
		kvsStats.cacheHits.Add(1)
		return true
	}

	if meta.Status != http.StatusOK {
		// 非 200（404 也会入缓存）：ServeContent 只会回 200/206/304，退回
		// 内存路径保住状态码语义（这类响应通常很小）。
		body, err := os.ReadFile(bodyPath)
		if err != nil {
			return false
		}
		log.Printf("[cache] cache HIT  %s ← %s (%d bytes, status=%d)", targetURL, bodyPath, len(body), meta.Status)
		hit()
		setCachedFraming(w, meta, isGzipMeta(meta))
		w.Header().Del("Content-Length")
		w.WriteHeader(meta.Status)
		_, _ = w.Write(body)
		kvsStats.cacheHits.Add(1)
		return true
	}

	f, err := os.Open(bodyPath)
	if err != nil {
		return false
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		return false
	}
	log.Printf("[cache] cache HIT  %s ← %s (%d bytes, file direct)", targetURL, bodyPath, fi.Size())
	hit()
	setCachedFraming(w, meta, isGzipMeta(meta))
	w.Header().Del("Content-Length") // ServeContent 按文件大小自行给出
	// modtime 用缓存文件的写入时间：If-Modified-Since/If-Range 针对"缓存条目
	// 是否变化"作答（内容时间语义等同，只是不再用上游 Last-Modified）。
	http.ServeContent(w, r, filepath.Base(bodyPath), fi.ModTime(), f)
	kvsStats.cacheHits.Add(1)
	return true
}

// setCachedFraming reproduces the HIT Content-Encoding semantics (same rules
// as the write path): gzip bodies declare gzip, br & other passthrough
// encodings keep their original claim, plain bodies declare nothing.
func setCachedFraming(w http.ResponseWriter, meta *cacheMeta, outGzip bool) {
	w.Header().Del("Content-Encoding")
	if outGzip {
		w.Header().Set("Content-Encoding", "gzip")
	} else if enc := cacheContentEncoding(meta); enc != "" {
		w.Header().Set("Content-Encoding", enc)
	}
}

// handleCachedCache fetches the upstream on a MISS, mirrors it to the
// client and writes the cache entry on success.
func handleCachedCache(w http.ResponseWriter, r *http.Request, targetURL, bodyPath, metaPath string) {
	log.Printf("[cache] cache MISS (will cache) %s", targetURL)
	kvsStats.cacheMisses.Add(1)

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
	// Rule 2 only: entries hit by a cache_sed rule (file AND 来源主机 match) need
	// a pipeline-controllable encoding (gzip, decodable in-process; br via
	// brotli CLI). Force gzip upstream for those. Unmatched entries (rule 1:
	// pass through untouched) keep the client's original Accept-Encoding so
	// upstream responses — including br — are stored and served exactly as-is.
	upHost := targetHost(targetURL)
	if name := filepath.Base(bodyPath); len(sedMatchingRules(name, upHost)) > 0 {
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
		copyHeadersExcept(w.Header(), resp.Header, cacheHeadersFor(r.URL.Path))
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
	copyHeadersExcept(w.Header(), meta.Headers, cacheHeadersFor(r.URL.Path))
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
		// 规则按「来源主机」匹配；最后一个参数是请求 Host，只用于展开 >host<。
		store, send, splan, sendGzip := sedPipeline(sedMatchingRules(name, upHost), name, data, cacheContentEncoding(meta), r.Host)
		if splan.Mode == sedModeOnce {
			backupOriginalOnce(bodyPath, name, data)
		}
		// 响应头按计划替换（mode=none 时保持上游原样，正文没动）。
		applyEntryFraming(w, meta, splan, sendGzip)
		if splan.Mode != sedModeNone {
			meta.Sed = &splan
		}
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(send)
		if err := storeEntry(bodyPath, metaPath, store, meta); err != nil {
			log.Printf("[cache] cache_sed store FAIL: %v", err)
			return
		}
		log.Printf("[cache] CACHED %s → %s (store %d, send %d, sed=%s)", targetURL, bodyPath, len(store), len(send), splan.Mode)
		return
	}

	// Plain path: stream to the client while writing the temp file, then commit.
	n, err := streamToTemp(w, resp.Body, bodyPath, resp.StatusCode)
	if err != nil {
		log.Printf("[cache] store FAIL: %v", err)
		return
	}
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

	copyHeadersExcept(w.Header(), resp.Header, cacheHeadersFor(r.URL.Path))
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
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
	// ws/wss 只是 http/https 的别名：缓存客户端用 http.Client 抓上游，不做映射会得到
	// `unsupported protocol scheme "ws"`（与反代 Director 同一处理）。
	scheme := b.Scheme
	switch scheme {
	case "ws":
		scheme = "http"
	case "wss":
		scheme = "https"
	}
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
		applyCacheResponseHeaders(w, r)
		if cachePreflight(w, r) {
			return
		}

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

		// cache_rec 短路规则：命中就不发上游、直接回规则指定的响应。
		// 主机取目标 URL 的 host（b.Target 可能带基础路径/尾斜杠，见 targetHost）。
		if rule := recMatchingRule(targetHost(targetURL), rest); rule != nil {
			serveCacheRec(w, r, rule, targetHost(targetURL), rest)
			return
		}

		log.Printf("[cache] cc~ %s %s → %s (cache=%s)", r.Method, r.URL.Path, targetURL, cacheHost)

		if r.Method != http.MethodGet {
			handlePassThroughCache(w, r, targetURL)
			return
		}

		// 磁盘键小写（见 cachePaths），上游请求用 upstreamHost/rest 原大小写。
		bodyPath, metaPath := cachePaths(cacheBase, scheme, cacheHost, rest)
		if serveFromCache(w, r, bodyPath, metaPath, targetURL, r.Host) {
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
