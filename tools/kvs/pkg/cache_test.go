package pkg

// cache_test.go — cc~ 缓存命中/写入路径的回归测试。
//
// 覆盖本轮改动的三条语义：
//   - 命中直出（mode=none/once/legacy）：ServeContent 文件直出，带 Content-Length、
//     Accept-Ranges 与 Range/If-Modified-Since 支持，字节与状态码语义不变；
//   - mode=each 仍按请求重写（整份正文进内存），行为与旧实现一致；
//   - 元数据内存缓存在"同秒重写"（inode 变化）时必须失效；
//   - MISS 路径上游状态码（404 也入缓存）不再被隐式 WriteHeader 改成 200。

import (
	"fmt"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"
)

// mkEntry 在临时目录写一个缓存条目（正文 + 元数据），返回两个文件路径。
func mkEntry(t *testing.T, body []byte, status int, hdrs map[string][]string, sed *sedMeta) (bodyPath, metaPath string) {
	t.Helper()
	dir := t.TempDir()
	bodyPath = filepath.Join(dir, "app.js")
	metaPath = bodyPath + "_.json"
	if err := os.WriteFile(bodyPath, body, 0o644); err != nil {
		t.Fatalf("write body: %v", err)
	}
	if err := writeCacheMeta(metaPath, &cacheMeta{Status: status, Headers: hdrs, Sed: sed}); err != nil {
		t.Fatalf("write meta: %v", err)
	}
	return bodyPath, metaPath
}

func hitReq(path string, hdr map[string]string) *http.Request {
	r := httptest.NewRequest(http.MethodGet, path, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	return r
}

// TestServeFromCacheFileDirect 直出路径：200 状态走 ServeContent，字节一致、
// Content-Length 来自文件、Accept-Ranges 打开。
func TestServeFromCacheFileDirect(t *testing.T) {
	body := []byte("console.log('hello from cache');\n// more bytes\n")
	bodyPath, metaPath := mkEntry(t, body, 200, map[string][]string{
		"Content-Type": {"application/javascript"},
	}, nil)

	w := httptest.NewRecorder()
	if !serveFromCache(w, hitReq("/app.js", nil), bodyPath, metaPath, "https://x/app.js", "x") {
		t.Fatal("命中失败")
	}
	res := w.Result()
	if res.StatusCode != 200 {
		t.Fatalf("状态码 %d，期望 200", res.StatusCode)
	}
	if got := res.Header.Get("X-Cache"); got != "HIT" {
		t.Fatalf("X-Cache=%q，期望 HIT", got)
	}
	if got := res.Header.Get("Content-Type"); got != "application/javascript" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := res.Header.Get("Content-Length"); got != strconv.Itoa(len(body)) {
		t.Fatalf("Content-Length=%q，期望 %d", got, len(body))
	}
	if got := res.Header.Get("Accept-Ranges"); got != "bytes" {
		t.Fatalf("Accept-Ranges=%q，期望 bytes", got)
	}
	gotBody, _ := io.ReadAll(res.Body)
	if string(gotBody) != string(body) {
		t.Fatalf("正文不一致：%q vs %q", gotBody, body)
	}
}

// TestServeFromCacheRange 直出路径的 Range 支持（ServeContent 自带）。
func TestServeFromCacheRange(t *testing.T) {
	body := []byte("0123456789abcdef")
	bodyPath, metaPath := mkEntry(t, body, 200, map[string][]string{"Content-Type": {"text/plain"}}, nil)

	w := httptest.NewRecorder()
	if !serveFromCache(w, hitReq("/f.txt", map[string]string{"Range": "bytes=2-5"}), bodyPath, metaPath, "https://x/f.txt", "x") {
		t.Fatal("命中失败")
	}
	res := w.Result()
	if res.StatusCode != http.StatusPartialContent {
		t.Fatalf("状态码 %d，期望 206", res.StatusCode)
	}
	got, _ := io.ReadAll(res.Body)
	if string(got) != "2345" {
		t.Fatalf("Range 正文 %q，期望 2345", got)
	}
	if cr := res.Header.Get("Content-Range"); cr != fmt.Sprintf("bytes 2-5/%d", len(body)) {
		t.Fatalf("Content-Range=%q", cr)
	}
}

// TestServeFromCacheEachRewrites mode=each 仍按请求重写正文（不走直出）。
func TestServeFromCacheEachRewrites(t *testing.T) {
	body := []byte("hello OLD world")
	bodyPath, metaPath := mkEntry(t, body, 200, nil, &sedMeta{
		Mode: sedModeEach,
		Each: []sedEachRule{{Old: "OLD", New: "NEW"}},
	})

	w := httptest.NewRecorder()
	if !serveFromCache(w, hitReq("/app.js", nil), bodyPath, metaPath, "https://x/app.js", "example.com") {
		t.Fatal("命中失败")
	}
	res := w.Result()
	got, _ := io.ReadAll(res.Body)
	if string(got) != "hello NEW world" {
		t.Fatalf("重写结果 %q", got)
	}
	if cl := res.Header.Get("Content-Length"); cl != strconv.Itoa(len(got)) {
		t.Fatalf("each 路径 Content-Length=%q，期望 %d", cl, len(got))
	}
}

// TestServeFromCacheKeeps404Status 404 也入缓存；直出不能把它变成 200。
func TestServeFromCacheKeeps404Status(t *testing.T) {
	body := []byte("not found page")
	bodyPath, metaPath := mkEntry(t, body, 404, map[string][]string{"Content-Type": {"text/html"}}, nil)

	w := httptest.NewRecorder()
	if !serveFromCache(w, hitReq("/gone.html", nil), bodyPath, metaPath, "https://x/gone.html", "x") {
		t.Fatal("命中失败")
	}
	res := w.Result()
	if res.StatusCode != 404 {
		t.Fatalf("状态码 %d，期望 404", res.StatusCode)
	}
	got, _ := io.ReadAll(res.Body)
	if string(got) != string(body) {
		t.Fatalf("正文 %q", got)
	}
}

// TestReadCacheMetaInvalidatesOnSameSecondRewrite 同秒重写（mtime 粒度内）也必须
// 失效：判据里带 inode，原子写（temp+rename）必然换 inode。
func TestReadCacheMetaInvalidatesOnSameSecondRewrite(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "m_.json")
	fixed := time.Unix(1700000000, 0)

	write := func(status int) {
		if err := writeCacheMeta(path, &cacheMeta{Status: status, Headers: map[string][]string{}}); err != nil {
			t.Fatalf("write meta: %v", err)
		}
		if err := os.Chtimes(path, fixed, fixed); err != nil {
			t.Fatalf("chtimes: %v", err)
		}
	}
	write(200)
	if m, err := readCacheMeta(path); err != nil || m.Status != 200 {
		t.Fatalf("首次读取: %+v, %v", m, err)
	}
	write(201) // 同样大小、同样的 mtime，只有 inode 不同
	if m, err := readCacheMeta(path); err != nil || m.Status != 201 {
		t.Fatalf("同秒重写后读到 %+v, %v（内存缓存未失效）", m, err)
	}
}

