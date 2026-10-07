package pkg

// vscRelay — relay://vscode 后端：Remote-Server tunnel 的进程内中继。单端点复用：
// worker（vscode-server 受控端）与 client（VS Code remote 端）访问同一路径，用 query
// 区分角色：
//
//	worker  GET <路径>?kind=worker&name=<显示名>&tkn=<token>.<deviceId>[.<tunnelId>]
//	        WebSocket 升级；101 之后同一连接变复用帧通道，可随时重连顶掉旧通道
//	client  GET <路径>?tkn=<token>…   按第一段 token 路由到隧道；升级请求由中继
//	                                  代答 101，之后两条腿都是原始字节管道
//	ops     GET <路径>/status         运行状态 JSON
//
// 复用帧协议（101 之后，worker 是掩码方，中继只收掩码帧、发不掩码帧）：
//	文本帧 = 控制 JSON（≤4KB）
//	  relay→worker  {"type":"ready","protocol":2,"relay":…,"tunnelId":null,"name","path"}
//	                {"type":"open","id","addr","path"}   ← client 会话开始，worker 拨本机 server
//	                {"type":"version","protocol":2}      ← 30s 一次在线探测，必须应答
//	                {"type":"pause"|"resume","id"}       ← client 积压/排空：停/恢复读后端
//	  worker→relay  {"type":"version","device","protocol","version"}（探测应答）
//	                {"type":"close","id","reason"}
//	                {"type":"pause"|"resume","id"}       ← 后端积压/排空：停/恢复发该会话数据
//	二进制帧 = 会话数据：[1B idLen][id ASCII][payload]
//	另有 WS 层 ping/pong（wsconn 自动回）。
//
// 连接长命性（隧道的空闲是常态，任何"一段时间没消息就断"的逻辑都是错的）：
//   - hijack 后清掉 server 预设的绝对 deadline + 显式 15s TCP keepalive
//     （hijackRawConn；空闲连接靠内核保活维系，不被 read_timeout/NAT 掐死）；
//   - 在线判定只靠 version 探测（30s 周期、连续缺答判死）与 WS 关闭/EOF，没有
//     空闲超时；半开连接由 TCP keepalive 兜底。
//
// 背压端到端：谁积压谁喊停。client 收不动（单会话 staging ≥1MB）→ 请 worker 停发
// 该会话；worker 侧后端收不动 → 请中继停读 client；中继→worker 通道写积压
// ≥8MB（链路慢/对端读得慢）→ 会话写在中继侧自然停住（TCP 背压回传源头）。
// 通道读方向永远积极（worker 与中继都不停读通道），控制帧不会被数据堵死；
// 中继侧内存只由两道高水位封顶。
//
// 令牌模型（URL 即凭证）：tkn 三元组 <token>.<deviceId>[.<tunnelId>]，只认 query
// （tkn= 与别名 token=），不读自定义头；token 全局唯一即隧道唯一键；deviceId 一台
// 设备同时只允许一条通道（第二条 409）；tunnelId 第三段是 relay 身份标识（配置
// tunnel_id）：中继配置了它，worker 注册就必须带上同一段（否则 403 拒绝——连错
// relay 了），缺段则放行、由 ready 帧回告 worker 写回配置；未配置则收下即忽略。
// client 侧永远只按第一段路由，第三段不作门禁。掉线隧道的占用保留 10 分钟（期间
// client 得 503 而非 403，worker 重连顶回即恢复）。本协议没有 cookie 概念，tkn
// 就是凭证：login_authz 开启时把挂载前缀加进 path_public，或让各方 tkn 等于网关
// login token。

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// 量级上限与参数（只防把中继当内存池用）。可调项挂在实例
// 字段上（newVscRelay 给默认值）：测试缩短/调小它们验证探测、背压与保留期路径，
// 且互不干扰。
const (
	vscRelayVersion         = "kvs"
	vscProtocolVersion      = 2
	vscMaxTunnels           = 256
	vscMaxSessions          = 4096 // 全局活跃会话
	vscMaxSessionsPerTunnel = 512
	vscControlCap           = 4 << 10  // 控制帧 JSON 上限，超限忽略
	vscChunkBytes           = 64 << 10 // client 读块（帧负载上限）
	vscProbeInterval        = 30 * time.Second
	vscProbeMaxMiss         = 2                // 连续探测缺答判死通道
	vscGrace                = 10 * time.Minute // 掉线隧道占用保留（防抢注，期间 client 得 503）
	vscSinkHighWater        = 1 << 20          // 单会话 staging 高水位：超过则请 worker 停发
	vscSinkCap              = 8 << 20          // 单会话 staging 绝对上限：worker 无视 pause 则杀会话
	vscChanHighWater        = 8 << 20          // 通道写积压高水位：超过则停读所有 client（TCP 背压回传）
)

