package pkg

// relay://vscode（tunnel 模型）的端到端测试：用裸 TCP 连接扮演协议里的
// 三方（worker 复用帧通道 / client 原始字节管道），覆盖注册、鉴权、配对、
// 背压、探测与生命周期。

import (
	"bufio"
	"bytes"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"
)

// ---- 裸 TCP 帮手 ----

func vscGet(t *testing.T, addr, req string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, err := net.Dial("tcp", addr)
	if err != nil {
		t.Fatalf("dial %s: %v", addr, err)
	}
	if _, err := conn.Write([]byte(req)); err != nil {
		t.Fatalf("write request: %v", err)
	}
	return conn, bufio.NewReader(conn)
}

func vscReadHead(t *testing.T, conn net.Conn, br *bufio.Reader) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var b strings.Builder
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			t.Fatalf("read response head: %v (got %q)", err, b.String())
		}
		b.WriteString(line)
		if line == "\r\n" {
			return b.String()
		}
	}
}

func vscStatusCode(t *testing.T, head string) int {
	t.Helper()
	var code int
	if _, err := fmt.Sscanf(head, "HTTP/1.1 %d", &code); err != nil {
		t.Fatalf("bad status line in %q: %v", head, err)
	}
	return code
}

func vscReadRaw(t *testing.T, conn net.Conn, br *bufio.Reader, want string) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var got []byte
	buf := make([]byte, 512)
	for !strings.Contains(string(got), want) {
		n, err := br.Read(buf)
		got = append(got, buf[:n]...)
		if err != nil {
			t.Fatalf("read raw: %v (got %q, want %q)", err, string(got), want)
		}
	}
	return string(got)
}

// ---- WS 帧帮手（worker 角色：发掩码帧，收不掩码帧） ----

func vscWsMaskFrame(op byte, payload []byte) []byte {
	var hdr []byte
	n := len(payload)
	switch {
	case n < 126:
		hdr = []byte{0x80 | op, 0x80 | byte(n)}
	case n <= 0xFFFF:
		hdr = []byte{0x80 | op, 0x80 | 126, byte(n >> 8), byte(n)}
	default:
		hdr = []byte{0x80 | op, 0x80 | 127, 0, 0, 0, 0,
			byte(n >> 24), byte(n >> 16), byte(n >> 8), byte(n)}
	}
	mask := []byte{1, 2, 3, 4}
	masked := make([]byte, len(payload))
	for i, b := range payload {
		masked[i] = b ^ mask[i&3]
	}
	return append(append(hdr, mask...), masked...)
}

func vscWsSendJSONE(conn net.Conn, v any) error {
	b, err := json.Marshal(v)
	if err != nil {
		return err
	}
	_, err = conn.Write(vscWsMaskFrame(0x1, b))
	return err
}

func vscWsSendJSON(t *testing.T, conn net.Conn, v any) {
	t.Helper()
	if err := vscWsSendJSONE(conn, v); err != nil {
		t.Fatalf("send control frame: %v", err)
	}
}

func vscWsSendData(t *testing.T, conn net.Conn, id, payload string) {
	t.Helper()
	frame := append([]byte{byte(len(id))}, []byte(id)...)
	frame = append(frame, []byte(payload)...)
	if _, err := conn.Write(vscWsMaskFrame(0x2, frame)); err != nil {
		t.Fatalf("send data frame: %v", err)
	}
}

// vscWsRecvE reads one frame, transparently answering WS ping with pong.
func vscWsRecvE(conn net.Conn, br *bufio.Reader) (byte, []byte, error) {
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	var hdr [2]byte
	if _, err := io.ReadFull(br, hdr[:]); err != nil {
		return 0, nil, err
	}
	op := hdr[0] & 0x0f
	n := uint64(hdr[1] & 0x7f)
	switch n {
	case 126:
		var ext [2]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = uint64(binary.BigEndian.Uint16(ext[:]))
	case 127:
		var ext [8]byte
		if _, err := io.ReadFull(br, ext[:]); err != nil {
			return 0, nil, err
		}
		n = binary.BigEndian.Uint64(ext[:])
	}
	payload := make([]byte, n)
	if _, err := io.ReadFull(br, payload); err != nil {
		return 0, nil, err
	}
	if op == 0x9 { // ping → pong（回显负载）
		if _, err := conn.Write(vscWsMaskFrame(0xA, payload)); err != nil {
			return 0, nil, err
		}
		return vscWsRecvE(conn, br)
	}
	if op == 0xA {
		return vscWsRecvE(conn, br)
	}
	return op, payload, nil
}