// TestCacheMissPlainPreservesUpstreamStatus MISS 普通路径必须把上游状态码写出去：
// 404 会入缓存，隐式 WriteHeader 会把它变成 200（本轮修复）。
func TestCacheMissPlainPreservesUpstreamStatus(t *testing.T) {
	SetCacheSed("") // 保证走 plain（非 sed）分支
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", "4")
		w.WriteHeader(404)
		_, _ = w.Write([]byte("gone"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "gone.txt")
	metaPath := bodyPath + "_.json"
	w := httptest.NewRecorder()
	handleCachedCache(w, httptest.NewRequest(http.MethodGet, "/gone.txt", nil), upstream.URL+"/gone.txt", bodyPath, metaPath)

	res := w.Result()
	if res.StatusCode != 404 {
		t.Fatalf("MISS 状态码 %d，期望 404（上游状态被吞掉）", res.StatusCode)
	}
	if got := res.Header.Get("X-Cache"); got != "MISS" {
		t.Fatalf("X-Cache=%q", got)
	}
	got, _ := io.ReadAll(res.Body)
	if string(got) != "gone" {
		t.Fatalf("MISS 正文 %q", got)
	}
	// 缓存的元数据也应是 404，命中路径才能保持一致。
	if m, err := readCacheMeta(metaPath); err != nil || m.Status != 404 {
		t.Fatalf("缓存元数据 %+v, %v", m, err)
	}
}

// TestCacheMissThenHitRoundTrip MISS→HIT 整链：第一次上游 200，第二次走直出且字节一致。
func TestCacheMissThenHitRoundTrip(t *testing.T) {
	SetCacheSed("")
	const body = "round-trip-body-1234567890"
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write([]byte(body))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "f.txt")
	metaPath := bodyPath + "_.json"

	w1 := httptest.NewRecorder()
	handleCachedCache(w1, httptest.NewRequest(http.MethodGet, "/f.txt", nil), upstream.URL+"/f.txt", bodyPath, metaPath)
	if got, _ := io.ReadAll(w1.Result().Body); string(got) != body {
		t.Fatalf("MISS 正文 %q", got)
	}

	w2 := httptest.NewRecorder()
	if !serveFromCache(w2, hitReq("/f.txt", nil), bodyPath, metaPath, upstream.URL+"/f.txt", "x") {
		t.Fatal("二次请求未命中缓存")
	}
	res2 := w2.Result()
	if res2.StatusCode != 200 || res2.Header.Get("X-Cache") != "HIT" {
		t.Fatalf("HIT 响应 %d %q", res2.StatusCode, res2.Header.Get("X-Cache"))
	}
	got, _ := io.ReadAll(res2.Body)
	if string(got) != body {
		t.Fatalf("HIT 正文 %q", got)
	}
	if cl := res2.Header.Get("Content-Length"); cl != strconv.Itoa(len(body)) {
		t.Fatalf("HIT Content-Length=%q", cl)
	}
}

