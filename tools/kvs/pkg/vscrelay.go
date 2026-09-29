package pkg

// vscRelay — relay://vscode 后端：Remote-Server 中继（relay.js 参考实现）的
// 进程内移植。纯原始字节管道：/connect 升级握手之后不再有 WebSocket 分帧，
// 中继不解析任何协议内容，只做两条 socket 的对接；host（vscode-server 一侧）
// 与 client（VS Code 一侧）通过 query 里的 tkn 配对。
//
//	agent   GET /agent?name=<注册名>&tkn=<令牌>    NDJSON 控制通道（200 之后同一条
//	                                              socket 变 NDJSON：收到 {"type":"session",
//	                                              id,addr,query} 时用 /session/<id> 接走）
//	client  GET /connect?name=<注册名>&tkn=<令牌>
//	        &skipWebSocketFrames=true             WebSocket 升级，101 之后原始字节
//	host    GET /session/<id>?tkn=<令牌>          接走 pending 的 client → 200 之后原始字节
//	ops     GET /status                           运行状态 JSON
//
// 令牌模型与参考实现一致（URL 即凭证）：令牌只从 query 取（tkn= 或别名
// token=），不识别自定义请求头；client 的 tkn 与目标主机注册的不一致直接
// 403，不向 host 转发；/session 接管时同样校验。同名注册：旧通道还在且令牌
// 不同 → 409 拒绝，防止顶名；令牌相同 → 顶掉旧通道（已建立的会话不经过控制
// 通道，保持不动；孤儿 pending 由各自的超时定时器兜底）。socket 断开即释放
// 名字。
//
// 网关鉴权：本协议没有 cookie 概念，tkn 就是凭证。login_authz 开启时，要么
// 把挂载前缀加进 path_public，要么让各方 tkn 等于网关 login token（配置
// query_token_key=tkn 时同一参数顺带过网关鉴权）。

import (
	"bufio"
	"crypto/rand"
	"crypto/sha1"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 会话参数（与参考实现同量级）：
const (
	// vscMaxSessions：活跃会话上限，防止中继被当内存池用。
	vscMaxSessions = 4096
	// vscControlWriteTimeout：控制通道单次写超时（ NDJSON 行很小，10s 足够）。
	vscControlWriteTimeout = 10 * time.Second
)

// vscHelloTimeout：client 等待 host 接管的超时；超时未接管则 503 结束。
// var 而非 const：测试缩短它验证定时器路径。
var vscHelloTimeout = 10 * time.Second

// vscRelay is one relay://vscode instance. All state is in-memory: hosts
// re-register on reconnect, so a restart only drops in-flight sessions.
type vscRelay struct {
	mu       sync.Mutex
	agents   map[string]*vscAgent   // 注册名 → host 控制通道
	pending  map[string]*vscPending // 会话 id → 等待 host 接管的 client
	sessions map[string]*vscSession // 会话 id → 已对接的管道
}

type vscAgent struct {
	name     string
	token    string
	conn     net.Conn
	br       *bufio.Reader
	since    time.Time
	writeMu  sync.Mutex // 保护 NDJSON 行写出的原子性
	sessions map[string]bool
}

// vscPending 是已回应 101、等待 host 用 /session/<id> 接走的 client。
type vscPending struct {
	id    string
	name  string
	addr  string
	query string
	conn  net.Conn
	br    *bufio.Reader
}

// vscSession 是已对接的一对 socket。
type vscSession struct {
	id     string
	name   string
	client net.Conn
	cbr    *bufio.Reader
	host   net.Conn
	hbr    *bufio.Reader
}

var (
	vscOnce sync.Once
	vscInst *vscRelay
)

// vscGetRelay returns the process-wide relay://vscode instance.
func vscGetRelay() *vscRelay {
	vscOnce.Do(func() { vscInst = newVscRelay() })
	return vscInst
}

// newVscRelay builds a fresh relay instance (also the test-isolation entry).
func newVscRelay() *vscRelay {
	return &vscRelay{
		agents:   make(map[string]*vscAgent),
		pending:  make(map[string]*vscPending),
		sessions: make(map[string]*vscSession),
	}
}

// RouteHandler mounts the relay's four sub-routes under the route prefix.
// Prefix stripping is segment-boundary exact (mount, mount+"/…"); when the
// path does not carry the prefix (e.g. ^-regex mounts), it is used as-is, so
// root-style /agent /connect /session/<id> /status always work.
func (r *vscRelay) RouteHandler(mount string) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		p := req.URL.Path
		if mount != "" && mount != "/" {
			if p == mount {
				p = "/"
			} else if strings.HasPrefix(p, mount+"/") {
				p = p[len(mount):]
			}
		}
		switch {
		case p == "/status":
			r.handleStatus(w)
		case p == "/agent":
			r.handleAgent(w, req)
		case strings.HasPrefix(p, "/session/"):
			r.handleSession(w, req, strings.TrimPrefix(p, "/session/"))
		case p == "/connect" || p == "/":
			r.handleConnect(w, req)
		default:
			http.Error(w, "vscode relay: unknown path "+p, http.StatusNotFound)
		}
	})
}

