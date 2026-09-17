package pkg

// zcode relay — in-process "wsws://" backend porting tools/zrelay (device ↔
// terminal pairing for ZCode web-remote-control). Role-agnostic: desktop and
// web client share one endpoint; who is who comes from the first auth_init
// message, not the URL. The routing prefix is pure operator config:
//
//	ws~/ws = wsws://zcode
//
// Desktop: ZCODE_WEB_REMOTE_CONTROL_RELAY_WS_URL=ws://host:port/ws[?mid=<uuid>]
// Web page: endpoint injected via KVS_CC_SED (overrideUrl:`wss://>host</ws`).
//
// Protocol: JSON text frames; device registers once then authenticates via
// HMAC-SHA256 challenge/response; terminal authenticates with the device_sid
// + pass_hash from the QR URL; when both online ("matched"), data frames are
// forwarded verbatim. State file is zrelay-state.json (v1) compatible.

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
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
	zcodeDeviceSIDPrefix = "d_"
	zcodeMaxFrameBytes   = 8 * 1024 * 1024
	zcodeAuthTimeout     = 15 * time.Second
)

// zcodeRole is the peer kind declared in auth_init.
type zcodeRole string

const (
	zcodeRoleDevice   zcodeRole = "device"
	zcodeRoleTerminal zcodeRole = "terminal"
)

// zcodePairStatus is the pairing state reported to clients.
type zcodePairStatus string

const (
	zcodePairWaiting zcodePairStatus = "waiting"
	zcodePairMatched zcodePairStatus = "matched"
)

// Error codes understood by the official clients.
const (
	zcodeErrKicked        = "KICKED"
	zcodeErrAuthFailed    = "AUTH_FAILED"
	zcodeErrDeviceOffline = "DEVICE_OFFLINE"
	zcodeErrWrongParam    = "WRONG_PARAM"
	zcodeErrInternal      = "INTERNAL"
)

// zcodeMeta is the optional diagnostics block clients may attach.
type zcodeMeta map[string]any

// ---------------------------------------------------------------------------
// wire messages
// ---------------------------------------------------------------------------

// zcodeClientMsg is one inbound frame; fields are shared across message
// types (Type discriminates).
type zcodeClientMsg struct {
	Type      string     `json:"type"`
	DeviceMid string     `json:"device_mid,omitempty"` // device_register_init
	PassHash  string     `json:"pass_hash,omitempty"`  // device_register_init
	Role      zcodeRole  `json:"role,omitempty"`       // auth_init
	DeviceSid string     `json:"device_sid,omitempty"` // auth_init/auth_response/pair_status_query
	Proof     string     `json:"proof,omitempty"`      // auth_response
	Meta      zcodeMeta  `json:"meta,omitempty"`
	ClientTs  *int64     `json:"client_ts,omitempty"`
	Payload   json.RawMessage `json:"payload,omitempty"` // data
}

// zcodeServerMsg is one outbound frame.
type zcodeServerMsg struct {
	Type       string          `json:"type"`
	DeviceSid  string          `json:"device_sid,omitempty"` // device_register_ack
	Nonce      string          `json:"nonce,omitempty"`      // auth_challenge
	PairStatus zcodePairStatus `json:"pair_status,omitempty"`
	Code       string          `json:"code,omitempty"` // error
	Message    string          `json:"message,omitempty"`
}

// ---------------------------------------------------------------------------
// crypto helpers (must match the clients byte for byte)
// ---------------------------------------------------------------------------

// zcodePassHash = base64(sha256(password)); password is a 24-byte base64url string.
func zcodePassHash(password string) string {
	sum := sha256.Sum256([]byte(password))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// zcodeProof = base64url(HMAC-SHA256(key=passHash, msg=nonce|role|deviceSid)).
// The HMAC key is the *string* pass_hash (utf-8), exactly like the clients.
func zcodeProof(passHash, nonce string, role zcodeRole, deviceSid string) string {
	mac := hmac.New(sha256.New, []byte(passHash))
	fmt.Fprintf(mac, "%s|%s|%s", nonce, role, deviceSid)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))
}

// zcodeVerifyProof is a constant-time proof comparison.
func zcodeVerifyProof(passHash, nonce string, role zcodeRole, deviceSid, proof string) bool {
	expected := zcodeProof(passHash, nonce, role, deviceSid)
	return subtle.ConstantTimeCompare([]byte(expected), []byte(proof)) == 1
}