// vscRelay is one relay://vscode instance. All state is in-memory: workers
// re-register on reconnect, so a restart only drops in-flight sessions.
type vscRelay struct {
	mu       sync.Mutex
	tunnels  map[string]*vscTunnel // token → tunnel（token 全局唯一即隧道唯一键）
	devices  map[string]*vscChannel
	sessions int // 全局活跃会话数

	tunnelID string // relay 身份标识（tunnel_id 配置）；空=未配置，不门禁

	probeInterval time.Duration
	probeMaxMiss  int
	grace         time.Duration
	sinkHighWater int
	sinkCap       int
	chanHighWater int
}

// vscTunnel is one registered worker: the token is the route key, the channel
// is the multiplexed WS link (nil = offline, within the grace window).
type vscTunnel struct {
	token     string
	device    string
	name      string
	path      string // 注册路径（展示用；路由只认 token）
	addr      string
	version   string // worker 探测应答里上报的服务版本
	since     time.Time
	deadSince time.Time // 非零 = 通道已断、占用保留中
	ch        *vscChannel
	sessions  map[string]*vscSession
}

// vscChannel is the worker's multiplexed WS link. One goroutine reads it
// (control dispatch + demux), a ticker probes service liveness; writes are
// serialized by wsConn's write mutex. Session data writes go through
// writeSession, which self-throttles on channel congestion (pend/cond):
// control frames never queue behind that gate.
type vscChannel struct {
	relay  *vscRelay
	tun    *vscTunnel
	device string // 注册设备（teardown 按通道释放独占映射）
	ws     *wsConn
	missed atomic.Int32 // 连续探测缺答次数
	done   chan struct{}
	gone   sync.Once

	mu   sync.Mutex
	cond *sync.Cond // 与 mu 配对；Wait 唤醒条件：积压回落到高水位以下或通道死亡
	pend int64      // 已交给 writeFrame 尚未写完的字节量（通道写积压）
	dead bool
}

// vscSession is one bridged client. Bytes worker→client stage in queue
// (bounded: push never blocks, so the channel read loop can't head-of-line
// block on a slow client); bytes client→worker are forwarded synchronously
// from the read loop.
type vscSession struct {
	id      string
	tun     *vscTunnel
	ch      *vscChannel
	client  net.Conn
	br      *bufio.Reader
	upgrade bool        // client 是 WebSocket 升级（中继已代答 101，无 502 兜底）
	wrote   atomic.Bool // 已向 client 转发过后端数据字节

	mu       sync.Mutex
	cond     *sync.Cond
	queue    [][]byte
	bytes    int
	paused   bool // 已请 worker 停发该会话（staging 超高水位）
	peerHold bool // worker 请中继停读 client
	dead     bool
}

var (
	vscOnce sync.Once
	vscInst *vscRelay
)

func init() {
	registerRelay("vscode", func(target, mount string) http.Handler {
		return vscGetRelay().RouteHandler(mount)
	})
}

