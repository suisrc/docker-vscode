package pkg

// manager — the generic app management hub (api://manager). It aggregates
// every access segment kvs knows about into one UI:
//
//	[ZCD]local — the zcode web app this kvs serves under /zcode (built-in)
//	[ZCD]name  — zcode remote desktops registered through the pairing relay
//	[VSC]name  — user-added VS Code apps, optionally carrying workspace
//	             folder quick-links (one device, several folders)
//
// Persistent state (custom apps, first-seen / last-visit stamps, folder
// quick-links) lives in {home}/zcodex.json together with the relay device
// registry (one file, one flush loop). The page itself (manager.html) is a
// static client-rendered document fed by the JSON API served here:
//
//	GET    /__manager                        list all entries (merged view)
//	POST   /__manager                        add a custom app {type,icon?,name,url,folders?}
//	                                           type: vsc (VS Code, folders ok) | other (any
//                                           device, logo picked from vsc/zcd/linux/other)
//	DELETE /__manager?id=…                   remove a custom app
//	POST   /__manager/visit                  record an access {id}
//	POST   /__manager/order                  save the dragged card order {ids}
//	POST   /__manager/tool                   upsert a toolbar quick link {name,addr,old?}
//	DELETE /__manager/tool?name=…            remove a toolbar quick link
//	POST   /__manager/tags                   set an entry's label chips {id,tags}
//	POST   /__manager/folder                 add/edit a workspace folder {id,name,path,old?}
//	DELETE /__manager/folder?id=…&name=…     remove a workspace folder
//	GET    /__manager/note?id=…              fetch one entry's remark note (own endpoint —
//	                                           notes never travel with the list payload)
//	POST   /__manager/note                   save / clear a note {id,note} ("" clears)
//	(workspace folders open on the card's own host: {base}/?folder=<path>)

import (
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"sort"
	"strings"
	"sync"
	"time"
)

const (
	mgrIDZcdLocal = "zcd-local"
	mgrIDVscLocal = "vsc-local" // legacy stamps only; no synthesized entry anymore
	mgrZcodePort  = "7587"      // default zcode local app port (KVS_ZCODE_PORT)
	mgrProbeTTL   = 45 * time.Second
	mgrFolderMax  = 12
	mgrNoteMax    = 500 // per-entry remark note length cap (runes)
	mgrMaxBody    = 64 * 1024
)

// ---------------------------------------------------------------------------
// persisted model (the "apps" section of zcodex.json)
// ---------------------------------------------------------------------------

// managerFolder is one workspace quick-link on a VS Code entry: Path is the
// workspace address on that device, opened as {card base}/?folder=<Path>.
type managerFolder struct {
	Name string `json:"name"`
	Path string `json:"path"`
}

// managerApp is one persisted app record. Default entries (zcd-local /
// vsc-local) and relay devices are synthesized at render time; their records
// exist only to carry first-seen / last-visit stamps and folder lists.
// Custom marks user-added entries, which are the only deletable ones.
type managerApp struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"`           // "vsc" | "other" (custom); "zcd" legacy stamps
	Icon      string          `json:"icon,omitempty"` // logo key: vsc | zcd | linux | other
	Name      string          `json:"name"`
	URL       string          `json:"url,omitempty"`
	Folders   []managerFolder `json:"folders,omitempty"`
	Tags      []string        `json:"tags,omitempty"` // user-defined label chips (editable in page)
	Version   string          `json:"version,omitempty"`
	OS        string          `json:"os,omitempty"`
	CreatedAt int64           `json:"created_at,omitempty"`
	LastSeen  int64           `json:"last_seen,omitempty"`
	Custom    bool            `json:"custom,omitempty"`
}

// appsList returns a copy of all persisted app records.
func (s *zcodeStore) appsList() []managerApp {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]managerApp(nil), s.apps...)
}

// appFind returns the record for id.
func (s *zcodeStore) appFind(id string) (managerApp, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range s.apps {
		if a.ID == id {
			return a, true
		}
	}
	return managerApp{}, false
}

// appSave inserts or replaces a record (keyed by ID), preserving order.
func (s *zcodeStore) appSave(app managerApp) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.apps {
		if s.apps[i].ID == app.ID {
			s.apps[i] = app
			s.dirty = true
			s.requestFlushLocked()
			return
		}
	}
	s.apps = append(s.apps, app)
	s.dirty = true
	s.requestFlushLocked()
}

// appDelete removes a record by ID; reports whether it existed.
func (s *zcodeStore) appDelete(id string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.apps {
		if s.apps[i].ID == id {
			s.apps = append(s.apps[:i], s.apps[i+1:]...)
			s.dirty = true
			s.requestFlushLocked()
			return true
		}
	}
	return false
}