func vscWsRecv(t *testing.T, conn net.Conn, br *bufio.Reader) (byte, []byte) {
	t.Helper()
	op, payload, err := vscWsRecvE(conn, br)
	if err != nil {
		t.Fatalf("read ws frame: %v", err)
	}
	return op, payload
}

func vscWsRecvText(t *testing.T, conn net.Conn, br *bufio.Reader) map[string]any {
	t.Helper()
	op, payload := vscWsRecv(t, conn, br)
	if op != 0x1 {
		t.Fatalf("expected text frame, got opcode 0x%x", op)
	}
	var m map[string]any
	if err := json.Unmarshal(payload, &m); err != nil {
		t.Fatalf("control json: %v (%q)", err, payload)
	}
	return m
}

func vscWsRecvData(t *testing.T, conn net.Conn, br *bufio.Reader) string {
	t.Helper()
	op, payload := vscWsRecv(t, conn, br)
	if op != 0x2 {
		t.Fatalf("expected binary frame, got opcode 0x%x (%q)", op, payload)
	}
	if len(payload) < 1 {
		t.Fatal("empty data frame")
	}
	return string(payload[1+payload[0]:])
}

// ---- 角色帮手 ----

// vscRegisterWorker registers a worker channel and consumes the 101 head plus
// the ready frame (returns it for assertion).
func vscRegisterWorker(t *testing.T, addr, path, name, triple string) (net.Conn, *bufio.Reader, map[string]any) {
	t.Helper()
	conn, br := vscGet(t, addr, fmt.Sprintf(
		"GET %s?kind=worker&name=%s&tkn=%s HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n",
		path, name, triple))
	if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 101 {
		t.Fatalf("worker registration got %d, want 101", code)
	}
	ready := vscWsRecvText(t, conn, br)
	if ready["type"] != "ready" {
		t.Fatalf("first control frame = %v, want ready", ready)
	}
	return conn, br, ready
}

// vscConnectClient opens a client leg in upgrade mode (the relay answers 101)
// with early payload bytes appended after the request head.
func vscConnectClient(t *testing.T, addr, path, tkn, early string) (net.Conn, *bufio.Reader) {
	t.Helper()
	head := fmt.Sprintf("GET %s?tkn=%s&foo=bar HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", path, tkn)
	conn, br := vscGet(t, addr, head+early)
	resp := vscReadHead(t, conn, br)
	if code := vscStatusCode(t, resp); code != 101 {
		t.Fatalf("client connect got %d, want 101", code)
	}
	if !strings.Contains(resp, "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=") {
		t.Fatalf("101 head missing correct accept: %q", resp)
	}
	return conn, br
}

func vscStatus(t *testing.T, addr string) map[string]any {
	t.Helper()
	conn, br := vscGet(t, addr, "GET /status HTTP/1.1\r\nHost: r\r\nConnection: close\r\n\r\n")
	defer conn.Close()
	if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 200 {
		t.Fatalf("status got %d, want 200", code)
	}
	body, _ := io.ReadAll(br)
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("status body: %v (%q)", err, body)
	}
	return m
}

// ---- 测试 ----

func TestVscRelayTunnelEndToEnd(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	workerConn, workerBr, ready := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	defer workerConn.Close()
	if ready["protocol"].(float64) != 2 || ready["tunnelId"] != nil || ready["name"] != "h1" {
		t.Fatalf("ready frame = %v", ready)
	}

	// client 接入（升级模式，携带早到数据；验证中继代答 101 且 accept 正确）。
	clientConn, clientBr := vscConnectClient(t, addr, "/tunnels/ws", "secret.dev2.x", "client-early")
	defer clientConn.Close()

	// worker 收到 open（addr/path），随后是原始请求头（query 原样保留）与早到数据。
	open := vscWsRecvText(t, workerConn, workerBr)
	if open["type"] != "open" || open["id"] == "" || !strings.Contains(open["addr"].(string), "127.0.0.1") || open["path"] != "/tunnels/ws" {
		t.Fatalf("open frame = %v", open)
	}
	sid := open["id"].(string)
	if head := vscWsRecvData(t, workerConn, workerBr); !strings.HasPrefix(head, "GET /tunnels/ws?tkn=secret.dev2.x&foo=bar HTTP/1.1") {
		t.Fatalf("forwarded head = %q", head)
	}
	if got := vscWsRecvData(t, workerConn, workerBr); got != "client-early" {
		t.Fatalf("worker did not receive client early data: %q", got)
	}

	// 双向原始字节。
	vscWsSendData(t, workerConn, sid, "host-early")
	if got := vscReadRaw(t, clientConn, clientBr, "host-early"); !strings.Contains(got, "host-early") {
		t.Fatalf("client did not receive host data: %q", got)
	}
	if _, err := fmt.Fprint(clientConn, "c2h-live"); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if got := vscWsRecvData(t, workerConn, workerBr); got != "c2h-live" {
		t.Fatalf("worker did not receive client data: %q", got)
	}

	// /status：1 个隧道在线、1 个活跃会话。
	st := vscStatus(t, addr)
	if st["tunnelSessions"].(float64) != 1 {
		t.Fatalf("tunnelSessions = %v", st["tunnelSessions"])
	}
	tunnels := st["tunnels"].([]any)
	if len(tunnels) != 1 {
		t.Fatalf("tunnels = %v", tunnels)
	}
	tun := tunnels[0].(map[string]any)
	if tun["online"] != true || tun["device"] != "dev1" || tun["name"] != "h1" || tun["path"] != "/tunnels/ws" || tun["sessions"].(float64) != 1 {
		t.Fatalf("tunnel status = %v", tun)
	}

	// worker 关会话 → client 读到 EOF。
	vscWsSendJSON(t, workerConn, map[string]any{"type": "close", "id": sid, "reason": "done"})
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := clientBr.Read(make([]byte, 16)); err == nil {
		t.Fatal("client connection should be closed after the worker closed the session")
	}
}