// vscGetRelay returns the process-wide relay://vscode instance.
func vscGetRelay() *vscRelay {
	vscOnce.Do(func() { vscInst = newVscRelay() })
	return vscInst
}

// newVscRelay builds a fresh relay instance (also the test-isolation entry).
func newVscRelay() *vscRelay {
	return &vscRelay{
		tunnels:       make(map[string]*vscTunnel),
		devices:       make(map[string]*vscChannel),
		tunnelID:      tunnelIDCfg,
		probeInterval: vscProbeInterval,
		probeMaxMiss:  vscProbeMaxMiss,
		grace:         vscGrace,
		sinkHighWater: vscSinkHighWater,
		sinkCap:       vscSinkCap,
		chanHighWater: vscChanHighWater,
	}
}

// RouteHandler mounts the relay under the route prefix. Prefix stripping is
// segment-boundary exact (mount, mount+"/…"); when the path does not carry
// the prefix (e.g. ^-regex mounts), it is used as-is. The single endpoint
// lives on any path: kind=worker registers a tunnel, anything carrying
// ?tkn= is a client, /status reports state.
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
		q := req.URL.Query()
		switch {
		case p == "/status":
			r.handleStatus(w)
		case q.Get("kind") == "worker":
			r.handleWorker(w, req, p)
		case vscTokenOf(q) != "":
			r.handleClient(w, req, p)
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

// vscHijack takes the connection over for raw byte use. Reads MUST go through
// the returned bufio.Reader: it may already hold bytes the peer sent beyond
// the request headers (early client data would otherwise be lost). The conn
// is a long-lived pipe: server deadlines are cleared and TCP keepalive is
// turned on (hijackRawConn).
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
	hijackRawConn(conn)
	return conn, brw.Reader, nil
}

// vscSessionID mints a 12-hex session id (frame overhead stays small).
func vscSessionID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("%012x", time.Now().UnixNano()&0xffffffffffff)
	}
	return hex.EncodeToString(b[:])
}

// vscTunnelLabel — 日志里的隧道标签：名字/设备 + 令牌短前缀（完整令牌是接入
// 凭证，不落日志）。
func vscTunnelLabel(tun *vscTunnel) string {
	label := tun.name
	if label == "" {
		label = tun.device
	}
	prefix := tun.token
	if len(prefix) > 6 {
		prefix = prefix[:6]
	}
	return fmt.Sprintf("%q#%s…", label, prefix)
}

// vscClip truncates a peer-supplied string for logging.
func vscClip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

// ---- 控制帧 ----

type (
	vscReadyMsg struct {
		Type     string  `json:"type"`
		Protocol int     `json:"protocol"`
		Relay    string  `json:"relay"`
		TunnelID *string `json:"tunnelId"` // 本实现无 relay 身份配置，恒 null
		Name     string  `json:"name"`
		Path     string  `json:"path"`
	}
	vscOpenMsg struct {
		Type string `json:"type"`
		ID   string `json:"id"`
		Addr string `json:"addr"`
		Path string `json:"path"`
	}
	vscFlowMsg struct {
		Type string `json:"type"` // pause | resume
		ID   string `json:"id"`
	}
	vscCloseMsg struct {
		Type   string `json:"type"`
		ID     string `json:"id"`
		Reason string `json:"reason"`
	}
	vscProbeMsg struct {
		Type     string `json:"type"`
		Protocol int    `json:"protocol"`
	}
)

func (ch *vscChannel) sendControl(v any) {
	b, err := json.Marshal(v)
	if err != nil {
		return
	}
	_ = ch.ws.wsWriteText(b)
}