// requestFlushLocked is requestFlush for callers already holding s.mu.
func (s *zcodeStore) requestFlushLocked() {
	select {
	case s.notify <- struct{}{}:
	default:
	}
}

// orderList returns the persisted card order (user-arranged entry ids).
func (s *zcodeStore) orderList() []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]string(nil), s.order...)
}

// orderSave replaces the persisted card order.
func (s *zcodeStore) orderSave(ids []string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.order = append([]string(nil), ids...)
	s.dirty = true
	s.requestFlushLocked()
}

// managerTool is one toolbar quick link: Name shows on the button, Addr is
// opened in a new tab (download or page render is up to the browser).
type managerTool struct {
	Name string `json:"name"`
	Addr string `json:"addr"`
}

// toolsList returns the persisted toolbar quick links.
func (s *zcodeStore) toolsList() []managerTool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]managerTool(nil), s.tools...)
}

// toolSave upserts a quick link by name (old, when set, is renamed away first).
func (s *zcodeStore) toolSave(t managerTool, old string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if old != "" && old != t.Name {
		kept := s.tools[:0]
		for _, x := range s.tools {
			if x.Name != old {
				kept = append(kept, x)
			}
		}
		s.tools = kept
	}
	for i := range s.tools {
		if s.tools[i].Name == t.Name {
			s.tools[i] = t
			s.dirty = true
			s.requestFlushLocked()
			return
		}
	}
	s.tools = append(s.tools, t)
	s.dirty = true
	s.requestFlushLocked()
}

// toolDelete removes a quick link by name; reports whether it existed.
func (s *zcodeStore) toolDelete(name string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	kept := s.tools[:0]
	removed := false
	for _, x := range s.tools {
		if x.Name == name {
			removed = true
			continue
		}
		kept = append(kept, x)
	}
	if removed {
		s.tools = kept
		s.dirty = true
		s.requestFlushLocked()
	}
	return removed
}

// noteGet returns the stored note for an entry id ("" when none).
func (s *zcodeStore) noteGet(id string) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.notes[id]
}

// noteSet stores (text non-empty) or clears (text empty) an entry's note;
// reports whether the persisted state changed.
func (s *zcodeStore) noteSet(id, text string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if text == "" {
		if _, ok := s.notes[id]; !ok {
			return false
		}
		delete(s.notes, id)
		s.dirty = true
		s.requestFlushLocked()
		return true
	}
	if s.notes == nil {
		s.notes = make(map[string]string)
	}
	if s.notes[id] == text {
		return false
	}
	s.notes[id] = text
	s.dirty = true
	s.requestFlushLocked()
	return true
}

// ---------------------------------------------------------------------------
// liveness / version probes (best-effort, cached, never blocking)
// ---------------------------------------------------------------------------

// mgrProbeResult is one cached probe outcome.
type mgrProbeResult struct {
	at      time.Time
	version string
	online  bool
}

var (
	mgrProbeMu      sync.Mutex
	mgrProbeCache   = map[string]mgrProbeResult{}
	mgrProbePending = map[string]bool{}
)

// mgrProbeCached returns the cached probe for target (version, online, known),
// refreshing in the background when older than mgrProbeTTL. It never blocks
// the requesting goroutine: unknown targets report ("" , false, false) until
// the first background probe lands.
func mgrProbeCached(target string) (string, bool, bool) {
	mgrProbeMu.Lock()
	res, ok := mgrProbeCache[target]
	fresh := ok && time.Since(res.at) < mgrProbeTTL
	if !fresh && !mgrProbePending[target] {
		mgrProbePending[target] = true
	}
	mgrProbeMu.Unlock()

	if !fresh && mgrProbePending[target] {
		go func() {
			v, on := mgrProbe(target)
			mgrProbeMu.Lock()
			mgrProbeCache[target] = mgrProbeResult{at: time.Now(), version: v, online: on}
			delete(mgrProbePending, target)
			mgrProbeMu.Unlock()
		}()
	}
	if !ok {
		return "", false, false
	}
	return res.version, res.online, true
}

// mgrProbe GETs target: any 2xx means online. A short plain-text body is
// kept as the version (the /__version endpoint shape); a JSON body contributes
// its "version" field (the zcode /api/server-info shape).
func mgrProbe(target string) (string, bool) {
	client := &http.Client{
		Timeout: 3 * time.Second,
		Transport: &http.Transport{
			TLSClientConfig:   &tls.Config{InsecureSkipVerify: true}, // self-signed front proxies
			DisableKeepAlives: true,
		},
	}
	resp, err := client.Get(target)
	if err != nil {
		return "", false
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 8*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", false
	}
	if err != nil {
		return "", true
	}
	v := strings.TrimSpace(string(body))
	if strings.HasPrefix(v, "{") {
		var m map[string]any
		if json.Unmarshal(body, &m) == nil {
			if s, _ := m["version"].(string); s != "" {
				return s, true
			}
		}
		return "", true
	}
	if len(v) > 40 {
		v = "" // not a version string
	}
	return v, true
}

