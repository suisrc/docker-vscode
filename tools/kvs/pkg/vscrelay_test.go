package pkg

// relay://vscode 的端到端测试：用裸 TCP 连接扮演 relay.js 协议的三方
// （host 控制通道 / host 数据管道 / client），覆盖配对、鉴权与生命周期。

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// vscReadHead reads the response head (through the blank line) from a
// hijacked-style raw connection.
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

func vscReadLine(t *testing.T, conn net.Conn, br *bufio.Reader) string {
	t.Helper()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	line, err := br.ReadString('\n')
	if err != nil {
		t.Fatalf("read NDJSON line: %v", err)
	}
	return line
}

// vscReadUntil accumulates raw bytes until want shows up (or 3s deadline).
func vscReadUntil(t *testing.T, conn net.Conn, br *bufio.Reader, want string) string {
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

// vscRegisterHost registers a host control channel and consumes the 200 head
// plus the welcome line.
func vscRegisterHost(t *testing.T, addr, name, token string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, br := vscGet(t, addr, fmt.Sprintf("GET /agent?name=%s&token=%s HTTP/1.1\r\nHost: relay\r\n\r\n", name, token))
	if code := vscStatusCode(t, vscReadHead(t, conn, br)); code != 200 {
		t.Fatalf("agent registration got %d, want 200", code)
	}
	var welcome vscWelcomeMsg
	if err := json.Unmarshal([]byte(vscReadLine(t, conn, br)), &welcome); err != nil {
		t.Fatalf("welcome line: %v", err)
	}
	if welcome.Type != "welcome" || welcome.Name != name {
		t.Fatalf("welcome = %+v, want name %q", welcome, name)
	}
	return conn, br
}

// vscConnectClient opens a client connection (raw mode, with early data) and
// consumes the 101 head, verifying the WebSocket accept.
func vscConnectClient(t *testing.T, addr, name, token, early string) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn, br := vscGet(t, addr, fmt.Sprintf(
		"GET /connect?name=%s&tkn=%s&skipWebSocketFrames=true&foo=bar HTTP/1.1\r\nHost: relay\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Key: dGhlIHNhbXBsZSBub25jZQ==\r\n\r\n%s",
		name, token, early))
	head := vscReadHead(t, conn, br)
	if code := vscStatusCode(t, head); code != 101 {
		t.Fatalf("client connect got %d, want 101", code)
	}
	// RFC 6455 sample key → known accept value.
	if !strings.Contains(head, "Sec-WebSocket-Accept: s3pPLMBiTxaQ9kYGzzhZRbK+xOo=") {
		t.Fatalf("101 head missing correct accept: %q", head)
	}
	return conn, br
}

