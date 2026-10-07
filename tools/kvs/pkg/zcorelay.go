package pkg

// zco relay (relay://zcode) — in-process backend porting tools/zrelay (device ↔
// terminal pairing for ZCode web-remote-control). Role-agnostic: desktop and
// web client share one endpoint; who is who comes from the first auth_init
// message, not the URL. The routing prefix is pure operator config:
//
//	ws~/ws = relay://zcode
//
// Desktop: ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://host:port/ws[?mid=<uuid>]
// Web page: endpoint injected via KVS_CC_SED_<名称> rules (overrideUrl:`wss://>host</ws`).
//
// Protocol: JSON text frames; device registers once then authenticates via
// HMAC-SHA256 challenge/response; terminal authenticates with the device_sid
// + pass_hash from the QR URL; when both online ("matched"), data frames are
// forwarded verbatim. State (device registry + manager app records) lives in
// zcodex.json, managed by the shared store singleton (see zcoState).

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// protocol constants (mirror zrelay/src/protocol.ts)
// ---------------------------------------------------------------------------

const (
	zcoDeviceSIDPrefix = "d_"
	zcoMaxFrameBytes   = 8 * 1024 * 1024
	zcoAuthTimeout     = 15 * time.Second
)

// zcoRole is the peer kind declared in auth_init.
type zcoRole string

const (
	zcoRoleDevice   zcoRole = "device"
	zcoRoleTerminal zcoRole = "terminal"
)

// zcoPairStatus is the pairing state reported to clients.
type zcoPairStatus string

const (
	zcoPairWaiting zcoPairStatus = "waiting"
	zcoPairMatched zcoPairStatus = "matched"
)

// Error codes understood by the official clients.
const (
	zcoErrKicked        = "KICKED"
	zcoErrAuthFailed    = "AUTH_FAILED"
	zcoErrDeviceOffline = "DEVICE_OFFLINE"
	zcoErrWrongParam    = "WRONG_PARAM"
	zcoErrInternal      = "INTERNAL"
)

// zcoMeta is the optional diagnostics block clients may attach.
type zcoMeta map[string]any

// ---------------------------------------------------------------------------
// wire messages
// ---------------------------------------------------------------------------

// zcoClientMsg is one inbound frame; fields are shared across message
// types (Type discriminates).
type zcoClientMsg struct {
	Type      string          `json:"type"`
	DeviceMid string          `json:"device_mid,omitempty"` // device_register_init
	PassHash  string          `json:"pass_hash,omitempty"`  // device_register_init
	Role      zcoRole         `json:"role,omitempty"`       // auth_init
	DeviceSid string          `json:"device_sid,omitempty"` // auth_init/auth_response/pair_status_query
	Proof     string          `json:"proof,omitempty"`      // auth_response
	Meta      zcoMeta         `json:"meta,omitempty"`
	ClientTs  *int64          `json:"client_ts,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"` // data
}

