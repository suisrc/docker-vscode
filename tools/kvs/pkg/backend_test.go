package pkg

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAPIBackendMountPath checks that api:// backends are mount-point aware:
// the handler is written against "/" and the route prefix is stripped before
// dispatch, so the same api:// service works at any path (page at the mount
// path, API at <mount>/__manager*) and root mounts behave unchanged.
func TestAPIBackendMountPath(t *testing.T) {
	SetZcodeHome(t.TempDir())

	get := func(b Backend, path string) *httptest.ResponseRecorder {
		h := CreateBackendHandler(b, nil, false)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest("GET", path, nil))
		return rec
	}

	// Mounted at /admin: page at /admin (and /admin/), API under /admin/__manager*.
	admin := Backend{Source: "/admin", Scheme: "api", Target: "manager", RawURL: "api://manager"}
	if rec := get(admin, "/admin"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "应用中心") {
		t.Fatalf("GET /admin -> %d, want 200 page", rec.Code)
	}
	if rec := get(admin, "/admin/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "应用中心") {
		t.Fatalf("GET /admin/ -> %d, want 200 page", rec.Code)
	}
	if rec := get(admin, "/admin/__manager"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("GET /admin/__manager -> %d, want 200 json", rec.Code)
	}
	// A path that merely shares the prefix must not leak the handler.
	if rec := get(admin, "/administrator"); rec.Code != 404 {
		t.Fatalf("GET /administrator -> %d, want 404", rec.Code)
	}

	// Mounted at the root: no strip, page at / and API at /__manager.
	root := Backend{Source: "/", Scheme: "api", Target: "manager", RawURL: "api://manager"}
	if rec := get(root, "/"); rec.Code != 200 || !strings.Contains(rec.Body.String(), "应用中心") {
		t.Fatalf("GET / -> %d, want 200 page", rec.Code)
	}
	if rec := get(root, "/__manager"); rec.Code != 200 || !strings.Contains(rec.Header().Get("Content-Type"), "json") {
		t.Fatalf("GET /__manager -> %d, want 200 json", rec.Code)
	}
}

// TestMgrFolderURLRelative checks workspace quick-link building for absolute
// and mount-relative bases (built-in cards open on the page's own origin).
func TestMgrFolderURLRelative(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://x.example.com", "/a b", "https://x.example.com/?folder=" + url.QueryEscape("/a b")},
		{"/zcode", "/wsc/p1", "/zcode/?folder=%2Fwsc%2Fp1"},
		{"/zcode/", "/wsc/p1", "/zcode/?folder=%2Fwsc%2Fp1"},
		{"/", "/wsc/p1", "/?folder=%2Fwsc%2Fp1"},
		{"", "/x", ""},
		{"not-a-url", "/x", ""},
		{"https://x.example.com", "", ""},
	}
	for _, c := range cases {
		if got := mgrFolderURL(c.base, c.path); got != c.want {
			t.Fatalf("mgrFolderURL(%q,%q) = %q, want %q", c.base, c.path, got, c.want)
		}
	}
}

