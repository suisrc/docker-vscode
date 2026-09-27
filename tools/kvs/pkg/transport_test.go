package pkg

// transport_test.go — 反代响应路径（缓冲池 / 刷出间隔）的基准，用于给调优参数定值。
// 走真实链路：httptest 后端 → CreateBackendHandler → httptest 代理。
// 基线用 stdlib 的 httputil.NewSingleHostReverseProxy（BufferPool=nil、FlushInterval=0），
// 也就是优化前的行为。

import (
	"bytes"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"testing"
	"time"
)

func benchBackend(b *testing.B, size int) *httptest.Server {
	b.Helper()
	body := bytes.Repeat([]byte("abcdefgh"), size/8)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", fmt.Sprint(len(body)))
		_, _ = w.Write(body)
	}))
	b.Cleanup(srv.Close)
	return srv
}

// benchThroughput 用给定 handler 转发 size 字节响应，报告吞吐与分配。
func benchThroughput(b *testing.B, h http.Handler, size int) {
	b.Helper()
	proxy := httptest.NewServer(h)
	defer proxy.Close()
	b.SetBytes(int64(size))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		res, err := http.Get(proxy.URL + "/px/data")
		if err != nil {
			b.Fatalf("get: %v", err)
		}
		n, err := io.Copy(io.Discard, res.Body)
		res.Body.Close()
		if err != nil || n != int64(size) {
			b.Fatalf("copy: n=%d err=%v", n, err)
		}
	}
}

// ourProxy 构建被测 handler（可指定刷出间隔）。
func ourProxy(b *testing.B, backend *httptest.Server, flushMs int) http.Handler {
	b.Helper()
	bk := newBackend("/px", "http://"+backend.Listener.Addr().String())
	if bk == nil {
		b.Fatal("newBackend nil")
	}
	old := proxyFlushInterval
	proxyFlushInterval = time.Duration(flushMs) * time.Millisecond
	b.Cleanup(func() { proxyFlushInterval = old })
	return CreateBackendHandler(*bk, nil, false)
}

// stdlibProxy 是优化前的基线：stdlib 反代默认值。
func stdlibProxy(b *testing.B, backend *httptest.Server) http.Handler {
	b.Helper()
	u, err := url.Parse("http://" + backend.Listener.Addr().String())
	if err != nil {
		b.Fatal(err)
	}
	return httputil.NewSingleHostReverseProxy(u)
}

func BenchmarkStdlib1MiB(b *testing.B) {
	s := benchBackend(b, 1<<20)
	benchThroughput(b, stdlibProxy(b, s), 1<<20)
}
func BenchmarkOurs1MiB(b *testing.B) {
	s := benchBackend(b, 1<<20)
	benchThroughput(b, ourProxy(b, s, 100), 1<<20)
}
func BenchmarkOurs1MiBNoFlush(b *testing.B) {
	s := benchBackend(b, 1<<20)
	benchThroughput(b, ourProxy(b, s, 0), 1<<20)
}
func BenchmarkOurs1MiBFlushNeg(b *testing.B) {
	s := benchBackend(b, 1<<20)
	benchThroughput(b, ourProxy(b, s, -1), 1<<20)
}

func BenchmarkStdlib32KiB(b *testing.B) {
	s := benchBackend(b, 32<<10)
	benchThroughput(b, stdlibProxy(b, s), 32<<10)
}
func BenchmarkOurs32KiB(b *testing.B) {
	s := benchBackend(b, 32<<10)
	benchThroughput(b, ourProxy(b, s, 100), 32<<10)
}
func BenchmarkOurs32KiBNoFlush(b *testing.B) {
	s := benchBackend(b, 32<<10)
	benchThroughput(b, ourProxy(b, s, 0), 32<<10)
}