func TestVscRelayClientRouting(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	cases := []struct {
		title string
		req   string
		want  int
	}{
		{"no routing intent", "GET /tunnels/ws HTTP/1.1\r\nHost: r\r\n\r\n", 404},
		{"unknown token", "GET /tunnels/ws?tkn=ghost HTTP/1.1\r\nHost: r\r\n\r\n", 403},
		{"unknown token (triple)", "GET /tunnels/ws?tkn=ghost.dev1 HTTP/1.1\r\nHost: r\r\n\r\n", 403},
	}
	for _, tc := range cases {
		conn, br := vscGet(t, addr, tc.req)
		code := vscStatusCode(t, vscReadHead(t, conn, br))
		conn.Close()
		if code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.title, code, tc.want)
		}
	}

	// 离线隧道（占用保留期内）→ 503 而非 403。
	workerConn, _, _ := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	workerConn.Close()
	time.Sleep(50 * time.Millisecond) // 等中继侧通道拆完
	{
		conn, br := vscGet(t, addr, "GET /tunnels/ws?tkn=secret HTTP/1.1\r\nHost: r\r\n\r\n")
		if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 503 {
			t.Fatalf("offline tunnel got %d, want 503", code)
		}
		conn.Close()
	}

	// token= 别名 + 三元组按第一段路由：重连后 client 可达。
	workerConn2, workerBr2, _ := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	defer workerConn2.Close()
	clientConn, clientBr := vscConnectClient(t, addr, "/tunnels/ws", "secret.dev2.x", "")
	defer clientConn.Close()
	open := vscWsRecvText(t, workerConn2, workerBr2)
	if open["type"] != "open" {
		t.Fatalf("open frame = %v", open)
	}
	vscWsSendData(t, workerConn2, open["id"].(string), "pong")
	if got := vscReadRaw(t, clientConn, clientBr, "pong"); !strings.Contains(got, "pong") {
		t.Fatalf("alias/triple routing broken: %q", got)
	}
}