// zcodeNewDeviceSid mints a device_sid shaped like the official ones:
// "d_" + 22 url-safe chars.
func zcodeNewDeviceSid() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return zcodeDeviceSIDPrefix + base64.RawURLEncoding.EncodeToString(b[:])
}

// zcodeNewNonce returns a random 24-byte base64url nonce.
func zcodeNewNonce() string {
	var b [24]byte
	_, _ = rand.Read(b[:])
	return base64.RawURLEncoding.EncodeToString(b[:])
}

// ---------------------------------------------------------------------------
// device store (zrelay-state.json compatible)
// ---------------------------------------------------------------------------

// zcodeDeviceRecord is a registered desktop identity.
type zcodeDeviceRecord struct {
	DeviceSid  string    `json:"device_sid"`
	DeviceMid  string    `json:"device_mid"`
	PassHash   string    `json:"pass_hash"`
	Meta       zcodeMeta `json:"meta,omitempty"`
	CreatedAt  int64     `json:"created_at"`
	UpdatedAt  int64     `json:"updated_at"`
	LastSeenAt int64     `json:"last_seen_at,omitempty"`
	LastIP     string    `json:"last_ip,omitempty"` // last connection source IP
}

// zcodeStoreFile is the on-disk state file format (version 1, like zrelay).
type zcodeStoreFile struct {
	Version int                `json:"version"`
	Devices []zcodeDeviceRecord `json:"devices"`
}

// zcodeStore is a JSON-file backed device registry with two in-memory
// indexes (sid → record, mid → sid). Writes are atomic and debounced by the
// relay (flush on change, at most once per second).
type zcodeStore struct {
	file  string
	mu    sync.Mutex
	bySid map[string]*zcodeDeviceRecord
	byMid map[string]string
	dirty bool
}

// zcodeLoadStore opens (or creates) the state file.
func zcodeLoadStore(file string) *zcodeStore {
	s := &zcodeStore{
		file:  file,
		bySid: make(map[string]*zcodeDeviceRecord),
		byMid: make(map[string]string),
	}
	data, err := os.ReadFile(file)
	if err != nil {
		return s // missing file → empty store
	}
	var parsed zcodeStoreFile
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
	return s
}

// get returns the record for a device_sid (nil when unknown).
func (s *zcodeStore) get(sid string) *zcodeDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.bySid[sid]
}

// getByMid returns the record for a device_mid (nil when unknown).
func (s *zcodeStore) getByMid(mid string) *zcodeDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	sid, ok := s.byMid[mid]
	if !ok {
		return nil
	}
	return s.bySid[sid]
}

// upsert inserts or updates a device and returns the effective record.
func (s *zcodeStore) upsert(rec zcodeDeviceRecord) *zcodeDeviceRecord {
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
func (s *zcodeStore) touch(sid, ip string) {
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
func (s *zcodeStore) size() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.bySid)
}

// list returns all device records (map order; callers sort if needed).
func (s *zcodeStore) list() []zcodeDeviceRecord {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]zcodeDeviceRecord, 0, len(s.bySid))
	for _, r := range s.bySid {
		out = append(out, *r)
	}
	return out
}