// vscTokenOf — 令牌只认 query（tkn= 与别名 token=）。
func vscTokenOf(q url.Values) string {
	if v := q.Get("tkn"); v != "" {
		return v
	}
	return q.Get("token")
}

// vscQueryOf 透传给 host 的 query：剔除 relay 自己的路由参数（name），其余
// 原样保留（含 tkn —— host 侧再用它对 vscode-server 做鉴权）。
func vscQueryOf(q url.Values) string {
	vv := make(url.Values, len(q))
	for k, vs := range q {
		if k == "name" {
			continue
		}
		vv[k] = vs
	}
	return vv.Encode()
}

// vscRespond writes a final HTTP/1.1 response onto an already-hijacked
// connection and closes it (error paths after the socket was taken over).
func vscRespond(conn net.Conn, status int, reason, body string) {
	fmt.Fprintf(conn, "HTTP/1.1 %d %s\r\nContent-Type: text/plain; charset=utf-8\r\nContent-Length: %d\r\nConnection: close\r\n\r\n%s",
		status, reason, len(body), body)
	_ = conn.Close()
}

// vscHijack takes the connection over for raw byte use. Reads MUST go through
// the returned bufio.Reader: it may already hold bytes the peer sent beyond
// the request headers (early client data would otherwise be lost).
func vscHijack(w http.ResponseWriter) (net.Conn, *bufio.Reader, error) {
	hj, ok := w.(http.Hijacker)
	if !ok {
		http.Error(w, "connection cannot be hijacked", http.StatusInternalServerError)
		return nil, nil, fmt.Errorf("response writer cannot hijack")
	}
	conn, brw, err := hj.Hijack()
	if err != nil {
		return nil, nil, err
	}
	return conn, brw.Reader, nil
}

// vscSessionID mints a UUID-v4-shaped session id (same shape as the
// reference relay's crypto.randomUUID).
var vscIDSalt atomic.Int64

func vscSessionID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%08x-%04x", time.Now().UnixNano()&0xffffffff, vscIDSalt.Add(1)&0xffff)
	}
	b[6] = (b[6] & 0x0f) | 0x40
	b[8] = (b[8] & 0x3f) | 0x80
	return fmt.Sprintf("%x-%x-%x-%x-%x", b[0:4], b[4:6], b[6:8], b[8:10], b[10:16])
}

// vscControl messages mirror the reference relay's NDJSON exactly.
type (
	vscWelcomeMsg struct {
		Type string `json:"type"`
		Name string `json:"name"`
	}
	vscSessionMsg struct {
		Type  string `json:"type"`
		ID    string `json:"id"`
		Addr  string `json:"addr"`
		Query string `json:"query"`
	}
	vscTypeMsg struct {
		Type string `json:"type"`
	}
)

// writeMsg sends one NDJSON line on the agent control channel.
func (a *vscAgent) writeMsg(v any) {
	data, err := json.Marshal(v)
	if err != nil {
		return
	}
	a.writeMu.Lock()
	defer a.writeMu.Unlock()
	_ = a.conn.SetWriteDeadline(time.Now().Add(vscControlWriteTimeout))
	_, _ = a.conn.Write(append(data, '\n'))
	_ = a.conn.SetWriteDeadline(time.Time{})
}

