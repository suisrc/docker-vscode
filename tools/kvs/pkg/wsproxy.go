package pkg

// wsproxy.go — WebSocket 升级快路径（自研隧道）。
//
// 为什么不直接用 httputil.ReverseProxy 的升级分支（详见 temp/ws-proxy-perf-plan.md）：
//
//  1. 双向都用 32KB 用户态缓冲拷贝：上游 body 是包装类型（*httputil.readWriteCloserBody），
//     而 net 的 splice 快路径只认裸 *net.TCPConn / *net.UnixConn，所以 socket↔socket
//     也永远命中不了内核 splice。
//  2. hijack 之后不消费 http.Server 已缓冲的客户端字节（brw.Reader）：客户端把首帧与
//     握手写在同一个 TCP 段时，首帧被静默丢弃（已实测复现）。
//  3. 隧道没有任何空闲超时：黑洞对端会一直占着两个 goroutine 与缓冲区。
//
// 本文件自己 dial 后端（因此拿到裸 conn）、两侧都先 drain 缓冲字节，再用 io.Copy 在裸
// conn 之间拷贝（socket↔socket 命中内核 splice）。**只接管 101 升级请求**，非升级请求
// 仍走标准 ReverseProxy，行为不变；握手阶段任何失败都回退 stdlib 路径（此时尚未写响应）。

import (
	"bufio"
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/textproto"
	"net/url"
	"os"
	"strings"
	"sync/atomic"
	"time"
)

// 升级处理策略（配置键 ws_proxy）。
const (
	wsProxyAuto   = "auto"   // 默认：先走自研隧道，握手失败回退 stdlib
	wsProxyStdlib = "stdlib" // 总是走 stdlib 升级分支（旧行为）
	wsProxyOff    = "off"    // 等同 stdlib，保留开关语义
)

// 调优参数（包级变量，启动时由配置覆盖；见 SetWSTunnelOptions）。
var (
	wsProxyMode = wsProxyAuto
	// wsIdleTimeout > 0 时启用隧道空闲超时：一段时间内没有数据流动则断开两端。
	// 代价是该连接的拷贝走分块路径（分块仍走 splice，只是刷新 deadline 的粒度变粗）。
	wsIdleTimeout time.Duration
	// wsWriteGraceMin 是"写超时相对读超时的最小宽限"：只要对端还在前进（哪怕很慢，
	// 128KB 一个 chunk 在宽限内能读走），就不会被判成 stalled。见 copyTunnel。
	wsWriteGraceMin = 30 * time.Second
	// wsHandshakeTimeout 是"写请求 + 读 101 响应"的整体超时。
	wsHandshakeTimeout = 30 * time.Second
	// wsCopyChunk 是启用空闲超时后每轮拷贝的字节数（分块刷新 deadline）。
	wsCopyChunk = int64(128 << 10)
	// wsBufSize 是回退到用户态拷贝时的缓冲大小（splice 不可用时）。
	wsBufSize = 128 << 10
	// wsUpstreamBufSize 是读后端响应的 bufio 大小（101 握手响应很小，取默认值即可，
	// 这里给大一点以便首包数据少一次 syscall）。
	wsUpstreamBufSize = 16 << 10
)

// SetWSTunnelOptions 配置升级隧道的模式与空闲超时（由 main 从配置读取）。
func SetWSTunnelOptions(mode string, idleSeconds int) {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case "", wsProxyAuto:
		wsProxyMode = wsProxyAuto
	case wsProxyStdlib:
		wsProxyMode = wsProxyStdlib
	case wsProxyOff:
		wsProxyMode = wsProxyOff
	default:
		log.Printf("WARNING: unknown ws_proxy %q, using %q", mode, wsProxyAuto)
		wsProxyMode = wsProxyAuto
	}
	if idleSeconds > 0 {
		wsIdleTimeout = time.Duration(idleSeconds) * time.Second
	} else {
		wsIdleTimeout = 0
	}
}

// wsUpstream 描述升级快路径如何到达后端。
type wsUpstream struct {
	b      Backend
	target *url.URL // http/https/ws/wss 目标；unix 后端为合成 http://unix（仅作改写标签）
	hdrs   map[string]string
	mods   func(*http.Response) error // login_authz 的响应修饰链（非 101 响应同标准路径一致）
}

// describe 返回日志用的目标描述。
func (u *wsUpstream) describe() string {
	if u.b.Scheme == "unix" {
		return "unix://" + u.b.Target
	}
	return u.b.Scheme + "://" + u.target.Host
}