// mgrZcodeCheckURL is the local zcode app liveness URL (KVS_SVC_CHECK_URL in
// zcodex mode; otherwise the default zcode port).
func mgrZcodeCheckURL() string {
	if u := strings.TrimSpace(os.Getenv("KVS_SVC_CHECK_URL")); u != "" {
		return u
	}
	port := os.Getenv("KVS_ZCODE_PORT")
	if port == "" {
		port = mgrZcodePort
	}
	return "http://127.0.0.1:" + port + "/api/server-info"
}

// ---------------------------------------------------------------------------
// entry assembly — the merged client-facing view
// ---------------------------------------------------------------------------

// mgrEntry is one app row served by GET /__manager.
type mgrEntry struct {
	ID        string          `json:"id"`
	Type      string          `json:"type"` // "zcd" | "vsc" | "other"
	Icon      string          `json:"icon"` // logo key: vsc | zcd | linux | other
	Name      string          `json:"name"`
	URL       string          `json:"url,omitempty"`  // empty → not openable
	Host      string          `json:"host,omitempty"` // VS Code domain form
	Folders   []managerFolder `json:"folders,omitempty"`
	Tags      []string        `json:"tags,omitempty"`
	Version   string          `json:"version,omitempty"`
	OS        string          `json:"os,omitempty"`
	Online    *bool           `json:"online,omitempty"` // nil = unknown
	Local     bool            `json:"local,omitempty"`
	Custom    bool            `json:"custom,omitempty"`
	CreatedAt int64           `json:"created_at,omitempty"`
	LastSeen  int64           `json:"last_seen,omitempty"`
}

// mgrIconFor maps a type (and optional stored icon) to a logo key.
func mgrIconFor(typ, icon string) string {
	switch icon {
	case "vsc", "zcd", "linux", "other":
		return icon
	}
	switch typ {
	case "vsc":
		return "vsc"
	case "zcd":
		return "zcd"
	}
	return "other"
}

// zcodeDeviceInfo is one relay device row of the merged view (manager-side
// projection of a zcodeDeviceRecord; never sent to the client verbatim).
type zcodeDeviceInfo struct {
	Sid        string
	Mid        string
	Name       string
	Platform   string // desktop OS (from register meta)
	Version    string // desktop app version (from register meta)
	PassHash   string // pairing credential, used to build the terminal URL
	Online     bool
	CreatedAt  int64  // first registration
	UpdatedAt  int64  // last register/re-register (上线时间)
	LastSeenAt int64  // last authenticated activity (最后使用)
	LastIP     string // last connection source IP
}

// deviceList returns the registered relay devices with live online status.
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
			CreatedAt:  rec.CreatedAt,
			UpdatedAt:  rec.UpdatedAt,
			LastSeenAt: rec.LastSeenAt,
			LastIP:     rec.LastIP,
		})
	}
	return out
}

// mgrProto guesses the public scheme for absolute links: honor
// X-Forwarded-Proto (first token) from the front proxy, then r.TLS.
func mgrProto(r *http.Request) string {
	if p := r.Header.Get("X-Forwarded-Proto"); p != "" {
		if i := strings.IndexByte(p, ','); i >= 0 {
			p = p[:i]
		}
		if p = strings.TrimSpace(p); p != "" {
			return p
		}
	}
	if r.TLS != nil {
		return "https"
	}
	return "http"
}

// mgrFolderURL builds a workspace quick-link: {base}/?folder=<path> — the
// folder opens on the SAME host as the card, selected by the ?folder query
// parameter (VS Code web's workspace selector). Returns "" when either side
// is empty or base is not an absolute http(s) URL.
func mgrFolderURL(base, path string) string {
	path = strings.TrimSpace(path)
	b := strings.TrimSuffix(strings.TrimSpace(base), "/")
	if path == "" || !strings.Contains(b, "://") {
		return ""
	}
	return b + "/?folder=" + url.QueryEscape(path)
}

// mgrApplyProbe fills version/online on e from the cached probe of target
// (http(s) targets only).
func mgrApplyProbe(e *mgrEntry, target string) {
	if !strings.HasPrefix(target, "http://") && !strings.HasPrefix(target, "https://") {
		return
	}
	v, on, known := mgrProbeCached(target)
	if !known {
		return
	}
	e.Online = &on
	if v != "" {
		e.Version = v
	}
}

