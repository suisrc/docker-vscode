package pkg

import (
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func TestSplitQueryGate(t *testing.T) {
	cases := []struct {
		in   string
		base string
		key  string
		val  string
	}{
		{"/remote/vscode", "/remote/vscode", "", ""},
		// 标准形态：前缀收尾的 [k=v] 提取为门禁（单条件）。
		{"/remote/vscode[akey=vscode-remote]", "/remote/vscode", "akey", "vscode-remote"},
		{"/x[ akey = vscode-remote ]", "/x", "akey", "vscode-remote"},
		// 值里可以带 =（按第一个 = 切分）。
		{"/x[akey=a=b]", "/x", "akey", "a=b"},
		// 贪婪配对：最后一个 [ 与收尾 ] 之间是门禁。
		{"/a[b[c=d]", "/a[b", "c", "d"},
		// 多条件/含逗号：只支持单个 k=v，告警后按字面路径处理。
		{"/x[a=1,b=2]", "/x[a=1,b=2]", "", ""},
		{"/x[a=1,b]", "/x[a=1,b]", "", ""},
		// 无 = 或空键（正则字符类、裸键、空括号）→ 安静地按字面路径处理。
		{"^/x[0-9]", "^/x[0-9]", "", ""},
		{"/x[a]", "/x[a]", "", ""},
		{"/x[]", "/x[]", "", ""},
		// 未收尾 / 无左括号 → 字面路径。
		{"/x[a=1", "/x[a=1", "", ""},
		{"/x]", "/x]", "", ""},
	}
	for _, c := range cases {
		base, key, val := splitQueryGate(c.in)
		if base != c.base || key != c.key || val != c.val {
			t.Errorf("%q: = (%q, %q, %q), want (%q, %q, %q)", c.in, base, key, val, c.base, c.key, c.val)
		}
	}
}

func TestQueryGateMatch(t *testing.T) {
	// 复刻 main 分发循环里的门禁判断（gateKey 为空短路在前，Query() 不白付）。
	ok := func(gateKey, gateVal, rawQuery string) bool {
		if gateKey == "" {
			return true
		}
		q, err := url.ParseQuery(rawQuery)
		if err != nil {
			t.Fatalf("ParseQuery(%q): %v", rawQuery, err)
		}
		return q.Get(gateKey) == gateVal
	}
	if !ok("akey", "vscode-remote", "akey=vscode-remote&extra=x") {
		t.Error("条件满足且带额外参数：应命中")
	}
	if !ok("akey", "vscode-remote", "akey=vscode%2Dremote") {
		t.Error("URL 编码解码后比较：应命中")
	}
	for _, bad := range []string{
		"",           // 参数缺失
		"akey=other", // 值不等
		"akey=",      // 空值不等
	} {
		if ok("akey", "vscode-remote", bad) {
			t.Errorf("%q: 应不命中", bad)
		}
	}
	if !ok("", "x", "anything") {
		t.Error("无门禁恒命中")
	}
}

func TestParseBackendsQueryGate(t *testing.T) {
	bs := parseBackends([][2]string{
		{"cc~/remote/vscode[akey=vscode-remote]", "https://vscode.dev/"},
		{"ws~/p[q=1]", "relay://vscode"},
		{"/a/,/b[akey=v]", "text://X"},
		{"^/x[0-9]", "http://u:1"},
	})
	if len(bs) != 5 {
		t.Fatalf("backends = %d, want 5", len(bs))
	}
	if !bs[0].IsCache || bs[0].Source != "/remote/vscode" || bs[0].QueryKey != "akey" || bs[0].QueryVal != "vscode-remote" {
		t.Errorf("cc~ 门禁路由解析错误: %+v", bs[0])
	}
	if !bs[1].IsWSock || bs[1].QueryKey != "q" || bs[1].QueryVal != "1" {
		t.Errorf("ws~ 门禁路由解析错误: %+v", bs[1])
	}
	// 逗号展开的多个前缀共享同一门禁。
	if bs[2].Source != "/a/" || bs[3].Source != "/b" || bs[2].QueryKey != "akey" || bs[3].QueryKey != "akey" {
		t.Errorf("逗号展开共享门禁错误: %+v / %+v", bs[2], bs[3])
	}
	// 正则字符类不被误当门禁，pattern 原样保留。
	if !bs[4].IsRegex || bs[4].Source != "/x[0-9]" || bs[4].QueryKey != "" {
		t.Errorf("正则字符类被误解析为门禁: %+v", bs[4])
	}
}

// TestCutKVOutsideBrackets 门禁里的 "=" 不能破坏 key=value 切分：
// kvs.ini 行解析与 -n 内联解析共用这条规则（括号外首个 "="）。
func TestCutKVOutsideBrackets(t *testing.T) {
	k, v, ok := cutKVOutsideBrackets("cc~/remote/vscode[akey=vscode-remote]=https://vscode.dev/")
	if !ok || k != "cc~/remote/vscode[akey=vscode-remote]" || v != "https://vscode.dev/" {
		t.Errorf("cutKVOutsideBrackets = %q, %q, %v", k, v, ok)
	}
	// 值侧的 = 不参与（在首个切分点之后）。
	if _, v, _ := cutKVOutsideBrackets("/p=http://u/?a=b"); v != "http://u/?a=b" {
		t.Errorf("value side = %q", v)
	}
	if _, _, ok := cutKVOutsideBrackets("/no-equals"); ok {
		t.Error("无 = 应返回 ok=false")
	}
	// 兜底：未闭合 "[" 吞掉 "=" 时退回旧的首个 = 切分，不让配置加载失败。
	if k, v, ok := cutKVOutsideBrackets("/x[old=http://u"); !ok || k != "/x[old" || v != "http://u" {
		t.Errorf("unbalanced fallback = %q, %q, %v", k, v, ok)
	}
}

// TestIniAndInlineGatedRoutes 端到端解析：门禁行经 ini 表与 -n 内联两条入口
// 都要原样进入 parseBackends（parseBackends 自行剥 "proxies." 前缀）。
func TestIniAndInlineGatedRoutes(t *testing.T) {
	line := "cc~/remote/vscode[akey=vscode-remote]=https://vscode.dev/[v=vscode-version]"
	ini, err := parseIniData([]byte("[proxies]\n"+line+"\n"), "test.ini")
	if err != nil {
		t.Fatalf("parseIniData: %v", err)
	}
	entries := ini.tableEntries("")
	if len(entries) != 1 || entries[0][0] != "cc~/remote/vscode[akey=vscode-remote]" || entries[0][1] != "https://vscode.dev/[v=vscode-version]" {
		t.Fatalf("ini entries = %v", entries)
	}
	inline := parseProxiesArg(line)
	if len(inline) != 1 || inline[0][0] != "proxies."+entries[0][0] {
		t.Fatalf("parseProxiesArg = %v", inline)
	}
	for _, pair := range [][2]string{entries[0], inline[0]} {
		bs := parseBackends([][2]string{pair})
		if len(bs) != 1 || bs[0].Source != "/remote/vscode" || bs[0].QueryKey != "akey" || bs[0].QueryVal != "vscode-remote" {
			t.Errorf("route %q → %+v", pair[0], bs)
		}
	}
}

// TestGatedRouteDispatch 复刻 main 的分发循环（前缀命中 → 门禁校验 → 否则下一条），
// 验证门禁失败会落回同前缀的无门禁兜底路由。
func TestGatedRouteDispatch(t *testing.T) {
	backends := parseBackends([][2]string{
		{"/vx[akey=vscode-remote]", "text://GATED"},
		{"/vx", "text://FALLBACK"},
	})
	if len(backends) != 2 {
		t.Fatalf("backends = %d, want 2", len(backends))
	}
	serve := func(rawQuery string) string {
		req := httptest.NewRequest("GET", "/vx?"+rawQuery, nil)
		rec := httptest.NewRecorder()
		for _, b := range backends {
			if !strings.HasPrefix(req.URL.Path, b.Source) {
				continue
			}
			// 与 main 分发循环一致：gateKey 为空短路在前。
			if b.QueryKey != "" && req.URL.Query().Get(b.QueryKey) != b.QueryVal {
				continue
			}
			CreateBackendHandler(b, nil, false).ServeHTTP(rec, req)
			return rec.Body.String()
		}
		return "NOMATCH"
	}
	cases := map[string]string{
		"akey=vscode-remote":         "GATED",
		"akey=vscode-remote&other=1": "GATED", // 额外参数不影响
		"akey=vscode%2Dremote":       "GATED", // 编码解码后比较
		"":                           "FALLBACK",
		"akey=other":                 "FALLBACK",
	}
	for q, want := range cases {
		if got := serve(q); got != want {
			t.Errorf("/vx?%s = %q, want %q", q, got, want)
		}
	}
}