// writeSession writes one session data frame on the channel (session read
// loops only). Past the channel high water (peer reading slowly, slow link)
// it blocks until the backlog drains: client reads stop here, and TCP
// backpressure propagates to the source — the backlog never piles up in relay
// memory, and healthy control frames (open/close/probe, which bypass this
// gate) keep flowing. Contrast with a plain blocking write: without the gate,
// a congested channel stalls writers into the 30s write deadline, whose
// errors kill sessions and then starve the liveness probe.
func (ch *vscChannel) writeSession(frame []byte) error {
	ch.mu.Lock()
	for ch.pend >= int64(ch.relay.chanHighWater) {
		if ch.dead {
			ch.mu.Unlock()
			return wsErrClosed
		}
		ch.cond.Wait()
	}
	ch.pend += int64(len(frame))
	ch.mu.Unlock()

	err := ch.ws.writeFrame(wsOpBinary, frame)

	ch.mu.Lock()
	ch.pend -= int64(len(frame))
	if ch.pend < int64(ch.relay.chanHighWater) {
		ch.cond.Broadcast()
	}
	ch.mu.Unlock()
	return err
}

func (ch *vscChannel) session(id string) *vscSession {
	ch.relay.mu.Lock()
	defer ch.relay.mu.Unlock()
	return ch.tun.sessions[id]
}

// ---- worker 注册与通道 ----

var vscDeviceRe = regexp.MustCompile(`^[\w-]{1,64}$`)

// handleWorker registers a host channel: validate, complete the WebSocket
// handshake, then serve the multiplexed link until it dies.
func (r *vscRelay) handleWorker(w http.ResponseWriter, req *http.Request, p string) {
	q := req.URL.Query()
	parts := strings.SplitN(vscTokenOf(q), ".", 3)
	token, device := parts[0], ""
	if len(parts) > 1 {
		device = parts[1]
	}
	tunnelRef := ""
	if len(parts) > 2 {
		tunnelRef = parts[2]
	}
	name := vscClip(strings.TrimSpace(q.Get("name")), 128)
	switch {
	case token == "":
		http.Error(w, "vscode relay: worker needs ?tkn=<token>.<deviceId>[.<tunnelId>]", http.StatusUnauthorized)
		return
	case device == "":
		http.Error(w, "vscode relay: worker tkn must be <token>.<deviceId>[.<tunnelId>]", http.StatusBadRequest)
		return
	case !vscDeviceRe.MatchString(device):
		http.Error(w, "vscode relay: deviceId must be 1..64 chars of [A-Za-z0-9_-]", http.StatusBadRequest)
		return
	// relay 身份校验：配置了 tunnel_id 时，带了第三段就必须是同一段（连错 relay
	// 了）；缺段放行，由 ready 帧回告，worker 采纳后写回配置。
	case r.tunnelID != "" && tunnelRef != "" && tunnelRef != r.tunnelID:
		log.Printf("[vscrelay] worker registration rejected: tunnel id mismatch (expected %q, got %q)", r.tunnelID, tunnelRef)
		http.Error(w, "vscode relay: tunnel id mismatch — this relay expects \""+r.tunnelID+"\" in the tkn", http.StatusForbidden)
		return
	case !isWebSocketUpgrade(req):
		http.Error(w, "vscode relay: worker registration expects a WebSocket upgrade request", http.StatusBadRequest)
		return
	}

	// 判定（容量/设备独占）在锁内完成，应答在锁外发出。
	var status int
	var msg string
	r.mu.Lock()
	r.sweepLocked()
	existing := r.tunnels[token]
	switch {
	case existing == nil && len(r.tunnels) >= vscMaxTunnels:
		status, msg = http.StatusServiceUnavailable, "vscode relay: too many tunnels"
	case r.devices[device] != nil && r.devices[device].tun.token != token:
		status, msg = http.StatusConflict, "vscode relay: device "+device+" is already connected as another tunnel"
	}
	r.mu.Unlock()
	if status != 0 {
		log.Printf("[vscrelay] worker registration rejected: %s", msg)
		http.Error(w, msg, status)
		return
	}

	ws, err := wsUpgrade(w, req)
	if err != nil {
		return
	}

	// 同 token 重连：顶掉旧通道（先摘后拆，teardown 不改写新通道状态）。
	var oldCh *vscChannel
	r.mu.Lock()
	if existing = r.tunnels[token]; existing != nil {
		oldCh = existing.ch
		existing.ch = nil
	}
	r.mu.Unlock()
	if oldCh != nil {
		log.Printf("[vscrelay] tunnel %s re-registered; dropping the previous channel", vscTunnelLabel(existing))
		r.dropChannel(existing, oldCh, "re-registered")
	}

	r.mu.Lock()
	tun := r.tunnels[token]
	created := false
	if tun == nil {
		tun = &vscTunnel{token: token, sessions: make(map[string]*vscSession)}
		r.tunnels[token] = tun
		created = true
	}
	// 注册间隙被别的通道抢先（同 token 或同设备）：让位给先到者。
	if tun.ch != nil || (r.devices[device] != nil && r.devices[device].tun.token != token) {
		if created {
			delete(r.tunnels, token) // 空条目不落账（deadSince 为零，惰性清扫不会回收它）
		}
		r.mu.Unlock()
		log.Printf("[vscrelay] worker %s registration superseded by a concurrent registration", device)
		_ = ws.conn.Close()
		return
	}
	ch := &vscChannel{relay: r, tun: tun, ws: ws, device: device, done: make(chan struct{})}
	ch.cond = sync.NewCond(&ch.mu)
	tun.device = device
	tun.name = name
	tun.path = p
	tun.addr = clientRemoteAddr(req)
	tun.since = time.Now()
	tun.deadSince = time.Time{}
	tun.ch = ch
	r.devices[device] = ch
	r.mu.Unlock()

	// ready 帧回告 relay 身份：未配置 tunnel_id 时恒为 null。
	var tid *string
	if r.tunnelID != "" {
		tid = &r.tunnelID
	}
	ch.sendControl(vscReadyMsg{Type: "ready", Protocol: vscProtocolVersion, Relay: vscRelayVersion, TunnelID: tid, Name: name, Path: p})
	log.Printf("[vscrelay] tunnel %s registered (device=%s name=%s path=%s remote=%s)",
		vscTunnelLabel(tun), device, nameOrDash(name), pathOrDash(p), tun.addr)

	go ch.probeLoop()
	ch.readLoop()
}