// TestManagerBuiltInApps exercises the 内置 (built-in hosted service) cards:
// one card per sniffed svc:// service, zcode data-dir/workspace overrides
// applied to (and cleared from) the process env, the svcs listing, and the
// absence of any synthesized default entry. Runs against the shared manager
// store; the created records are deleted on cleanup.
func TestManagerBuiltInApps(t *testing.T) {
	ManagerSvcMount("zcode", "/zcode")
	ManagerSvcMount("vscode", "/vscode")
	t.Cleanup(func() {
		mgrSvcMu.Lock()
		mgrSvcMounts = map[string]string{}
		mgrSvcMu.Unlock()
		os.Unsetenv("ZCODE_DATA_BASE_DIR")
		os.Unsetenv("ZCODE_DESKTOP_HOME_DIR")
		os.Unsetenv("KVS_SVC_ZCODE_WSC")
		ClearOverrideVersion("zcode")
		ClearOverrideVersion("vscode")
	})

	call := func(method, target, body string) *httptest.ResponseRecorder {
		var rd io.Reader
		if body != "" {
			rd = strings.NewReader(body)
		}
		req := httptest.NewRequest(method, target, rd)
		req.Header.Set("Content-Type", "application/json")
		rec := httptest.NewRecorder()
		managerHandler(rec, req)
		return rec
	}

	// Create the zcode built-in: env overrides + version pin applied immediately.
	if rec := call("POST", "/__manager", `{"svc":"zcode","name":"myzcode","data_dir":"/data/z1","wsc":"/wsc1","ver":"9.9.9"}`); rec.Code != 200 {
		t.Fatalf("POST zcode built-in -> %d: %s", rec.Code, rec.Body.String())
	}
	if v := os.Getenv("ZCODE_DATA_BASE_DIR"); v != "/data/z1" {
		t.Fatalf("ZCODE_DATA_BASE_DIR = %q, want /data/z1", v)
	}
	if v := os.Getenv("ZCODE_DESKTOP_HOME_DIR"); v != "/data/z1" {
		t.Fatalf("ZCODE_DESKTOP_HOME_DIR = %q, want /data/z1", v)
	}
	if v := os.Getenv("KVS_SVC_ZCODE_WSC"); v != "/wsc1" {
		t.Fatalf("KVS_SVC_ZCODE_WSC = %q, want /wsc1", v)
	}
	if v := GetOverrideVersion("zcode"); v != "9.9.9" {
		t.Fatalf("override version = %q, want 9.9.9", v)
	}
	t.Cleanup(func() { call("DELETE", "/__manager?id=svc-zcode", "") })

	// Editing with an empty version clears the pin ("" = latest).
	if rec := call("POST", "/__manager", `{"svc":"zcode","id":"svc-zcode","data_dir":"/data/z1","wsc":"/wsc1"}`); rec.Code != 200 {
		t.Fatalf("POST zcode edit -> %d: %s", rec.Code, rec.Body.String())
	}
	if v := GetOverrideVersion("zcode"); v != "" {
		t.Fatalf("override version after clear-edit = %q, want empty", v)
	}

	// One card per service; unknown services rejected.
	if rec := call("POST", "/__manager", `{"svc":"zcode"}`); rec.Code != 409 {
		t.Fatalf("duplicate built-in -> %d, want 409", rec.Code)
	}
	if rec := call("POST", "/__manager", `{"svc":"nope"}`); rec.Code != 400 {
		t.Fatalf("unknown svc -> %d, want 400", rec.Code)
	}

	// VS Code built-in with workspace folders (mount-relative base).
	if rec := call("POST", "/__manager", `{"svc":"vscode","folders":[{"name":"p1","path":"/wsc/p1"}]}`); rec.Code != 200 {
		t.Fatalf("POST vscode built-in -> %d: %s", rec.Code, rec.Body.String())
	}
	t.Cleanup(func() { call("DELETE", "/__manager?id=svc-vscode", "") })

	// List: built-ins rendered with their fields; no synthesized default.
	rec := call("GET", "/__manager", "")
	var list struct {
		Apps []mgrEntry `json:"apps"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("list unmarshal: %v", err)
	}
	var zcdE, vscE *mgrEntry
	for i := range list.Apps {
		e := &list.Apps[i]
		if e.ID == mgrIDZcdLocal {
			t.Fatalf("default zcd-local entry must not be synthesized")
		}
		switch e.Svc {
		case "zcode":
			zcdE = e
		case "vscode":
			vscE = e
		}
	}
	if zcdE == nil || zcdE.URL != "/zcode" || zcdE.DataDir != "/data/z1" || zcdE.WSC != "/wsc1" || zcdE.Ver != "" || !zcdE.Custom {
		t.Fatalf("zcode entry wrong: %+v", zcdE)
	}
	if vscE == nil || vscE.URL != "/vscode" || len(vscE.Folders) != 1 || vscE.Folders[0].Path != "/wsc/p1" {
		t.Fatalf("vscode entry wrong: %+v", vscE)
	}

	// svcs listing reflects the current env-derived defaults.
	rec = call("GET", "/__manager/svcs", "")
	var ls struct {
		Svcs []mgrSvcInfo `json:"svcs"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &ls); err != nil {
		t.Fatalf("svcs unmarshal: %v", err)
	}
	var zcS, vcS *mgrSvcInfo
	for i := range ls.Svcs {
		switch ls.Svcs[i].Svc {
		case "zcode":
			zcS = &ls.Svcs[i]
		case "vscode":
			vcS = &ls.Svcs[i]
		}
	}
	if zcS == nil || zcS.Type != "zcd" || zcS.URL != "/zcode" || zcS.DataDir != "/data/z1" || zcS.WSC != "/wsc1" {
		t.Fatalf("zcode svc info wrong: %+v", zcS)
	}
	if zcS.HasVersion {
		t.Fatalf("zcode svc info must not claim a version chain without registered services: %+v", zcS)
	}
	if vcS == nil || vcS.Type != "vsc" || vcS.URL != "/vscode" {
		t.Fatalf("vscode svc info wrong: %+v", vcS)
	}

	// Deleting the zcode built-in reverts the env to config defaults.
	if rec := call("DELETE", "/__manager?id=svc-zcode", ""); rec.Code != 200 {
		t.Fatalf("DELETE zcode built-in -> %d", rec.Code)
	}
	for _, k := range []string{"ZCODE_DATA_BASE_DIR", "ZCODE_DESKTOP_HOME_DIR", "KVS_SVC_ZCODE_WSC"} {
		if v := os.Getenv(k); v != "" {
			t.Fatalf("%s = %q after delete, want unset", k, v)
		}
	}
}