// rewrite 把入站请求改写成发往后端的请求（与 ReverseProxy 的 Director 同规则，
// 含 -/+ 路径模式；unix 后端经合成 target 走同一条路径）。
func (u *wsUpstream) rewrite(req *http.Request) {
	rewriteProxyRequest(req, u.b, u.target, u.hdrs)
}

// dial 打开一条到后端的裸连接（https/wss 目标在裸 conn 上包一层 TLS）。
func (u *wsUpstream) dial(ctx context.Context) (net.Conn, error) {
	d := &net.Dialer{Timeout: wsHandshakeTimeout, KeepAlive: 30 * time.Second}
	if u.b.Scheme == "unix" {
		return d.DialContext(ctx, "unix", u.b.Target)
	}
	addr := u.target.Host
	if _, _, err := net.SplitHostPort(addr); err != nil {
		if u.target.Scheme == "https" || u.target.Scheme == "wss" {
			addr = net.JoinHostPort(addr, "443")
		} else {
			addr = net.JoinHostPort(addr, "80")
		}
	}
	conn, err := d.DialContext(ctx, "tcp", addr)
	if err != nil {
		return nil, err
	}
	if u.target.Scheme == "https" || u.target.Scheme == "wss" {
		tc := tls.Client(conn, &tls.Config{
			ServerName: u.target.Hostname(),
			MinVersion: tls.VersionTLS12,
			NextProtos: []string{"http/1.1"},
		})
		if err := tc.HandshakeContext(ctx); err != nil {
			_ = conn.Close()
			return nil, fmt.Errorf("tls handshake: %w", err)
		}
		return tc, nil
	}
	return conn, nil
}