// handleAgent registers a host control channel: 200, then the same socket
// speaks NDJSON (session notifications down, ping/pong up).
func (r *vscRelay) handleAgent(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		http.Error(w, "vscode relay: /agent needs ?name=<host name>", http.StatusBadRequest)
		return
	}
	token := vscTokenOf(q)
	if token == "" {
		http.Error(w, "vscode relay: access token required in the URL (?tkn= or ?token=)", http.StatusUnauthorized)
		return
	}

	conn, br, err := vscHijack(w)
	if err != nil {
		return
	}

	var agent *vscAgent
	r.mu.Lock()
	old := r.agents[name]
	conflict := old != nil && old.token != token
	if !conflict {
		agent = &vscAgent{name: name, token: token, conn: conn, br: br, since: time.Now(), sessions: make(map[string]bool)}
		r.agents[name] = agent
	}
	r.mu.Unlock()
	if conflict {
		log.Printf("[vscrelay] agent %q registration rejected: name is in use with a different token", name)
		vscRespond(conn, http.StatusConflict, "Conflict", "vscode relay: host name "+name+" is already registered with a different token\n")
		return
	}
	if old != nil {
		// 同名同令牌重注册：顶掉旧通道。已建立的会话不经过控制通道，保持
		// 不动；孤儿 pending 由各自的超时定时器兜底。
		old.conn.Close()
		log.Printf("[vscrelay] agent %q re-registered (same token); dropping the previous channel", name)
	}

	log.Printf("[vscrelay] agent %q registered (remote=%s)", name, req.RemoteAddr)

	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/x-ndjson\r\nConnection: keep-alive\r\n\r\n"))
	agent.writeMsg(vscWelcomeMsg{Type: "welcome", Name: name})

	// 控制通道读取循环：目前只回应 ping；通道断开即释放名字（顶名替换时
	// 走不到这里 —— identity 检查保证只释放自己的注册）。
	sc := bufio.NewScanner(br)
	sc.Buffer(make([]byte, 64*1024), 1<<20)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if line == "" {
			continue
		}
		var msg vscTypeMsg
		if json.Unmarshal([]byte(line), &msg) != nil {
			continue
		}
		if msg.Type == "ping" {
			agent.writeMsg(vscTypeMsg{Type: "pong"})
		}
	}

	r.mu.Lock()
	mine := r.agents[name] == agent
	if mine {
		delete(r.agents, name)
		n := len(agent.sessions)
		r.mu.Unlock()
		log.Printf("[vscrelay] agent %q disconnected; name released (active sessions: %d)", name, n)
		return
	}
	r.mu.Unlock()
	log.Printf("[vscrelay] agent %q channel closed (superseded by a new registration)", name)
}

// handleConnect accepts a client: validate, answer the WebSocket handshake,
// notify the host on its control channel, and leave the raw socket parked
// until the host picks it up via /session/<id>.
func (r *vscRelay) handleConnect(w http.ResponseWriter, req *http.Request) {
	q := req.URL.Query()
	name := strings.TrimSpace(q.Get("name"))
	if name == "" {
		http.Error(w, "vscode relay: /connect needs ?name=<host name>", http.StatusBadRequest)
		return
	}
	token := vscTokenOf(q)

	// 校验顺序与参考实现一致；判定在锁内完成，应答在锁外发出。
	var agent *vscAgent
	r.mu.Lock()
	var status int
	var msg string
	switch {
	case len(r.sessions) >= vscMaxSessions:
		status, msg = http.StatusServiceUnavailable, "too many active sessions"
	case r.agents[name] == nil:
		status, msg = http.StatusNotFound, "no host registered as "+name
	case !isWebSocketUpgrade(req):
		status, msg = http.StatusBadRequest, "/connect expects a WebSocket upgrade request"
	case q.Get("skipWebSocketFrames") != "true":
		// 中继只做原始字节对接、不重新分帧；permessage-deflate 的压缩上下文
		// 是端到端的，任何重新分帧都会破坏压缩流 —— 必须显式 raw 模式。
		status, msg = http.StatusBadRequest, "raw mode required — this relay carries raw byte streams only; connect with the Remote - Server client"
	case token == "":
		status, msg = http.StatusUnauthorized, "access token required in the URL (?tkn= or ?token=)"
	case token != r.agents[name].token:
		status, msg = http.StatusForbidden, "invalid access token for this host"
	}
	agent = r.agents[name]
	r.mu.Unlock()
	if status != 0 {
		if status == http.StatusForbidden {
			log.Printf("[vscrelay] client %s rejected for host %q: invalid tkn", req.RemoteAddr, name)
		}
		http.Error(w, "vscode relay: "+msg, status)
		return
	}

	// 先接管 socket，再登记 pending：hijack 失败不能留下会触发定时器应答
	// 的孤儿条目（定时器闭包持有 conn，nil 会炸）。
	conn, br, err := vscHijack(w)
	if err != nil {
		return
	}
	id := vscSessionID()
	p := &vscPending{id: id, name: name, addr: clientRemoteAddr(req), query: vscQueryOf(q), conn: conn, br: br}
	r.mu.Lock()
	r.pending[id] = p
	r.mu.Unlock()

	// 兜底定时器：host 没在超时内接走就 503 收尾。client 提前断开时不专门
	// 监测（读检测会消费管道字节），最多滞留到定时器触发。
	time.AfterFunc(vscHelloTimeout, func() {
		r.mu.Lock()
		_, alive := r.pending[id]
		delete(r.pending, id)
		r.mu.Unlock()
		if !alive {
			return
		}
		log.Printf("[vscrelay] session %s timed out waiting for host %q", id[:8], name)
		vscRespond(p.conn, http.StatusServiceUnavailable, "Service Unavailable", "vscode relay: host "+name+" did not pick up the session in time\n")
	})

	// 101 之后是原始字节流：握手应答直接写 socket（与 wsUpgrade 同式）。
	key := strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))
	h := sha1.New()
	h.Write([]byte(key))
	h.Write([]byte(wsMagicGUID))
	accept := base64.StdEncoding.EncodeToString(h.Sum(nil))
	_, _ = conn.Write([]byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: " + accept + "\r\n\r\n"))

	agent.writeMsg(vscSessionMsg{Type: "session", ID: id, Addr: p.addr, Query: p.query})
	log.Printf("[vscrelay] session %s requested by %s for host %q", id[:8], p.addr, name)
}

