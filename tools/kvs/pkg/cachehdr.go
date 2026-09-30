package pkg

// cachehdr.go — 缓存响应的头部处理：把上游头复制到响应，以及 [cache_headers]
// 段声明的附加头（CORS 等）与 OPTIONS 预检应答。
//
// 作用域：所有 [cache_headers] 只对 cache_path 前缀下的请求生效（cacheHeadersFor
// 判定）——挂在 "/" 的路由级 cc~ 服务于同源页面，上游头必须原样保留。设置点在
// 任何写入之前，所以 HIT / MISS / 透传 / 短路 / 错误响应都带同一组头。

import (
	"bytes"
	"compress/gzip"
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
)

// cacheHeaders 是 [cache_headers] 段解析结果（set by SetCacheConfig in cache.go）。
// 键为小写（ini 解析时统一折叠），写回响应时由 net/http 规范化为标准大小写。
var cacheHeaders map[string]string

// cacheHeadersFor returns the configured [cache_headers] when path is served by
// the cache_path prefix, else nil. The CORS allowance is deliberately scoped to
// the cache proxy route: a cc~ route mounted at "/" serves same-origin pages
// and must keep the upstream headers untouched.
func cacheHeadersFor(path string) map[string]string {
	if cachePathPrefix == "" || len(cacheHeaders) == 0 || !strings.HasPrefix(path, cachePathPrefix) {
		return nil
	}
	return cacheHeaders
}

// headerNameIn reports whether name (case-insensitive) is a key of hdrs.
func headerNameIn(hdrs map[string]string, name string) bool {
	for k := range hdrs {
		if strings.EqualFold(k, name) {
			return true
		}
	}
	return false
}

// applyCacheResponseHeaders writes the configured [cache_headers] onto the
// response (empty set outside the cache_path prefix). Call it before writing
// anything so HIT / MISS / passthrough / error responses all carry them.
func applyCacheResponseHeaders(w http.ResponseWriter, r *http.Request) {
	for k, v := range cacheHeadersFor(r.URL.Path) {
		w.Header().Set(k, v)
	}
}

// cachePreflight answers a CORS preflight (OPTIONS) on the cache_path prefix
// without touching the cache or the upstream: 204 with the configured
// [cache_headers]. Returns false when the request is not a preflight, so the
// caller continues normally. The method/header allowance is whatever the
// [cache_headers] section declares (e.g. Access-Control-Allow-Methods).
func cachePreflight(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodOptions || cachePathPrefix == "" || !strings.HasPrefix(r.URL.Path, cachePathPrefix) {
		return false
	}
	log.Printf("[cache] preflight %s", r.URL.Path)
	w.WriteHeader(http.StatusNoContent)
	return true
}

// copyHeaders adds every header from src into dst (used to mirror cached or
// upstream headers onto a response).
func copyHeaders(dst http.Header, src map[string][]string) {
	copyHeadersExcept(dst, src, nil)
}

// copyHeadersExcept is copyHeaders minus the names in skip: the locally
// configured [cache_headers] must not be duplicated nor overridden by whatever
// the upstream happens to send. Outside the cache_path prefix skip is nil, so
// an upstream CORS header stays observable untouched.
func copyHeadersExcept(dst http.Header, src map[string][]string, skip map[string]string) {
	for k, vs := range src {
		if headerNameIn(skip, k) {
			continue
		}
		for _, v := range vs {
			dst.Add(k, v)
		}
	}
}

// =============================================================================
//	压缩/解压  gzip 编解码 + brotli 解压（外部 brotli CLI，一次性探测可用性）；
//	备份      once 规则改写前的原文备份 <file>_.bak1（只做一次，不覆盖）；
//	替换落盘  按 sed 计划替换响应头（Content-Encoding/Content-Length）、原子写
//	          正文与元数据，以及无规则路径「边回边落」的流式写入。
// =============================================================================

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

// backupOriginalOnce keeps the original upstream bytes of a rewritten entry as
// <file>_.bak1. Written at most once: the first write is the true original,
// later writes store the rewritten body and must not clobber the backup.
func backupOriginalOnce(bodyPath, name string, raw []byte) {
	bakPath := bodyPath + "_.bak1"
	if _, err := os.Stat(bakPath); !os.IsNotExist(err) {
		return
	}
	if err := atomicWriteFile(bakPath, raw, 0o644); err != nil {
		log.Printf("[cache] cache_sed backup FAIL: %v", err)
		return
	}
	log.Printf("[cache] cache_sed: backup %s → %s", name, filepath.Base(bakPath))
}