// TestClearServiceVersionEnv checks the version-env lifecycle: with a refill
// source (version_latest_url) only kvs's self-assigned env is cleared (the
// version placeholder would otherwise read it back and re-pin the old value)
// and an externally pinned value survives; WITHOUT a refill source the last
// known value is always kept (never emptied — bin_home would expand
// …/servers//bin).
func TestClearServiceVersionEnv(t *testing.T) {
	Services().Register(map[string]*ServiceConfig{
		"tver": {Name: "tver", Version: "1.2.3", VersionLatestURL: "http://127.0.0.1:1/latest"},
	})
	t.Cleanup(func() { Services().Register(map[string]*ServiceConfig{}) })

	// Self-assigned artifact (equals the resolved version) + refill source → cleared.
	t.Setenv("KVS_SVC_TVER_VERSION", "1.2.3")
	ClearServiceVersionEnv("tver")
	if v := os.Getenv("KVS_SVC_TVER_VERSION"); v != "" {
		t.Fatalf("self-assigned env must be cleared, got %q", v)
	}

	// External pin (differs from the resolved version) → kept.
	t.Setenv("KVS_SVC_TVER_VERSION", "9.9.9")
	ClearServiceVersionEnv("tver")
	if v := os.Getenv("KVS_SVC_TVER_VERSION"); v != "9.9.9" {
		t.Fatalf("external pin must survive, got %q", v)
	}

	// No refill source → last known value kept even when self-assigned.
	Services().Register(map[string]*ServiceConfig{
		"tnosrc": {Name: "tnosrc", Version: "2.0.0"},
	})
	t.Setenv("KVS_SVC_TNOSRC_VERSION", "2.0.0")
	ClearServiceVersionEnv("tnosrc")
	if v := os.Getenv("KVS_SVC_TNOSRC_VERSION"); v != "2.0.0" {
		t.Fatalf("last known version must be kept without a refill source, got %q", v)
	}
}

// TestParseBackendsVerTemplate checks <verhash>/<version> route-template
// detection: templates are svc://-only (the placeholders resolve from the
// target service); other schemes match the placeholder text literally and
// regex routes never template.
func TestParseBackendsVerTemplate(t *testing.T) {
	backends := parseBackends([][2]string{
		{"/stable-<verhash>/", "svc://vscode"},
		{"/v<version>/x", "svc://vscode"},
		{"/stable-<verhash>/web-extension-resource/", "api://whatever"},
		{"/stable-<verhash>/", "http://127.0.0.1:1"},
		{"^/stable-[a-z]+/", "svc://vscode"},
	})
	if len(backends) != 5 {
		t.Fatalf("got %d backends: %+v", len(backends), backends)
	}
	for i, want := range []bool{true, true, false, false, false} {
		if backends[i].VerTemplate != want {
			t.Fatalf("backends[%d].VerTemplate = %v, want %v: %+v", i, backends[i].VerTemplate, want, backends[i])
		}
	}
	if !backends[0].IsService || backends[2].IsService {
		t.Fatalf("service flag wrong: %+v / %+v", backends[0], backends[2])
	}
}