func TestVscRelayPairingEndToEnd(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	hostCtl, hostBr := vscRegisterHost(t, addr, "h1", "secret")
	defer hostCtl.Close()

	// 控制通道 ping → pong。
	fmt.Fprintf(hostCtl, "{\"type\":\"ping\"}\n")
	pong := vscReadLine(t, hostCtl, hostBr)
	if !strings.Contains(pong, `"pong"`) {
		t.Fatalf("ping reply = %q, want pong", pong)
	}

	// client 接入（携带早到数据，验证 hijack 后 bufio 里的残留不丢）。
	clientConn, clientBr := vscConnectClient(t, addr, "h1", "secret", "client-early")
	defer clientConn.Close()

	// host 收到 session 通知，query 剔除 name、保留其余。
	notify := vscReadLine(t, hostCtl, hostBr)
	var msg vscSessionMsg
	if err := json.Unmarshal([]byte(notify), &msg); err != nil {
		t.Fatalf("session notification %q: %v", notify, err)
	}
	if msg.Type != "session" || msg.ID == "" || !strings.Contains(msg.Addr, "127.0.0.1") {
		t.Fatalf("session notification = %+v", msg)
	}
	if msg.Query != "foo=bar&skipWebSocketFrames=true&tkn=secret" {
		t.Fatalf("forwarded query = %q", msg.Query)
	}

	// host 数据管道接走 pending（同样带早到数据）。
	pipeConn, pipeBr := vscGet(t, addr, fmt.Sprintf("GET /session/%s?tkn=secret HTTP/1.1\r\nHost: relay\r\n\r\nhost-early", msg.ID))
	defer pipeConn.Close()
	if code := vscStatusCode(t, vscReadHead(t, pipeConn, pipeBr)); code != 200 {
		t.Fatalf("session pickup got %d, want 200", code)
	}

	// 双向原始字节：client 早到数据 → host；host 早到数据 → client。
	if got := vscReadUntil(t, pipeConn, pipeBr, "client-early"); !strings.Contains(got, "client-early") {
		t.Fatalf("host did not receive client early data: %q", got)
	}
	if got := vscReadUntil(t, clientConn, clientBr, "host-early"); !strings.Contains(got, "host-early") {
		t.Fatalf("client did not receive host early data: %q", got)
	}

	// 活跃后的双向转发。
	fmt.Fprintf(clientConn, "c2h-live")
	if got := vscReadUntil(t, pipeConn, pipeBr, "c2h-live"); !strings.Contains(got, "c2h-live") {
		t.Fatalf("client → host forwarding broken: %q", got)
	}
	fmt.Fprintf(pipeConn, "h2c-live")
	if got := vscReadUntil(t, clientConn, clientBr, "h2c-live"); !strings.Contains(got, "h2c-live") {
		t.Fatalf("host → client forwarding broken: %q", got)
	}

	// /status：1 个 host、1 个活跃会话。（Connection: close 让服务端回完
	// 就断，下面的 io.ReadAll 才能等到 EOF。）
	stConn, stBr := vscGet(t, addr, "GET /status HTTP/1.1\r\nHost: relay\r\nConnection: close\r\n\r\n")
	defer stConn.Close()
	if code := vscStatusCode(t, vscReadHead(t, stConn, stBr)); code != 200 {
		t.Fatal("status should be 200")
	}
	statusBody, _ := io.ReadAll(stBr)
	var st struct {
		Hosts []struct {
			Name     string `json:"name"`
			Sessions int    `json:"sessions"`
		} `json:"hosts"`
		ActiveSessions int `json:"activeSessions"`
	}
	if err := json.Unmarshal(statusBody, &st); err != nil {
		t.Fatalf("status body: %v (%q)", err, statusBody)
	}
	if len(st.Hosts) != 1 || st.Hosts[0].Name != "h1" || st.Hosts[0].Sessions != 1 || st.ActiveSessions != 1 {
		t.Fatalf("status = %+v", st)
	}

	// 一端断开 → 会话关闭、两端 socket 都关（client 读到 EOF/错误）。
	pipeConn.Close()
	_ = clientConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := clientBr.Read(make([]byte, 16)); err == nil {
		t.Fatal("client connection should be closed after the host pipe went away")
	}
}

