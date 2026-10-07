package pkg

// zcd relay (relay://zcode) — in-process backend porting tools/zrelay (device ↔
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
// zcodex.json, managed by the shared store singleton (see zcdState).

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
	zcdDeviceSIDPrefix = "d_"
	zcdMaxFrameBytes   = 8 * 1024 * 1024
	zcdAuthTimeout     = 15 * time.Second
)

// zcdRole is the peer kind declared in auth_init.
type zcdRole string

const (
	zcdRoleDevice   zcdRole = "device"
	zcdRoleTerminal zcdRole = "terminal"
)

// zcdPairStatus is the pairing state reported to clients.
type zcdPairStatus string

const (
	zcdPairWaiting zcdPairStatus = "waiting"
	zcdPairMatched zcdPairStatus = "matched"
)

// Error codes understood by the official clients.
const (
	zcdErrKicked        = "KICKED"
	zcdErrAuthFailed    = "AUTH_FAILED"
	zcdErrDeviceOffline = "DEVICE_OFFLINE"
	zcdErrWrongParam    = "WRONG_PARAM"
	zcdErrInternal      = "INTERNAL"
)

// zcdMeta is the optional diagnostics block clients may attach.
type zcdMeta map[string]any

// ---------------------------------------------------------------------------
// wire messages
// ---------------------------------------------------------------------------

// zcdClientMsg is one inbound frame; fields are shared across message
// types (Type discriminates).
type zcdClientMsg struct {
	Type      string          `json:"type"`
	DeviceMid string          `json:"device_mid,omitempty"` // device_register_init
	PassHash  string          `json:"pass_hash,omitempty"`  // device_register_init
	Role      zcdRole         `json:"role,omitempty"`       // auth_init
	DeviceSid string          `json:"device_sid,omitempty"` // auth_init/auth_response/pair_status_query
	Proof     string          `json:"proof,omitempty"`      // auth_response
	Meta      zcdMeta         `json:"meta,omitempty"`
	ClientTs  *int64          `json:"client_ts,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"` // data
}