func TestVscRelayWorkerValidation(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	cases := []struct {
		title string
		req   string
		want  int
	}{
		{"worker without token", "GET /tunnels/ws?kind=worker HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", 401},
		{"worker without deviceId", "GET /tunnels/ws?kind=worker&tkn=secret HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", 400},
		{"worker with bad deviceId", "GET /tunnels/ws?kind=worker&tkn=secret.b%40d HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n", 400},
		{"worker without upgrade", "GET /tunnels/ws?kind=worker&tkn=secret.dev1 HTTP/1.1\r\nHost: r\r\n\r\n", 400},
		{"unknown path", "GET /whatever HTTP/1.1\r\nHost: r\r\n\r\n", 404},
	}
	for _, tc := range cases {
		conn, br := vscGet(t, addr, tc.req)
		code := vscStatusCode(t, vscReadHead(t, conn, br))
		conn.Close()
		if code != tc.want {
			t.Errorf("%s: got %d, want %d", tc.title, code, tc.want)
		}
	}

	// 设备独占：同设备第二条隧道 409。
	aConn, _, _ := vscRegisterWorker(t, addr, "/a", "ha", "aaa.dev3")
	defer aConn.Close()
	bConn, _, _ := vscRegisterWorker(t, addr, "/b", "hb", "bbb.dev4")
	defer bConn.Close()
	{
		conn, br := vscGet(t, addr, "GET /c?kind=worker&tkn=ccc.dev3 HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
		if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 409 {
			t.Fatalf("device conflict got %d, want 409", code)
		}
		conn.Close()
	}

	// 同 token 重注册：旧通道被顶掉（EOF），旧会话一起结束，新通道可用。
	oldConn, oldBr, _ := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	clientConn, _ := vscConnectClient(t, addr, "/tunnels/ws", "secret", "")
	open := vscWsRecvText(t, oldConn, oldBr)
	if open["type"] != "open" {
		t.Fatalf("open frame = %v", open)
	}
	vscWsRecvData(t, oldConn, oldBr) // 请求头帧（排空缓冲，之后才能断言连接关闭）
	newConn, newBr, _ := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	defer newConn.Close()
	_ = oldConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := oldBr.ReadByte(); err == nil {
		t.Fatal("old channel should be closed after re-registration")
	}
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := clientConn.Read(make([]byte, 16)); err == nil {
		t.Fatal("session on the old channel should be closed after re-registration")
	}

	// 新通道可用：open + 请求头如期到达。
	clientConn2, _ := vscConnectClient(t, addr, "/tunnels/ws", "secret", "")
	defer clientConn2.Close()
	if open2 := vscWsRecvText(t, newConn, newBr); open2["type"] != "open" {
		t.Fatalf("new channel missed open: %v", open2)
	}
	if head := vscWsRecvData(t, newConn, newBr); !strings.HasPrefix(head, "GET /tunnels/ws?tkn=secret&foo=bar") {
		t.Fatalf("forwarded head = %q", head)
	}
}

func TestVscRelayProbe(t *testing.T) {
	rel := newVscRelay()
	rel.probeInterval, rel.probeMaxMiss = 40*time.Millisecond, 2
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	liveConn, liveBr, _ := vscRegisterWorker(t, addr, "/live", "hl", "live.dev1")
	defer liveConn.Close()
	deadConn, deadBr, _ := vscRegisterWorker(t, addr, "/dead", "hd", "dead.dev2")
	defer deadConn.Close()

	// live worker 的探测应答协程：连续应答 version 帧（错误延后上报）。
	liveErr := make(chan error, 1)
	done := make(chan struct{})
	defer close(done)
	go func() {
		for {
			op, payload, err := vscWsRecvE(liveConn, liveBr)
			if err != nil {
				select {
				case liveErr <- err:
				default:
				}
				return
			}
			if op == 0x1 && strings.Contains(string(payload), `"version"`) {
				if err := vscWsSendJSONE(liveConn, map[string]any{"type": "version", "device": "dev1", "protocol": 2, "version": "1.2.3"}); err != nil {
					select {
					case liveErr <- err:
					default:
					}
					return
				}
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	// dead worker 不应答：读走第一个探测帧后保持沉默。
	if op, payload := vscWsRecv(t, deadConn, deadBr); op != 0x1 || !strings.Contains(string(payload), `"version"`) {
		t.Fatalf("expected version probe, got op 0x%x %q", op, payload)
	}
	time.Sleep(250 * time.Millisecond) // > 3 个探测周期

	st := vscStatus(t, addr)
	online := map[string]bool{}
	version := map[string]string{}
	for _, tv := range st["tunnels"].([]any) {
		tun := tv.(map[string]any)
		online[tun["device"].(string)] = tun["online"].(bool)
		if v, ok := tun["version"].(string); ok {
			version[tun["device"].(string)] = v
		}
	}
	if !online["dev1"] || online["dev2"] {
		t.Fatalf("probe liveness wrong: %v", online)
	}
	if version["dev1"] != "1.2.3" {
		t.Fatalf("live tunnel version = %q, want 1.2.3", version["dev1"])
	}

	// 超时通道已判死：挂在它上面的 client 会话被拒绝（离线 503）。
	deadClient, deadClientBr := vscGet(t, addr, "GET /dead?tkn=dead HTTP/1.1\r\nHost: r\r\n\r\n")
	if code := vscStatusCode(t, vscReadHead(t, deadClient, deadClientBr)); code != 503 {
		t.Fatalf("dead tunnel client got %d, want 503", code)
	}
	deadClient.Close()

	select {
	case err := <-liveErr:
		t.Fatalf("live responder failed: %v", err)
	default:
	}
}

func TestVscRelayBackpressure(t *testing.T) {
	rel := newVscRelay()
	rel.sinkHighWater = 4 << 10
	tun := &vscTunnel{token: "secret", device: "dev1", name: "h1", sessions: map[string]*vscSession{}}
	// 假通道：控制帧落到 net.Pipe，测试直接读帧断言（不依赖真实 socket）。
	w, r := net.Pipe()
	defer w.Close()
	defer r.Close()
	ch := &vscChannel{relay: rel, tun: tun, device: "dev1",
		ws: &wsConn{conn: w, br: bufio.NewReader(w)}, done: make(chan struct{})}
	tun.ch = ch

	// client 是 net.Pipe 的另一端：不读它，写入立即阻塞 → staging 确定性增长。
	clientPipe, clientEnd := net.Pipe()
	defer clientPipe.Close()
	defer clientEnd.Close()
	s := &vscSession{id: "testses", tun: tun, ch: ch, client: clientPipe, upgrade: true}
	s.cond = sync.NewCond(&s.mu)
	rel.mu.Lock()
	tun.sessions[s.id] = s
	rel.sessions++
	rel.mu.Unlock()
	go s.writeLoop()

	// 控制帧收集协程：模拟真实 worker 边转发边读通道（pause/resume 异步分流）。
	pauseCh := make(chan map[string]any, 4)
	resumeCh := make(chan map[string]any, 4)
	done := make(chan struct{})
	defer close(done)
	go func() {
		br := bufio.NewReader(r)
		for {
			op, payload, err := vscWsRecvE(r, br)
			if err != nil {
				return
			}
			if op != 0x1 {
				continue
			}
			var m map[string]any
			if json.Unmarshal(payload, &m) != nil {
				continue
			}
			switch m["type"] {
			case "pause":
				pauseCh <- m
			case "resume":
				resumeCh <- m
			}
			select {
			case <-done:
				return
			default:
			}
		}
	}()

	// worker 侧涌入 10KB（client 不读）：超 4KB 高水位 → 请 worker 停发。
	go func() {
		for i := 0; i < 10; i++ {
			s.push(bytes.Repeat([]byte("x"), 1024))
		}
	}()
	select {
	case m := <-pauseCh:
		if m["id"] != s.id {
			t.Fatalf("pause for wrong session: %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay never asked the worker to pause the flooding session")
	}

	// client 排空 → 中继放行（resume）。
	drained := 0
	buf := make([]byte, 4096)
	for drained < 10<<10 {
		_ = clientEnd.SetReadDeadline(time.Now().Add(3 * time.Second))
		n, err := clientEnd.Read(buf)
		drained += n
		if err != nil {
			t.Fatalf("drain: %v (at %d)", err, drained)
		}
	}
	select {
	case m := <-resumeCh:
		if m["id"] != s.id {
			t.Fatalf("resume for wrong session: %v", m)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("relay never resumed the drained session")
	}
}

func TestVscRelaySinkOverflowKill(t *testing.T) {
	rel := newVscRelay()
	rel.sinkCap = 8 << 10
	tun := &vscTunnel{token: "secret", device: "dev1", name: "h1", sessions: map[string]*vscSession{}}
	w, r := net.Pipe()
	defer w.Close()
	defer r.Close()
	ch := &vscChannel{relay: rel, tun: tun, device: "dev1",
		ws: &wsConn{conn: w, br: bufio.NewReader(w)}, done: make(chan struct{})}
	tun.ch = ch

	clientPipe, clientEnd := net.Pipe()
	defer clientPipe.Close()
	s := &vscSession{id: "testses", tun: tun, ch: ch, client: clientPipe, upgrade: true}
	s.cond = sync.NewCond(&s.mu)
	rel.mu.Lock()
	tun.sessions[s.id] = s
	rel.sessions++
	rel.mu.Unlock()
	go s.writeLoop()

	// worker 无视 pause 持续涌入：staging 破 cap → 会话被杀（client 被关闭）。
	// writeLoop 里挂起的首块写可能在关闭前送达，读到残余数据属正常，等关闭为止。
	go func() {
		for i := 0; i < 20; i++ {
			s.push(bytes.Repeat([]byte("x"), 1024))
		}
	}()
	buf := make([]byte, 1024)
	for {
		_ = clientEnd.SetReadDeadline(time.Now().Add(3 * time.Second))
		if _, err := clientEnd.Read(buf); err != nil {
			break
		}
	}
}

func TestVscRelay502Fallback(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	workerConn, workerBr, _ := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	defer workerConn.Close()

	// 非升级 client：无 101，请求头照转；后端不可达（worker 立即 close）时
	// client 收到干净的 502。
	clientConn, clientBr := vscGet(t, addr, "GET /tunnels/ws?tkn=secret HTTP/1.1\r\nHost: r\r\n\r\n")
	defer clientConn.Close()
	open := vscWsRecvText(t, workerConn, workerBr)
	if open["type"] != "open" {
		t.Fatalf("open frame = %v", open)
	}
	vscWsRecvData(t, workerConn, workerBr) // 请求头
	vscWsSendJSON(t, workerConn, map[string]any{"type": "close", "id": open["id"], "reason": "backend unavailable"})
	if got := vscReadRaw(t, clientConn, clientBr, "502"); !strings.Contains(got, "HTTP/1.1 502 Bad Gateway") {
		t.Fatalf("client got %q, want 502", got)
	}
}

func TestVscRelayMountedPrefix(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler("/vx"))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	workerConn, workerBr, ready := vscRegisterWorker(t, addr, "/vx/tunnels/ws", "h1", "secret.dev1")
	defer workerConn.Close()
	if ready["path"] != "/tunnels/ws" {
		t.Fatalf("ready path = %v, want /tunnels/ws (mount stripped)", ready["path"])
	}
	clientConn, _ := vscConnectClient(t, addr, "/vx/tunnels/ws", "secret", "")
	defer clientConn.Close()
	if open := vscWsRecvText(t, workerConn, workerBr); open["type"] != "open" || open["path"] != "/tunnels/ws" {
		t.Fatalf("open frame = %v", open)
	}

	stConn, stBr := vscGet(t, addr, "GET /vx/status HTTP/1.1\r\nHost: r\r\nConnection: close\r\n\r\n")
	if code := vscStatusCode(t, vscReadHead(t, stConn, stBr)); code != 200 {
		t.Fatal("mounted /vx/status should be 200")
	}
	stConn.Close()

	for _, p := range []string{"/vx/nope", "/vx"} {
		conn, br := vscGet(t, addr, "GET "+p+" HTTP/1.1\r\nHost: r\r\n\r\n")
		if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 404 {
			t.Fatalf("%s got %d, want 404", p, code)
		}
		conn.Close()
	}
}

func TestVscRelayGrace(t *testing.T) {
	rel := newVscRelay()
	rel.grace = 200 * time.Millisecond
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	workerConn, _, _ := vscRegisterWorker(t, addr, "/tunnels/ws", "h1", "secret.dev1")
	workerConn.Close()
	time.Sleep(50 * time.Millisecond)

	// 保留期内：503（离线），隧道条目仍在。
	conn, br := vscGet(t, addr, "GET /tunnels/ws?tkn=secret HTTP/1.1\r\nHost: r\r\n\r\n")
	if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 503 {
		t.Fatalf("in-grace client got %d, want 503", code)
	}
	conn.Close()

	// 超过保留期：下一次注册/状态查询触发惰性清扫 → 403（未知令牌）。
	time.Sleep(300 * time.Millisecond)
	vscStatus(t, addr)
	conn2, br2 := vscGet(t, addr, "GET /tunnels/ws?tkn=secret HTTP/1.1\r\nHost: r\r\n\r\n")
	if code := vscStatusCode(t, vscReadHead(t, conn2, br2)); code != 403 {
		t.Fatalf("post-grace client got %d, want 403", code)
	}
	conn2.Close()
}

func TestVscRelayTunnelCap(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	conns := make([]net.Conn, 0, vscMaxTunnels+1)
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()
	for i := 0; i < vscMaxTunnels; i++ {
		conn, _, _ := vscRegisterWorker(t, addr, "/t", "h", fmt.Sprintf("tok%03d.dev%03d", i, i))
		conns = append(conns, conn)
	}
	conn, br := vscGet(t, addr, "GET /t?kind=worker&tkn=overflow.dev999 HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 503 {
		t.Fatalf("tunnel overflow got %d, want 503", code)
	}
	conn.Close()
}

// TestVscRelayHijackSurvivesReadTimeout 长命连接契约：http.Server 在读升级请求前
// 挂上的是**绝对**读 deadline（read_timeout），hijack 之后它绝不能继续生效——
// 否则每条被劫持的连接（worker 通道与 client 腿）都会在升级请求到达 read_timeout
// 后被杀，与健康与否无关（新版 stdlib 在 Hijack 里清 deadline，hijackRawConn 是
// 自备的一道保险，并顺带开 TCP keepalive）。这里把 read_timeout 压到 1s，要求
// worker 通道与 client 腿都活着跨过它，且双向数据照常流动。
func TestVscRelayHijackSurvivesReadTimeout(t *testing.T) {
	oldDL := serverReadTimeout
	serverReadTimeout = time.Second
	defer func() { serverReadTimeout = oldDL }()

	rel := newVscRelay()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := newCacheServer(l.Addr().String(), rel.RouteHandler(""), nil)
	serveDone := make(chan struct{})
	go func() {
		_ = srv.Serve(l)
		close(serveDone)
	}()
	t.Cleanup(func() {
		_ = srv.Close()
		<-serveDone
	})
	addr := l.Addr().String()

	workerConn, workerBr, _ := vscRegisterWorker(t, addr, "/", "h1", "secret.dev1")
	defer workerConn.Close()

	// client 腿在 deadline 之前建立。
	clientConn, clientBr := vscConnectClient(t, addr, "/", "secret.dev2.x", "")
	defer clientConn.Close()

	// worker 收 open（记下会话 id）+ 转发的原始请求头。
	open := vscWsRecvText(t, workerConn, workerBr)
	if open["type"] != "open" {
		t.Fatalf("worker frame = %v, want open", open)
	}
	id := open["id"].(string)
	if data := vscWsRecvData(t, workerConn, workerBr); !strings.Contains(data, "tkn=secret.dev2.x") {
		t.Fatalf("forwarded head = %q", data)
	}

	// 跨过 read_timeout：修复前两条腿在这里都已被 i/o timeout 杀死。
	time.Sleep(1500 * time.Millisecond)

	st := vscStatus(t, addr)
	tunnels := st["tunnels"].([]any)
	if len(tunnels) != 1 {
		t.Fatalf("tunnels = %d, want 1", len(tunnels))
	}
	if online := tunnels[0].(map[string]any)["online"]; online != true {
		t.Fatalf("tunnel offline after %s — hijacked conn still carries the server read deadline", serverReadTimeout)
	}

	// 通道双向都还活着：worker 发的数据越过 deadline 后到达 client……
	vscWsSendData(t, workerConn, id, "late-payload")
	if got := vscReadRaw(t, clientConn, clientBr, "late-payload"); !strings.Contains(got, "late-payload") {
		t.Fatalf("client got %q", got)
	}
	// ……client 发的数据也越过 deadline 后到达 worker。
	if _, err := clientConn.Write([]byte("late-reply")); err != nil {
		t.Fatalf("client write: %v", err)
	}
	if data := vscWsRecvData(t, workerConn, workerBr); data != "late-reply" {
		t.Fatalf("worker got %q, want late-reply", data)
	}
}

// TestVscRelayChannelCongestion 验证通道写积压门（writeSession）：中继→worker
// 方向积压超高水位时，会话写停在门上（积压不再增长、来源读取停住），排水后
// 全部放行（无死锁）；通道死亡时等待者立即带错返回，不会永远等一个不会来的
// 高水位回落。
func TestVscRelayChannelCongestion(t *testing.T) {
	rel := newVscRelay()
	rel.chanHighWater = 16 << 10 // 16KB，帧 8KB：最多两块在途

	innerR, innerW := net.Pipe()
	defer innerW.Close()
	ch := &vscChannel{relay: rel, done: make(chan struct{})}
	ch.cond = sync.NewCond(&ch.mu)
	ch.ws = &wsConn{conn: innerW, br: bufio.NewReader(innerR)}

	const frame = 8 << 10
	const writes = 16 // 4 写循环 × 16 × 8KB = 512KB

	storm := func() <-chan error {
		errs := make(chan error, 4)
		for g := 0; g < 4; g++ {
			go func() {
				var err error
				for i := 0; i < writes && err == nil; i++ {
					err = ch.writeSession(make([]byte, frame))
				}
				errs <- err
			}()
		}
		return errs
	}

	// 阶段一：不读对端 → 写全部堵住；采样 pend，封顶不得超过高水位+单帧。
	maxPend := make(chan int64, 1)
	stop := make(chan struct{})
	go func() {
		var best int64
		for {
			select {
			case <-stop:
				maxPend <- best
				return
			case <-time.After(5 * time.Millisecond):
				ch.mu.Lock()
				if ch.pend > best {
					best = ch.pend
				}
				ch.mu.Unlock()
			}
		}
	}()
	errs := storm()
	time.Sleep(300 * time.Millisecond)
	select {
	case err := <-errs:
		t.Fatalf("write finished during full congestion: %v", err)
	default:
	}
	// 排水：读完对端全部积压，写循环应逐一放行直至完成（无死锁）。
	drained := 0
	buf := make([]byte, 64<<10)
	_ = innerR.SetReadDeadline(time.Now().Add(10 * time.Second))
	for drained < 4*writes*frame {
		n, err := innerR.Read(buf)
		drained += n
		if err != nil {
			t.Fatalf("drain: %v (got %d/%d)", err, drained, 4*writes*frame)
		}
	}
	for g := 0; g < 4; g++ {
		if err := <-errs; err != nil {
			t.Fatalf("write loop: %v", err)
		}
	}
	close(stop)
	if best := <-maxPend; best > int64(rel.chanHighWater)+frame {
		t.Fatalf("channel backlog peaked at %d, cap is %d (+1 frame)", best, rel.chanHighWater)
	}

	// 阶段二：再次堵住后判死通道 → 等待者立刻带错返回。
	errs = storm()
	time.Sleep(100 * time.Millisecond)
	_ = innerW.Close()
	ch.mu.Lock()
	ch.dead = true
	ch.cond.Broadcast()
	ch.mu.Unlock()
	for g := 0; g < 4; g++ {
		select {
		case err := <-errs:
			if err == nil {
				t.Fatal("write loop survived channel death")
			}
		case <-time.After(3 * time.Second):
			t.Fatal("write loop still blocked after channel death")
		}
	}
}

// TestVscRelayTunnelIDGate 验证 relay 身份门禁（tunnel_id 配置）：worker 注册带
// 了第三段就必须与配置一致（否则 403——连错 relay 了），缺段放行；两种情况都经
// ready 帧回告；client 只按第一段路由，第三段不作门禁。
func TestVscRelayTunnelIDGate(t *testing.T) {
	rel := newVscRelay()
	rel.tunnelID = "chan1"
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	// 第三段不一致 → 403。
	conn, br := vscGet(t, addr, "GET /?kind=worker&tkn=tok1.dev1.other HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n")
	if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 403 {
		t.Fatalf("mismatched tunnel id got %d, want 403", code)
	}
	conn.Close()

	// 带上同一段 → 注册成功，ready 帧回告 tunnelId。
	workerConn, _, ready := vscRegisterWorker(t, addr, "/", "h1", "tok1.dev1.chan1")
	defer workerConn.Close()
	if ready["tunnelId"] != "chan1" {
		t.Fatalf("ready tunnelId = %v, want chan1", ready["tunnelId"])
	}

	// 缺段放行 → ready 帧同样回告（worker 采纳后写回配置）。
	workerConn2, workerBr2, ready2 := vscRegisterWorker(t, addr, "/", "h2", "tok2.dev2")
	defer workerConn2.Close()
	if ready2["tunnelId"] != "chan1" {
		t.Fatalf("ready tunnelId = %v, want chan1", ready2["tunnelId"])
	}

	// client 的 tkn 第三段不作门禁：任意第三段都按第一段路由。
	clientConn, _ := vscConnectClient(t, addr, "/", "tok2.mixed.seg", "")
	defer clientConn.Close()
	if open := vscWsRecvText(t, workerConn2, workerBr2); open["type"] != "open" {
		t.Fatalf("worker2 frame = %v, want open", open)
	}

	// /status 顶层回告 tunnelId。
	st := vscStatus(t, addr)
	if st["tunnelId"] != "chan1" {
		t.Fatalf("status tunnelId = %v, want chan1", st["tunnelId"])
	}
}

// TestVscRelaySinkBytesIsLiveDepth 防回归：sink 计数必须是"当前积压"而不是"累计
// 传输量"。writeLoop 出队不扣减的话，任何累计跨过 sinkCap 的会话（哪怕客户端读
// 得飞快、队列实际是空的）都会被 client sink overflow 误杀——生产表现就是会话
// 开 2~3 秒被关、客户端不停重连（一次大传输必然踩中）。
func TestVscRelaySinkBytesIsLiveDepth(t *testing.T) {
	rel := newVscRelay()
	rel.sinkCap = 64 << 10 // 上限 64KB，远小于下面的累计传输量
	rel.sinkHighWater = 16 << 10
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	workerConn, workerBr, _ := vscRegisterWorker(t, addr, "/", "h1", "secret.dev1")
	defer workerConn.Close()
	clientConn, clientBr := vscConnectClient(t, addr, "/", "secret.dev2.x", "")
	defer clientConn.Close()

	open := vscWsRecvText(t, workerConn, workerBr)
	if open["type"] != "open" {
		t.Fatalf("worker frame = %v, want open", open)
	}
	id := open["id"].(string)
	vscWsRecvData(t, workerConn, workerBr) // 转发的请求头

	// 客户端并发全速读取，直到收满 total 或连接被关（误杀时 EOF 提前到来）。
	const total = 4 * (64 << 10) // 256KB = 4× sinkCap 累计量
	const frame = 16 << 10       // 单帧 16KB < sinkCap：合法推送，不触发逐帧上限
	got := make(chan int, 1)
	go func() {
		n := 0
		buf := make([]byte, 32<<10)
		for n < total {
			c, err := clientBr.Read(buf)
			n += c
			if err != nil {
				break
			}
		}
		got <- n
	}()

	// worker 连发 4×sinkCap，停一拍让中继排空，再正常关闭会话。
	for i := 0; i < total/frame; i++ {
		vscWsSendData(t, workerConn, id, string(make([]byte, frame)))
	}
	time.Sleep(300 * time.Millisecond)
	vscWsSendJSON(t, workerConn, map[string]any{"type": "close", "id": id, "reason": "backend closed"})

	select {
	case n := <-got:
		if n != total {
			t.Fatalf("client received %d/%d bytes — session killed early (sink counter is cumulative, not live?)", n, total)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("client read timed out")
	}
}
