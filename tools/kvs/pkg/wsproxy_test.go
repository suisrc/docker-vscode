package pkg

// wsproxy_test.go — 升级隧道与空闲/停滞判定的回归测试。
//
// 这里的三条规则来自实测（temp/audit/loss）：
//   - 对端长时间不读（写超时）：内核 splice 管道里"已从源读出但没写出去"的字节会丢，
//     必须判为致命并断开，绝不能续期继续（否则是静默截断）；
//   - 慢但在前进的对端：不能被误杀；
//   - 两个方向都静默：按 2×idle 断开。

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// withTunnelKnobs 临时改包级隧道参数，测试结束恢复。
func withTunnelKnobs(t *testing.T, idle time.Duration, grace time.Duration, chunk int64) {
	t.Helper()
	oldIdle, oldGrace, oldChunk := wsIdleTimeout, wsWriteGraceMin, wsCopyChunk
	wsIdleTimeout, wsWriteGraceMin, wsCopyChunk = idle, grace, chunk
	t.Cleanup(func() { wsIdleTimeout, wsWriteGraceMin, wsCopyChunk = oldIdle, oldGrace, oldChunk })
}

// tcpPair 建立一对真实 TCP 连接（本机随机端口），并按需缩小两端的收发缓冲，
// 这样小块数据也能触发"写阻塞"（否则内核缓冲会把 chunk 全部吞掉，测不出停滞）。
func tcpPair(t *testing.T, sndbuf, rcvbuf int) (held, peer net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	type res struct {
		c   net.Conn
		err error
	}
	ch := make(chan res, 1)
	go func() {
		c, err := net.Dial("tcp", ln.Addr().String())
		ch <- res{c, err}
	}()
	held, err = ln.Accept()
	if err != nil {
		t.Fatalf("accept: %v", err)
	}
	r := <-ch
	if r.err != nil {
		t.Fatalf("dial: %v", r.err)
	}
	peer = r.c
	if tc, ok := held.(*net.TCPConn); ok {
		if sndbuf > 0 {
			_ = tc.SetWriteBuffer(sndbuf)
		}
		if rcvbuf > 0 {
			_ = tc.SetReadBuffer(rcvbuf)
		}
	}
	if tc, ok := peer.(*net.TCPConn); ok && rcvbuf > 0 {
		_ = tc.SetReadBuffer(rcvbuf)
	}
	t.Cleanup(func() { _ = held.Close(); _ = peer.Close() })
	return held, peer
}

// TestCopyTunnelWriteStallIsFatal 对端不读时，拷贝必须在写超时处判为致命（errTunnelStalled），
// 而不是"判活续期"——后者会静默丢掉 splice 管道里已读出的字节。
func TestCopyTunnelWriteStallIsFatal(t *testing.T) {
	withTunnelKnobs(t, 200*time.Millisecond, 400*time.Millisecond, 1<<20)
	src, up := tcpPair(t, 0, 4<<20)
	dst, sink := tcpPair(t, 8<<10, 8<<10)
	_ = sink.SetReadDeadline(time.Now().Add(5 * time.Second)) // 宿端始终不读

	go func() { // 源侧灌 2MB，保证源队列里有远超宿端缓冲的数据
		buf := make([]byte, 64<<10)
		for i := 0; i < 32; i++ {
			if _, err := up.Write(buf); err != nil {
				return
			}
		}
	}()
	time.Sleep(200 * time.Millisecond)

	var mine, peers atomic.Int64
	start := time.Now()
	n, err := copyTunnel(dst, src, &mine, &peers)
	elapsed := time.Since(start)
	if err == nil {
		t.Fatalf("对端不读时拷贝未报错（n=%d，elapsed=%v）——会静默丢字节", n, elapsed)
	}
	if !errors.Is(err, errTunnelStalled) {
		t.Fatalf("期望 errTunnelStalled，得到 %v", err)
	}
	if elapsed > 3*time.Second {
		t.Fatalf("判定过慢：%v", elapsed)
	}
	t.Logf("写停滞在 %v 内判为致命：n=%d err=%v", elapsed.Round(time.Millisecond), n, err)
}

