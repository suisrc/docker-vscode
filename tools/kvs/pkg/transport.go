package pkg

// transport.go — 反向代理共用的 Transport 调优。
//
// Go 的默认值对"反代"这个场景偏保守：
//
//   - MaxIdleConnsPerHost 默认是 2（http.DefaultTransport 与自建 Transport 的零值都会
//     落到 DefaultMaxIdleConnsPerHost=2）：VS Code Web 首屏是并发几十上百个请求，突发
//     时连接池只留 2 条空闲连接，其余用完即关，于是每轮突发都要重新 dial（unix socket
//     还要让后端重新 accept）。
//   - DisableCompression 默认 false：Transport 会给"没带 Accept-Encoding 的客户端"
//     加 gzip 再透明解压。代理应该原样透传客户端的 Accept-Encoding，让后端与客户端直接
//     协商，既省一次解压/压缩，也避免缓存与 ETag 语义被改写。
//
// 这几个值都可以用配置键覆盖（proxy_idle_conns_per_host / proxy_max_idle_conns /
// proxy_disable_compression / proxy_flush_interval）。
//
// 另外这里提供响应体拷贝的 BufferPool：stdlib 的 BufferPool 默认是 nil，每个带 body
// 的响应都要现 make 一块 32KB（实测 47.5KB/op → 15.7KB/op，见 transport_test.go）。

import (
	"context"
	"log"
	"net"
	"net/http"
	"sync"
	"time"
)

var (
	proxyIdleConnsPerHost = 32
	proxyMaxIdleConns     = 256
	proxyDisableGzip      = true
	// proxyFlushInterval 是 ReverseProxy 的刷出间隔。0 = 不主动刷（Go 默认，流式响应可能
	// 在缓冲区里等 32KB 才发出）；负值 = 每次写都刷；正值 = 定时刷（这里默认 100ms，
	// 兼顾流式延迟与吞吐；text/event-stream 由 net/http 自动按 -1 处理）。
	proxyFlushInterval = 100 * time.Millisecond
)

// SetProxyTuning 由 main 从配置读取后调用（在构建路由之前）。
func SetProxyTuning(idleConnsPerHost, maxIdleConns int, disableGzip bool, flushIntervalMs int) {
	if idleConnsPerHost > 0 {
		proxyIdleConnsPerHost = idleConnsPerHost
	}
	if maxIdleConns > 0 {
		proxyMaxIdleConns = maxIdleConns
	}
	proxyDisableGzip = disableGzip
	if flushIntervalMs >= -1 {
		proxyFlushInterval = time.Duration(flushIntervalMs) * time.Millisecond
	}
	logProxyTuning()
}

// newProxyTransport 构建一个调优过的 Transport。dial 为 nil 时使用默认 TCP dial
// （30s 超时 + 30s keepalive）。
func newProxyTransport(dial func(ctx context.Context, network, addr string) (net.Conn, error)) *http.Transport {
	if dial == nil {
		d := &net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}
		dial = d.DialContext
	}
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           dial,
		MaxIdleConns:          proxyMaxIdleConns,
		MaxIdleConnsPerHost:   proxyIdleConnsPerHost,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ExpectContinueTimeout: 1 * time.Second,
		DisableCompression:    proxyDisableGzip,
		ForceAttemptHTTP2:     true,
	}
}

var (
	sharedTransportOnce sync.Once
	sharedTransport     *http.Transport
	unixTransportMu     sync.Mutex
	unixTransports      = map[string]*http.Transport{}
)

// proxyCopyBufferSize 是反代拷贝响应体时的缓冲大小（httputil.BufferPool 的块大小）。
// 与 stdlib 默认的 32KB 一致；调大能减少大响应的读写调用次数（见 benchmark）。
const proxyCopyBufferSize = 32 << 10

// proxyBufferPool 复用响应体拷贝缓冲。httputil.ReverseProxy 的 BufferPool 默认为 nil，
// 于是**每个带 body 的响应**都要 make 一块 32KB：VS Code Web 首屏几百个请求、以及
// 长连接（SSE/长轮询）持续占用，这段分配量不可忽略。池化后稳态零分配。
type proxyBufferPool struct{ pool sync.Pool }

func (p *proxyBufferPool) Get() []byte {
	if v := p.pool.Get(); v != nil {
		return v.([]byte)
	}
	return make([]byte, proxyCopyBufferSize)
}

func (p *proxyBufferPool) Put(b []byte) {
	if cap(b) < proxyCopyBufferSize {
		return // 尺寸不对（被换过）就丢弃，避免池里混入大小不一的块
	}
	p.pool.Put(b[:proxyCopyBufferSize])
}

var sharedProxyBufferPool = &proxyBufferPool{}

// sharedProxyTransport 返回 http/https/ws/wss 后端共用的 Transport（连接池共享，
// 同一个 host 的所有路由都受益）。
func sharedProxyTransport() *http.Transport {
	sharedTransportOnce.Do(func() { sharedTransport = newProxyTransport(nil) })
	return sharedTransport
}

// unixProxyTransport 返回某个 unix socket 的 Transport（按 socket 路径缓存，
// 多个路由指向同一 socket 时共享连接池）。
func unixProxyTransport(socketPath string) *http.Transport {
	unixTransportMu.Lock()
	defer unixTransportMu.Unlock()
	if t, ok := unixTransports[socketPath]; ok {
		return t
	}
	sock := socketPath
	t := newProxyTransport(func(ctx context.Context, _, _ string) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", sock)
	})
	// unix socket 没有 TLS/h2，也没必要走代理环境变量。
	t.Proxy = nil
	t.ForceAttemptHTTP2 = false
	unixTransports[socketPath] = t
	return t
}

// logProxyTuning 打印一次调优参数，便于线上确认配置生效。
func logProxyTuning() {
	log.Printf("proxy tuning: idle_conns_per_host=%d max_idle_conns=%d disable_compression=%v flush_interval=%v",
		proxyIdleConnsPerHost, proxyMaxIdleConns, proxyDisableGzip, proxyFlushInterval)
}