// handleSession lets the host pick up a pending client: 200, then the two
// sockets are spliced raw in both directions.
func (r *vscRelay) handleSession(w http.ResponseWriter, req *http.Request, id string) {
	token := vscTokenOf(req.URL.Query())

	// 判定+摘除 pending 在锁内一次完成（与接管定时器互斥），应答在锁外。
	r.mu.Lock()
	p := r.pending[id]
	var status int
	var msg string
	switch {
	case p == nil:
		status, msg = http.StatusNotFound, "no pending client session with that id"
	case r.agents[p.name] == nil:
		status, msg = http.StatusNotFound, "host "+p.name+" is no longer registered"
	case token != r.agents[p.name].token:
		status, msg = http.StatusForbidden, "invalid token for this host"
	default:
		delete(r.pending, id)
	}
	r.mu.Unlock()
	if status != 0 {
		http.Error(w, "vscode relay: "+msg, status)
		return
	}

	conn, br, err := vscHijack(w)
	if err != nil {
		return
	}

	s := &vscSession{id: id, name: p.name, client: p.conn, cbr: p.br, host: conn, hbr: br}
	r.mu.Lock()
	r.sessions[id] = s
	if a := r.agents[s.name]; a != nil {
		a.sessions[id] = true
	}
	r.mu.Unlock()

	_, _ = conn.Write([]byte("HTTP/1.1 200 OK\r\nContent-Type: application/octet-stream\r\nConnection: keep-alive\r\n\r\n"))
	log.Printf("[vscrelay] session %s established: %s -> host %q", id[:8], p.addr, s.name)

	// vscPipeWriter 隐藏 ReadFrom/WriteTo：强制用户态读循环而不是内核
	// splice —— 对端 Close 必须立刻打断拷贝（splice 路径下会悬挂到对端
	// 真正有数据/超时为止），任一端断开才能即时级联关闭整个会话。
	go func() {
		_, _ = io.Copy(vscPipeWriter{s.host}, s.cbr) // client → host
		r.closeSession(id, "client closed")
	}()
	go func() {
		_, _ = io.Copy(vscPipeWriter{s.client}, s.hbr) // host → client
		r.closeSession(id, "host pipe closed")
	}()
}

// vscPipeWriter wraps a net.Conn exposing only Write (io.Copy's generic
// read loop instead of splice/readFrom fast paths).
type vscPipeWriter struct{ io.Writer }

// closeSession tears a session down exactly once; whichever direction fails
// first wins the log line, both sockets are always closed.
func (r *vscRelay) closeSession(id, why string) {
	r.mu.Lock()
	s := r.sessions[id]
	if s == nil {
		r.mu.Unlock()
		return
	}
	delete(r.sessions, id)
	if a := r.agents[s.name]; a != nil {
		delete(a.sessions, id)
	}
	r.mu.Unlock()
	_ = s.client.Close()
	_ = s.host.Close()
	log.Printf("[vscrelay] session %s closed (%s)", id[:8], why)
}

// handleStatus reports running state as JSON (ops endpoint).
func (r *vscRelay) handleStatus(w http.ResponseWriter) {
	r.mu.Lock()
	hosts := make([]map[string]any, 0, len(r.agents))
	for _, a := range r.agents {
		hosts = append(hosts, map[string]any{
			"name":     a.name,
			"since":    a.since.Format(time.RFC3339),
			"sessions": len(a.sessions),
		})
	}
	nPending, nActive := len(r.pending), len(r.sessions)
	r.mu.Unlock()
	sort.Slice(hosts, func(i, j int) bool { return hosts[i]["name"].(string) < hosts[j]["name"].(string) })

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"now":             time.Now().Format(time.RFC3339),
		"hosts":           hosts,
		"pendingSessions": nPending,
		"activeSessions":  nActive,
	})
}