// flush writes the state file atomically when dirty.
func (s *zcodeStore) flush() {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.dirty {
		return
	}
	out := zcodeStoreFile{Version: 1, Devices: make([]zcodeDeviceRecord, 0, len(s.bySid))}
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

// ---------------------------------------------------------------------------
// relay — the pairing core
// ---------------------------------------------------------------------------

// zcodeConn is one live connection, authenticated or not.
type zcodeConn struct {
	id          int64
	ws          *wsConn
	remote      string
	mid         string // ?mid= query parameter
	role        zcodeRole
	deviceSid   string
	authMeta    zcodeMeta
	pendingNonce string
	authenticated bool
	detached    bool
	superseded  bool // replaced by a newer connection; suppresses status churn
}

// zcodeRelay is the in-process pairing relay. One instance serves any number
// of "wsws://" backends; typically there is exactly one, named by the
// backend target (e.g. wsws://zcode).
type zcodeRelay struct {
	mu     sync.Mutex
	store  *zcodeStore
	stateFile string
	nextID int64
	conns  map[int64]*zcodeConn
	// device_sid → the single live device connection
	devices map[string]*zcodeConn
	// device_sid → live terminal connections
	terminals map[string]map[int64]*zcodeConn
	flushCh   chan struct{}
}

var (
	zcodeRelaysMu sync.Mutex
	zcodeRelays   = map[string]*zcodeRelay{}

	// zcodeHome is the kvs working directory (Config.SvcHome), injected
	// via SetZcodeHome from main() before backends are built.
	zcodeHome string
)

// SetZcodeHome sets the base directory for the relay state file
// ({home}/zstate.json). Call once from main() with cfg.SvcHome.
func SetZcodeHome(home string) { zcodeHome = home }

// zcodeGetRelay returns (creating on first use) the named relay instance.
// The name is the wsws:// target (e.g. "zcode"). The device registry must
// persist across restarts (otherwise the desktop's saved credentials become
// unknown and it gets AUTH_FAILED), so the state file lives under the
// kvs home directory: {KVS_HOME:-.}/zstate.json.
func zcodeGetRelay(name string) *zcodeRelay {
	if name == "" {
		name = "zcode"
	}
	stateFile := filepath.Join(zcodeHome, "zstate.json")
	zcodeRelaysMu.Lock()
	defer zcodeRelaysMu.Unlock()
	if r, ok := zcodeRelays[name]; ok {
		return r
	}
	r := &zcodeRelay{
		store:     zcodeLoadStore(stateFile),
		stateFile: stateFile,
		conns:     make(map[int64]*zcodeConn),
		devices:   make(map[string]*zcodeConn),
		terminals: make(map[string]map[int64]*zcodeConn),
		flushCh:   make(chan struct{}, 1),
	}
	zcodeRelays[name] = r
	log.Printf("[zcode] relay %q ready (state: %s, devices: %d)", name, stateFile, r.store.size())
	go r.flushLoop()
	return r
}

// flushLoop debounces state-file writes (at most one per second).
func (r *zcodeRelay) flushLoop() {
	for range r.flushCh {
		time.Sleep(time.Second)
		r.store.flush()
	}
}

// scheduleFlush asks the flush loop to persist state soon.
func (r *zcodeRelay) scheduleFlush() {
	select {
	case r.flushCh <- struct{}{}:
	default:
	}
}

// Handle upgrades the request and runs the connection until it closes.
// This is the http.Handler entry point wired by the wsws:// backend case.
func (r *zcodeRelay) Handle(w http.ResponseWriter, req *http.Request) {
	ws, err := wsUpgrade(w, req)
	if err != nil {
		return
	}
	r.handleConnection(ws, req)
}

// handleConnection registers the connection and pumps messages until close.
func (r *zcodeRelay) handleConnection(ws *wsConn, req *http.Request) {
	r.mu.Lock()
	r.nextID++
	conn := &zcodeConn{
		id:     r.nextID,
		ws:     ws,
		remote: clientRemoteAddr(req),
		mid:    strings.TrimSpace(req.URL.Query().Get("mid")),
	}
	r.conns[conn.id] = conn
	r.mu.Unlock()
	log.Printf("[zcode] conn#%d open (remote=%s mid=%s)", conn.id, conn.remote, conn.mid)

	authTimer := time.AfterFunc(zcodeAuthTimeout, func() {
		r.mu.Lock()
		authenticated := conn.authenticated
		r.mu.Unlock()
		if !authenticated {
			log.Printf("[zcode] conn#%d auth timeout, closing", conn.id)
			r.sendError(conn, zcodeErrWrongParam, "authentication timeout")
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
		if len(payload) > zcodeMaxFrameBytes {
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
func (r *zcodeRelay) onMessage(conn *zcodeConn, frame []byte) error {
	var msg zcodeClientMsg
	if err := json.Unmarshal(frame, &msg); err != nil || msg.Type == "" {
		log.Printf("[zcode] conn#%d invalid relay message", conn.id)
		r.mu.Lock()
		authed := conn.authenticated
		r.mu.Unlock()
		if !authed {
			r.sendError(conn, zcodeErrWrongParam, "malformed message")
		}
		return nil
	}

	r.mu.Lock()
	authed := conn.authenticated
	r.mu.Unlock()

	if authed {
		switch msg.Type {
		case "pair_status_query":
			r.onPairStatusQuery(conn, &msg)
		case "data":
			r.onData(conn, frame)
		default:
			log.Printf("[zcode] conn#%d unexpected %s after auth", conn.id, msg.Type)
			r.sendError(conn, zcodeErrWrongParam, "unexpected message "+msg.Type)
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
		r.sendError(conn, zcodeErrWrongParam, msg.Type+" before auth_init")
	}
	return nil
}

// onRegisterInit handles the desktop's very first handshake (no persisted
// credentials yet): register (or re-register) the identity, kick any stale
// socket for the same identity.
func (r *zcodeRelay) onRegisterInit(conn *zcodeConn, msg *zcodeClientMsg) {
	mid := strings.TrimSpace(msg.DeviceMid)
	passHash := strings.TrimSpace(msg.PassHash)
	if mid == "" || passHash == "" {
		r.sendError(conn, zcodeErrWrongParam, "device_register_init requires device_mid and pass_hash")
		return
	}
	// The ?mid= query parameter identifies the device when several
	// desktops share one relay; a mismatch means the connection and the
	// message disagree about the identity — refuse so devices cannot
	// register over each other.
	if conn.mid != "" && conn.mid != mid {
		log.Printf("[zcode] conn#%d device_mid mismatch (query=%s message=%s), rejected", conn.id, conn.mid, mid)
		r.sendError(conn, zcodeErrWrongParam, "device_mid mismatch with ?mid=")
		return
	}

	existing := r.store.getByMid(mid)
	var record *zcodeDeviceRecord
	if existing != nil {
		rec := *existing
		rec.DeviceMid, rec.PassHash, rec.Meta = mid, passHash, msg.Meta
		rec.LastIP = conn.remote
		record = r.store.upsert(rec)
		log.Printf("[zcode] conn#%d re-register for mid=%s -> %s", conn.id, mid, record.DeviceSid)
	} else {
		record = r.store.upsert(zcodeDeviceRecord{
			DeviceSid: zcodeNewDeviceSid(),
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

	r.send(conn, zcodeServerMsg{Type: "device_register_ack", DeviceSid: record.DeviceSid})
	r.scheduleFlush()
}

// onAuthInit starts the challenge/response for both roles.
func (r *zcodeRelay) onAuthInit(conn *zcodeConn, msg *zcodeClientMsg) {
	sid := strings.TrimSpace(msg.DeviceSid)
	if sid == "" || (msg.Role != zcodeRoleDevice && msg.Role != zcodeRoleTerminal) {
		r.sendError(conn, zcodeErrWrongParam, "auth_init requires role and device_sid")
		return
	}
	record := r.store.get(sid)
	if record == nil {
		log.Printf("[zcode] conn#%d unknown device_sid %s (role=%s)", conn.id, sid, msg.Role)
		r.sendError(conn, zcodeErrAuthFailed, "unknown device_sid")
		return
	}
	// Devices are partitioned by ?mid=: a device connection whose mid does
	// not match the registered one is rejected, so multiple desktops on one
	// relay never see or kick each other. Terminals usually connect without
	// ?mid= (or with the device's mid) and are exempt when absent.
	if conn.mid != "" && record.DeviceMid != "" && conn.mid != record.DeviceMid {
		log.Printf("[zcode] conn#%d auth mid mismatch (query=%s registered=%s), rejected", conn.id, conn.mid, record.DeviceMid)
		r.sendError(conn, zcodeErrWrongParam, "mid mismatch with registered device")
		return
	}
	r.mu.Lock()
	conn.role = msg.Role
	conn.deviceSid = sid
	conn.authMeta = msg.Meta
	conn.pendingNonce = zcodeNewNonce()
	nonce := conn.pendingNonce
	r.mu.Unlock()
	r.send(conn, zcodeServerMsg{Type: "auth_challenge", Nonce: nonce})
}

// onAuthResponse verifies the HMAC proof and completes authentication.
func (r *zcodeRelay) onAuthResponse(conn *zcodeConn, msg *zcodeClientMsg) {
	r.mu.Lock()
	role, sid, nonce := conn.role, conn.deviceSid, conn.pendingNonce
	r.mu.Unlock()
	if role == "" || sid == "" || nonce == "" {
		r.sendError(conn, zcodeErrWrongParam, "auth_response without auth_init")
		return
	}
	record := r.store.get(sid)
	if record == nil {
		r.sendError(conn, zcodeErrAuthFailed, "unknown device_sid")
		return
	}
	if msg.DeviceSid != sid {
		r.sendError(conn, zcodeErrWrongParam, "device_sid mismatch")
		return
	}
	if !zcodeVerifyProof(record.PassHash, nonce, role, sid, msg.Proof) {
		log.Printf("[zcode] conn#%d invalid proof (role=%s sid=%s remote=%s)", conn.id, role, sid, conn.remote)
		r.sendError(conn, zcodeErrAuthFailed, "invalid proof")
		return
	}

	r.store.touch(sid, conn.remote)
	r.scheduleFlush()

	r.mu.Lock()
	conn.pendingNonce = ""
	conn.authenticated = true

	if role == zcodeRoleDevice {
		if previous, ok := r.devices[sid]; ok && previous != conn {
			r.mu.Unlock()
			r.kick(previous, "replaced by a newer desktop connection")
			r.mu.Lock()
		}
		r.devices[sid] = conn
	} else {
		set := r.terminals[sid]
		if set == nil {
			set = make(map[int64]*zcodeConn)
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

	r.send(conn, zcodeServerMsg{Type: "auth_ack", PairStatus: status})
	log.Printf("[zcode] conn#%d authenticated (role=%s sid=%s pair=%s)", conn.id, role, sid, status)

	if status == zcodePairMatched {
		r.notifyPeer(sid, conn, status)
	}
}

// onPairStatusQuery is the authenticated heartbeat.
func (r *zcodeRelay) onPairStatusQuery(conn *zcodeConn, msg *zcodeClientMsg) {
	r.mu.Lock()
	sid := conn.deviceSid
	r.mu.Unlock()
	if sid == "" {
		r.sendError(conn, zcodeErrWrongParam, "pair_status_query before auth")
		return
	}
	r.store.touch(sid, conn.remote)
	r.send(conn, zcodeServerMsg{Type: "pair_status_ack", PairStatus: r.pairStatus(sid)})
}

// onData forwards an opaque payload to the other side, byte for byte.
func (r *zcodeRelay) onData(conn *zcodeConn, frame []byte) {
	r.mu.Lock()
	sid := conn.deviceSid
	authed := conn.authenticated
	role := conn.role
	if sid == "" || !authed {
		r.mu.Unlock()
		r.sendError(conn, zcodeErrWrongParam, "data before auth")
		return
	}
	if r.pairStatusLocked(sid) != zcodePairMatched {
		r.mu.Unlock()
		log.Printf("[zcode] conn#%d data dropped: pair_status=waiting", conn.id)
		return
	}
	var targets []*zcodeConn
	if role == zcodeRoleDevice {
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
func (r *zcodeRelay) pairStatusLocked(sid string) zcodePairStatus {
	if _, ok := r.devices[sid]; ok && len(r.terminals[sid]) > 0 {
		return zcodePairMatched
	}
	return zcodePairWaiting
}

// pairStatus is the locking wrapper.
func (r *zcodeRelay) pairStatus(sid string) zcodePairStatus {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.pairStatusLocked(sid)
}

// notifyPeer pushes a pair_status_ack to the other side of a pairing.
func (r *zcodeRelay) notifyPeer(sid string, from *zcodeConn, status zcodePairStatus) {
	msg := zcodeServerMsg{Type: "pair_status_ack", PairStatus: status}
	r.mu.Lock()
	defer r.mu.Unlock()
	if from.role == zcodeRoleDevice {
		for _, t := range r.terminals[sid] {
			r.sendLocked(t, msg)
		}
	} else if d, ok := r.devices[sid]; ok {
		r.sendLocked(d, msg)
	}
}

// send marshals and writes one server message (outside r.mu).
func (r *zcodeRelay) send(conn *zcodeConn, msg zcodeServerMsg) {
	data, err := json.Marshal(msg)
	if err != nil {
		log.Printf("[zcode] conn#%d outbound %s marshal FAIL: %v", conn.id, msg.Type, err)
		return
	}
	if len(data) > zcodeMaxFrameBytes {
		log.Printf("[zcode] conn#%d outbound %s exceeds frame limit", conn.id, msg.Type)
		return
	}
	r.sendRaw(conn, data)
}

// sendLocked is send() for callers already holding r.mu.
func (r *zcodeRelay) sendLocked(conn *zcodeConn, msg zcodeServerMsg) {
	r.mu.Unlock()
	r.send(conn, msg)
	r.mu.Lock()
}

// sendRaw writes a pre-encoded frame verbatim.
func (r *zcodeRelay) sendRaw(conn *zcodeConn, frame []byte) {
	r.mu.Lock()
	detached := conn.detached
	r.mu.Unlock()
	if detached {
		return
	}
	if err := conn.ws.wsWriteText(frame); err != nil {
		log.Printf("[zcode] conn#%d send FAIL: %v", conn.id, err)
	}
}

// sendError reports a protocol error to the client.
func (r *zcodeRelay) sendError(conn *zcodeConn, code, message string) {
	log.Printf("[zcode] conn#%d -> error %s: %s", conn.id, code, message)
	r.send(conn, zcodeServerMsg{Type: "error", Code: code, Message: message})
}

// kick tells a peer a newer connection took over, then closes it.
func (r *zcodeRelay) kick(conn *zcodeConn, reason string) {
	r.mu.Lock()
	if conn.detached {
		r.mu.Unlock()
		return
	}
	conn.superseded = true
	r.mu.Unlock()

	log.Printf("[zcode] conn#%d kicked (%s, role=%s)", conn.id, reason, conn.role)
	r.send(conn, zcodeServerMsg{Type: "error", Code: zcodeErrKicked, Message: reason})
	conn.ws.Close("kicked")
	r.detach(conn)
}

// detach removes a closed connection from the relay state.
func (r *zcodeRelay) detach(conn *zcodeConn) {
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

	if conn.role == zcodeRoleDevice {
		if r.devices[sid] == conn {
			delete(r.devices, sid)
			// Terminals stay connected and wait; a superseded connection
			// has already been replaced, so it must not push status.
			if !conn.superseded {
				msg := zcodeServerMsg{Type: "pair_status_ack", PairStatus: zcodePairWaiting}
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
				msg := zcodeServerMsg{Type: "pair_status_ack", PairStatus: status}
				if conn.role == zcodeRoleDevice {
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

// clientRemoteAddr extracts the client address (X-Forwarded-For aware).
func clientRemoteAddr(req *http.Request) string {
	if fwd := strings.TrimSpace(req.Header.Get("X-Forwarded-For")); fwd != "" {
		if i := strings.Index(fwd, ","); i >= 0 {
			return strings.TrimSpace(fwd[:i])
		}
		return fwd
	}
	host, _, err := net.SplitHostPort(req.RemoteAddr)
	if err != nil {
		return req.RemoteAddr
	}
	return host
}

// ---------------------------------------------------------------------------
// api://zlist — device list page (non-core: presentation only)
// ---------------------------------------------------------------------------

// zcodeDeviceInfo is one row of the device list page.
type zcodeDeviceInfo struct {
	Sid        string
	Mid        string
	Name       string
	Platform   string
	Version    string // desktop app version (from register meta)
	PassHash   string
	Online     bool
	UpdatedAt  int64 // last register/re-register (上线时间)
	LastSeenAt int64 // last authenticated activity (最后使用)
	LastIP     string // last connection source IP
}

// deviceList returns the registered device list with live online status.
func (r *zcodeRelay) deviceList() []zcodeDeviceInfo {
	r.mu.Lock()
	online := make(map[string]bool, len(r.devices))
	for sid := range r.devices {
		online[sid] = true
	}
	r.mu.Unlock()

	var out []zcodeDeviceInfo
	for _, rec := range r.store.list() {
		name, _ := rec.Meta["name"].(string)
		if name == "" {
			name = "ZCode-" + rec.DeviceMid[max(0, len(rec.DeviceMid)-4):]
		}
		platform, _ := rec.Meta["platform"].(string)
		version, _ := rec.Meta["version"].(string)
		out = append(out, zcodeDeviceInfo{
			Sid:        rec.DeviceSid,
			Mid:        rec.DeviceMid,
			Name:       name,
			Platform:   platform,
			Version:    version,
			PassHash:   rec.PassHash,
			Online:     online[rec.DeviceSid],
			UpdatedAt:  rec.UpdatedAt,
			LastSeenAt: rec.LastSeenAt,
			LastIP:     rec.LastIP,
		})
	}
	return out
}

// zcodeHTMLEscape escapes s for safe inclusion in HTML text/attribute content.
func zcodeHTMLEscape(s string) string {
	r := strings.NewReplacer("&", "&amp;", "<", "&lt;", ">", "&gt;", `"`, "&#34;", "'", "&#39;")
	return r.Replace(s)
}

// zcodeTime renders a concrete local date-time label ("2006-01-02 15:04").
// Zero timestamps render as "-".
func zcodeTime(t int64) string {
	if t <= 0 {
		return "-"
	}
	return time.UnixMilli(t).Format("2006-01-02 15:04")
}

// ipMeta renders the device's last known IP as a card meta line
// (omitted entirely when never seen).
func ipMeta(ip string) string {
	if ip == "" {
		return ""
	}
	return `<div class="device-meta">IP ` + zcodeHTMLEscape(ip) + `</div>`
}

// ServeZList renders the zlist.html device-list page: every registered
// device as a flat card; online devices link to the remote-control terminal
// page using the official QR-URL query shape:
//
//	{pagePrefix}?sid=<device_sid>&hash=<pass_hash>&t=<ts>&mid=<device_mid>
//	          &name=<name>&app_version=<version>
//
// (values URL-escaped exactly like the desktop's QR code; the page prefix
// defaults to "/remote/v4" — the injected zcode web bundle).
func (r *zcodeRelay) ServeZList(w http.ResponseWriter, req *http.Request, pagePrefix string) {
	// Public URL base from the request Host so links work behind whatever
	// domain/proxy fronts kvs; X-Forwarded-Proto wins when present.
	scheme := "http"
	if req.TLS != nil {
		scheme = "https"
	}
	if p := req.Header.Get("X-Forwarded-Proto"); p != "" {
		scheme = p
	}
	base := scheme + "://" + req.Host + pagePrefix

	list := r.deviceList()
	now := time.Now().UnixMilli()
	var rows strings.Builder
	onlineCount := 0
	for _, d := range list {
		status, cls := "离线", "off"
		open, closeTag := `<div class="device off-device">`, `</div>`
		if d.Online {
			status, cls = "在线", "on"
			onlineCount++
			q := url.Values{}
			q.Set("sid", d.Sid)
			q.Set("hash", d.PassHash)
			q.Set("t", fmt.Sprint(now))
			q.Set("mid", d.Mid)
			q.Set("name", d.Name)
			if d.Version != "" {
				q.Set("app_version", d.Version)
			}
			link := base + "?" + q.Encode()
			open, closeTag = `<a class="device" href="`+link+`" target="_blank" rel="noopener">`, `</a>`
		}
		meta := d.Platform
		if d.Version != "" {
			meta = d.Version + " · " + meta
		}
		if meta == "" {
			meta = d.Mid
		}
		rows.WriteString(open +
			`<div class="device-top"><div class="device-name"><span class="dot ` + cls + `"></span><span>` + zcodeHTMLEscape(d.Name) + `</span></div></div>` +
			`<div class="device-meta">` + zcodeHTMLEscape(meta) + ` · ` + status + `</div>` +
			`<div class="device-meta">上线 ` + zcodeTime(d.UpdatedAt) + `</div>` +
			`<div class="device-meta">使用 ` + zcodeTime(d.LastSeenAt) + `</div>` +
			ipMeta(d.LastIP) +
			closeTag)
	}

	html := string(MustAsset("zlist.html"))
	html = strings.Replace(html, "{{DEVICES}}", rows.String(), 1)
	html = strings.Replace(html, "{{TOTAL}}", fmt.Sprint(len(list)), 1)
	html = strings.Replace(html, "{{ONLINE}}", fmt.Sprint(onlineCount), 1)
	empty := "none"
	if len(list) == 0 {
		empty = "block"
	}
	html = strings.Replace(html, "{{EMPTY_DISPLAY}}", empty, 1)

	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = w.Write([]byte(html))
}

// api://zlist handler registration: "zlist" serves the device list linking
// to the injected zcode web bundle (cc~ proxied /remote/v4).
func init() {
	registerAPI("zlist", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		zcodeGetRelay("zcode").ServeZList(w, r, "/remote/v4")
	}))
}