// wsUpgradeHandler 包装标准反代：升级请求走自研隧道，其余请求原样透传给 next。
func wsUpgradeHandler(up *wsUpstream, next http.Handler) http.Handler {
	if wsProxyMode == wsProxyOff || wsProxyMode == wsProxyStdlib {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if up.b.IsWSock && isWebSocketUpgrade(r) {
			log.Printf("[ws] upgrade %s%s → %s (scheme=%s)", r.Host, r.URL.Path, up.b.RawURL, up.b.Scheme)
		}
		if isWebSocketUpgrade(r) && handleWSUpgrade(w, r, up) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// handleWSUpgrade 尝试用裸 conn 隧道代理一次升级请求。返回 true 表示请求已被完全处理
// （响应已写出、隧道已跑完，调用方不得再写 w）；返回 false 表示尚未写任何东西，调用方
// 必须回退到标准反代路径。
func handleWSUpgrade(w http.ResponseWriter, r *http.Request, up *wsUpstream) bool {
	hj, ok := w.(http.Hijacker)
	if !ok {
		return false
	}
	reqUpType := r.Header.Get("Upgrade")
	if !isTokenASCII(reqUpType) {
		return false // 非法协议名，交给 stdlib 产生标准错误响应
	}

	// 构造出站请求：与 ReverseProxy.ServeHTTP 的头部处理保持一致
	// （去掉 hop-by-hop 头后再补 Connection/Upgrade，并追加 X-Forwarded-For）。
	outReq := r.Clone(r.Context())
	if outReq.ContentLength == 0 {
		outReq.Body = nil
	}
	if outReq.Header == nil {
		outReq.Header = make(http.Header)
	}
	up.rewrite(outReq)
	outReq.Close = false
	removeHopByHopHeaders(outReq.Header)
	outReq.Header.Set("Connection", "Upgrade")
	outReq.Header.Set("Upgrade", reqUpType)
	if clientIP, _, err := net.SplitHostPort(r.RemoteAddr); err == nil {
		if prior := outReq.Header.Values("X-Forwarded-For"); len(prior) > 0 {
			clientIP = strings.Join(prior, ", ") + ", " + clientIP
		}
		outReq.Header.Set("X-Forwarded-For", clientIP)
	}

	backendConn, err := up.dial(r.Context())
	if err != nil {
		log.Printf("[ws] dial %s: %v (fallback: stdlib proxy)", up.describe(), err)
		kvsStats.wsUpgradeFallback.Add(1)
		return false
	}
	// 握手阶段用整体超时；成功切到隧道后清除（隧道自身按需设置 deadline）。
	_ = backendConn.SetDeadline(time.Now().Add(wsHandshakeTimeout))
	if err := outReq.Write(backendConn); err != nil {
		_ = backendConn.Close()
		log.Printf("[ws] write upgrade request to %s: %v (fallback: stdlib proxy)", up.describe(), err)
		kvsStats.wsUpgradeFallback.Add(1)
		return false
	}
	br := bufio.NewReaderSize(backendConn, wsUpstreamBufSize)
	res, err := http.ReadResponse(br, outReq)
	// 跳过中间 1xx（100 Continue 之类）：它们不是最终响应，不能回给客户端。
	for i := 0; err == nil && res.StatusCode >= 100 && res.StatusCode < 200 && res.StatusCode != http.StatusSwitchingProtocols && i < 8; i++ {
		_ = res.Body.Close()
		res, err = http.ReadResponse(br, outReq)
	}
	if err != nil {
		_ = backendConn.Close()
		log.Printf("[ws] read upgrade response from %s: %v (fallback: stdlib proxy)", up.describe(), err)
		kvsStats.wsUpgradeFallback.Add(1)
		return false
	}
	_ = backendConn.SetDeadline(time.Time{})

	if res.StatusCode != http.StatusSwitchingProtocols {
		// 后端没同意升级（例如 401/404/426）：按普通响应回写。请求已被消费，
		// 不能回退（回退会重发一次请求），这里自行完成响应并应用登录页修饰链。
		defer res.Body.Close()
		if up.mods != nil {
			if err := up.mods(res); err != nil {
				log.Printf("[ws] modify non-101 response from %s: %v", up.describe(), err)
			}
		}
		writeUpstreamResponse(w, res)
		return true
	}
	if !strings.EqualFold(res.Header.Get("Upgrade"), reqUpType) {
		_ = backendConn.Close()
		http.Error(w, fmt.Sprintf("backend tried to switch protocol %q when %q was requested",
			res.Header.Get("Upgrade"), reqUpType), http.StatusBadGateway)
		return true
	}

	clientConn, brw, err := hj.Hijack()
	if err != nil {
		_ = backendConn.Close()
		http.Error(w, "hijack failed: "+err.Error(), http.StatusInternalServerError)
		return true
	}
	// 101 响应原样回给客户端（Connection/Upgrade/Sec-WebSocket-Accept 都在里面）。
	if err := writeUpgradeResponse(clientConn, res); err != nil {
		_ = clientConn.Close()
		_ = backendConn.Close()
		log.Printf("[ws] write 101 to client %s: %v", r.RemoteAddr, err)
		return true
	}
	// 两侧已缓冲的字节都要转发：后端侧可能是 101 之后紧跟的数据，
	// 客户端侧是 http.Server 读握手时多读进来的首帧（stdlib 会丢，这里必须补上）。
	// 只要有一侧没整段写完，帧序就已经错位，这条隧道只能放弃（101 已发出，
	// 无法回退到 stdlib 路径）。
	drainedDown, errDown := drainBuffered(clientConn, br)
	if errDown != nil {
		_ = clientConn.Close()
		_ = backendConn.Close()
		log.Printf("[ws] drain pipelined bytes to backend %s: %v", up.describe(), errDown)
		return true
	}
	drainedUp, errUp := drainBuffered(backendConn, brw.Reader)
	if errUp != nil {
		_ = clientConn.Close()
		_ = backendConn.Close()
		log.Printf("[ws] drain pipelined bytes to client %s: %v", r.RemoteAddr, errUp)
		return true
	}

	mode := "generic"
	if spliceCandidate(clientConn) && spliceCandidate(backendConn) {
		mode = "splice"
		kvsStats.wsTunnelsSplice.Add(1)
	} else {
		kvsStats.wsTunnelsGeneric.Add(1)
	}
	kvsStats.wsTunnels.Add(1)
	kvsStats.wsTunnelBytesDown.Add(drainedDown)
	kvsStats.wsTunnelBytesUp.Add(drainedUp)
	log.Printf("[ws] tunnel open %s → %s (mode=%s, idle=%v, pipelined up/down=%d/%d)",
		r.RemoteAddr, up.describe(), mode, wsIdleTimeout, drainedUp, drainedDown)

	runWSTunnel(clientConn, backendConn)
	kvsStats.wsTunnelsClosed.Add(1)
	log.Printf("[ws] tunnel closed %s → %s", r.RemoteAddr, up.describe())
	return true
}

// runWSTunnel 双向搬运数据，直到任一方向结束或出错。
//
//   - 无空闲超时时（默认）：直接 io.Copy，裸 conn 之间命中内核 splice（socket↔socket）。
//   - 有空闲超时时：按 chunk 拷贝，每个方向自己维护读/写超时；本方向超时后若对方向
//     在等待期间有过数据，则说明隧道仍然活跃（只有单向流量的场景），继续等待；两个
//     方向都静默超过 wsIdleTimeout 才判定为空闲并断开。
//
// 两种情况都会打开 TCP keepalive，让内核发现黑洞对端（不需要用户态轮询，也不影响
// splice 快路径）。
func runWSTunnel(clientConn, backendConn net.Conn) {
	defer clientConn.Close()
	defer backendConn.Close()

	enableKeepAlive(clientConn)
	enableKeepAlive(backendConn)

	var upProgress, downProgress atomic.Int64 // 各方向的字节数，用于空闲判定
	errc := make(chan error, 2)
	go func() {
		n, err := copyTunnel(backendConn, clientConn, &upProgress, &downProgress)
		kvsStats.wsTunnelBytesUp.Add(n)
		closeWrite(backendConn)
		errc <- err
	}()
	go func() {
		n, err := copyTunnel(clientConn, backendConn, &downProgress, &upProgress)
		kvsStats.wsTunnelBytesDown.Add(n)
		closeWrite(clientConn)
		errc <- err
	}()

	first := <-errc
	if first != nil {
		// 出错（含空闲超时、对端停滞）：立即断开两端，避免另一半无限等待。
		_ = clientConn.Close()
		_ = backendConn.Close()
		log.Printf("[ws] tunnel teardown: %v", first)
	}
	<-errc
}

// 隧道收尾原因（供日志与测试判别）。
var (
	errTunnelIdle    = errors.New("tunnel idle")   // 两个方向都静默超过 2×wsIdleTimeout
	errTunnelStalled = errors.New("write stalled") // 对端长时间不读，写入无法推进
)

// copyTunnel 单向拷贝：dst <- src。mine/peers 是本方向与对方向的累计字节数，
// 仅用于空闲判定（见 runWSTunnel）。
//
// 空闲判定要点：io.CopyN 只在"拷满一个 chunk"或"读超时"时返回，所以窗口内的小流量
// 只能在超时那一刻被看到（n>0）。规则是：本方向窗口内有数据就直接续期；本方向完全
// 静默时再看对方向计数是否变化；两者都没有才记一次 strike，连续两个窗口静默才断开
// ——单向流量与慢速涓流不会误杀，黑洞连接仍会被清理。
//
// 读超时与写超时必须区别对待（这是本函数最要紧的一点）：
//
//   - 读超时（源侧无数据）：管道里没有半成品数据，续期是安全的，只影响"是否判空闲"。
//   - 写超时（对端长时间不读）：io.CopyN 返回前，已经从源读出、但还没写出去的那部分
//     会随内核 splice 管道（或用户态缓冲）一起被丢弃，而返回的 n 只统计真正写出去的
//     字节 —— 数据已经在源头被消费掉了。此时"续期继续"= 静默截断：隧道继续工作，
//     对端拿到的流里凭空少一段（temp/audit/loss 复现：8MB 流量静默丢 1.8MB）。
//     所以写超时一律致命：断开两端，让对端看到"连接被截断"这个明确信号。
//
// 两个 deadline 因此故意错开：写 = 读 + 宽限（下限 wsWriteGraceMin），超时返回时用
// "写 deadline 是否已到"来区分是谁超时。
func copyTunnel(dst, src net.Conn, mine, peers *atomic.Int64) (int64, error) {
	if wsIdleTimeout <= 0 || wsCopyChunk <= 0 {
		return copyConn(dst, src)
	}
	grace := wsIdleTimeout
	if grace < wsWriteGraceMin {
		grace = wsWriteGraceMin
	}
	var total int64
	strikes := 0
	for {
		peerBefore := peers.Load()
		readDL := time.Now().Add(wsIdleTimeout)
		writeDL := readDL.Add(grace)
		_ = src.SetReadDeadline(readDL)
		_ = dst.SetWriteDeadline(writeDL)
		n, err := io.CopyN(dst, src, wsCopyChunk)
		total += n
		if n > 0 {
			mine.Add(n)
		}
		switch {
		case err == nil:
			strikes = 0
			continue
		case errors.Is(err, io.EOF):
			return total, nil
		case errors.Is(err, os.ErrDeadlineExceeded):
			if !time.Now().Before(writeDL) {
				return total, fmt.Errorf("%w: peer read nothing for %v", errTunnelStalled, grace)
			}
			if n > 0 || peers.Load() != peerBefore {
				strikes = 0
				continue
			}
			if strikes++; strikes >= 2 {
				return total, fmt.Errorf("%w: both directions silent for %v", errTunnelIdle, 2*wsIdleTimeout)
			}
			continue
		default:
			return total, err
		}
	}
}

// enableKeepAlive 打开 TCP keepalive（unix socket 无此概念，直接跳过）。
func enableKeepAlive(c net.Conn) {
	tc, ok := c.(*net.TCPConn)
	if !ok {
		return
	}
	_ = tc.SetKeepAlive(true)
	_ = tc.SetKeepAliveConfig(net.KeepAliveConfig{Enable: true, Idle: 60 * time.Second, Interval: 15 * time.Second, Count: 4})
}

// copyConn 走 net 的快路径（splice/ReadFrom/WriteTo），不可用时用大缓冲用户态拷贝。
func copyConn(dst, src net.Conn) (int64, error) {
	buf := make([]byte, wsBufSize)
	return io.CopyBuffer(dst, src, buf)
}

// closeWrite 半关闭：让对端读到 EOF 而不用先把本端也关掉。
func closeWrite(c net.Conn) {
	type closeWriter interface{ CloseWrite() error }
	if cw, ok := c.(closeWriter); ok {
		_ = cw.CloseWrite()
		return
	}
	_ = c.Close()
}

// spliceCandidate 报告该 conn 是否可能命中内核 splice（裸 socket 类型）。
// 命中判定与 net 内部一致：*net.TCPConn / *net.UnixConn 才走 splice；
// TLS 包装层、其他实现都会退化为用户态拷贝。
func spliceCandidate(c net.Conn) bool {
	switch c.(type) {
	case *net.TCPConn, *net.UnixConn:
		return true
	}
	return false
}

// drainBuffered 把 bufio.Reader 里已经读进来的字节全部转发到对端。
// 返回转发的字节数与错误；只有整段写完才算成功——半截写出去等于把 WS 帧撕开，
// 会让隧道里出现无法恢复的错位，调用方必须放弃这条隧道（见 handleWSUpgrade）。
func drainBuffered(dst net.Conn, br *bufio.Reader) (int64, error) {
	n := br.Buffered()
	if n <= 0 {
		return 0, nil
	}
	buf := make([]byte, n)
	if _, err := io.ReadFull(br, buf); err != nil {
		return 0, fmt.Errorf("read buffered bytes: %w", err)
	}
	_ = dst.SetWriteDeadline(time.Now().Add(wsHandshakeTimeout))
	defer func() { _ = dst.SetWriteDeadline(time.Time{}) }()
	written, err := dst.Write(buf)
	if err != nil {
		return int64(written), fmt.Errorf("write buffered bytes: %w", err)
	}
	return int64(written), nil
}

// writeUpgradeResponse 把后端的 101 响应原样写给客户端。
func writeUpgradeResponse(w io.Writer, res *http.Response) error {
	var sb strings.Builder
	reason := strings.TrimSpace(strings.TrimPrefix(res.Status, fmt.Sprint(res.StatusCode)))
	if reason == "" {
		reason = http.StatusText(res.StatusCode)
	}
	fmt.Fprintf(&sb, "HTTP/1.1 %d %s\r\n", res.StatusCode, reason)
	// 101 无实体：实体相关的首部对客户端只有干扰。
	res.Header.Del("Content-Length")
	res.Header.Del("Transfer-Encoding")
	res.Header.Del("Trailer")
	if err := res.Header.Write(&sb); err != nil {
		return err
	}
	sb.WriteString("\r\n")
	_, err := io.WriteString(w, sb.String())
	return err
}

// writeUpstreamResponse 把（非 101 的）上游响应写回客户端，语义与 ReverseProxy 一致：
// 复制首部、原状态码、流式复制 body（未知长度时由 net/http 决定分块）。
func writeUpstreamResponse(w http.ResponseWriter, res *http.Response) {
	copyHeaders(w.Header(), res.Header)
	w.WriteHeader(res.StatusCode)
	_, _ = io.Copy(w, res.Body)
}

// isTokenASCII 报告 s 是否是非空的 ASCII 可见 token（与 stdlib 对 Upgrade 头的校验一致）。
func isTokenASCII(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < 0x21 || s[i] > 0x7e {
			return false
		}
	}
	return true
}

// removeHopByHopHeaders 与 net/http/httputil 的同名函数一致：先按 Connection
// 头列出的 token 删除，再删除固定集合。
func removeHopByHopHeaders(h http.Header) {
	for _, f := range h.Values("Connection") {
		for _, sf := range strings.Split(f, ",") {
			if sf = textproto.TrimString(sf); sf != "" {
				h.Del(sf)
			}
		}
	}
	for _, f := range hopHeaders {
		h.Del(f)
	}
}

// hopHeaders 是 RFC 2616 §13.5.1 的固定 hop-by-hop 首部集合。
var hopHeaders = []string{
	"Connection",
	"Proxy-Connection",
	"Keep-Alive",
	"Proxy-Authenticate",
	"Proxy-Authorization",
	"Te",
	"Trailer",
	"Transfer-Encoding",
	"Upgrade",
}