func TestVscRelayAuthAndConflicts(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	hostCtl, hostBr := vscRegisterHost(t, addr, "h1", "secret")
	defer hostCtl.Close()

	cases := []struct {
		title string
		req   string
		want  int
	}{
		{"agent without token", "GET /agent?name=h2 HTTP/1.1\r\nHost: r\r\n\r\n", 401},
		{"agent name conflict with different token", "GET /agent?name=h1&token=other HTTP/1.1\r\nHost: r\r\n\r\n", 409},
		{"connect unknown host", "GET /connect?name=ghost&tkn=secret&skipWebSocketFrames=true HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\n\r\n", 404},
		{"connect without upgrade", "GET /connect?name=h1&tkn=secret&skipWebSocketFrames=true HTTP/1.1\r\nHost: r\r\n\r\n", 400},
		{"connect without raw mode", "GET /connect?name=h1&tkn=secret HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", 400},
		{"connect without token", "GET /connect?name=h1&skipWebSocketFrames=true HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", 401},
		{"connect with wrong token", "GET /connect?name=h1&tkn=WRONG&skipWebSocketFrames=true HTTP/1.1\r\nHost: r\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n", 403},
		{"session unknown id", "GET /session/nope?tkn=secret HTTP/1.1\r\nHost: r\r\n\r\n", 404},
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

	// 409 之后原注册不受影响（token 校验只拒绝不同令牌的顶名）。
	clientConn, clientBr := vscConnectClient(t, addr, "h1", "secret", "")
	clientConn.Close()
	_ = clientBr

	// pending 接管时令牌不符 → 403；原 pending 仍在，正确令牌可以接走。
	notify := vscReadLine(t, hostCtl, hostBr)
	var msg vscSessionMsg
	if err := json.Unmarshal([]byte(notify), &msg); err != nil {
		t.Fatalf("session notification %q: %v", notify, err)
	}
	wrongConn, wrongBr := vscGet(t, addr, fmt.Sprintf("GET /session/%s?tkn=WRONG HTTP/1.1\r\nHost: r\r\n\r\n", msg.ID))
	code := vscStatusCode(t, vscReadHead(t, wrongConn, wrongBr))
	wrongConn.Close()
	if code != 403 {
		t.Fatalf("pickup with wrong token got %d, want 403", code)
	}
	rightConn, rightBr := vscGet(t, addr, fmt.Sprintf("GET /session/%s?tkn=secret HTTP/1.1\r\nHost: r\r\n\r\n", msg.ID))
	defer rightConn.Close()
	if code := vscStatusCode(t, vscReadHead(t, rightConn, rightBr)); code != 200 {
		t.Fatalf("pickup after rejected attempt got %d, want 200", code)
	}
}

func TestVscRelayReregisterSameToken(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	oldConn, _ := vscRegisterHost(t, addr, "h1", "secret")
	defer oldConn.Close()

	newConn, newBr := vscRegisterHost(t, addr, "h1", "secret")
	defer newConn.Close()

	// 旧通道被顶掉：读应得到 EOF（连接被中继关闭）。
	_ = oldConn.SetReadDeadline(time.Now().Add(3 * time.Second))
	if _, err := oldConn.Read(make([]byte, 16)); err == nil {
		t.Fatal("old control channel should be closed after re-registration")
	}

	// 新通道可用：session 通知能到达新通道。
	clientConn, _ := vscConnectClient(t, addr, "h1", "secret", "")
	defer clientConn.Close()
	notify := vscReadLine(t, newConn, newBr)
	if !strings.Contains(notify, `"session"`) {
		t.Fatalf("new channel missed session notification: %q", notify)
	}
}

func TestVscRelayConnectTimeout(t *testing.T) {
	old := vscHelloTimeout
	vscHelloTimeout = 150 * time.Millisecond
	defer func() { vscHelloTimeout = old }()

	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler(""))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	hostCtl, _ := vscRegisterHost(t, addr, "h1", "secret")
	defer hostCtl.Close()

	clientConn, clientBr := vscConnectClient(t, addr, "h1", "secret", "")
	defer clientConn.Close()

	// host 不接管：超时后 client 在 101 之后收到 503 收尾。
	head := vscReadHead(t, clientConn, clientBr)
	if code := vscStatusCode(t, head); code != 503 {
		t.Fatalf("timed-out client got %d, want 503", code)
	}
}

func TestVscRelayMountedPrefix(t *testing.T) {
	rel := newVscRelay()
	ts := httptest.NewServer(rel.RouteHandler("/relayx"))
	defer ts.Close()
	addr := ts.Listener.Addr().String()

	// 前缀挂载下的四个子路由。
	hostCtl, _ := vscRegisterHost(t, addr, "h1", "secret")
	defer hostCtl.Close()
	clientConn, clientBr := vscConnectClient(t, addr, "h1", "secret", "")
	clientConn.Close()
	_ = clientBr

	stConn, stBr := vscGet(t, addr, "GET /relayx/status HTTP/1.1\r\nHost: r\r\n\r\n")
	code := vscStatusCode(t, vscReadHead(t, stConn, stBr))
	stConn.Close()
	if code != 200 {
		t.Fatal("mounted /relayx/status should be 200")
	}

	conn, br := vscGet(t, addr, "GET /relayx/nope HTTP/1.1\r\nHost: r\r\n\r\n")
	code = vscStatusCode(t, vscReadHead(t, conn, br))
	conn.Close()
	if code != 404 {
		t.Fatal("unknown mounted path should be 404")
	}
}