// TestParseBackendsPathMode checks the -/+ path mode markers: kept for the
// proxy-family schemes and file; text/relay/api ignore them with a warning
// (paths are meaningless to those targets).
func TestParseBackendsPathMode(t *testing.T) {
	backends := parseBackends([][2]string{
		{"/api/", "-http://127.0.0.1:1/x"},
		{"/app/", "+http://127.0.0.1:1"},
		{"/vscode", "-svc://vscode"},
		{"/sock/", "-unix:///run/app.sock"},
		{"/f/", "+file:///var/www"},
		{"/t/", "-text://hello"},
		{"/r/", "-relay://zcode"},
		{"/m/", "+api://manager"},
	})
	if len(backends) != 8 {
		t.Fatalf("got %d backends: %+v", len(backends), backends)
	}
	for i, want := range []string{"-", "+", "-", "-", "+", "", "", ""} {
		if backends[i].PathMode != want {
			t.Fatalf("backends[%d].PathMode = %q, want %q: %+v", i, backends[i].PathMode, want, backends[i])
		}
	}
	if backends[2].Target != "vscode" || !backends[2].IsService {
		t.Fatalf("svc target mangled by mode strip: %+v", backends[2])
	}
	if backends[2].ServiceName != "vscode" {
		t.Fatalf("ServiceName = %q, want vscode: %+v", backends[2].ServiceName, backends[2])
	}
	if backends[7].Target != "manager" {
		t.Fatalf("api target mangled by mode strip: %+v", backends[7])
	}
}

// TestInjectLogoutButtonSvc checks the injected logout-script tag carries the
// backend's hosted-service name as ?svc= (so the script targets its own
// service); a non-svc backend injects the plain script without a query.
func TestInjectLogoutButtonSvc(t *testing.T) {
	page := []byte(`<html><body data-config="vscode-workbench-web-configuration"></body></html>`)
	resp := func() *http.Response {
		return &http.Response{
			StatusCode: 200,
			Header:     http.Header{"Content-Type": []string{"text/html; charset=utf-8"}},
			Body:       io.NopCloser(bytes.NewReader(bytes.Clone(page))),
			Request:    httptest.NewRequest(http.MethodGet, "/", nil),
		}
	}
	r := resp()
	if err := injectLogoutButtonFor("vscode")(r); err != nil {
		t.Fatal(err)
	}
	b, _ := io.ReadAll(r.Body)
	if !bytes.Contains(b, []byte(`src="/__logout.svc.js?svc=vscode"`)) {
		t.Fatalf("script tag missing ?svc= query: %s", b)
	}
	r2 := resp()
	if err := injectLogoutButtonFor("")(r2); err != nil {
		t.Fatal(err)
	}
	b2, _ := io.ReadAll(r2.Body)
	if !bytes.Contains(b2, []byte(`src="/__logout.svc.js"></script>`)) || bytes.Contains(b2, []byte(`?svc=`)) {
		t.Fatalf("plain tag expected: %s", b2)
	}
}

// TestRewritePathModes checks rewriteProxyRequest against the synthetic unix
// target: "" forwards the full path, "-" strips the mount, "+" appends the
// full path after the (empty) target path — same result as "" there.
func TestRewritePathModes(t *testing.T) {
	rewrite := func(mode string) string {
		req := httptest.NewRequest(http.MethodGet, "/vscode/x/y?q=1", nil)
		rewriteProxyRequest(req, Backend{Source: "/vscode", PathMode: mode}, unixSyntheticTarget, nil)
		return req.URL.Path
	}
	for mode, want := range map[string]string{"": "/vscode/x/y", "-": "/x/y", "+": "/vscode/x/y"} {
		if got := rewrite(mode); got != want {
			t.Fatalf("mode %q: path = %q, want %q", mode, got, want)
		}
	}
	// "-" onto a target with its own base path.
	req := httptest.NewRequest(http.MethodGet, "/vscode/x/y", nil)
	rewriteProxyRequest(req, Backend{Source: "/vscode", PathMode: "-"}, &url.URL{Scheme: "http", Host: "h", Path: "/base"}, nil)
	if req.URL.Path != "/base/x/y" {
		t.Fatalf("replace mode: path = %q, want /base/x/y", req.URL.Path)
	}
}