// mgrBuildEntries merges every app source into the client-facing list.
func mgrBuildEntries(r *http.Request) []mgrEntry {
	store := zcodeState()
	proto := mgrProto(r)
	now := time.Now().UnixMilli()
	out := make([]mgrEntry, 0, 8)

	// [ZCD]local — the zcode web app under /zcode (this kvs serves it; kvs
	// prepares the backend lazily on first access, so it is always openable).
	zcdRec, _ := store.appFind(mgrIDZcdLocal)
	e := mgrEntry{
		ID: mgrIDZcdLocal, Type: "zcd", Icon: "zcd", Name: "local",
		URL: "/zcode", OS: runtime.GOOS, Local: true, Tags: zcdRec.Tags,
		CreatedAt: zcdRec.CreatedAt, LastSeen: zcdRec.LastSeen,
	}
	mgrApplyProbe(&e, mgrZcodeCheckURL())
	out = append(out, e)

	// [ZCD]<name> — relay devices (remote zcode desktops). Online devices get
	// the remote-control terminal URL (official QR-URL query shape); the
	// pass_hash never leaves the server as data — only inside the link.
	liveSids := make(map[string]bool)
	for _, d := range zcodeGetRelay("zcode-clients").deviceList() {
		liveSids[d.Sid] = true
		id := "zcd-" + d.Sid
		rec, _ := store.appFind(id)
		e := mgrEntry{
			ID: id, Type: "zcd", Icon: "zcd", Name: d.Name,
			Version: d.Version, OS: d.Platform, Tags: rec.Tags,
			CreatedAt: d.CreatedAt, LastSeen: d.LastSeenAt,
		}
		online := d.Online
		e.Online = &online
		if rec.LastSeen > e.LastSeen {
			e.LastSeen = rec.LastSeen // manager-side visit may be newer
		}
		if rec.CreatedAt != 0 && (e.CreatedAt == 0 || rec.CreatedAt < e.CreatedAt) {
			e.CreatedAt = rec.CreatedAt
		}
		if d.Online {
			q := url.Values{}
			q.Set("sid", d.Sid)
			q.Set("hash", d.PassHash)
			q.Set("t", fmt.Sprint(now))
			q.Set("mid", d.Mid)
			q.Set("name", d.Name)
			if d.Version != "" {
				q.Set("app_version", d.Version)
			}
			e.URL = proto + "://" + r.Host + "/remote/v4?" + q.Encode()
		}
		out = append(out, e)
	}

	// Custom apps (and orphan-free defaults): records whose id is not one of
	// the merged defaults and not a live relay device stamp.
	for _, app := range store.appsList() {
		if app.ID == mgrIDZcdLocal || app.ID == mgrIDVscLocal {
			continue
		}
		if sid, ok := strings.CutPrefix(app.ID, "zcd-"); ok && liveSids[sid] {
			continue // relay-device stamp, merged above
		}
		e := mgrEntry{
			ID: app.ID, Type: app.Type, Icon: mgrIconFor(app.Type, app.Icon), Name: app.Name,
			URL: app.URL, Folders: app.Folders, Tags: app.Tags,
			Version: app.Version, OS: app.OS,
			Custom: app.Custom, CreatedAt: app.CreatedAt, LastSeen: app.LastSeen,
		}
		if u, err := url.Parse(app.URL); err == nil && (u.Scheme == "http" || u.Scheme == "https") {
			e.Host = u.Hostname()
			mgrApplyProbe(&e, strings.TrimSuffix(app.URL, "/")+"/__version")
		}
		out = append(out, e)
	}

	// 用户拖拽后的固定顺序（zcodex.json order 段）：有序条目按序在前，
	// 新出现（未排序）的条目按服务器顺序追加在后。
	order := store.orderList()
	pos := make(map[string]int, len(order))
	for i, id := range order {
		if _, dup := pos[id]; !dup {
			pos[id] = i
		}
	}
	rank := func(e mgrEntry) int {
		if i, ok := pos[e.ID]; ok {
			return i
		}
		return len(order)
	}
	sort.SliceStable(out, func(i, j int) bool { return rank(out[i]) < rank(out[j]) })
	return out
}

// ---------------------------------------------------------------------------
// HTTP handler (api://manager)
// ---------------------------------------------------------------------------

