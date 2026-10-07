package pkg

// relay.go 的共享小工具测试。

import (
	"net/http"
	"testing"
)

func TestClientRemoteAddr(t *testing.T) {
	mk := func(remote, xff string) *http.Request {
		req, _ := http.NewRequest(http.MethodGet, "http://gw/", nil)
		req.RemoteAddr = remote
		if xff != "" {
			req.Header.Set("X-Forwarded-For", xff)
		}
		return req
	}
	cases := []struct {
		name, remote, xff, want string
	}{
		{"直连取 TCP 对端并去端口", "1.2.3.4:5678", "", "1.2.3.4"},
		{"XFF 首值优先（最外层代理写入）", "1.2.3.4:5678", "5.6.7.8, 10.0.0.1", "5.6.7.8"},
		{"对端 IPv4-mapped IPv6 还原", "[::ffff:1.2.3.4]:5678", "", "1.2.3.4"},
		{"XFF 值本身 mapped 也还原", "1.2.3.4:5678", "::ffff:5.6.7.8", "5.6.7.8"},
		{"无端口对端原样返回", "1.2.3.4", "", "1.2.3.4"},
		{"空白 XFF 回落 TCP 对端", "1.2.3.4:5678", "   ", "1.2.3.4"},
	}
	for _, c := range cases {
		if got := clientRemoteAddr(mk(c.remote, c.xff)); got != c.want {
			t.Errorf("%s: clientRemoteAddr = %q, want %q", c.name, got, c.want)
		}
	}
}

// 注册表是闭集：只有预置的 vscode / zcode，未注册 target 不允许悄悄落到某个
// 实现上（backend 构建期直接报错）。
func TestRelayRegistryClosed(t *testing.T) {
	for _, target := range []string{"vscode", "zcode"} {
		if _, ok := RelayHandler(target, "/"); !ok {
			t.Errorf("target %q should be registered", target)
		}
	}
	if h, ok := RelayHandler("unknown-relay", "/"); ok || h != nil {
		t.Error("unknown target must not resolve")
	}
}
