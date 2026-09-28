package pkg

// stats.go — 轻量运行计数与可选 pprof。
//
// 目的：把"效率低"变成可对比的数字（优化前后各采一次），线上也能观察隧道是否走了
// splice、中继每帧开销是否下降。全部 stdlib，零依赖；计数只在几处热路径用原子加。

import (
	"encoding/json"
	"log"
	"net/http"
	"net/http/pprof"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// kvsStatCounters 是全局计数集合（atomic，无锁）。
type kvsStatCounters struct {
	// WS 升级隧道（自研快路径）
	wsTunnels         atomic.Int64 // 建立成功的隧道数
	wsTunnelsSplice   atomic.Int64 // 两端都是裸 socket（预期命中内核 splice）
	wsTunnelsGeneric  atomic.Int64 // 至少一端不是裸 socket（用户态拷贝）
	wsTunnelsClosed   atomic.Int64 // 已结束的隧道数
	wsUpgradeFallback atomic.Int64 // 快路径握手失败、回退 stdlib 的次数
	wsTunnelBytesUp   atomic.Int64 // 客户端 → 后端 字节数
	wsTunnelBytesDown atomic.Int64 // 后端 → 客户端 字节数

	// relay 中继（zcode 远程控制）
	relayFramesIn  atomic.Int64 // 入站帧数
	relayFramesOut atomic.Int64 // 出站帧数
	relayBytesIn   atomic.Int64 // 入站字节
	relayBytesOut  atomic.Int64 // 出站字节
	relayDataFast  atomic.Int64 // 命中 data 帧快路径（跳过整帧 JSON 解析）的帧数
	relayFallbacks atomic.Int64 // 未命中快路径、走完整解析的帧数

	// cc~ 缓存
	cacheHits   atomic.Int64
	cacheMisses atomic.Int64
}

var kvsStats kvsStatCounters

// statsStartedAt 用于输出 uptime。
var statsStartedAt = time.Now()

// StatsHandler 输出 JSON 计数。挂在 /__stats 上，随全局鉴权中间件保护
// （不在 isPublicAuthPath 里，因此 login_token 非空时必须带 Cookie/令牌）。
func StatsHandler(w http.ResponseWriter, r *http.Request) {
	var ms runtime.MemStats
	runtime.ReadMemStats(&ms)
	resp := map[string]any{
		"uptime_sec": int64(time.Since(statsStartedAt).Seconds()),
		"goroutines": runtime.NumGoroutine(),
		"heap_alloc": ms.HeapAlloc,
		"ws_tunnel": map[string]any{
			"total":          kvsStats.wsTunnels.Load(),
			"splice":         kvsStats.wsTunnelsSplice.Load(),
			"generic":        kvsStats.wsTunnelsGeneric.Load(),
			"closed":         kvsStats.wsTunnelsClosed.Load(),
			"open":           kvsStats.wsTunnels.Load() - kvsStats.wsTunnelsClosed.Load(),
			"fallback":       kvsStats.wsUpgradeFallback.Load(),
			"bytes_up":       kvsStats.wsTunnelBytesUp.Load(),
			"bytes_down":     kvsStats.wsTunnelBytesDown.Load(),
			"mode":           wsProxyMode,
			"idle_timeout_s": int64(wsIdleTimeout.Seconds()),
		},
		"relay": map[string]any{
			"frames_in":  kvsStats.relayFramesIn.Load(),
			"frames_out": kvsStats.relayFramesOut.Load(),
			"bytes_in":   kvsStats.relayBytesIn.Load(),
			"bytes_out":  kvsStats.relayBytesOut.Load(),
			"data_fast":  kvsStats.relayDataFast.Load(),
			"full_parse": kvsStats.relayFallbacks.Load(),
		},
		"cache": map[string]any{
			"hits":   kvsStats.cacheHits.Load(),
			"misses": kvsStats.cacheMisses.Load(),
		},
	}
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")
	_ = enc.Encode(resp)
}

// StartPProfIfEnabled 在 KVS_PPROF 非空时额外起一个 pprof 监听（默认只绑 127.0.0.1）。
//
//	KVS_PPROF=1            → 127.0.0.1:6060
//	KVS_PPROF=6061         → 127.0.0.1:6061
//	KVS_PPROF=0.0.0.0:6060 → 显式指定地址（非本机地址会打警告）
//
// 关闭时函数直接返回，不影响主流程。
func StartPProfIfEnabled() {
	v := strings.TrimSpace(os.Getenv("KVS_PPROF"))
	switch v {
	case "", "0", "false", "off":
		return
	case "1", "true", "on":
		v = "127.0.0.1:6060"
	default:
		if !strings.Contains(v, ":") {
			v = "127.0.0.1:" + v
		}
	}
	if !strings.HasPrefix(v, "127.0.0.1:") && !strings.HasPrefix(v, "localhost:") {
		log.Printf("WARNING: pprof listening on %s (not loopback) — anyone who can reach it can profile kvs", v)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/debug/pprof/", pprof.Index)
	mux.HandleFunc("/debug/pprof/cmdline", pprof.Cmdline)
	mux.HandleFunc("/debug/pprof/profile", pprof.Profile)
	mux.HandleFunc("/debug/pprof/symbol", pprof.Symbol)
	mux.HandleFunc("/debug/pprof/trace", pprof.Trace)
	srv := &http.Server{Addr: v, Handler: mux, ReadHeaderTimeout: 10 * time.Second}
	go func() {
		log.Printf("pprof enabled: http://%s/debug/pprof/", v)
		if err := srv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Printf("pprof listener: %v", err)
		}
	}()
}