// managerHandler routes the manager's own paths: the page at "/", the JSON
// API under /__manager, 404 for anything else.
func managerHandler(w http.ResponseWriter, r *http.Request) {
	switch r.URL.Path {
	case "/", "":
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.Header().Set("Cache-Control", "no-store")
		_, _ = w.Write(MustAsset("manager.html"))

	case "/__manager":
		switch r.Method {
		case http.MethodGet:
			managerListApps(w, r)
		case http.MethodPost:
			managerAddApp(w, r)
		case http.MethodDelete:
			managerDeleteApp(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "/__manager/visit":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		managerVisit(w, r)

	case "/__manager/folder":
		switch r.Method {
		case http.MethodPost:
			managerFolderAdd(w, r)
		case http.MethodDelete:
			managerFolderDel(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "/__manager/order":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		managerSaveOrder(w, r)

	case "/__manager/tool":
		switch r.Method {
		case http.MethodPost:
			managerToolSave(w, r)
		case http.MethodDelete:
			managerToolDel(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	case "/__manager/tags":
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		managerSaveTags(w, r)

	case "/__manager/note":
		switch r.Method {
		case http.MethodGet:
			managerNoteGet(w, r)
		case http.MethodPost:
			managerNoteSet(w, r)
		default:
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		}

	default:
		http.NotFound(w, r)
	}
}

// mgrDecode reads a limited JSON body into v; writes the error response
// itself and reports success.
func mgrDecode(w http.ResponseWriter, r *http.Request, v any) bool {
	if r.Body == nil {
		http.Error(w, "empty body", http.StatusBadRequest)
		return false
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, mgrMaxBody)).Decode(v); err != nil {
		http.Error(w, "invalid JSON: "+err.Error(), http.StatusBadRequest)
		return false
	}
	return true
}

// mgrWriteJSON emits v as a no-store JSON response.
func mgrWriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// managerListApps serves the merged entry list.
func managerListApps(w http.ResponseWriter, r *http.Request) {
	mgrWriteJSON(w, http.StatusOK, map[string]any{
		"now":   time.Now().UnixMilli(),
		"apps":  mgrBuildEntries(r),
		"tools": zcodeState().toolsList(),
	})
}

// mgrAppReq is the add/edit-app request body. A non-empty ID switches to edit
// mode (the named custom app is updated instead of a new one created).
type mgrAppReq struct {
	ID      string          `json:"id"`
	Type    string          `json:"type"` // "vsc" | "other" ("zcd" kept as legacy alias)
	Icon    string          `json:"icon"` // logo key for "other": vsc | zcd | linux | other
	Name    string          `json:"name"`
	URL     string          `json:"url"`
	Folders []managerFolder `json:"folders"`
}

// managerAddApp adds a custom app entry. VS Code apps always carry the VS Code
// logo and optional workspace folders; "other" apps (Linux desktop, anything)
// pick one of the four logos and take no folders.
func managerAddApp(w http.ResponseWriter, r *http.Request) {
	var req mgrAppReq
	if !mgrDecode(w, r, &req) {
		return
	}
	req.Type = strings.ToLower(strings.TrimSpace(req.Type))
	req.Icon = strings.ToLower(strings.TrimSpace(req.Icon))
	req.Name = strings.TrimSpace(req.Name)
	req.URL = strings.TrimSpace(req.URL)
	if req.ID != "" && req.Type == "" {
		// edit mode may omit type: keep the stored one
		if app, ok := zcodeState().appFind(req.ID); ok {
			req.Type = app.Type
		}
	}
	if req.Type != "vsc" && req.Type != "other" && req.Type != "zcd" {
		http.Error(w, "type must be vsc or other", http.StatusBadRequest)
		return
	}
	if req.Type == "zcd" {
		req.Type = "other" // legacy alias: a custom zcode entry is just "other" + zcode logo
	}
	if req.Name == "" || len([]rune(req.Name)) > 40 {
		http.Error(w, "name required (max 40 chars)", http.StatusBadRequest)
		return
	}
	if req.URL == "" || len(req.URL) > 512 ||
		(!strings.HasPrefix(req.URL, "/") && !strings.HasPrefix(req.URL, "http://") && !strings.HasPrefix(req.URL, "https://")) {
		http.Error(w, "url required (must start with / or http(s)://)", http.StatusBadRequest)
		return
	}
	icon := mgrIconFor(req.Type, req.Icon)
	if req.Type != "vsc" && req.Icon != "" && icon != req.Icon {
		http.Error(w, "icon must be one of vsc, zcd, linux, other", http.StatusBadRequest)
		return
	}

	store := zcodeState()

	// 编辑模式：仅自定义应用可改，保留 id/创建时间/访问记录/标记
	if req.ID != "" {
		app, ok := store.appFind(req.ID)
		if !ok {
			http.Error(w, "unknown app id", http.StatusNotFound)
			return
		}
		if !app.Custom {
			http.Error(w, "only custom apps can be edited", http.StatusForbidden)
			return
		}
		app.Type, app.Icon, app.Name, app.URL = req.Type, icon, req.Name, req.URL
		app.Folders = nil
		if req.Type == "vsc" {
			app.Folders = mgrParseFolders(req.Folders, req.URL)
		}
		store.appSave(app)
		log.Printf("[manager] app updated: %s [%s/%s] %s", app.Name, app.Type, app.Icon, app.URL)
		mgrWriteJSON(w, http.StatusOK, app)
		return
	}

	app := managerApp{
		ID: "app-" + randomHex(4), Type: req.Type, Icon: icon, Name: req.Name, URL: req.URL,
		CreatedAt: time.Now().UnixMilli(), Custom: true,
	}
	for {
		if _, exists := store.appFind(app.ID); !exists {
			break
		}
		app.ID = "app-" + randomHex(4) // id collision, pick another
	}
	if req.Type == "vsc" {
		app.Folders = mgrParseFolders(req.Folders, req.URL)
	}
	store.appSave(app)
	log.Printf("[manager] app added: %s [%s/%s] %s", app.Name, app.Type, app.Icon, app.URL)
	mgrWriteJSON(w, http.StatusOK, app)
}

// mgrParseFolders normalizes an inbound folder list: names default to the
// path's last segment; entries with unusable paths or overlong names drop out.
func mgrParseFolders(in []managerFolder, baseURL string) []managerFolder {
	if len(in) > mgrFolderMax {
		in = in[:mgrFolderMax]
	}
	var out []managerFolder
	for _, f := range in {
		f.Name = strings.TrimSpace(f.Name)
		f.Path = strings.TrimSpace(f.Path)
		if f.Name == "" {
			f.Name = mgrPathName(f.Path) // 名称缺省取路径末段
		}
		if f.Path == "" || len(f.Path) > 256 || f.Name == "" || len([]rune(f.Name)) > 24 {
			continue
		}
		if mgrFolderURL(baseURL, f.Path) == "" {
			continue // base 不可用（非绝对地址）
		}
		out = append(out, f)
	}
	return out
}

// mgrPathName returns the last path segment of p ("a/b/c" → "c").
func mgrPathName(p string) string {
	if i := strings.LastIndexByte(p, '/'); i >= 0 && i < len(p)-1 {
		return p[i+1:]
	}
	return p
}

// managerDeleteApp removes a custom app (defaults and relay stamps are not
// deletable).
func managerDeleteApp(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	store := zcodeState()
	app, ok := store.appFind(id)
	if !ok {
		http.Error(w, "unknown app id", http.StatusNotFound)
		return
	}
	if !app.Custom {
		http.Error(w, "only custom apps can be deleted", http.StatusForbidden)
		return
	}
	store.appDelete(id)
	store.noteSet(id, "") // 同步清理该应用的备注，避免 notes 段残留孤儿数据
	log.Printf("[manager] app removed: %s (%s)", app.Name, id)
	mgrWriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// managerSaveOrder persists the user-arranged card order (full id list as
// rendered; ids not currently present keep their slots for when they return).
func managerSaveOrder(w http.ResponseWriter, r *http.Request) {
	var req struct {
		IDs []string `json:"ids"`
	}
	if !mgrDecode(w, r, &req) {
		return
	}
	if len(req.IDs) > 200 {
		http.Error(w, "too many ids", http.StatusBadRequest)
		return
	}
	ids := make([]string, 0, len(req.IDs))
	seen := make(map[string]bool, len(req.IDs))
	for _, id := range req.IDs {
		if id = strings.TrimSpace(id); id == "" || seen[id] {
			continue
		}
		seen[id] = true
		ids = append(ids, id)
	}
	zcodeState().orderSave(ids)
	mgrWriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// managerToolSave upserts a toolbar quick link ({name,addr,old?}); addr must
// be an absolute http(s) URL or a path.
func managerToolSave(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name string `json:"name"`
		Addr string `json:"addr"`
		Old  string `json:"old"` // edit mode: previous name being renamed
	}
	if !mgrDecode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Addr = strings.TrimSpace(req.Addr)
	req.Old = strings.TrimSpace(req.Old)
	if req.Name == "" || len([]rune(req.Name)) > 24 {
		http.Error(w, "name required (max 24 chars)", http.StatusBadRequest)
		return
	}
	if req.Addr == "" || len(req.Addr) > 512 ||
		(!strings.HasPrefix(req.Addr, "/") && !strings.HasPrefix(req.Addr, "http://") && !strings.HasPrefix(req.Addr, "https://")) {
		http.Error(w, "addr required (must start with / or http(s)://)", http.StatusBadRequest)
		return
	}
	zcodeState().toolSave(managerTool{Name: req.Name, Addr: req.Addr}, req.Old)
	log.Printf("[manager] tool saved: %s → %s", req.Name, req.Addr)
	mgrWriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// managerToolDel removes a toolbar quick link by name.
func managerToolDel(w http.ResponseWriter, r *http.Request) {
	name := strings.TrimSpace(r.URL.Query().Get("name"))
	if name == "" {
		http.Error(w, "name required", http.StatusBadRequest)
		return
	}
	if !zcodeState().toolDelete(name) {
		http.Error(w, "tool not found", http.StatusNotFound)
		return
	}
	mgrWriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// mgrStubApp synthesizes an app record for a default entry that has never
// been persisted (zcd-local or a registered relay device), so tag / visit
// writes have something to attach to. ok=false when id names no known entry.
func mgrStubApp(store *zcodeStore, id string, now int64) (managerApp, bool) {
	if id == mgrIDZcdLocal {
		return managerApp{ID: id, Type: "zcd", Name: "local", CreatedAt: now}, true
	}
	if sid, ok := strings.CutPrefix(id, "zcd-"); ok {
		if rec := store.get(sid); rec != nil {
			app := managerApp{ID: id, Type: "zcd", CreatedAt: now}
			if name, _ := rec.Meta["name"].(string); name != "" {
				app.Name = name
			}
			return app, true
		}
	}
	return managerApp{}, false
}

// managerSaveTags replaces the user-defined label chips of an entry {id,tags}.
// Works for every entry kind (default / relay device / custom app); tags are
// stored on the entry's record.
func managerSaveTags(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string   `json:"id"`
		Tags []string `json:"tags"`
	}
	if !mgrDecode(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	store := zcodeState()
	app, ok := store.appFind(id)
	if !ok {
		if app, ok = mgrStubApp(store, id, time.Now().UnixMilli()); !ok {
			http.Error(w, "unknown app id", http.StatusNotFound)
			return
		}
	}
	if len(req.Tags) > 6 {
		http.Error(w, "too many tags (max 6)", http.StatusBadRequest)
		return
	}
	tags := make([]string, 0, len(req.Tags))
	seen := make(map[string]bool, len(req.Tags))
	for _, t := range req.Tags {
		t = strings.TrimSpace(t)
		if t == "" || len([]rune(t)) > 12 || seen[t] {
			continue
		}
		seen[t] = true
		tags = append(tags, t)
	}
	app.Tags = tags
	store.appSave(app)
	mgrWriteJSON(w, http.StatusOK, map[string]any{"success": true, "tags": tags})
}

// mgrEntryExists reports whether id names an entry the manager UI can see:
// the built-in zcd-local, any persisted app record (custom app or relay
// stamp), or a registered relay device.
func mgrEntryExists(store *zcodeStore, id string) bool {
	if id == mgrIDZcdLocal {
		return true
	}
	if _, ok := store.appFind(id); ok {
		return true
	}
	if sid, ok := strings.CutPrefix(id, "zcd-"); ok && store.get(sid) != nil {
		return true
	}
	return false
}

// managerNoteGet serves one entry's note ({id,note}). Notes travel on their
// own endpoint so the merged list stays lean.
func managerNoteGet(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSpace(r.URL.Query().Get("id"))
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	store := zcodeState()
	if !mgrEntryExists(store, id) {
		http.Error(w, "unknown app id", http.StatusNotFound)
		return
	}
	mgrWriteJSON(w, http.StatusOK, map[string]string{"id": id, "note": store.noteGet(id)})
}

// managerNoteSet stores (or clears, when empty) an entry's note {id,note}.
// Notes are capped at mgrNoteMax runes; a blank note deletes the entry.
func managerNoteSet(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID   string `json:"id"`
		Note string `json:"note"`
	}
	if !mgrDecode(w, r, &req) {
		return
	}
	req.ID = strings.TrimSpace(req.ID)
	req.Note = strings.TrimSpace(req.Note)
	if req.ID == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	if len([]rune(req.Note)) > mgrNoteMax {
		http.Error(w, fmt.Sprintf("note too long (max %d chars)", mgrNoteMax), http.StatusBadRequest)
		return
	}
	store := zcodeState()
	if !mgrEntryExists(store, req.ID) {
		http.Error(w, "unknown app id", http.StatusNotFound)
		return
	}
	store.noteSet(req.ID, req.Note)
	if req.Note == "" {
		log.Printf("[manager] note cleared: %s", req.ID)
	} else {
		log.Printf("[manager] note saved: %s (%d chars)", req.ID, len([]rune(req.Note)))
	}
	mgrWriteJSON(w, http.StatusOK, map[string]string{"id": req.ID, "note": req.Note})
}

// managerVisit records an access: the page fires this the moment a card is
// clicked, creating the entry's stamp (first-seen) and refreshing last-seen.
func managerVisit(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ID string `json:"id"`
	}
	if !mgrDecode(w, r, &req) {
		return
	}
	id := strings.TrimSpace(req.ID)
	if id == "" {
		http.Error(w, "id required", http.StatusBadRequest)
		return
	}
	store := zcodeState()
	if !mgrEntryExists(store, id) {
		http.Error(w, "unknown app id", http.StatusNotFound)
		return
	}

	now := time.Now().UnixMilli()
	app, _ := store.appFind(id)
	if app.ID == "" {
		app, _ = mgrStubApp(store, id, now) // id 已通过 mgrEntryExists 校验
	}
	app.LastSeen = now
	store.appSave(app)
	mgrWriteJSON(w, http.StatusOK, map[string]bool{"success": true})
}

// mgrFolderReq is the add-folder request body.
type mgrFolderReq struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Path string `json:"path"` // workspace address, opened as {base}/?folder=<path>
	Old  string `json:"old"`  // edit mode: previous name being renamed/replaced
}

// managerFolderAdd attaches a workspace quick-link to a VS Code entry
// (custom app or the built-in vsc-local). The folder opens on the same host
// as the card: {base}/?folder=<path>.
func managerFolderAdd(w http.ResponseWriter, r *http.Request) {
	var req mgrFolderReq
	if !mgrDecode(w, r, &req) {
		return
	}
	req.Name = strings.TrimSpace(req.Name)
	req.Path = strings.TrimSpace(req.Path)
	if req.Path == "" || len(req.Path) > 256 {
		http.Error(w, "path required (workspace address, max 256 chars)", http.StatusBadRequest)
		return
	}
	if req.Name == "" {
		req.Name = mgrPathName(req.Path) // 名称缺省取路径末段
	}
	if req.ID == "" || req.Name == "" || len([]rune(req.Name)) > 24 {
		http.Error(w, "id required (name max 24 chars)", http.StatusBadRequest)
		return
	}
	store := zcodeState()
	app, ok := store.appFind(req.ID)
	if !ok {
		http.Error(w, "unknown app id", http.StatusNotFound)
		return
	}
	if app.Type != "vsc" {
		http.Error(w, "folders are only supported on vscode apps", http.StatusBadRequest)
		return
	}
	// Edit mode: a rename (Old set and different) drops the previous entry
	// before the new one is upserted.
	if old := strings.TrimSpace(req.Old); old != "" && old != req.Name {
		kept := app.Folders[:0]
		for _, f := range app.Folders {
			if f.Name != old {
				kept = append(kept, f)
			}
		}
		app.Folders = kept
	}
	if len(app.Folders) >= mgrFolderMax {
		http.Error(w, "too many folders", http.StatusBadRequest)
		return
	}
	base := app.URL
	if mgrFolderURL(base, req.Path) == "" {
		http.Error(w, "app has no usable absolute base url", http.StatusBadRequest)
		return
	}
	folder := managerFolder{Name: req.Name, Path: req.Path}
	replaced := false
	for i := range app.Folders {
		if app.Folders[i].Name == req.Name {
			app.Folders[i] = folder
			replaced = true
		}
	}
	if !replaced {
		app.Folders = append(app.Folders, folder)
	}
	store.appSave(app)
	mgrWriteJSON(w, http.StatusOK, app)
}

// managerFolderDel removes a workspace quick-link by name.
func managerFolderDel(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	id := strings.TrimSpace(q.Get("id"))
	name := strings.TrimSpace(q.Get("name"))
	if id == "" || name == "" {
		http.Error(w, "id and name required", http.StatusBadRequest)
		return
	}
	store := zcodeState()
	app, ok := store.appFind(id)
	if !ok {
		http.Error(w, "unknown app id", http.StatusNotFound)
		return
	}
	kept := app.Folders[:0]
	removed := false
	for _, f := range app.Folders {
		if f.Name == name {
			removed = true
			continue
		}
		kept = append(kept, f)
	}
	if !removed {
		http.Error(w, "folder not found", http.StatusNotFound)
		return
	}
	app.Folders = kept
	store.appSave(app)
	mgrWriteJSON(w, http.StatusOK, app)
}

// api://manager handler registration (see backend.go for the registry).
func init() {
	registerAPI("manager", http.HandlerFunc(managerHandler))
}
