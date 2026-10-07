package pkg

// relay.go — relay:// 后端的公共层：target → handler 注册表 + 各进程内中继
// （vscrelay / zcode）共用的连接与地址小工具。
//
// 分流规则（backend.go 的 relay case 一行接入 RelayHandler）：
//
//	target 命中注册表 → 对应实现（目前预置 "vscode" 与 "zcode"）；
//	target 未命中     → 返回 !ok，backend 构建期直接报错退出（写错的 target
//	                    不允许悄悄落到某个实现上）。
//
// 各实现以 init() 自注册：新增中继实现只需 registerRelay 一行 + 自己的实现文件，
// 不需要动 backend.go，也不需要在这里了解任何协议细节。

import (
	"log"
	"net"
	"net/http"
	"strings"
	"time"
)

// relayKeepAlive 是 hijack 之后裸连接的 TCP keepalive 周期。中继连接的生命期以
// 天计：内核默认（http.Server 接受的连接约 3 分钟，多数 NAT 的静默回收窗口比这
// 更短）撑不住长空闲的隧道，必须显式开短周期保活。
const relayKeepAlive = 15 * time.Second

// relayRegistry：target → handler 构造器（init() 自注册，见 RelayHandler）。
var relayRegistry = map[string]func(target, mount string) http.Handler{}

// tunnelIDCfg 承接 tunnel_id 配置（SetTunnelID，main 在构建 backend 前调用）。
// 语义是 relay 层的渠道 ID：目前只有 vscode 实现消费它做注册门禁，其他实现
// 需要时直接读取，不再各挂一套配置。
var tunnelIDCfg string

// SetTunnelID sets the relay channel id (tunnel_id config). Call once from
// main() after config load, before backends are built.
func SetTunnelID(id string) { tunnelIDCfg = id }

// registerRelay 把一个 relay:// target 的实现挂进注册表（各实现文件 init() 调用）。
func registerRelay(target string, ctor func(target, mount string) http.Handler) {
	if _, dup := relayRegistry[target]; dup {
		log.Fatalf("relay: duplicate registration for target %q", target)
	}
	relayRegistry[target] = ctor
}

// RelayHandler 按 target 分流 relay:// 后端；未注册的 target 返回 !ok（调用方
// 在配置构建期报错，不悄悄兜底到任何实现）。
func RelayHandler(target, mount string) (http.Handler, bool) {
	ctor, ok := relayRegistry[target]
	if !ok {
		return nil, false
	}
	return ctor(target, mount), true
}

// hijackRawConn — hijack 之后的裸连接卫生，两件事都必做：
//
//  1. 清掉 http.Server 预设的绝对读写 deadline。hijack 之后 server 不再管理这条
//     连接，读请求时挂上的 deadline（read_timeout，默认 60s）绝不能继续生效——
//     否则每条被劫持的连接都会在升级请求到达 read_timeout 后必然读超时被杀，
//     与连接健康与否无关。新版 stdlib 在 Hijack 里会自行清掉，这里显式做一遍，
//     不依赖 stdlib 版本行为；
//  2. 显式开 TCP keepalive（relayKeepAlive），防 NAT/中间设备对空闲连接的静默回收。
//     这是长命连接真正常态化需要的一步：空闲是隧道与 WS 的常态，静默连接靠内核
//     保活维系。
//
// 供所有"从 http.Server 抢走长命连接"的路径使用：wsUpgrade（worker 通道、zcode
// 连接）、vscrelay 的 client 腿、wsproxy 隧道的客户端侧。超时与保活策略由此
// 交给各中继自己的协议（探测帧、空闲判定）。
func hijackRawConn(conn net.Conn) {
	_ = conn.SetDeadline(time.Time{})
	if nc, ok := conn.(interface{ NetConn() net.Conn }); ok {
		conn = nc.NetConn() // TLS 连接：保活要落在底层 TCP 上
	}
	if tcp, ok := conn.(*net.TCPConn); ok {
		_ = tcp.SetKeepAlive(true)
		_ = tcp.SetKeepAlivePeriod(relayKeepAlive)
	}
}

// clientRemoteAddr — 客户端来源地址（仅 IP，不含端口）：优先 X-Forwarded-For
// 首值（最外层代理写入的真实客户端），否则取 TCP 对端。前置代理必须设置
// X-Forwarded-For，否则中继只能见到代理自己的 IP。
func clientRemoteAddr(req *http.Request) string {
	if fwd := strings.TrimSpace(req.Header.Get("X-Forwarded-For")); fwd != "" {
		if i := strings.IndexByte(fwd, ','); i >= 0 {
			fwd = fwd[:i]
		}
		if ip := unmappedIP(strings.TrimSpace(fwd)); ip != "" {
			return ip
		}
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return unmappedIP(req.RemoteAddr)
	}
	return unmappedIP(host)
}

// unmappedIP 把 IPv4-mapped IPv6 地址还原成点分十进制（::ffff:1.2.3.4 → 1.2.3.4），
// 展示与日志不再出现 ::ffff: 前缀。
func unmappedIP(host string) string {
	return strings.TrimPrefix(host, "::ffff:")
}

// isWebSocketUpgrade reports whether the client requests a WebSocket
// connection: Upgrade: websocket plus a Connection header containing the
// "upgrade" token (Connection may list multiple tokens).
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}