// applyEntryFraming replaces the response headers so they match what is sent,
// per the plan — header driven, bytes are never sniffed.
//
//	mode=once → stored & served gzip: Content-Encoding becomes gzip and the
//	upstream Content-Length no longer matches and is dropped;
//	mode=each → stored plain, the served copy is re-encoded (sendGzip) or not:
//	the upstream encoding/length claims are dropped and gzip is set when the
//	response is compressed again;
//	mode=none (passthrough, rules 1/2/3) → upstream headers stay exactly as
//	recorded, because the body was not touched.
func applyEntryFraming(w http.ResponseWriter, meta *cacheMeta, plan sedMeta, sendGzip bool) {
	switch plan.Mode {
	case sedModeOnce:
		meta.Headers["Content-Encoding"] = []string{"gzip"}
		delete(meta.Headers, "Content-Length")
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Del("Content-Length")
	case sedModeEach:
		delete(meta.Headers, "Content-Encoding")
		delete(meta.Headers, "Content-Length")
		w.Header().Del("Content-Length")
		if sendGzip {
			w.Header().Set("Content-Encoding", "gzip")
		} else {
			w.Header().Del("Content-Encoding")
		}
	}
}

// storeEntry writes the entry's body and then its metadata, both atomically
// (temp file + rename). The error names the failing step so the caller's log
// stays diagnosable.
func storeEntry(bodyPath, metaPath string, body []byte, meta *cacheMeta) error {
	if err := atomicWriteFile(bodyPath, body, 0o644); err != nil {
		return fmt.Errorf("write body %s: %w", bodyPath, err)
	}
	if err := writeCacheMeta(metaPath, meta); err != nil {
		return fmt.Errorf("write meta %s: %w", metaPath, err)
	}
	return nil
}

// streamToTemp is the no-rules (plain) store path: the status is written first
// (a cached 404 must not be turned into 200 by an implicit WriteHeader), then
// the body streams to the client and the temp file at once; the temp file is
// renamed into place only after a clean copy.
func streamToTemp(w http.ResponseWriter, src io.Reader, bodyPath string, status int) (int64, error) {
	tmp, err := os.CreateTemp(filepath.Dir(bodyPath), ".tmp-*")
	if err != nil {
		return 0, fmt.Errorf("create temp: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	w.WriteHeader(status)
	n, copyErr := io.Copy(io.MultiWriter(w, tmp), src)
	_ = tmp.Close()
	if copyErr != nil {
		return n, fmt.Errorf("copy: %w", copyErr)
	}
	if err := os.Rename(tmpName, bodyPath); err != nil {
		return n, fmt.Errorf("rename %s: %w", bodyPath, err)
	}
	committed = true
	return n, nil
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
	if err := os.Rename(tmpName, path); err != nil {
		cleanup()
		return err
	}
	return nil
}

// =============================================================================
// 规则匹配与元数据小工具 — cache_sed / cache_rec 共用
// =============================================================================
// splitList splits a comma-separated rule field into trimmed non-empty items,
// so a stray comma is harmless.
func splitList(field string) []string {
	var out []string
	for _, p := range strings.Split(field, ",") {
		if p = strings.TrimSpace(p); p != "" {
			out = append(out, p)
		}
	}
	return out
}

// hostList renders a host list for logs ("-" = any host).
func hostList(hosts []string) string {
	if len(hosts) == 0 {
		return "-"
	}
	return strings.Join(hosts, ",")
}

// globMatch matches name against a files pattern with at most one '*' wildcard
// (prefix*/suffix match); without '*' the match is exact (so "__index" — the
// directory homepage entry, see cacheIndexName — matches by full name). fold
// compares case-insensitively — DNS host names are case-insensitive.
func globMatch(pattern, name string, fold bool) bool {
	if fold {
		pattern, name = strings.ToLower(pattern), strings.ToLower(name)
	}
	if !strings.Contains(pattern, "*") {
		return pattern == name
	}
	prefix, suffix, _ := strings.Cut(pattern, "*")
	return strings.HasPrefix(name, prefix) && strings.HasSuffix(name, suffix)
}

// targetHost returns the host a rule's host list is matched against: the
// PROXIED BACKEND's host — the target URL's host, port included when the target
// carries one (b.Target may also carry a base path or a [v=key] marker, both of
// which url.Parse drops into the path). It is deliberately NOT the front-end
// request Host: rules describe which content source they belong to (vscode.dev,
// main.vscode-cdn.net, zcode.z.ai …), so they keep working no matter which
// domain the gateway is reached through. The request Host is used only to
// expand ">host<".
func targetHost(targetURL string) string {
	u, err := url.Parse(targetURL)
	if err != nil {
		return ""
	}
	return u.Host
}

// hostMatch reports whether a rule's host list admits the 来源主机 (see
// targetHost). An empty list admits every host. Patterns are compared
// case-insensitively against both the full host (host:port) and its bare
// hostname, each allowing one '*' wildcard (e.g. "*.vscode-cdn.net").
func hostMatch(patterns []string, host string) bool {
	if len(patterns) == 0 {
		return true
	}
	bare := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		bare = h
	}
	for _, p := range patterns {
		if globMatch(p, host, true) || globMatch(p, bare, true) {
			return true
		}
	}
	return false
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