// zcoServerMsg is one outbound frame.
type zcoServerMsg struct {
	Type       string        `json:"type"`
	DeviceSid  string        `json:"device_sid,omitempty"` // device_register_ack
	Nonce      string        `json:"nonce,omitempty"`      // auth_challenge
	PairStatus zcoPairStatus `json:"pair_status,omitempty"`
	Code       string        `json:"code,omitempty"` // error
	Message    string        `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// crypto helpers (must match the clients byte for byte)
// ---------------------------------------------------------------------------

// zcoPassHash = base64(sha256(password)); password is a 24-byte base64url string.
func zcoPassHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// zcoProof = base64url(HMAC-SHA256(key=passHash, msg=nonce|role|deviceSid)).
// The HMAC key is the *string* pass_hash (utf-8), exactly like the clients.
func zcoProof(passHash, nonce string, role zcoRole, deviceSid string) string {
	mac := hmac.New(sha256.New, []byte(passHash))
	fmt.Fprintf(mac, "%s|%s|%s", nonce, role, deviceSid)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// zcoVerifyProof is a constant-time proof comparison.
func zcoVerifyProof(passHash, nonce string, role zcoRole, deviceSid, proof string) bool {
	expected := zcoProof(passHash, nonce, role, deviceSid)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(proof)) == 1
}

// zcoNewDeviceSid mints a device_sid shaped like the official ones:
// "d_" + 22 url-safe chars.
func zcoNewDeviceSid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return zcoDeviceSIDPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// zcoNewNonce returns a random 24-byte base64url nonce.
func zcoNewNonce() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// device store (zrelay-state.json compatible)
// ---------------------------------------------------------------------------

// zcoDeviceRecord is a registered desktop identity.
type zcoDeviceRecord struct {
	DeviceSid  string  `json:"device_sid"`
	DeviceMid  string  `json:"device_mid"`
	PassHash   string  `json:"pass_hash"`
	Meta       zcoMeta `json:"meta,omitempty"`
	CreatedAt  int64   `json:"created_at"`
	UpdatedAt  int64   `json:"updated_at"`
	LastSeenAt int64   `json:"last_seen_at,omitempty"`
	LastIP     string  `json:"last_ip,omitempty"` // last connection source IP
}

// zcoStoreFile is the on-disk state file format (version 1, zrelay-compatible).
type zcoStoreFile struct {
	Version    int               `json:"version"`
	Devices    []zcoDeviceRecord `json:"devices"`
	Apps       []managerApp      `json:"apps,omitempty"`         // manager app records (zcode is one app type among several)
	Order      []string          `json:"order,omitempty"`        // manager card order (user-arranged entry ids)
	Tools      []managerTool     `json:"tools,omitempty"`        // manager toolbar quick links (name → addr)
	Notes      map[string]string `json:"notes,omitempty"`        // manager per-entry remarks (entry id → note text)
	VscOpenExt string            `json:"vsc_open_ext,omitempty"` // "open in VS Code" deep-link extension id (vscode://<ext>/open…)
	// VscLocalAuth: deep links carry no gateway-signed tkn; the extension
	// resolves per-domain credentials from its own local store instead.
	VscLocalAuth bool `json:"vsc_local_auth,omitempty"`
}

// zcoStore is a JSON-file backed registry with two in-memory indexes
// (sid → record, mid → sid) plus the manager app list. Writes are atomic and
// debounced (flush on change, at most once per second).
type zcoStore struct {
	file         string
	mu           sync.Mutex
	bySid        map[string]*zcoDeviceRecord
	byMid        map[string]string
	apps         []managerApp
	order        []string          // manager card order (user-arranged entry ids)
	tools        []managerTool     // manager toolbar quick links
	notes        map[string]string // manager per-entry remarks (entry id → note)
	vscOpenExt   string            // "open in VS Code" deep-link extension id ("" = env default)
	vscLocalAuth bool              // deep links omit the gateway tkn; extension uses its local per-domain credentials
	notify       chan struct{}
	dirty        bool
}

// zcoLoadStore opens (or creates) the state file. When the file is absent
// and it is the new zcodex.json name, the legacy zcoded.json (devices only)
// is loaded instead so registrations survive the rename; the next flush
// writes the new name.
func zcoLoadStore(file string) *zcoStore {
	s := &zcoStore{
		file:   file,
		bySid:  make(map[string]*zcoDeviceRecord),
		byMid:  make(map[string]string),
		notify: make(chan struct{}, 1),
	}
	data, err := os.ReadFile(file)
	if err != nil && filepath.Base(file) == "zcodex.json" {
		if legacy, lerr := os.ReadFile(filepath.Join(filepath.Dir(file), "zcoded.json")); lerr == nil {
			data, err = legacy, nil
		}
	}
	if err != nil {
		return s // missing file → empty store
	}
	var parsed zcoStoreFile
	if err := json.Unmarshal(data, &parsed); err != nil {
		log.Printf("[zcode] could not read %s: %v — starting empty", file, err)
		return s
	}
	for _, rec := range parsed.Devices {
		if rec.DeviceSid == "" || rec.DeviceMid == "" {
			continue
		}
		r := rec
		s.bySid[r.DeviceSid] = &r
		s.byMid[r.DeviceMid] = r.DeviceSid
	}
	s.apps = parsed.Apps
	s.order = parsed.Order
	s.tools = parsed.Tools
	s.notes = parsed.Notes
	s.vscOpenExt = parsed.VscOpenExt
	s.vscLocalAuth = parsed.VscLocalAuth
	return s
}

// get returns the record for a device_sid (nil when unknown).
func (s *zcoStore) get(sid string) *zcoDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bySid[sid]
}

// getByMid returns the record for a device_mid (nil when unknown).
func (s *zcoStore) getByMid(mid string) *zcoDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.byMid[mid]
	if !ok {
		return nil
	}
	return s.bySid[sid]
}

// upsert inserts or updates a device and returns the effective record.
func (s *zcoStore) upsert(rec zcoDeviceRecord) *zcoDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := time.Now().UnixMilli()
	if existing, ok := s.bySid[rec.DeviceSid]; ok {
		rec.CreatedAt = existing.CreatedAt
	}
	rec.UpdatedAt = now
	r := rec
	s.bySid[r.DeviceSid] = &r
	s.byMid[r.DeviceMid] = r.DeviceSid
	s.dirty = true
	return &r
}

// touch updates last_seen_at (and last_ip when non-empty) for a device.
func (s *zcoStore) touch(sid, ip string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r, ok := s.bySid[sid]; ok {
		r.LastSeenAt = time.Now().UnixMilli()
		if ip != "" {
			r.LastIP = ip
		}
		s.dirty = true
	}
}

// size reports the number of registered devices.
func (s *zcoStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bySid)
}

// remove forgets a device entirely (pairing state + mid index). Returns
// whether the device existed. The manager's card delete needs this: device
// cards are synthesized from the device list, so without removal the card
// would re-appear on the next render. A live device re-registers (fresh
// pairing) on its next reconnect.
func (s *zcoStore) remove(sid string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.bySid[sid]
	if !ok {
		return false
	}
	delete(s.bySid, sid)
	if s.byMid[r.DeviceMid] == sid {
		delete(s.byMid, r.DeviceMid)
	}
	s.dirty = true
	return true
}

// list returns all device records (map order; callers sort if needed).
func (s *zcoStore) list() []zcoDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]zcoDeviceRecord, 0, len(s.bySid))
	for _, r := range s.bySid {
		out = append(out, *r)
	}
	return out
}

// vscOpenExtValue returns the stored "open in VS Code" extension id
// (vscode://<ext>/open… deep-link target); "" = use the env default.
func (s *zcoStore) vscOpenExtValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vscOpenExt
}

// saveVscOpenExt persists the "open in VS Code" extension id ("" = back to
// the env default). Takes effect immediately — the value is read per request.
func (s *zcoStore) saveVscOpenExt(ext string) {
	s.mu.Lock()
	s.vscOpenExt = ext
	s.dirty = true
	s.mu.Unlock()
	s.requestFlush()
}

// vscLocalAuthValue reports whether vscode:// deep links should omit the
// gateway-signed tkn (the extension then resolves credentials per domain
// from its own local store).
func (s *zcoStore) vscLocalAuthValue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vscLocalAuth
}

// saveVscLocalAuth toggles the "use local auth info" mode. Read per request.
func (s *zcoStore) saveVscLocalAuth(on bool) {
	s.mu.Lock()
	s.vscLocalAuth = on
	s.dirty = true
	s.mu.Unlock()
	s.requestFlush()
}

// flush writes the state file atomically when dirty.
func (s *zcoStore) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	out := zcoStoreFile{
		Version:      1,
		Devices:      make([]zcoDeviceRecord, 0, len(s.bySid)),
		Apps:         append([]managerApp(nil), s.apps...),
		Order:        append([]string(nil), s.order...),
		Tools:        append([]managerTool(nil), s.tools...),
		Notes:        s.notes,
		VscOpenExt:   s.vscOpenExt,
		VscLocalAuth: s.vscLocalAuth,
	}
	for _, r := range s.bySid {
		out.Devices = append(out.Devices, *r)
	}
	data, err := json.MarshalIndent(out, "", "  ")
	if err != nil {
		log.Printf("[zcode] state marshal FAIL: %v", err)
		return
	}
	if err := atomicWriteFile(s.file, append(data, '\n'), 0o644); err != nil {
		log.Printf("[zcode] state write FAIL: %s → %v", s.file, err)
		return
	}
	s.dirty = false
}

// FlushNow persists pending manager/relay state synchronously. Called on the
// shutdown path so changes made less than a debounce interval ago (manager app
// edits, order, notes) are not lost when kvs exits.
func FlushNow() { zcoState().flush() }

// flushLoop debounces state-file writes (at most one per second).
func (s *zcoStore) flushLoop() {
	for range s.notify {
		time.Sleep(time.Second)
		s.flush()
	}
}

// requestFlush asks the flush loop to persist state soon.
func (s *zcoStore) requestFlush() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// relay — the pairing core
// ---------------------------------------------------------------------------

// zcoConn is one live connection, authenticated or not.
type zcoConn struct {
	id            int64
	ws            *wsConn
	remote        string
	mid           string // ?mid= query parameter
	role          zcoRole
	deviceSid     string
	authMeta      zcoMeta
	pendingNonce  string
	authenticated bool
	detached      bool
	superseded    bool // replaced by a newer connection; suppresses status churn
}

// zcoRelay is the in-process pairing relay. One instance serves any number
// of "relay://" backends; typically there is exactly one, named by the
// backend target (e.g. relay://zcode). All relays share one device
// store (the zcodex.json singleton).
type zcoRelay struct {
	mu     sync.Mutex
	store  *zcoStore
	nextID int64
	conns  map[int64]*zcoConn
	// device_sid → the single live device connection
	devices map[string]*zcoConn
	// device_sid → live terminal connections
	terminals map[string]map[int64]*zcoConn
}

var (
	zcoRelaysMu sync.Mutex
	zcoRelays   = map[string]*zcoRelay{}

	// zcoHome is the kvs working directory (top-level home), injected
	// via SetZcoHome from main() before backends are built.
	zcoHome string

	// zcoStateOnce guards the shared zcodex.json store singleton, used by
	// every relay AND the manager so both always see the same data.
	zcoStateOnce sync.Once
	zcoStateInst *zcoStore
)

func init() {
	registerRelay("zcode", zcoRelayHandler)
}

// zcoRelayHandler mounts the named zcode relay instance behind an
// upgrade-only gate: anything but a WebSocket handshake is a 426 (the
// protocol has no plain-HTTP surface; status lives in the manager UI).
func zcoRelayHandler(target, mount string) http.Handler {
	relay := zcoGetRelay(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			w.Header().Set("Upgrade", "websocket")
			http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
			return
		}
		relay.Handle(w, r)
	})
}

// SetZcoHome sets the base directory for the state file
// ({home}/zcodex.json). Call once from main() with cfg.Home.
func SetZcoHome(home string) { zcoHome = home }

// zcoState returns the shared zcodex.json store singleton, creating it (and
// its flush loop) on first use. The device registry must persist across
// restarts (otherwise the desktop's saved credentials become unknown and it
// gets AUTH_FAILED), so the state file lives under the kvs home directory:
// {KVS_HOME:-.}/zcodex.json (legacy zcoded.json is loaded as a fallback).
func zcoState() *zcoStore {
	zcoStateOnce.Do(func() {
		zcoStateInst = zcoLoadStore(filepath.Join(zcoHome, "zcodex.json"))
		log.Printf("[manager] state store ready: %s (devices: %d, apps: %d)",
			zcoStateInst.file, zcoStateInst.size(), len(zcoStateInst.apps))
		go zcoStateInst.flushLoop()
	})
	return zcoStateInst
}

// zcoGetRelay returns (creating on first use) the named relay instance.
// The registry routes only "zcode" here, so that is the effective name; the
// parameter stays for explicit call sites (the manager reads the same
// instance).
func zcoGetRelay(name string) *zcoRelay {
	if name == "" {
		name = "zcode"
	}
	zcoRelaysMu.Lock()
	defer zcoRelaysMu.Unlock()
	if r, ok := zcoRelays[name]; ok {
		return r
	}
	r := &zcoRelay{
		store:     zcoState(),
		conns:     make(map[int64]*zcoConn),
		devices:   make(map[string]*zcoConn),
		terminals: make(map[string]map[int64]*zcoConn),
	}
	zcoRelays[name] = r
	log.Printf("[zcode] relay %q ready (state: %s, devices: %d)", name, r.store.file, r.store.size())
	return r
}

// Handle upgrades the request and runs the connection until it closes.
// This is the http.Handler entry point wired by the relay:// backend case.
func (r *zcoRelay) Handle(w http.ResponseWriter, req *http.Request) {
	ws, err := wsUpgrade(w, req)
	if err != nil {
		return
	}
	r.handleConnection(ws, req)
}

// handleConnection registers the connection and pumps messages until close.
func (r *zcoRelay) handleConnection(ws *wsConn, req *http.Request) {
	r.mu.Lock()
	r.nextID++
	conn := &zcoConn{
		id:     r.nextID,
		ws:     ws,
		remote: clientRemoteAddr(req),
		mid:    strings.TrimSpace(req.URL.Query().Get("mid")),
	}
	r.conns[conn.id] = conn
	r.mu.Unlock()
	log.Printf("[zcode] conn#%d open (remote=%s mid=%s)", conn.id, conn.remote, conn.mid)

	authTimer := time.AfterFunc(zcoAuthTimeout, func() {
		r.mu.Lock()
		authenticated := conn.authenticated
		r.mu.Unlock()
		if !authenticated {
			log.Printf("[zcode] conn#%d auth timeout, closing", conn.id)
			r.sendError(conn, zcoErrWrongParam, "authentication timeout")
			ws.Close("auth timeout")
			r.detach(conn)
		}
	})

	for {
		payload, op, err := ws.wsReadMessage()
		if err != nil {
			log.Printf("[zcode] conn#%d closed: %v", conn.id, err)
			authTimer.Stop()
			r.detach(conn)
			return
		}
		kvsStats.relayFramesIn.Add(1)
		kvsStats.relayBytesIn.Add(int64(len(payload)))
		if len(payload) > zcoMaxFrameBytes {
			log.Printf("[zcode] conn#%d oversize frame dropped (%d bytes)", conn.id, len(payload))
			continue
		}
		if op != wsOpText {
			log.Printf("[zcode] conn#%d binary frame dropped", conn.id)
			continue
		}
		if err := r.onMessage(conn, payload); err != nil {
			log.Printf("[zcode] conn#%d handler error: %v", conn.id, err)
		}
	}
}

// onMessage dispatches one inbound JSON frame.
func (r *zcoRelay) onMessage(conn *zcoConn, frame []byte) error {
	r.mu.Lock()
	authed := conn.authenticated
	r.mu.Unlock()

	// data 帧快路径：payload 对中继不透明，转发即可，不必把整帧（可能几 MB）解析
	// 一遍再丢掉。只扫描到顶层 "type" 成员为止；判断不出来就走下面的完整解析，
	// 语义不变（唯一区别：声明为 data 但整体不是合法 JSON 的帧会被原样转发给对端，
	// 由对端报错——中继对 data 帧的契约本来就是逐字节转发）。
	if authed && jsonHeadStringIs(frame, "type", "data") {
		kvsStats.relayDataFast.Add(1)
		r.onData(conn, frame)
		return nil
	}
	kvsStats.relayFallbacks.Add(1)

	var msg zcoClientMsg
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Type == "" {
		log.Printf("[zcode] conn#%d invalid relay message", conn.id)
		if !authed {
			r.sendError(conn, zcoErrWrongParam, "malformed message")
		}
		return nil
	}

	if authed {
		switch msg.Type {
		case "pair_status_query":
			r.onPairStatusQuery(conn, &msg)
		case "data":
			r.onData(conn, frame)
		default:
			log.Printf("[zcode] conn#%d unexpected %s after auth", conn.id, msg.Type)
			r.sendError(conn, zcoErrWrongParam, "unexpected message "+msg.Type)
		}
		return nil
	}

	switch msg.Type {
	case "device_register_init":
		r.onRegisterInit(conn, &msg)
	case "auth_init":
		r.onAuthInit(conn, &msg)
	case "auth_response":
		r.onAuthResponse(conn, &msg)
	default:
		log.Printf("[zcode] conn#%d %s before auth_init", conn.id, msg.Type)
		r.sendError(conn, zcoErrWrongParam, msg.Type+" before auth_init")
	}
	return nil
}

// onRegisterInit handles the desktop's very first handshake (no persisted
// credentials yet): register (or re-register) the identity, kick any stale
// socket for the same identity.
func (r *zcoRelay) onRegisterInit(conn *zcoConn, msg *zcoClientMsg) {
	mid := strings.TrimSpace(msg.DeviceMid)
	passHash := strings.TrimSpace(msg.PassHash)
	if mid == "" || passHash == "" {
		r.sendError(conn, zcoErrWrongParam, "device_register_init requires device_mid and pass_hash")
		return
	}
	// The ?mid= query parameter identifies the device when several
	// desktops share one relay; a mismatch means the connection and the
	// message disagree about the identity — refuse so devices cannot
	// register over each other.
	if conn.mid != "" && conn.mid != mid {
		log.Printf("[zcode] conn#%d device_mid mismatch (query=%s message=%s), rejected", conn.id, conn.mid, mid)
		r.sendError(conn, zcoErrWrongParam, "device_mid mismatch with ?mid=")
		return
	}

	existing := r.store.getByMid(mid)
	var record *zcoDeviceRecord
	if existing != nil {
		rec := *existing
		rec.DeviceMid, rec.PassHash, rec.Meta = mid, passHash, msg.Meta
		rec.LastIP = conn.remote
		record = r.store.upsert(rec)
		log.Printf("[zcode] conn#%d re-register for mid=%s -> %s", conn.id, mid, record.DeviceSid)
	} else {
		record = r.store.upsert(zcoDeviceRecord{
			DeviceSid: zcoNewDeviceSid(),
			DeviceMid: mid,
			PassHash:  passHash,
			Meta:      msg.Meta,
			LastIP:    conn.remote,
		})
		log.Printf("[zcode] conn#%d registered mid=%s -> %s (ip=%s)", conn.id, mid, record.DeviceSid, conn.remote)
	}

	r.mu.Lock()
	// A previous socket for this identity is stale now.
	if live, ok := r.devices[record.DeviceSid]; ok && live != conn {
		r.mu.Unlock()
		r.kick(live, "re-registered")
		r.mu.Lock()
	}
	conn.deviceSid = record.DeviceSid
	r.mu.Unlock()

	r.send(conn, zcoServerMsg{Type: "device_register_ack", DeviceSid: record.DeviceSid})
	r.store.requestFlush()
}

// onAuthInit starts the challenge/response for both roles.
func (r *zcoRelay) onAuthInit(conn *zcoConn, msg *zcoClientMsg) {
	sid := strings.TrimSpace(msg.DeviceSid)
	if sid == "" || (msg.Role != zcoRoleDevice && msg.Role != zcoRoleTerminal) {
		r.sendError(conn, zcoErrWrongParam, "auth_init requires role and device_sid")
		return
	}
	record := r.store.get(sid)
	if record == nil {
		log.Printf("[zcode] conn#%d unknown device_sid %s (role=%s)", conn.id, sid, msg.Role)
		r.sendError(conn, zcoErrAuthFailed, "unknown device_sid")
		return
	}
	// Devices are partitioned by ?mid=: a device connection whose mid does
	// not match the registered one is rejected, so multiple desktops on one
	// relay never see or kick each other. Terminals usually connect without
	// ?mid= (or with the device's mid) and are exempt when absent.
	if conn.mid != "" && record.DeviceMid != "" && conn.mid != record.DeviceMid {
		log.Printf("[zcode] conn#%d auth mid mismatch (query=%s registered=%s), rejected", conn.id, conn.mid, record.DeviceMid)
		r.sendError(conn, zcoErrWrongParam, "mid mismatch with registered device")
		return
	}
	r.mu.Lock()
	conn.role = msg.Role
	conn.deviceSid = sid
	conn.authMeta = msg.Meta
	conn.pendingNonce = zcoNewNonce()
	nonce := conn.pendingNonce
	r.mu.Unlock()
	r.send(conn, zcoServerMsg{Type: "auth_challenge", Nonce: nonce})
}

// onAuthResponse verifies the HMAC proof and completes authentication.
func (r *zcoRelay) onAuthResponse(conn *zcoConn, msg *zcoClientMsg) {
	r.mu.Lock()
	role, sid, nonce := conn.role, conn.deviceSid, conn.pendingNonce
	r.mu.Unlock()
	if role == "" || sid == "" || nonce == "" {
		r.sendError(conn, zcoErrWrongParam, "auth_response without auth_init")
		return
	}
	record := r.store.get(sid)
	if record == nil {
		r.sendError(conn, zcoErrAuthFailed, "unknown device_sid")
		return
	}
	if msg.DeviceSid != sid {
		r.sendError(conn, zcoErrWrongParam, "device_sid mismatch")
		return
	}
	if !zcoVerifyProof(record.PassHash, nonce, role, sid, msg.Proof) {
		log.Printf("[zcode] conn#%d invalid proof (role=%s sid=%s remote=%s)", conn.id, role, sid, conn.remote)
		r.sendError(conn, zcoErrAuthFailed, "invalid proof")
		return
	}

	r.store.touch(sid, conn.remote)
	r.store.requestFlush()

	r.mu.Lock()
	conn.pendingNonce = ""
	conn.authenticated = true

	if role == zcoRoleDevice {
		if previous, ok := r.devices[sid]; ok && previous != conn {
			r.mu.Unlock()
			r.kick(previous, "replaced by a newer desktop connection")
			r.mu.Lock()
		}
		r.devices[sid] = conn
	} else {
		set := r.terminals[sid]
		if set == nil {
			set = make(map[int64]*zcoConn)
			r.terminals[sid] = set
		}
		// Single terminal policy: newest wins.
		for _, other := range set {
			r.mu.Unlock()
			r.kick(other, "replaced by a newer terminal connection")
			r.mu.Lock()
		}
		set[conn.id] = conn
	}

	status := r.pairStatusLocked(sid)
	r.mu.Unlock()

	r.send(conn, zcoServerMsg{Type: "auth_ack", PairStatus: status})
	log.Printf("[zcode] conn#%d authenticated (role=%s sid=%s pair=%s)", conn.id, role, sid, status)

	if status == zcoPairMatched {
		r.notifyPeer(sid, conn, status)
	}
}

// onPairStatusQuery is the authenticated heartbeat.
func (r *zcoRelay) onPairStatusQuery(conn *zcoConn, _ *zcoClientMsg) {
	r.mu.Lock()
	sid := conn.deviceSid
	r.mu.Unlock()
	if sid == "" {
		r.sendError(conn, zcoErrWrongParam, "pair_status_query before auth")
		return
	}
	r.store.touch(sid, conn.remote)
	r.send(conn, zcoServerMsg{Type: "pair_status_ack", PairStatus: r.pairStatus(sid)})
}

// onData forwards an opaque payload to the other side, byte for byte.
func (r *zcoRelay) onData(conn *zcoConn, frame []byte) {
	r.mu.Lock()
	sid := conn.deviceSid
	authed := conn.authenticated
	role := conn.role
	if sid == "" || !authed {
		r.mu.Unlock()
		r.sendError(conn, zcoErrWrongParam, "data before auth")
		return
	}
	if r.pairStatusLocked(sid) != zcoPairMatched {
		r.mu.Unlock()
		log.Printf("[zcode] conn#%d data dropped: pair_status=waiting", conn.id)
		return
	}
	// 栈上小数组承接目标：设备↔终端典型的对端数是 1~2，data 帧热路径
	// 每帧省一次堆分配；超过 4 个对端时 append 自动落到堆上（罕见）。
	var targetsBuf [4]*zcoConn
	targets := targetsBuf[:0]
	if role == zcoRoleDevice {
		for _, t := range r.terminals[sid] {
			targets = append(targets, t)
		}
	} else if d, ok := r.devices[sid]; ok {
		targets = append(targets, d)
	}
	r.mu.Unlock()

	if len(targets) == 0 {
		log.Printf("[zcode] conn#%d data dropped: no peer", conn.id)
		return
	}
	for _, t := range targets {
		r.sendRaw(t, frame)
	}
}

// ---------------------------------------------------------------------------
// helpers (all called with r.mu NOT held unless noted)
// ---------------------------------------------------------------------------

// pairStatusLocked reports the pairing state; caller must hold r.mu.
func (r *zcoRelay) pairStatusLocked(sid string) zcoPairStatus {
	if _, ok := r.devices[sid]; ok && len(r.terminals[sid]) > 0 {
		return zcoPairMatched
	}
	return zcoPairWaiting
}

// pairStatus is the locking wrapper.
func (r *zcoRelay) pairStatus(sid string) zcoPairStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairStatusLocked(sid)
}

// notifyPeer pushes a pair_status_ack to the other side of a pairing.
func (r *zcoRelay) notifyPeer(sid string, from *zcoConn, status zcoPairStatus) {
	msg := zcoServerMsg{Type: "pair_status_ack", PairStatus: status}
	r.mu.Lock()
	defer r.mu.Unlock()
	if from.role == zcoRoleDevice {
		for _, t := range r.terminals[sid] {
			r.sendLocked(t, msg)
		}
	} else if d, ok := r.devices[sid]; ok {
		r.sendLocked(d, msg)
	}
}

// send marshals and writes one server message (outside r.mu).
func (r *zcoRelay) send(conn *zcoConn, msg zcoServerMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[zcode] conn#%d outbound %s marshal FAIL: %v", conn.id, msg.Type, err)
		return
	}
	if len(data) > zcoMaxFrameBytes {
		log.Printf("[zcode] conn#%d outbound %s exceeds frame limit", conn.id, msg.Type)
		return
	}
	r.sendRaw(conn, data)
}

// sendLocked is send() for callers already holding r.mu.
func (r *zcoRelay) sendLocked(conn *zcoConn, msg zcoServerMsg) {
	r.mu.Unlock()
	r.send(conn, msg)
	r.mu.Lock()
}

// sendRaw writes a pre-encoded frame verbatim.
func (r *zcoRelay) sendRaw(conn *zcoConn, frame []byte) {
	r.mu.Lock()
	detached := conn.detached
	r.mu.Unlock()
	if detached {
		return
	}
	kvsStats.relayFramesOut.Add(1)
	kvsStats.relayBytesOut.Add(int64(len(frame)))
	if err := conn.ws.wsWriteText(frame); err != nil {
		log.Printf("[zcode] conn#%d send FAIL: %v", conn.id, err)
	}
}

// sendError reports a protocol error to the client.
func (r *zcoRelay) sendError(conn *zcoConn, code, message string) {
	log.Printf("[zcode] conn#%d -> error %s: %s", conn.id, code, message)
	r.send(conn, zcoServerMsg{Type: "error", Code: code, Message: message})
}

// kick tells a peer a newer connection took over, then closes it.
func (r *zcoRelay) kick(conn *zcoConn, reason string) {
	r.mu.Lock()
	if conn.detached {
		r.mu.Unlock()
		return
	}
	conn.superseded = true
	r.mu.Unlock()

	log.Printf("[zcode] conn#%d kicked (%s, role=%s)", conn.id, reason, conn.role)
	r.send(conn, zcoServerMsg{Type: "error", Code: zcoErrKicked, Message: reason})
	conn.ws.Close("kicked")
	r.detach(conn)
}

// detach removes a closed connection from the relay state.
func (r *zcoRelay) detach(conn *zcoConn) {
	r.mu.Lock()
	if conn.detached {
		r.mu.Unlock()
		return
	}
	conn.detached = true
	delete(r.conns, conn.id)

	sid := conn.deviceSid
	if sid == "" || !conn.authenticated {
		r.mu.Unlock()
		return
	}

	if conn.role == zcoRoleDevice {
		if r.devices[sid] == conn {
			delete(r.devices, sid)
			// Terminals stay connected and wait; a superseded connection
			// has already been replaced, so it must not push status.
			if !conn.superseded {
				msg := zcoServerMsg{Type: "pair_status_ack", PairStatus: zcoPairWaiting}
				for _, t := range r.terminals[sid] {
					r.sendLocked(t, msg)
				}
			}
		}
	} else {
		if set := r.terminals[sid]; set != nil {
			delete(set, conn.id)
			// The (possibly empty) set is kept: deleting it would race
			// with a replacement terminal that onAuthResponse is about
			// to add.
		}
		if !conn.superseded {
			status := r.pairStatusLocked(sid)
			if _, ok := r.devices[sid]; ok {
				msg := zcoServerMsg{Type: "pair_status_ack", PairStatus: status}
				if conn.role == zcoRoleDevice {
					for _, t := range r.terminals[sid] {
						r.sendLocked(t, msg)
					}
				} else if d, ok := r.devices[sid]; ok {
					r.sendLocked(d, msg)
				}
			}
		}
	}
	r.mu.Unlock()
}