// BenchmarkServeFromCacheFileDirect 直出路径的基准：ServeContent 走 sendfile，
// 服务端不整文件进内存（对比 temp/audit/ccserve 的实测：readfile 1016MiB/s
// 每次 8MiB 堆分配，sendfile 2759MiB/s 零堆分配）。
func BenchmarkServeFromCacheFileDirect(b *testing.B) {
	body := make([]byte, 8<<20)
	for i := range body {
		body[i] = byte(i)
	}
	dir := b.TempDir()
	bodyPath := filepath.Join(dir, "bundle.js")
	metaPath := bodyPath + "_.json"
	if err := os.WriteFile(bodyPath, body, 0o644); err != nil {
		b.Fatal(err)
	}
	if err := writeCacheMeta(metaPath, &cacheMeta{Status: 200, Headers: map[string][]string{"Content-Type": {"application/javascript"}}}); err != nil {
		b.Fatal(err)
	}
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	logSet := log.Writer()
	log.SetOutput(io.Discard) // 每条 HIT 都有日志，别让日志淹没基准输出
	defer log.SetOutput(logSet)
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		w := httptest.NewRecorder()
		if !serveFromCache(w, hitReq("/bundle.js", nil), bodyPath, metaPath, "https://x/bundle.js", "x") {
			b.Fatal("miss")
		}
		n, _ := io.Copy(io.Discard, w.Result().Body)
		if n != int64(len(body)) {
			b.Fatalf("n=%d", n)
		}
	}
}

// TestServeFromCacheBodyMissingFallsBackCleanly 元数据在而正文文件被清掉时，必须
// 干净地回退 MISS（响应头不能残留 X-Cache: HIT / 元数据首部，否则 MISS 路径会叠加）。
func TestServeFromCacheBodyMissingFallsBackCleanly(t *testing.T) {
	bodyPath, metaPath := mkEntry(t, []byte("x"), 200, map[string][]string{"Content-Type": {"text/plain"}}, nil)
	if err := os.Remove(bodyPath); err != nil {
		t.Fatalf("remove body: %v", err)
	}
	w := httptest.NewRecorder()
	if serveFromCache(w, hitReq("/f.txt", nil), bodyPath, metaPath, "https://x/f.txt", "x") {
		t.Fatal("正文缺失却报命中")
	}
	// 干净回退 = 什么都没写（X-Cache 未出现、元数据首部未出现）。
	for k := range w.Header() {
		t.Fatalf("回退后残留响应头 %q（应完全干净）", k)
	}
}

// TestCacheCountersAdvance 命中/未命中计数随请求推进（/__stats 的 cache 段）。
func TestCacheCountersAdvance(t *testing.T) {
	SetCacheSed("")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		_, _ = w.Write([]byte("counted"))
	}))
	defer upstream.Close()

	dir := t.TempDir()
	bodyPath := filepath.Join(dir, "c.txt")
	metaPath := bodyPath + "_.json"
	hitsBefore, missesBefore := kvsStats.cacheHits.Load(), kvsStats.cacheMisses.Load()

	handleCachedCache(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/c.txt", nil), upstream.URL+"/c.txt", bodyPath, metaPath)
	if kvsStats.cacheMisses.Load() != missesBefore+1 {
		t.Fatalf("miss 计数未推进：%d → %d", missesBefore, kvsStats.cacheMisses.Load())
	}
	serveFromCache(httptest.NewRecorder(), hitReq("/c.txt", nil), bodyPath, metaPath, upstream.URL+"/c.txt", "x")
	if kvsStats.cacheHits.Load() != hitsBefore+1 {
		t.Fatalf("hit 计数未推进：%d → %d", hitsBefore, kvsStats.cacheHits.Load())
	}
}