// TestFileBackendPathMode checks the file backend's -/+ semantics: the
// default (like "-") strips the mount prefix before mapping into the dir,
// "+" keeps the mount prefix in the file layout.
func TestFileBackendPathMode(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "cdn"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "a.txt"), []byte("root"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "cdn", "a.txt"), []byte("kept"), 0o644); err != nil {
		t.Fatal(err)
	}
	get := func(h http.Handler, path string) *httptest.ResponseRecorder {
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, path, nil))
		return rec
	}
	strip := CreateBackendHandler(Backend{Source: "/cdn/", Scheme: "file", Target: dir}, nil, false)
	if rec := get(strip, "/cdn/a.txt"); rec.Code != 200 || rec.Body.String() != "root" {
		t.Fatalf("default strip: %d %q", rec.Code, rec.Body.String())
	}
	keep := CreateBackendHandler(Backend{Source: "/cdn/", Scheme: "file", Target: dir, PathMode: "+"}, nil, false)
	if rec := get(keep, "/cdn/a.txt"); rec.Code != 200 || rec.Body.String() != "kept" {
		t.Fatalf("keep mode: %d %q", rec.Code, rec.Body.String())
	}
}

// TestSplitSvcTarget checks svc:// target splitting: "name.ext" → service
// "name" + extension "ext" (last dot); plain names have no extension.
// TestManagerDeleteDeviceCard deletes a relay-device card that was never
// persisted as an app record: the stub path validates the id, and the device
// itself is forgotten (else the card would re-synthesize on the next render).
func TestManagerDeleteDeviceCard(t *testing.T) {
	sid := "testsid-deletecard"
	t.Cleanup(func() {
		zcodeState().remove(sid)
		zcodeState().appDelete("zcd-" + sid)
	})
	zcodeState().upsert(zcodeDeviceRecord{
		DeviceSid: sid,
		DeviceMid: "mid-" + sid,
		Meta:      zcodeMeta{"name": "测试设备"},
	})

	call := func(method, target string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, nil)
		rec := httptest.NewRecorder()
		managerHandler(rec, req)
		return rec
	}
	if rec := call("DELETE", "/__manager?id=zcd-"+sid); rec.Code != 200 {
		t.Fatalf("DELETE device card -> %d: %s", rec.Code, rec.Body.String())
	}
	if zcodeState().get(sid) != nil {
		t.Fatal("device record still present after card delete")
	}
	// Unknown ids still 404.
	if rec := call("DELETE", "/__manager?id=zcd-nope"); rec.Code != 404 {
		t.Fatalf("DELETE unknown device -> %d, want 404", rec.Code)
	}
}

// TestVscLocalAuthToggleAndDeepLink covers the 系统配置 local-auth checkbox:
// the deep-link issuer signs a tkn by default and omits it while the mode is
// on; the toggle persists through the store and an absent localAuth field
// keeps the current mode.
func TestVscLocalAuthToggleAndDeepLink(t *testing.T) {
	t.Cleanup(func() {
		zcodeState().saveVscLocalAuth(false)
		zcodeState().saveVscOpenExt("")
	})
	SetManagerLoginInfo(func() (string, string, int) { return "login-secret", "tkn", 3600 })
	defer SetManagerLoginInfo(nil)

	call := func(method, target string, body io.Reader) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, target, body)
		rec := httptest.NewRecorder()
		managerHandler(rec, req)
		return rec
	}
	post := func(payload string) *httptest.ResponseRecorder {
		return call("POST", "/__manager/vscext", strings.NewReader(payload))
	}

	// 默认（未勾选）：vscopen 签发 tkn。
	if rec := post(`{"ext":"suisrc.remote-server","localAuth":false}`); rec.Code != 200 {
		t.Fatalf("vscext save -> %d: %s", rec.Code, rec.Body.String())
	}
	rec := call("GET", "/__manager/vscopen", nil)
	if rec.Code != 200 {
		t.Fatalf("vscopen -> %d", rec.Code)
	}
	if link := rec.Body.String(); !strings.HasPrefix(link, "vscode://suisrc.remote-server/open?tkn=") {
		t.Fatalf("default mode should sign a tkn, got %q", link)
	}

	// 勾选本地认证：vscopen 不再携带 tkn，存储位翻转。
	if rec := post(`{"ext":"suisrc.remote-server","localAuth":true}`); rec.Code != 200 {
		t.Fatalf("vscext save(localAuth) -> %d: %s", rec.Code, rec.Body.String())
	}
	if link := call("GET", "/__manager/vscopen", nil).Body.String(); link != "vscode://suisrc.remote-server/open" {
		t.Fatalf("local-auth mode must omit tkn, got %q", link)
	}
	if !zcodeState().vscLocalAuthValue() {
		t.Fatal("local auth flag not persisted in the store")
	}

	// 缺省请求（不带 localAuth 字段）保持现状不变。
	if rec := post(`{"ext":""}`); rec.Code != 200 {
		t.Fatalf("vscext save(ext only) -> %d: %s", rec.Code, rec.Body.String())
	}
	if !zcodeState().vscLocalAuthValue() {
		t.Fatal("absent localAuth field must keep the current mode")
	}
}

