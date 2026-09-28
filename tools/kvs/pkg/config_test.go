package pkg

import (
	"encoding/json"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestParseIniMultilineJSON checks that a hand-written multi-line JSON value
// (comment lines interleaved, missing commas between members) parses and is
// repaired into valid JSON — the [service.vsc] actions block style.
func TestParseIniMultilineJSON(t *testing.T) {
	data := []byte(`
[service.vsc]
actions = {
    # a comment line inside the block
    "patch_cmd": "SERVER_DIR={KVS_SVC_VSCODE_BIN_HOME} patch-vscode"
    # another comment
    "patch_rev": "SERVER_DIR={KVS_SVC_VSCODE_BIN_HOME} patch-vscode --revert"
}
vsc_language = {"zh-cn":"zh-hans", "zh-tw":"zh-hant"}
vsc_agent_cmds = { \
    "vscode": "DISPLAY=:1 code --user-data-dir {KVS_SVC_VSCODE_HOME}/data", \
    "agentv": "{KVS_SVC_VSCODE_BIN_HOME}/node start" \
}
`)
	ini, err := parseIniData(data, "test.ini")
	if err != nil {
		t.Fatalf("parseIniData: %v", err)
	}
	actions := ini.get("service.vsc.actions")
	if actions == "" {
		t.Fatalf("actions value missing")
	}
	var am map[string]string
	if err := json.Unmarshal([]byte(actions), &am); err != nil {
		t.Fatalf("actions is not valid JSON after repair: %v\n%s", err, actions)
	}
	if len(am) != 2 || am["patch_cmd"] == "" || am["patch_rev"] == "" {
		t.Fatalf("unexpected actions content: %#v", am)
	}
	if v := ini.get("service.vsc.vsc_language"); v != `{"zh-cn":"zh-hans", "zh-tw":"zh-hant"}` {
		t.Fatalf("vsc_language mangled: %q", v)
	}
	if v := ini.get("service.vsc.vsc_agent_cmds"); !strings.Contains(v, `"agentv"`) {
		t.Fatalf("vsc_agent_cmds mangled: %q", v)
	}
}

// TestRepairJSONCommas unit-tests the missing-comma repair directly.
func TestRepairJSONCommas(t *testing.T) {
	in := "{\"a\": \"x\"\n\"b\": \"y\"}"
	out := repairJSONCommas(in)
	var m map[string]string
	if err := json.Unmarshal([]byte(out), &m); err != nil {
		t.Fatalf("repair produced invalid JSON: %v (%s)", err, out)
	}
	if m["a"] != "x" || m["b"] != "y" {
		t.Fatalf("repair corrupted content: %#v", m)
	}
	// Already-valid input must stay untouched (modulo whitespace).
	valid := "{\"a\": \"x\",\n\"b\": \"y\"}"
	if out := repairJSONCommas(valid); !json.Valid([]byte(out)) {
		t.Fatalf("valid JSON broken by repair: %s", out)
	}
}

// TestProxyTablesAndServices parses the embedded kvs.ini and checks the named
// route tables and service sections are discovered in file order.
func TestProxyTablesAndServices(t *testing.T) {
	ini, err := parseIniData(MustAsset("kvs.ini"), "kvs.ini")
	if err != nil {
		t.Fatalf("parse embedded kvs.ini: %v", err)
	}
	tables := ini.proxyTables()
	want := []string{"zcodex", "default", "zcode", "vscode", "dsh"}
	if len(tables) != len(want) {
		t.Fatalf("proxyTables = %v, want %v", tables, want)
	}
	for i, name := range want {
		if tables[i] != name {
			t.Fatalf("proxyTables[%d] = %q, want %q", i, tables[i], name)
		}
	}
	vscEntries := ini.tableEntries("vscode")
	if len(vscEntries) == 0 {
		t.Fatalf("table [proxies.vscode] not found")
	}
	found := false
	for _, e := range vscEntries {
		if e[0] == "/" && e[1] == "svc://vsc" {
			found = true
		}
	}
	if !found {
		t.Fatalf("[proxies.vscode] missing /=svc://vsc entry: %v", vscEntries)
	}
	var svcNames []string
	for _, kv := range ini.serviceSections() {
		svcNames = append(svcNames, kv[0])
	}
	if strings.Join(svcNames, ",") != "def,zcode,vsc,dsh" {
		t.Fatalf("serviceSections = %v", svcNames)
	}
}

// TestParseBackendsSvc checks that svc:// targets are recognized as hosted
// service backends.
func TestParseBackendsSvc(t *testing.T) {
	backends := parseBackends([][2]string{
		{"/__healthz", "text://OK:@now"},
		{"/zcode", "svc://zcode"},
		{"/assets/", "file:///var/www"},
	})
	if len(backends) != 3 {
		t.Fatalf("got %d backends: %+v", len(backends), backends)
	}
	if backends[1].Scheme != "svc" || backends[1].Target != "zcode" || !backends[1].IsService {
		t.Fatalf("svc:// backend parsed wrong: %+v", backends[1])
	}
	if backends[0].IsService || backends[2].IsService {
		t.Fatalf("non-svc backends must not be marked as service: %+v / %+v", backends[0], backends[2])
	}
}

// TestResolveServiceEnvReverseAssignment checks the KVS_SVC_<NAME>_* env
// reverse-assignment order: HOME default → ADDR/HOST/PORT/SOCK → VERSION.
func TestResolveServiceEnvReverseAssignment(t *testing.T) {
	t.Setenv("KVS_SVC_TDEF_ADDR", "")
	t.Setenv("KVS_SVC_TDEF_HOME", "")

	c := &Config{Home: t.TempDir()}
	sc := &ServiceConfig{
		Name:    "tdef",
		Address: "{KVS_SVC_TDEF_ADDR:-http://127.0.0.1:7599}",
		Healthz: "{KVS_SVC_TDEF_ADDR}/healthz#@200",
	}
	c.resolveService(sc)

	if sc.Address != "http://127.0.0.1:7599" {
		t.Fatalf("address = %q", sc.Address)
	}
	if sc.Healthz != "http://127.0.0.1:7599/healthz#@200" {
		t.Fatalf("healthz = %q", sc.Healthz)
	}
	if got := os.Getenv("KVS_SVC_TDEF_ADDR"); got != sc.Address {
		t.Fatalf("KVS_SVC_TDEF_ADDR = %q, want %q", got, sc.Address)
	}
	if got := os.Getenv("KVS_SVC_TDEF_HOST"); got != "127.0.0.1" {
		t.Fatalf("KVS_SVC_TDEF_HOST = %q", got)
	}
	if got := os.Getenv("KVS_SVC_TDEF_PORT"); got != "7599" {
		t.Fatalf("KVS_SVC_TDEF_PORT = %q", got)
	}
	if got := os.Getenv("KVS_SVC_TDEF_HOME"); got != filepath.Join(c.Home, "tdef") {
		t.Fatalf("KVS_SVC_TDEF_HOME = %q, want %q", got, filepath.Join(c.Home, "tdef"))
	}
	if sc.Home != filepath.Join(c.Home, "tdef") {
		t.Fatalf("home = %q", sc.Home)
	}

	// unix address → SOCK reverse-assignment.
	t.Setenv("KVS_SVC_TUNIX_ADDR", "")
	sc2 := &ServiceConfig{Name: "tunix", Address: "unix://{KVS_SVC_TUNIX_HOME}/kvs.sock", Home: "/tmp/tunix-home"}
	c.resolveService(sc2)
	if got := os.Getenv("KVS_SVC_TUNIX_SOCK"); got != "/tmp/tunix-home/kvs.sock" {
		t.Fatalf("KVS_SVC_TUNIX_SOCK = %q", got)
	}
	if sc2.BinHome != sc2.Home {
		t.Fatalf("empty bin_home should default to home: %q", sc2.BinHome)
	}
	if got := os.Getenv("KVS_SVC_TUNIX_BIN_HOME"); got != sc2.Home {
		t.Fatalf("KVS_SVC_TUNIX_BIN_HOME = %q", got)
	}

	// relative unix address → SOCK must survive verbatim (url.Parse would eat
	// the leading ".." as a host and produce "/../…").
	sc3 := &ServiceConfig{Name: "tunixrel", Address: "unix://{KVS_SVC_TUNIXREL_HOME}/kvs.sock", Home: "../../temp/rel-home"}
	c.resolveService(sc3)
	if got := os.Getenv("KVS_SVC_TUNIXREL_SOCK"); got != "../../temp/rel-home/kvs.sock" {
		t.Fatalf("KVS_SVC_TUNIXREL_SOCK = %q, want ../../temp/rel-home/kvs.sock", got)
	}
}

// TestResolveServiceEnvsDefault checks the envs_default semantics: system env
// vars that are unset/empty get the default (expanded with the service's
// reverse-assigned variables); already-set values are never overridden; empty
// defaults are skipped.
func TestResolveServiceEnvsDefault(t *testing.T) {
	t.Setenv("KVS_TEST_ED_EMPTY", "")
	t.Setenv("KVS_TEST_ED_SET", "user-value")

	c := &Config{Home: t.TempDir()}
	sc := &ServiceConfig{
		Name: "ted",
		EnvsDefaultRaw: `{
			"KVS_TEST_ED_EMPTY": "{KVS_SVC_TED_HOME}/data",
			"KVS_TEST_ED_SET": "default-ignored",
			"KVS_TEST_ED_BLANK": ""
		}`,
	}
	c.resolveService(sc)

	if got := os.Getenv("KVS_TEST_ED_EMPTY"); got != sc.Home+"/data" {
		t.Fatalf("empty var should get the expanded default: %q", got)
	}
	if got := os.Getenv("KVS_TEST_ED_SET"); got != "user-value" {
		t.Fatalf("set var must not be overridden: %q", got)
	}
	if got := os.Getenv("KVS_TEST_ED_BLANK"); got != "" {
		t.Fatalf("empty default must be skipped: %q", got)
	}
}

// TestResolveServiceDownloadTokens checks the bare VERSION/VERHASH token
// substitution in download URLs (applied after version resolution; VERHASH
// defaults to VERSION when version_hash_url is unset).
func TestResolveServiceDownloadTokens(t *testing.T) {
	c := &Config{Home: t.TempDir()}
	sc := &ServiceConfig{
		Name:     "tdl",
		Version:  "1.5.0",
		Download: "https://example.com/commit:VERHASH/server/VERSION/stable",
	}
	c.resolveService(sc)
	want := "https://example.com/commit:1.5.0/server/1.5.0/stable"
	if sc.Download != want {
		t.Fatalf("download = %q, want %q", sc.Download, want)
	}
}

// TestResolveVersionHashURLSubstitution checks that version_hash_url gets the
// VERSION token substituted (bare and {VERSION} brace forms) before it is
// fetched.
func TestResolveVersionHashURLSubstitution(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/api/versions/1.139.1/stable" {
			http.Error(w, "unexpected path: "+r.URL.Path, http.StatusBadRequest)
			return
		}
		_, _ = w.Write([]byte(`{"version": "abc123hash"}`))
	}))
	defer srv.Close()

	for _, tpl := range []string{
		srv.URL + "/api/versions/VERSION/stable#version",
		srv.URL + "/api/versions/{VERSION}/stable#version",
	} {
		v, vh, err := resolveVersion("tsub", "1.139.1", "", tpl)
		if err != nil || v != "1.139.1" || vh != "abc123hash" {
			t.Fatalf("hashURL %s: v=%q vh=%q err=%v", tpl, v, vh, err)
		}
	}
}