// TestCopyTunnelSlowReaderSurvives 慢但在前进的对端不能被误杀，数据也不能少。
// 两端缓冲都调小（8KB），保证写侧每轮都会阻塞一下（真正走到 deadline 逻辑）；
// 宿端按小步慢读，但只要在写宽限内持续推进，就应完整走完。
func TestCopyTunnelSlowReaderSurvives(t *testing.T) {
	withTunnelKnobs(t, 100*time.Millisecond, 5*time.Second, 32<<10)
	const total = 128 << 10
	src, up := tcpPair(t, 0, 4<<20)
	dst, sink := tcpPair(t, 8<<10, 8<<10)

	var got int64
	drainDone := make(chan struct{})
	go func() { // 慢速读者：32KB/次，每次歇 1ms
		defer close(drainDone)
		buf := make([]byte, 32<<10)
		for got < total {
			_ = sink.SetReadDeadline(time.Now().Add(3 * time.Second))
			n, err := sink.Read(buf)
			got += int64(n)
			if err != nil {
				return
			}
			time.Sleep(time.Millisecond)
		}
	}()
	go func() { // 源侧灌 total 字节后关闭 → 拷贝应以 EOF 正常结束
		buf := make([]byte, 64<<10)
		for sent := 0; sent < total; {
			m := len(buf)
			if left := total - sent; left < m {
				m = left
			}
			w, err := up.Write(buf[:m])
			if err != nil {
				return
			}
			sent += w
		}
		_ = up.Close()
	}()

	var mine, peers atomic.Int64
	copyStart := time.Now()
	n, err := copyTunnel(dst, src, &mine, &peers)
	t.Logf("拷贝 %d 字节耗时 %v", n, time.Since(copyStart).Round(time.Millisecond))
	if err != nil {
		t.Fatalf("慢速读者被误杀：%v（已拷 %d/%d）", err, n, total)
	}
	if n != total {
		t.Fatalf("拷贝字节数 %d，期望 %d", n, total)
	}
	<-drainDone
	t.Logf("对端收到 %d 字节，总耗时 %v", got, time.Since(copyStart).Round(time.Millisecond))
	if got != total {
		t.Fatalf("对端收到 %d，期望 %d（数据有丢失/截断）", got, total)
	}
}

// TestCopyTunnelIdleDetect 两个方向都静默时，约 2×idle 断开。
func TestCopyTunnelIdleDetect(t *testing.T) {
	const idle = 150 * time.Millisecond
	withTunnelKnobs(t, idle, 5*time.Second, 128<<10)
	src, _ := tcpPair(t, 0, 0)
	dst, _ := tcpPair(t, 0, 0)

	var mine, peers atomic.Int64
	start := time.Now()
	n, err := copyTunnel(dst, src, &mine, &peers)
	elapsed := time.Since(start)
	if !errors.Is(err, errTunnelIdle) {
		t.Fatalf("期望 errTunnelIdle，得到 %v (n=%d)", err, n)
	}
	if elapsed < 2*idle || elapsed > 2*time.Second {
		t.Fatalf("空闲判定耗时 %v，期望约 %v", elapsed, 2*idle)
	}
}

// halfWriteConn 模拟"只写出去一半就报错"的对端（半截帧 = 隧道错位）。
type halfWriteConn struct {
	net.Conn
	half int
}

func (c *halfWriteConn) Write(p []byte) (int, error) {
	n := c.half
	if n > len(p) {
		n = len(p)
	}
	return n, errors.New("短写")
}

func (c *halfWriteConn) SetWriteDeadline(time.Time) error { return nil }
func (c *halfWriteConn) SetReadDeadline(time.Time) error  { return nil }

// TestDrainBufferedShortWriteAborts 半截写必须返回错误（调用方据此放弃隧道）。
func TestDrainBufferedShortWriteAborts(t *testing.T) {
	br := bufio.NewReaderSize(bytes.NewReader([]byte("PIPELINED0")), 32)
	if _, err := br.Peek(10); err != nil {
		t.Fatalf("peek: %v", err)
	}
	got, err := drainBuffered(&halfWriteConn{half: 4}, br)
	if err == nil {
		t.Fatalf("半截写未报错（写了 %d 字节，缓冲区 10 字节）", got)
	}
	if got != 4 {
		t.Fatalf("返回字节数 %d，期望 4", got)
	}
	// 正常情况：整段写完、无错误。
	br2 := bufio.NewReaderSize(bytes.NewReader([]byte("PIPELINED0")), 32)
	if _, err := br2.Peek(10); err != nil {
		t.Fatalf("peek: %v", err)
	}
	c1, c2 := net.Pipe()
	defer c1.Close()
	defer c2.Close()
	go func() { _, _ = io.Copy(io.Discard, c2) }()
	if n, err := drainBuffered(c1, br2); err != nil || n != 10 {
		t.Fatalf("完整写：n=%d err=%v，期望 10/nil", n, err)
	}
}