// TestVscLocalAuthStoreRoundTrip verifies the toggle survives the state file.
func TestVscLocalAuthStoreRoundTrip(t *testing.T) {
	file := filepath.Join(t.TempDir(), "zcodex.json")
	s := zcodeLoadStore(file)
	s.saveVscLocalAuth(true)
	s.flush()
	if got := zcodeLoadStore(file).vscLocalAuthValue(); !got {
		t.Fatal("vsc_local_auth did not survive the store file round-trip")
	}
}

func TestSplitSvcTarget(t *testing.T) {
	if svc, ext, ok := SplitSvcTarget("vscode.ext"); !ok || svc != "vscode" || ext != "ext" {
		t.Fatalf("SplitSvcTarget(vscode.ext) = %q %q %v", svc, ext, ok)
	}
	if svc, ext, ok := SplitSvcTarget("a.b.ext"); !ok || svc != "a.b" || ext != "ext" {
		t.Fatalf("SplitSvcTarget(a.b.ext) = %q %q %v", svc, ext, ok)
	}
	if _, ext, ok := SplitSvcTarget("zcode"); ok || ext != "" {
		t.Fatalf("plain target must have no extension")
	}
}

// TestSvcExtensionDispatch checks the svc://<name>.<ext> route: the extension
// handler registered on the service is dispatched with the service config
// resolved for the current request.
func TestSvcExtensionDispatch(t *testing.T) {
	RegisterSvcExt("tsvc", "ext", func(w http.ResponseWriter, r *http.Request, sc *ServiceConfig, mount string) {
		_, _ = w.Write([]byte("svc=" + sc.Name + " home=" + sc.Home + " mount=" + mount))
	})
	t.Cleanup(func() { delete(svcExtRegistry, "tsvc") })

	Services().Register(map[string]*ServiceConfig{
		"tsvc": {Name: "tsvc", Address: "http://127.0.0.1:1", Home: "/tmp/tsvc-home",
			Extensions: svcExtensions("tsvc")},
	})
	t.Cleanup(func() { Services().Register(map[string]*ServiceConfig{}) })

	h := CreateBackendHandler(Backend{Source: "/x", Scheme: "svc", Target: "tsvc.ext", RawURL: "svc://tsvc.ext"}, nil, false)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/x/stable-abc/web-extension-resource/a.b.c/1.0/p", nil))
	if rec.Code != 200 || rec.Body.String() != "svc=tsvc home=/tmp/tsvc-home mount=/x" {
		t.Fatalf("extension dispatch -> %d %q", rec.Code, rec.Body.String())
	}
}

// TestExpandVerTemplate checks the per-request expansion of
// <verhash>/<version> from the service config and the skip-until-resolved
// contract (unknown service or missing value → ok=false).
func TestExpandVerTemplate(t *testing.T) {
	sc := &ServiceConfig{Version: "1.2.3", VersionHash: "abc"}
	if p, ok := ExpandVerTemplate("/stable-<verhash>/", sc); !ok || p != "/stable-abc/" {
		t.Fatalf("verhash expand = %q ok=%v", p, ok)
	}
	if p, ok := ExpandVerTemplate("/v<version>/", sc); !ok || p != "/v1.2.3/" {
		t.Fatalf("version expand = %q ok=%v", p, ok)
	}
	if p, ok := ExpandVerTemplate("/<version>-<verhash>/", sc); !ok || p != "/1.2.3-abc/" {
		t.Fatalf("combined expand = %q ok=%v", p, ok)
	}
	if _, ok := ExpandVerTemplate("/stable-<verhash>/", &ServiceConfig{Version: "1.2.3"}); ok {
		t.Fatalf("unresolved hash must skip the route")
	}
	if _, ok := ExpandVerTemplate("/v<version>/", &ServiceConfig{}); ok {
		t.Fatalf("unresolved version must skip the route")
	}
	if _, ok := ExpandVerTemplate("/stable-<verhash>/", nil); ok {
		t.Fatalf("unknown service must skip the route")
	}
}
