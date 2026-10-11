package pkg

import (
	"context"
	"net/http"
	"strings"
	"sync"
	"testing"
)

// TestExtractExt 扩展名按已知归档后缀匹配：带版本点的基名
// （dsh-0.2.0-rc.2.tgz）必须得到 tgz，而不是第一个点之后的 2.0-rc.2.tgz
// ——那会把下载缓存名拼成 {version}_{hash}.2.0-rc.2.tgz。
func TestExtractExt(t *testing.T) {
	cases := map[string]string{
		"https://registry.npmmirror.com/@deepseek-ai/dsh/-/dsh-0.2.0-rc.2.tgz": "tgz",
		"https://x/y/pkg-1.2.3.zip":                  "zip",
		"https://x/y/vscode-server-linux-x64.tar.gz": "tar.gz",
		"https://x/y/a.tar.gz?token=1#frag":          "tar.gz",
		"https://x/y/pkg.TGZ":                        "tgz",
		"https://x/y/pkg-1.0.0.tar.bz2":              "tar.bz2",
		"https://x/y/blob":                           "tar.gz", // 无后缀 → 默认
		"https://x/y/pkg-1.2.3.exe":                  "tar.gz", // 未知后缀 → 默认
	}
	for in, want := range cases {
		if got := extractExt(in); got != want {
			t.Errorf("extractExt(%q) = %q, want %q", in, got, want)
		}
	}
}

// TestSplitUpstreamTokenSpec [key] 标记解析：key 必须是标识符、pattern 非空，
// 非法整体返回空（调用方告警停用）。
func TestSplitUpstreamTokenSpec(t *testing.T) {
	cases := []struct {
		in      string
		pattern string
		key     string
	}{
		{"dsh web: http://127.0.0.1:7531/?token=[token]", "dsh web: http://127.0.0.1:7531/?token=", "token"},
		{"ready on &[tk_1]", "ready on &", "tk_1"}, // pattern 含 & 不歧义
		{"no-marker", "", ""},
		{"empty[]", "", ""},
		{"[key]", "", ""},         // 空 pattern 会匹配所有行，拒绝
		{"p[bad-key]", "", ""},    // key 含非法字符
		{"p[a=1&x=[tk]]", "", ""}, // 内嵌 ] 使 key 非法
	}
	for _, c := range cases {
		pattern, key := splitUpstreamTokenSpec(c.in)
		if pattern != c.pattern || key != c.key {
			t.Errorf("splitUpstreamTokenSpec(%q) = (%q, %q), want (%q, %q)", c.in, pattern, key, c.pattern, c.key)
		}
	}
}

// TestSetRawUpstreamKeys 三个 upstream_* ini 键进入 ServiceConfig（含
// upstream_headers 拼写别名）。
func TestSetRawUpstreamKeys(t *testing.T) {
	sc := &ServiceConfig{Name: "u"}
	sc.setRaw("upstream_token", `dsh web: http://a:1/?token=[token]`)
	sc.setRaw("upstream_headrs", `{"Referer":"http://up"}`)
	sc.setRaw("upstream_headers", `{"X-Alias":"1"}`)
	sc.setRaw("upstream_cookies", `{"sid":""}`)
	if sc.UpstreamTokenSpec != `dsh web: http://a:1/?token=[token]` {
		t.Errorf("UpstreamTokenSpec = %q", sc.UpstreamTokenSpec)
	}
	if sc.UpstreamHeadersRaw != `{"X-Alias":"1"}` { // 后写的别名覆盖先写的
		t.Errorf("UpstreamHeadersRaw = %q", sc.UpstreamHeadersRaw)
	}
	if sc.UpstreamCookiesRaw != `{"sid":""}` {
		t.Errorf("UpstreamCookiesRaw = %q", sc.UpstreamCookiesRaw)
	}
}

// recordTransport 记录收到的请求并固定返回同一状态码。
type recordTransport struct {
	mu     sync.Mutex
	reqs   []*http.Request
	status int
}

func (rt *recordTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	rt.mu.Lock()
	rt.reqs = append(rt.reqs, r.Clone(context.Background()))
	rt.mu.Unlock()
	return &http.Response{StatusCode: rt.status, Header: make(http.Header), Body: http.NoBody, Request: r}, nil
}

// TestSvcUpstreamTransport 头/Cookie 注入（含 Host 特判、"" 删除、异名 cookie
// 透传、{ENV} 引用同步 token）与 401 带 token 的单次重试（仅无 body 方法）。
func TestSvcUpstreamTransport(t *testing.T) {
	sc := &ServiceConfig{Name: "UP1"}
	sc.UpstreamTokenPattern, sc.UpstreamTokenKey = "prefix ", "token"
	sc.UpstreamHeaders = map[string]string{
		"Host":    "up:99", // Host 特判：必须走 req.Host 字段
		"Referer": "http://up/",
		"X-Del":   "",
		"X-Tok":   "{KVS_SVC_UP1_UPSTREAM_TOKEN}", // token 换代后引用同步
	}
	sc.UpstreamCookies = map[string]string{"gone": "", "fresh": "F"}
	sc.SetUpstreamToken("T1")

	rt := &recordTransport{status: http.StatusUnauthorized}
	tr := &svcUpstreamTransport{base: rt, sc: sc}

	req, _ := http.NewRequest("GET", "http://up/x?a=1", nil)
	req.Header.Set("X-Del", "old")
	req.Header.Add("Cookie", "gone=old; keep=1")
	resp, err := tr.RoundTrip(req)
	if err != nil || resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("RoundTrip = %v, %v", resp, err)
	}
	if len(rt.reqs) != 2 {
		t.Fatalf("requests = %d, want 2 (original + tokened retry)", len(rt.reqs))
	}
	first := rt.reqs[0]
	if first.Host != "up:99" {
		t.Errorf("Host = %q, want up:99（host 头走 req.Host 特判）", first.Host)
	}
	if got := first.Header.Get("Referer"); got != "http://up/" {
		t.Errorf("Referer = %q", got)
	}
	if got := first.Header.Get("X-Del"); got != "" {
		t.Errorf("X-Del 应被删除，got %q", got)
	}
	if got := first.Header.Get("X-Tok"); got != "T1" {
		t.Errorf("X-Tok = %q, want T1（env 展开同步）", got)
	}
	if got := first.Header.Get("Cookie"); got != "keep=1; fresh=F" {
		t.Errorf("Cookie = %q, want 同名删除+异名透传+追加", got)
	}
	second := rt.reqs[1]
	q := second.URL.Query()
	if q.Get("token") != "T1" || q.Get("a") != "1" {
		t.Errorf("retry query = %v, want token=T1 且保留 a=1", q)
	}

	// POST（有 body）不重放。
	rt.reqs = nil
	preq, _ := http.NewRequest("POST", "http://up/x", strings.NewReader("body"))
	if _, err := tr.RoundTrip(preq); err != nil {
		t.Fatal(err)
	}
	if len(rt.reqs) != 1 {
		t.Fatalf("POST requests = %d, want 1 (no replay)", len(rt.reqs))
	}

	// token 为空不重试。
	sc.SetUpstreamToken("")
	rt.reqs = nil
	greq, _ := http.NewRequest("GET", "http://up/x", nil)
	if _, err := tr.RoundTrip(greq); err != nil {
		t.Fatal(err)
	}
	if len(rt.reqs) != 1 {
		t.Fatalf("no-token requests = %d, want 1", len(rt.reqs))
	}
}