// zcdServerMsg is one outbound frame.
type zcdServerMsg struct {
	Type       string        `json:"type"`
	DeviceSid  string        `json:"device_sid,omitempty"` // device_register_ack
	Nonce      string        `json:"nonce,omitempty"`      // auth_challenge
	PairStatus zcdPairStatus `json:"pair_status,omitempty"`
	Code       string        `json:"code,omitempty"` // error
	Message    string        `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// crypto helpers (must match the clients byte for byte)
// ---------------------------------------------------------------------------

// zcdPassHash = base64(sha256(password)); password is a 24-byte base64url string.
func zcdPassHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// zcdProof = base64url(HMAC-SHA256(key=passHash, msg=nonce|role|deviceSid)).
// The HMAC key is the *string* pass_hash (utf-8), exactly like the clients.
func zcdProof(passHash, nonce string, role zcdRole, deviceSid string) string {
	mac := hmac.New(sha256.New, []byte(passHash))
	fmt.Fprintf(mac, "%s|%s|%s", nonce, role, deviceSid)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// zcdVerifyProof is a constant-time proof comparison.
func zcdVerifyProof(passHash, nonce string, role zcdRole, deviceSid, proof string) bool {
	expected := zcdProof(passHash, nonce, role, deviceSid)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(proof)) == 1
}

// zcdNewDeviceSid mints a device_sid shaped like the official ones:
// "d_" + 22 url-safe chars.
func zcdNewDeviceSid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return zcdDeviceSIDPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// zcdNewNonce returns a random 24-byte base64url nonce.
func zcdNewNonce() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// device store (zrelay-state.json compatible)
// ---------------------------------------------------------------------------

// zcdDeviceRecord is a registered desktop identity.
type zcdDeviceRecord struct {
	DeviceSid  string  `json:"device_sid"`
	DeviceMid  string  `json:"device_mid"`
	PassHash   string  `json:"pass_hash"`
	Meta       zcdMeta `json:"meta,omitempty"`
	CreatedAt  int64   `json:"created_at"`
	UpdatedAt  int64   `json:"updated_at"`
	LastSeenAt int64   `json:"last_seen_at,omitempty"`
	LastIP     string  `json:"last_ip,omitempty"` // last connection source IP
}

// zcdStoreFile is the on-disk state file format (version 1, zrelay-compatible).
type zcdStoreFile struct {
	Version    int               `json:"version"`
	Devices    []zcdDeviceRecord `json:"devices"`
	Apps       []managerApp      `json:"apps,omitempty"`         // manager app records (zcode is one app type among several)
	Order      []string          `json:"order,omitempty"`        // manager card order (user-arranged entry ids)
	Tools      []managerTool     `json:"tools,omitempty"`        // manager toolbar quick links (name → addr)
	Notes      map[string]string `json:"notes,omitempty"`        // manager per-entry remarks (entry id → note text)
	VscOpenExt string            `json:"vsc_open_ext,omitempty"` // "open in VS Code" deep-link extension id (vscode://<ext>/open…)
	// VscLocalAuth: deep links carry no gateway-signed tkn; the extension
	// resolves per-domain credentials from its own local store instead.
	VscLocalAuth bool `json:"vsc_local_auth,omitempty"`
}

// zcdStore is a JSON-file backed registry with two in-memory indexes
// (sid → record, mid → sid) plus the manager app list. Writes are atomic and
// debounced (flush on change, at most once per second).
type zcdStore struct {
	file         string
	mu           sync.Mutex
	bySid        map[string]*zcdDeviceRecord
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

// zcdLoadStore opens (or creates) the state file. When the file is absent
// and it is the new zcodex.json name, the legacy zcoded.json (devices only)
// is loaded instead so registrations survive the rename; the next flush
// writes the new name.
func zcdLoadStore(file string) *zcdStore {
	s := &zcdStore{
		file:   file,
		bySid:  make(map[string]*zcdDeviceRecord),
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
	var parsed zcdStoreFile
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
func (s *zcdStore) get(sid string) *zcdDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bySid[sid]
}

// getByMid returns the record for a device_mid (nil when unknown).
func (s *zcdStore) getByMid(mid string) *zcdDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.byMid[mid]
	if !ok {
		return nil
	}
	return s.bySid[sid]
}

// upsert inserts or updates a device and returns the effective record.
func (s *zcdStore) upsert(rec zcdDeviceRecord) *zcdDeviceRecord {
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
func (s *zcdStore) touch(sid, ip string) {
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
func (s *zcdStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bySid)
}

// remove forgets a device entirely (pairing state + mid index). Returns
// whether the device existed. The manager's card delete needs this: device
// cards are synthesized from the device list, so without removal the card
// would re-appear on the next render. A live device re-registers (fresh
// pairing) on its next reconnect.
func (s *zcdStore) remove(sid string) bool {
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
func (s *zcdStore) list() []zcdDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]zcdDeviceRecord, 0, len(s.bySid))
	for _, r := range s.bySid {
		out = append(out, *r)
	}
	return out
}

// vscOpenExtValue returns the stored "open in VS Code" extension id
// (vscode://<ext>/open… deep-link target); "" = use the env default.
func (s *zcdStore) vscOpenExtValue() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vscOpenExt
}

// saveVscOpenExt persists the "open in VS Code" extension id ("" = back to
// the env default). Takes effect immediately — the value is read per request.
func (s *zcdStore) saveVscOpenExt(ext string) {
	s.mu.Lock()
	s.vscOpenExt = ext
	s.dirty = true
	s.mu.Unlock()
	s.requestFlush()
}

// vscLocalAuthValue reports whether vscode:// deep links should omit the
// gateway-signed tkn (the extension then resolves credentials per domain
// from its own local store).
func (s *zcdStore) vscLocalAuthValue() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.vscLocalAuth
}

// saveVscLocalAuth toggles the "use local auth info" mode. Read per request.
func (s *zcdStore) saveVscLocalAuth(on bool) {
	s.mu.Lock()
	s.vscLocalAuth = on
	s.dirty = true
	s.mu.Unlock()
	s.requestFlush()
}

// flush writes the state file atomically when dirty.
func (s *zcdStore) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	out := zcdStoreFile{
		Version:      1,
		Devices:      make([]zcdDeviceRecord, 0, len(s.bySid)),
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
func FlushNow() { zcdState().flush() }

// flushLoop debounces state-file writes (at most one per second).
func (s *zcdStore) flushLoop() {
	for range s.notify {
		time.Sleep(time.Second)
		s.flush()
	}
}

// requestFlush asks the flush loop to persist state soon.
func (s *zcdStore) requestFlush() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// ---------------------------------------------------------------------------
// relay — the pairing core
// ---------------------------------------------------------------------------

// zcdConn is one live connection, authenticated or not.
type zcdConn struct {
	id            int64
	ws            *wsConn
	remote        string
	mid           string // ?mid= query parameter
	role          zcdRole
	deviceSid     string
	authMeta      zcdMeta
	pendingNonce  string
	authenticated bool
	detached      bool
	superseded    bool // replaced by a newer connection; suppresses status churn
}

// zcdRelay is the in-process pairing relay. One instance serves any number
// of "relay://" backends; typically there is exactly one, named by the
// backend target (e.g. relay://zcode). All relays share one device
// store (the zcodex.json singleton).
type zcdRelay struct {
	mu     sync.Mutex
	store  *zcdStore
	nextID int64
	conns  map[int64]*zcdConn
	// device_sid → the single live device connection
	devices map[string]*zcdConn
	// device_sid → live terminal connections
	terminals map[string]map[int64]*zcdConn
}

var (
	zcdRelaysMu sync.Mutex
	zcdRelays   = map[string]*zcdRelay{}

	// zcdHome is the kvs working directory (top-level home), injected
	// via SetZcdHome from main() before backends are built.
	zcdHome string

	// zcdStateOnce guards the shared zcodex.json store singleton, used by
	// every relay AND the manager so both always see the same data.
	zcdStateOnce sync.Once
	zcdStateInst *zcdStore
)

func init() {
	registerRelay("zcode", zcdRelayHandler)
}

// zcdRelayHandler mounts the named zcode relay instance behind an
// upgrade-only gate: anything but a WebSocket handshake is a 426 (the
// protocol has no plain-HTTP surface; status lives in the manager UI).
func zcdRelayHandler(target, mount string) http.Handler {
	relay := zcdGetRelay(target)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWebSocketUpgrade(r) {
			w.Header().Set("Upgrade", "websocket")
			http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
			return
		}
		relay.Handle(w, r)
	})
}

// SetZcdHome sets the base directory for the state file
// ({home}/zcodex.json). Call once from main() with cfg.Home.
func SetZcdHome(home string) { zcdHome = home }

// zcdState returns the shared zcodex.json store singleton, creating it (and
// its flush loop) on first use. The device registry must persist across
// restarts (otherwise the desktop's saved credentials become unknown and it
// gets AUTH_FAILED), so the state file lives under the kvs home directory:
// {KVS_HOME:-.}/zcodex.json (legacy zcoded.json is loaded as a fallback).
func zcdState() *zcdStore {
	zcdStateOnce.Do(func() {
		zcdStateInst = zcdLoadStore(filepath.Join(zcdHome, "zcodex.json"))
		log.Printf("[manager] state store ready: %s (devices: %d, apps: %d)",
			zcdStateInst.file, zcdStateInst.size(), len(zcdStateInst.apps))
		go zcdStateInst.flushLoop()
	})
	return zcdStateInst
}

// zcdGetRelay returns (creating on first use) the named relay instance.
// The registry routes only "zcode" here, so that is the effective name; the
// parameter stays for explicit call sites (the manager reads the same
// instance).
func zcdGetRelay(name string) *zcdRelay {
	if name == "" {
		name = "zcode"
	}
	zcdRelaysMu.Lock()
	defer zcdRelaysMu.Unlock()
	if r, ok := zcdRelays[name]; ok {
		return r
	}
	r := &zcdRelay{
		store:     zcdState(),
		conns:     make(map[int64]*zcdConn),
		devices:   make(map[string]*zcdConn),
		terminals: make(map[string]map[int64]*zcdConn),
	}
	zcdRelays[name] = r
	log.Printf("[zcode] relay %q ready (state: %s, devices: %d)", name, r.store.file, r.store.size())
	return r
}

// Handle upgrades the request and runs the connection until it closes.
// This is the http.Handler entry point wired by the relay:// backend case.
func (r *zcdRelay) Handle(w http.ResponseWriter, req *http.Request) {
	ws, err := wsUpgrade(w, req)
	if err != nil {
		return
	}
	r.handleConnection(ws, req)
}

// handleConnection registers the connection and pumps messages until close.
func (r *zcdRelay) handleConnection(ws *wsConn, req *http.Request) {
	r.mu.Lock()
	r.nextID++
	conn := &zcdConn{
		id:     r.nextID,
		ws:     ws,
		remote: clientRemoteAddr(req),
		mid:    strings.TrimSpace(req.URL.Query().Get("mid")),
	}
	r.conns[conn.id] = conn
	r.mu.Unlock()
	log.Printf("[zcode] conn#%d open (remote=%s mid=%s)", conn.id, conn.remote, conn.mid)

	authTimer := time.AfterFunc(zcdAuthTimeout, func() {
		r.mu.Lock()
		authenticated := conn.authenticated
		r.mu.Unlock()
		if !authenticated {
			log.Printf("[zcode] conn#%d auth timeout, closing", conn.id)
			r.sendError(conn, zcdErrWrongParam, "authentication timeout")
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
		if len(payload) > zcdMaxFrameBytes {
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
func (r *zcdRelay) onMessage(conn *zcdConn, frame []byte) error {
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

	var msg zcdClientMsg
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Type == "" {
		log.Printf("[zcode] conn#%d invalid relay message", conn.id)
		if !authed {
			r.sendError(conn, zcdErrWrongParam, "malformed message")
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
			r.sendError(conn, zcdErrWrongParam, "unexpected message "+msg.Type)
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
		r.sendError(conn, zcdErrWrongParam, msg.Type+" before auth_init")
	}
	return nil
}

// onRegisterInit handles the desktop's very first handshake (no persisted
// credentials yet): register (or re-register) the identity, kick any stale
// socket for the same identity.
func (r *zcdRelay) onRegisterInit(conn *zcdConn, msg *zcdClientMsg) {
	mid := strings.TrimSpace(msg.DeviceMid)
	passHash := strings.TrimSpace(msg.PassHash)
	if mid == "" || passHash == "" {
		r.sendError(conn, zcdErrWrongParam, "device_register_init requires device_mid and pass_hash")
		return
	}
	// The ?mid= query parameter identifies the device when several
	// desktops share one relay; a mismatch means the connection and the
	// message disagree about the identity — refuse so devices cannot
	// register over each other.
	if conn.mid != "" && conn.mid != mid {
		log.Printf("[zcode] conn#%d device_mid mismatch (query=%s message=%s), rejected", conn.id, conn.mid, mid)
		r.sendError(conn, zcdErrWrongParam, "device_mid mismatch with ?mid=")
		return
	}

	existing := r.store.getByMid(mid)
	var record *zcdDeviceRecord
	if existing != nil {
		rec := *existing
		rec.DeviceMid, rec.PassHash, rec.Meta = mid, passHash, msg.Meta
		rec.LastIP = conn.remote
		record = r.store.upsert(rec)
		log.Printf("[zcode] conn#%d re-register for mid=%s -> %s", conn.id, mid, record.DeviceSid)
	} else {
		record = r.store.upsert(zcdDeviceRecord{
			DeviceSid: zcdNewDeviceSid(),
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

	r.send(conn, zcdServerMsg{Type: "device_register_ack", DeviceSid: record.DeviceSid})
	r.store.requestFlush()
}

// onAuthInit starts the challenge/response for both roles.
func (r *zcdRelay) onAuthInit(conn *zcdConn, msg *zcdClientMsg) {
	sid := strings.TrimSpace(msg.DeviceSid)
	if sid == "" || (msg.Role != zcdRoleDevice && msg.Role != zcdRoleTerminal) {
		r.sendError(conn, zcdErrWrongParam, "auth_init requires role and device_sid")
		return
	}
	record := r.store.get(sid)
	if record == nil {
		log.Printf("[zcode] conn#%d unknown device_sid %s (role=%s)", conn.id, sid, msg.Role)
		r.sendError(conn, zcdErrAuthFailed, "unknown device_sid")
		return
	}
	// Devices are partitioned by ?mid=: a device connection whose mid does
	// not match the registered one is rejected, so multiple desktops on one
	// relay never see or kick each other. Terminals usually connect without
	// ?mid= (or with the device's mid) and are exempt when absent.
	if conn.mid != "" && record.DeviceMid != "" && conn.mid != record.DeviceMid {
		log.Printf("[zcode] conn#%d auth mid mismatch (query=%s registered=%s), rejected", conn.id, conn.mid, record.DeviceMid)
		r.sendError(conn, zcdErrWrongParam, "mid mismatch with registered device")
		return
	}
	r.mu.Lock()
	conn.role = msg.Role
	conn.deviceSid = sid
	conn.authMeta = msg.Meta
	conn.pendingNonce = zcdNewNonce()
	nonce := conn.pendingNonce
	r.mu.Unlock()
	r.send(conn, zcdServerMsg{Type: "auth_challenge", Nonce: nonce})
}

// onAuthResponse verifies the HMAC proof and completes authentication.
func (r *zcdRelay) onAuthResponse(conn *zcdConn, msg *zcdClientMsg) {
	r.mu.Lock()
	role, sid, nonce := conn.role, conn.deviceSid, conn.pendingNonce
	r.mu.Unlock()
	if role == "" || sid == "" || nonce == "" {
		r.sendError(conn, zcdErrWrongParam, "auth_response without auth_init")
		return
	}
	record := r.store.get(sid)
	if record == nil {
		r.sendError(conn, zcdErrAuthFailed, "unknown device_sid")
		return
	}
	if msg.DeviceSid != sid {
		r.sendError(conn, zcdErrWrongParam, "device_sid mismatch")
		return
	}
	if !zcdVerifyProof(record.PassHash, nonce, role, sid, msg.Proof) {
		log.Printf("[zcode] conn#%d invalid proof (role=%s sid=%s remote=%s)", conn.id, role, sid, conn.remote)
		r.sendError(conn, zcdErrAuthFailed, "invalid proof")
		return
	}

	r.store.touch(sid, conn.remote)
	r.store.requestFlush()

	r.mu.Lock()
	conn.pendingNonce = ""
	conn.authenticated = true

	if role == zcdRoleDevice {
		if previous, ok := r.devices[sid]; ok && previous != conn {
			r.mu.Unlock()
			r.kick(previous, "replaced by a newer desktop connection")
			r.mu.Lock()
		}
		r.devices[sid] = conn
	} else {
		set := r.terminals[sid]
		if set == nil {
			set = make(map[int64]*zcdConn)
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

	r.send(conn, zcdServerMsg{Type: "auth_ack", PairStatus: status})
	log.Printf("[zcode] conn#%d authenticated (role=%s sid=%s pair=%s)", conn.id, role, sid, status)

	if status == zcdPairMatched {
		r.notifyPeer(sid, conn, status)
	}
}

// onPairStatusQuery is the authenticated heartbeat.
func (r *zcdRelay) onPairStatusQuery(conn *zcdConn, _ *zcdClientMsg) {
	r.mu.Lock()
	sid := conn.deviceSid
	r.mu.Unlock()
	if sid == "" {
		r.sendError(conn, zcdErrWrongParam, "pair_status_query before auth")
		return
	}
	r.store.touch(sid, conn.remote)
	r.send(conn, zcdServerMsg{Type: "pair_status_ack", PairStatus: r.pairStatus(sid)})
}

// onData forwards an opaque payload to the other side, byte for byte.
func (r *zcdRelay) onData(conn *zcdConn, frame []byte) {
	r.mu.Lock()
	sid := conn.deviceSid
	authed := conn.authenticated
	role := conn.role
	if sid == "" || !authed {
		r.mu.Unlock()
		r.sendError(conn, zcdErrWrongParam, "data before auth")
		return
	}
	if r.pairStatusLocked(sid) != zcdPairMatched {
		r.mu.Unlock()
		log.Printf("[zcode] conn#%d data dropped: pair_status=waiting", conn.id)
		return
	}
	// 栈上小数组承接目标：设备↔终端典型的对端数是 1~2，data 帧热路径
	// 每帧省一次堆分配；超过 4 个对端时 append 自动落到堆上（罕见）。
	var targetsBuf [4]*zcdConn
	targets := targetsBuf[:0]
	if role == zcdRoleDevice {
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
func (r *zcdRelay) pairStatusLocked(sid string) zcdPairStatus {
	if _, ok := r.devices[sid]; ok && len(r.terminals[sid]) > 0 {
		return zcdPairMatched
	}
	return zcdPairWaiting
}

// pairStatus is the locking wrapper.
func (r *zcdRelay) pairStatus(sid string) zcdPairStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairStatusLocked(sid)
}

// notifyPeer pushes a pair_status_ack to the other side of a pairing.
func (r *zcdRelay) notifyPeer(sid string, from *zcdConn, status zcdPairStatus) {
	msg := zcdServerMsg{Type: "pair_status_ack", PairStatus: status}
	r.mu.Lock()
	defer r.mu.Unlock()
	if from.role == zcdRoleDevice {
		for _, t := range r.terminals[sid] {
			r.sendLocked(t, msg)
		}
	} else if d, ok := r.devices[sid]; ok {
		r.sendLocked(d, msg)
	}
}

// send marshals and writes one server message (outside r.mu).
func (r *zcdRelay) send(conn *zcdConn, msg zcdServerMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[zcode] conn#%d outbound %s marshal FAIL: %v", conn.id, msg.Type, err)
		return
	}
	if len(data) > zcdMaxFrameBytes {
		log.Printf("[zcode] conn#%d outbound %s exceeds frame limit", conn.id, msg.Type)
		return
	}
	r.sendRaw(conn, data)
}

// sendLocked is send() for callers already holding r.mu.
func (r *zcdRelay) sendLocked(conn *zcdConn, msg zcdServerMsg) {
	r.mu.Unlock()
	r.send(conn, msg)
	r.mu.Lock()
}

// sendRaw writes a pre-encoded frame verbatim.
func (r *zcdRelay) sendRaw(conn *zcdConn, frame []byte) {
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
func (r *zcdRelay) sendError(conn *zcdConn, code, message string) {
	log.Printf("[zcode] conn#%d -> error %s: %s", conn.id, code, message)
	r.send(conn, zcdServerMsg{Type: "error", Code: code, Message: message})
}

// kick tells a peer a newer connection took over, then closes it.
func (r *zcdRelay) kick(conn *zcdConn, reason string) {
	r.mu.Lock()
	if conn.detached {
		r.mu.Unlock()
		return
	}
	conn.superseded = true
	r.mu.Unlock()

	log.Printf("[zcode] conn#%d kicked (%s, role=%s)", conn.id, reason, conn.role)
	r.send(conn, zcdServerMsg{Type: "error", Code: zcdErrKicked, Message: reason})
	conn.ws.Close("kicked")
	r.detach(conn)
}

// detach removes a closed connection from the relay state.
func (r *zcdRelay) detach(conn *zcdConn) {
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

	if conn.role == zcdRoleDevice {
		if r.devices[sid] == conn {
			delete(r.devices, sid)
			// Terminals stay connected and wait; a superseded connection
			// has already been replaced, so it must not push status.
			if !conn.superseded {
				msg := zcdServerMsg{Type: "pair_status_ack", PairStatus: zcdPairWaiting}
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
				msg := zcdServerMsg{Type: "pair_status_ack", PairStatus: status}
				if conn.role == zcdRoleDevice {
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
