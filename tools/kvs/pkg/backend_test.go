package pkg

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
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

// TestClearServiceVersionEnv checks that only kvs's self-assigned version env
// is cleared (the version placeholder would otherwise read it back and re-pin
// the old value); an externally pinned value survives.
func TestClearServiceVersionEnv(t *testing.T) {
	Services().Register(map[string]*ServiceConfig{
		"tver": {Name: "tver", Version: "1.2.3"},
	})
	t.Cleanup(func() { Services().Register(map[string]*ServiceConfig{}) })

	// Self-assigned artifact (equals the resolved version) → cleared.
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

// TestSplitSvcTarget checks svc:// target splitting: "name.ext" → service
// "name" + extension "ext" (last dot); plain names have no extension.
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