// TestIsServiceAlive covers the healthz "#@code" suffix semantics.
func TestIsServiceAlive(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusNotFound)
	}))
	defer srv.Close()

	if !isServiceAlive(srv.URL) {
		t.Fatalf("reachable-but-404 must pass without a code requirement")
	}
	if !isServiceAlive(srv.URL + "#@404") {
		t.Fatalf("#@404 must pass on a 404 response")
	}
	if isServiceAlive(srv.URL + "#@200") {
		t.Fatalf("#@200 must fail on a 404 response")
	}
	if isServiceAlive("http://127.0.0.1:1/nope") {
		t.Fatalf("unreachable http must fail")
	}
	if isServiceAlive("unix:///tmp/definitely-missing-kvs-test.sock") {
		t.Fatalf("missing unix socket must fail")
	}

	// A live unix socket must probe alive even with the "#@<code>" suffix and
	// trailing "/" that the healthz template ("{ADDR}/#@200") produces.
	sockPath := filepath.Join(t.TempDir(), "live.sock")
	ln, err := net.Listen("unix", sockPath)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	defer ln.Close()
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			conn.Close()
		}
	}()
	if !isServiceAlive("unix://" + sockPath + "/#@200") {
		t.Fatalf("live unix socket with #@200 suffix must probe alive")
	}
}

// TestEnvSuffix checks service name → env fragment mapping.
func TestEnvSuffix(t *testing.T) {
	cases := map[string]string{
		"def": "DEF", "zcode": "ZCODE", "vsc": "VSC",
		"my-svc": "MY_SVC", "a.b": "A_B",
	}
	for in, want := range cases {
		if got := envSuffix(in); got != want {
			t.Fatalf("envSuffix(%q) = %q, want %q", in, got, want)
		}
	}
}