func nameOrDash(s string) string {
	if s == "" {
		return "-"
	}
	return s
}

func pathOrDash(s string) string {
	if s == "" || s == "/" {
		return "-"
	}
	return s
}

// probeLoop asks the worker for a version reply every interval; consecutive
// misses past the max kill the channel (WS close/EOF covers physical death).
func (ch *vscChannel) probeLoop() {
	t := time.NewTicker(ch.relay.probeInterval)
	defer t.Stop()
	for {
		select {
		case <-ch.done:
			return
		case <-t.C:
			if ch.missed.Add(1) > int32(ch.relay.probeMaxMiss) {
				ch.relay.dropChannel(ch.tun, ch, "service probe timeout")
				return
			}
			ch.sendControl(vscProbeMsg{Type: "version", Protocol: vscProtocolVersion})
		}
	}
}

// readLoop demultiplexes the channel until it dies: text frames are control
// JSON, binary frames carry session data ([1B idLen][id][payload]).
func (ch *vscChannel) readLoop() {
	for {
		payload, op, err := ch.ws.wsReadMessage()
		if err != nil {
			ch.relay.dropChannel(ch.tun, ch, "channel read: "+err.Error())
			return
		}
		if op == wsOpText {
			ch.onControl(payload)
		} else {
			ch.onData(payload)
		}
	}
}