// TestWSUpgradeFastPathKeepsPipelinedFrame 端到端：握手与首帧在同一次 write 里发出
// （流水线首帧）时，快路径必须把首帧交给后端——stdlib 的升级分支会静默丢掉它。
func TestWSUpgradeFastPathKeepsPipelinedFrame(t *testing.T) {
	payload := []byte("FIRST-FRAME-PAYLOAD")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	gotCh := make(chan []byte, 1)
	go func() { // 假后端：回 101，然后把客户端后续字节原样回显
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			gotCh <- nil
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		buf := make([]byte, len(payload))
		n, err := io.ReadFull(br, buf)
		if err != nil {
			gotCh <- nil
			return
		}
		gotCh <- append([]byte(nil), buf[:n]...)
		_, _ = c.Write(buf[:n])
	}()

	b := newBackend("/ws", "ws://"+ln.Addr().String())
	if b == nil {
		t.Fatal("newBackend returned nil")
	}
	srv := httptest.NewServer(CreateBackendHandler(*b, nil, false))
	defer srv.Close()

	spliceBefore := kvsStats.wsTunnelsSplice.Load()
	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	handshake := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", srv.Listener.Addr().String())
	if _, err := conn.Write(append([]byte(handshake), payload...)); err != nil {
		t.Fatalf("write handshake+frame: %v", err)
	}

	br := bufio.NewReader(conn)
	line, err := br.ReadString('\n')
	if err != nil || !strings.Contains(line, " 101 ") {
		t.Fatalf("期望 101 响应，得到 %q (%v)", line, err)
	}
	for {
		l, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("读 101 首部: %v", err)
		}
		if l == "\r\n" || l == "\n" {
			break
		}
	}
	select {
	case got := <-gotCh:
		if !bytes.Equal(got, payload) {
			t.Fatalf("后端收到 %q，期望 %q（流水线首帧丢失）", got, payload)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("后端没收到流水线首帧")
	}
	echo := make([]byte, len(payload))
	if _, err := io.ReadFull(br, echo); err != nil {
		t.Fatalf("读回显: %v", err)
	}
	if !bytes.Equal(echo, payload) {
		t.Fatalf("回显 %q，期望 %q（双向隧道不通）", echo, payload)
	}
	if kvsStats.wsTunnelsSplice.Load() <= spliceBefore {
		t.Fatal("未走 splice 快路径（计数器未增加）")
	}
}

// TestWSUpgradeFallbackWhenBackendDown 后端连不上时必须回退 stdlib（观察到 502），
// 并计入 fallback 计数——保证"不接管"的路径没有丢行为。
func TestWSUpgradeFallbackWhenBackendDown(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := ln.Addr().String()
	_ = ln.Close() // 端口没人监听

	b := newBackend("/ws", "ws://"+addr)
	if b == nil {
		t.Fatal("newBackend returned nil")
	}
	srv := httptest.NewServer(CreateBackendHandler(*b, nil, false))
	defer srv.Close()

	before := kvsStats.wsUpgradeFallback.Load()
	req, err := http.NewRequest("GET", srv.URL+"/ws", nil)
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	res, err := srv.Client().Do(req)
	if err != nil {
		t.Fatalf("do: %v", err)
	}
	defer res.Body.Close()
	if res.StatusCode != http.StatusBadGateway {
		t.Fatalf("状态码 %d，期望 502", res.StatusCode)
	}
	if kvsStats.wsUpgradeFallback.Load() <= before {
		t.Fatal("fallback 计数未增加")
	}
}

// TestWSUpgradeStdlibDropsPipelinedFrame 固化"对比面"：同样的流水线首帧，走
// ws_proxy=stdlib 时后端收不到（stdlib handleUpgradeResponse 不消费 brw.Reader）。
//
// 本用例断言的是 **stdlib 的既有行为**（Go 1.26.5 实测）。将来若标准库修好这一点，
// 本用例会失败——那时删掉即可（快路径的其它收益：splice、空闲超时，仍然成立）。
func TestWSUpgradeStdlibDropsPipelinedFrame(t *testing.T) {
	payload := []byte("FIRST-FRAME-PAYLOAD")
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	defer ln.Close()
	gotCh := make(chan []byte, 1)
	go func() {
		c, err := ln.Accept()
		if err != nil {
			return
		}
		defer c.Close()
		br := bufio.NewReader(c)
		if _, err := http.ReadRequest(br); err != nil {
			gotCh <- nil
			return
		}
		_, _ = io.WriteString(c, "HTTP/1.1 101 Switching Protocols\r\nConnection: Upgrade\r\nUpgrade: websocket\r\n\r\n")
		_ = c.SetReadDeadline(time.Now().Add(1500 * time.Millisecond))
		buf := make([]byte, len(payload))
		if _, err := io.ReadFull(br, buf); err != nil {
			gotCh <- nil // 超时/EOF：首帧没到
			return
		}
		gotCh <- append([]byte(nil), buf...)
	}()

	SetWSTunnelOptions(wsProxyStdlib, 0) // 切到 stdlib 升级分支
	t.Cleanup(func() { SetWSTunnelOptions(wsProxyAuto, 0) })

	b := newBackend("/ws", "ws://"+ln.Addr().String())
	if b == nil {
		t.Fatal("newBackend returned nil")
	}
	srv := httptest.NewServer(CreateBackendHandler(*b, nil, false))
	defer srv.Close()

	conn, err := net.Dial("tcp", srv.Listener.Addr().String())
	if err != nil {
		t.Fatalf("dial proxy: %v", err)
	}
	defer conn.Close()
	handshake := fmt.Sprintf("GET /ws HTTP/1.1\r\nHost: %s\r\nUpgrade: websocket\r\n"+
		"Connection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n"+
		"Sec-WebSocket-Version: 13\r\n\r\n", srv.Listener.Addr().String())
	if _, err := conn.Write(append([]byte(handshake), payload...)); err != nil {
		t.Fatalf("write handshake+frame: %v", err)
	}
	select {
	case got := <-gotCh:
		if got != nil {
			t.Fatalf("stdlib 路径竟然收到了流水线首帧 %q —— 标准库行为已变，可删掉本用例", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("后端既没收到首帧也没超时，用例无法判定")
	}
}