func (ch *vscChannel) onControl(b []byte) {
	if len(b) > vscControlCap {
		return
	}
	var m struct {
		Type    string `json:"type"`
		ID      string `json:"id"`
		Reason  string `json:"reason"`
		Version string `json:"version"`
	}
	if json.Unmarshal(b, &m) != nil {
		return
	}
	switch m.Type {
	case "version":
		// host 对探测的应答：刷新在线判定，顺带记录服务版本。
		ch.missed.Store(0)
		if m.Version != "" {
			ch.relay.mu.Lock()
			ch.tun.version = m.Version
			ch.relay.mu.Unlock()
		}
	case "close":
		if m.ID != "" {
			ch.relay.endSession(ch.tun, m.ID, "host closed: "+vscClip(m.Reason, 120))
		}
	case "pause", "resume":
		if s := ch.session(m.ID); s != nil {
			s.setPeerHold(m.Type == "pause")
		}
	}
}

func (ch *vscChannel) onData(b []byte) {
	if len(b) < 1 {
		return
	}
	idLen := int(b[0])
	if len(b) < 1+idLen {
		return
	}
	s := ch.session(string(b[1 : 1+idLen]))
	if s == nil {
		return
	}
	s.push(b[1+idLen:])
}

// dropChannel tears a worker channel down exactly once: drop the socket
// (conn.Close directly — wsConn's close would queue behind an in-flight
// blocked write), release the device mapping and every session, mark the
// tunnel offline inside the grace window.
func (r *vscRelay) dropChannel(tun *vscTunnel, ch *vscChannel, why string) {
	ch.gone.Do(func() {
		close(ch.done)
		_ = ch.ws.conn.Close()
		r.mu.Lock()
		if tun.ch == ch {
			tun.ch = nil
			tun.deadSince = time.Now()
		}
		if r.devices[ch.device] == ch {
			delete(r.devices, ch.device)
		}
		ids := make([]string, 0, len(tun.sessions))
		for id := range tun.sessions {
			ids = append(ids, id)
		}
		r.mu.Unlock()
		for _, id := range ids {
			r.endSession(tun, id, "channel down: "+why)
		}
		// 唤醒在通道积压门上等待的会话写循环（writeSession）：通道已死，让它们
		// 立即带错返回，而不是永远等一个不会再来的高水位回落。
		ch.mu.Lock()
		ch.dead = true
		ch.cond.Broadcast()
		ch.mu.Unlock()
		log.Printf("[vscrelay] tunnel %s channel down (%s); token reserved for %s", vscTunnelLabel(tun), why, r.grace)
	})
}

// ---- client 会话 ----

// handleClient routes a client request by the first tkn segment and bridges
// it onto the tunnel: the original request head (rebuilt) is forwarded as the
// first data frame, then raw bytes flow both ways.
func (r *vscRelay) handleClient(w http.ResponseWriter, req *http.Request, p string) {
	tkn := vscTokenOf(req.URL.Query())
	key := tkn
	if i := strings.IndexByte(tkn, '.'); i >= 0 {
		key = tkn[:i]
	}

	// 判定在锁内完成，应答在锁外发出。
	var status int
	var msg string
	var tun *vscTunnel
	var ch *vscChannel
	r.mu.Lock()
	r.sweepLocked()
	tun = r.tunnels[key]
	switch {
	case tun == nil:
		status, msg = http.StatusForbidden, "vscode relay: unknown tunnel token"
	case tun.ch == nil:
		status, msg = http.StatusServiceUnavailable, "vscode relay: tunnel is offline"
	case r.sessions >= vscMaxSessions || len(tun.sessions) >= vscMaxSessionsPerTunnel:
		status, msg = http.StatusServiceUnavailable, "vscode relay: too many active sessions"
	default:
		ch = tun.ch
	}
	r.mu.Unlock()
	if status != 0 {
		if status == http.StatusForbidden {
			log.Printf("[vscrelay] client %s rejected: unknown tunnel token", req.RemoteAddr)
		}
		http.Error(w, msg, status)
		return
	}

	addr := clientRemoteAddr(req)
	conn, br, err := vscHijack(w)
	if err != nil {
		return
	}
	upgrade := isWebSocketUpgrade(req)
	if upgrade {
		// 升级请求由中继代答 101（worker 会吞掉本机 server 自己的 101 头），
		// 之后这条腿就是原始字节流。
		fmt.Fprintf(conn, "HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\nSec-WebSocket-Accept: %s\r\n\r\n",
			wsAcceptValue(strings.TrimSpace(req.Header.Get("Sec-WebSocket-Key"))))
	}

	s := &vscSession{
		id: vscSessionID(), tun: tun, ch: ch,
		client: conn, br: br, upgrade: upgrade,
	}
	s.cond = sync.NewCond(&s.mu)
	r.mu.Lock()
	tun.sessions[s.id] = s
	r.sessions++
	r.mu.Unlock()

	ch.sendControl(vscOpenMsg{Type: "open", ID: s.id, Addr: addr, Path: p})
	log.Printf("[vscrelay] tunnel %s session %s opened by %s → %s", vscTunnelLabel(tun), s.id[:8], addr, p)
	go s.writeLoop()
	s.readLoop(req)
}

// readLoop forwards the client's bytes to the worker: the original request
// head first (rebuilt via req.Write — query stays as the client sent it),
// then raw chunks off the hijacked connection. peerHold gates the read
// direction (worker asked us to stop because its backend can't keep up);
// channel congestion gates the writes (writeSession).
func (s *vscSession) readLoop(req *http.Request) {
	var head bytes.Buffer
	if err := req.Write(&head); err != nil || head.Len() == 0 {
		s.ch.relay.endSession(s.tun, s.id, "client request head")
		return
	}
	frame := make([]byte, 0, 1+len(s.id)+head.Len())
	frame = append(frame, byte(len(s.id)))
	frame = append(frame, s.id...)
	frame = append(frame, head.Bytes()...)
	if err := s.ch.writeSession(frame); err != nil {
		s.ch.relay.endSession(s.tun, s.id, "channel write: "+err.Error())
		return
	}
	idOff := 1 + len(s.id)
	buf := make([]byte, idOff+vscChunkBytes)
	buf[0] = byte(len(s.id))
	copy(buf[1:], s.id)
	for {
		s.mu.Lock()
		for s.peerHold && !s.dead {
			s.cond.Wait()
		}
		dead := s.dead
		s.mu.Unlock()
		if dead {
			return
		}
		n, err := s.br.Read(buf[idOff:])
		if n > 0 {
			if werr := s.ch.writeSession(buf[:idOff+n]); werr != nil {
				s.ch.relay.endSession(s.tun, s.id, "channel write: "+werr.Error())
				return
			}
		}
		if err != nil {
			s.ch.sendControl(vscCloseMsg{Type: "close", ID: s.id, Reason: "client closed"})
			s.ch.relay.endSession(s.tun, s.id, "client closed")
			return
		}
	}
}

// push stages backend→client bytes (copied: the payload aliases the channel
// read buffer). Over the high water we ask the worker to stop sending this
// session; past the absolute cap (worker ignored pause) the session dies.
func (s *vscSession) push(data []byte) {
	s.mu.Lock()
	if s.dead {
		s.mu.Unlock()
		return
	}
	s.wrote.Store(true)
	if s.bytes+len(data) > s.ch.relay.sinkCap {
		s.mu.Unlock()
		s.ch.relay.endSession(s.tun, s.id, "client sink overflow")
		return
	}
	b := make([]byte, len(data))
	copy(b, data)
	s.queue = append(s.queue, b)
	s.bytes += len(data)
	var pause bool
	if !s.paused && s.bytes >= s.ch.relay.sinkHighWater {
		s.paused = true
		pause = true
	}
	s.cond.Broadcast()
	s.mu.Unlock()
	if pause {
		s.ch.sendControl(vscFlowMsg{Type: "pause", ID: s.id})
	}
}

// setPeerHold honors the worker's pause/resume for this session's client
// read direction.
func (s *vscSession) setPeerHold(hold bool) {
	s.mu.Lock()
	s.peerHold = hold
	s.cond.Broadcast()
	s.mu.Unlock()
}

// writeLoop drains the staging queue into the client socket; once a pause is
// in effect and the queue empties, the worker is told to resume.
func (s *vscSession) writeLoop() {
	for {
		s.mu.Lock()
		for len(s.queue) == 0 && !s.dead {
			s.cond.Wait()
		}
		if s.dead {
			s.mu.Unlock()
			return
		}
		chunk := s.queue[0]
		s.queue[0] = nil
		s.queue = s.queue[1:]
		var resume bool
		if s.paused && len(s.queue) == 0 {
			s.paused = false
			resume = true
		}
		s.mu.Unlock()

		if _, err := s.client.Write(chunk); err != nil {
			s.ch.relay.endSession(s.tun, s.id, "client write: "+err.Error())
			return
		}
		if resume {
			s.ch.sendControl(vscFlowMsg{Type: "resume", ID: s.id})
		}
	}
}

// endSession tears a session down exactly once (whoever gets it out of the
// map first wins); the client socket always ends up closed. A non-upgrade
// client that never received backend bytes gets a clean 502 (its request was
// forwarded but the backend never answered).
func (r *vscRelay) endSession(tun *vscTunnel, id, why string) {
	r.mu.Lock()
	s := tun.sessions[id]
	if s == nil {
		r.mu.Unlock()
		return
	}
	delete(tun.sessions, id)
	r.sessions--
	r.mu.Unlock()

	s.mu.Lock()
	s.dead = true
	s.cond.Broadcast()
	s.mu.Unlock()
	if !s.upgrade && !s.wrote.Load() {
		fmt.Fprintf(s.client, "HTTP/1.1 502 Bad Gateway\r\nContent-Length: 0\r\nConnection: close\r\n\r\n")
	}
	_ = s.client.Close()
	log.Printf("[vscrelay] tunnel %s session %s closed (%s)", vscTunnelLabel(tun), vscClip(id, 8), why)
}

// ---- 状态与保留期 ----

// sweepLocked releases tunnel entries whose grace has run out. Lazy — runs on
// registration and status, no background timer.
func (r *vscRelay) sweepLocked() {
	now := time.Now()
	for token, tun := range r.tunnels {
		if tun.ch == nil && !tun.deadSince.IsZero() && now.Sub(tun.deadSince) >= r.grace {
			delete(r.tunnels, token)
			log.Printf("[vscrelay] tunnel %s released (offline beyond the %s grace)", vscTunnelLabel(tun), r.grace)
		}
	}
}

// handleStatus reports running state as JSON (ops endpoint).
func (r *vscRelay) handleStatus(w http.ResponseWriter) {
	r.mu.Lock()
	r.sweepLocked()
	tunnels := make([]map[string]any, 0, len(r.tunnels))
	for _, tun := range r.tunnels {
		tunnels = append(tunnels, map[string]any{
			"name":     tun.name,
			"device":   tun.device,
			"path":     tun.path,
			"online":   tun.ch != nil,
			"since":    tun.since.Format(time.RFC3339),
			"addr":     tun.addr,
			"version":  tun.version,
			"sessions": len(tun.sessions),
		})
	}
	n := r.sessions
	r.mu.Unlock()
	sort.Slice(tunnels, func(i, j int) bool { return tunnels[i]["name"].(string) < tunnels[j]["name"].(string) })

	w.Header().Set("Content-Type", "application/json")
	var tunnelID any
	if r.tunnelID != "" {
		tunnelID = r.tunnelID
	}
	_ = json.NewEncoder(w).Encode(map[string]any{
		"now":            time.Now().Format(time.RFC3339),
		"relay":          vscRelayVersion,
		"protocol":       vscProtocolVersion,
		"tunnelId":       tunnelID,
		"tunnels":        tunnels,
		"tunnelSessions": n,
	})
}
