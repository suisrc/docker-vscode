package pkg

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
)

// ServerInstance binds an *http.Server with its TLS flag.
type ServerInstance struct {
	Server *http.Server
	IsTLS  bool
}

// buildServers constructs one (plain HTTP) or two (HTTP + HTTPS) servers with
// sensible timeouts and the self-signed cert when SSL is enabled.
// BuildServers builds the HTTP/HTTPS servers for the configured port.
func BuildServers(port string, useSSL bool, mux http.Handler) []ServerInstance {
	return buildServers(port, useSSL, mux)
}

func buildServers(port string, useSSL bool, mux http.Handler) []ServerInstance {
	base := newCacheServer(":"+port, mux, nil)
	if !useSSL {
		return []ServerInstance{{Server: base, IsTLS: false}}
	}
	portNum, err := strconv.Atoi(port)
	if err != nil {
		log.Fatalf("invalid proxy port %q: %v", port, err)
	}
	cert, err := generateSelfSignedCert()
	if err != nil {
		log.Fatalf("generate self-signed cert: %v", err)
	}
	httpsSrv := newCacheServer(fmt.Sprintf(":%d", portNum+1), mux, &tls.Config{
		Certificates: []tls.Certificate{cert},
		MinVersion:   tls.VersionTLS12,
	})
	return []ServerInstance{
		{Server: base, IsTLS: false},
		{Server: httpsSrv, IsTLS: true},
	}
}

// newCacheServer returns an *http.Server with the proxy's standard timeouts.
// A nil tlsCfg yields a plain HTTP server.
func newCacheServer(addr string, mux http.Handler, tlsCfg *tls.Config) *http.Server {
	srv := &http.Server{
		Addr:              addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       60 * time.Second,
		WriteTimeout:      0, // streaming (proxying, downloads) may take long; bounded by IdleTimeout + client disconnect
		IdleTimeout:       120 * time.Second,
	}
	if tlsCfg != nil {
		srv.TLSConfig = tlsCfg
	}
	return srv
}

// ServerAddrs joins the listening addresses for logging.
func ServerAddrs(servers []ServerInstance) string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		scheme := "http"
		if s.IsTLS {
			scheme = "https"
		}
		out = append(out, scheme+"://"+s.Server.Addr)
	}
	return strings.Join(out, ", ")
}

// safeReferer returns a same-origin redirect target, falling back to "/".
// It accepts both relative paths and absolute URLs: when the referer is an
// absolute URL on the same host (r.Host), only the path+query is returned so
// the redirect stays same-origin. Cross-origin or unparseable referers fall
// back to "/". This preserves query strings (e.g. ?folder=/wsc) that would
// otherwise be lost.
// SafeReferer sanitizes the Referer header for post-login redirect.
func SafeReferer(ref, host string) string { return safeReferer(ref, host) }

func safeReferer(ref, host string) string {
	if ref == "" {
		return "/"
	}
	u, err := url.Parse(ref)
	if err != nil {
		return "/"
	}
	// Relative referer (no host) — use as-is, unless it's the login page itself.
	if u.Host == "" {
		if u.Path == "/__login" {
			return "/"
		}
		return ref
	}
	// Absolute referer — only allow same-origin, then strip to path+query.
	if u.Host != host {
		return "/"
	}
	path := u.Path
	if path == "" || path == "/__login" {
		return "/"
	}
	if u.RawQuery != "" {
		path += "?" + u.RawQuery
	}
	return path
}

// serveStaticAsset writes an embedded asset with the given content type.
// ServeStaticAsset writes an embedded asset with its content type.
func ServeStaticAsset(w http.ResponseWriter, name string) { serveStaticAsset(w, name) }

func serveStaticAsset(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mustAsset(name))
}

// serveLoginAsset renders login.html with an optional error message.
// The {{ERROR}} placeholder in login.html is replaced with the message
// (HTML-escaped). When msg is empty the placeholder becomes empty too.
// ServeLoginAsset writes the login page with an optional error message.
func ServeLoginAsset(w http.ResponseWriter, msg string) { serveLoginAsset(w, msg) }

func serveLoginAsset(w http.ResponseWriter, msg string) {
	html := string(mustAsset("login.html"))
	escaped := htmlEscape(msg)
	html = strings.Replace(html, "{{ERROR}}", escaped, 1)
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write([]byte(html))
}

// htmlEscape escapes a string for safe inclusion in HTML text content.
func htmlEscape(s string) string {
	s = strings.ReplaceAll(s, "&", "&amp;")
	s = strings.ReplaceAll(s, "<", "&lt;")
	s = strings.ReplaceAll(s, ">", "&gt;")
	s = strings.ReplaceAll(s, `"`, "&#34;")
	return s
}

// generateCookieValue builds the cookie value for a successful login.
//
// login_timeout == 0: cookie value = loginToken (plain, session lifetime).
// login_timeout > 0:  cookie value = "<hash>.<ts>.<salt>" where
//   - ts    = current unix timestamp (seconds)
//   - salt  = 16-char random hex string
//   - hash  = sha256(ts + salt + loginToken)[:24] (hex)
//
// The ts and salt are embedded so the gateway can re-derive the hash and
// check expiry without keeping server-side state.
// GenerateCookieValue builds the cookie value for the configured login mode.
func GenerateCookieValue(loginToken string, loginTimeout int) string {
	return generateCookieValue(loginToken, loginTimeout)
}

func generateCookieValue(loginToken string, loginTimeout int) string {
	if loginTimeout <= 0 {
		return loginToken
	}
	ts := time.Now().Unix()
	salt := randomHex(8) // 8 bytes → 16 hex chars
	return computeCookieHash(ts, salt, loginToken)
}

// computeCookieHash returns "<hash>.<ts>.<salt>".
// hash = sha256(ts+salt+loginToken) truncated to 12 bytes (24 hex chars).
func computeCookieHash(ts int64, salt, loginToken string) string {
	h := sha256.Sum256(fmt.Appendf(nil, "%d%s%s", ts, salt, loginToken))
	return fmt.Sprintf("%x.%d.%s", h[:12], ts, salt) // 12 bytes → 24 hex chars
}

// randomHex returns n random bytes as a hex string (2n chars).
func randomHex(n int) string {
	b := make([]byte, n)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// authMiddleware wraps next with cookie-based authentication.
//
// When login_timeout == 0: cookie value must equal loginToken (plain compare).
// When login_timeout > 0:  cookie value is "<hash>.<ts>.<salt>"; the middleware
// re-derives the hash and checks that (now - ts) < login_timeout. When the
// remaining time drops to ≤ 1/4 of login_timeout, a refreshed cookie is set
// (sliding expiration).
//
// Public paths (/__login, /__logout, /favicon.ico, /__logout.vsc.js, cache)
// are always exempt.
// AuthMiddleware wraps next with the cookie/token check.
func AuthMiddleware(next http.Handler, cfg Config, setCookie func(http.ResponseWriter, string, int)) http.Handler {
	return authMiddleware(next, cfg, setCookie)
}

func authMiddleware(next http.Handler, cfg Config, setCookie func(http.ResponseWriter, string, int)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicAuthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(cfg.CookieName)
		if err != nil || c.Value == "" {
			serveLoginAsset(w, "")
			return
		}

		if cfg.LoginTimeout <= 0 {
			// Plain mode: direct comparison.
			if subtle.ConstantTimeCompare([]byte(c.Value), []byte(cfg.LoginToken)) != 1 {
				serveLoginAsset(w, "")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Hashed mode: parse "<hash>.<ts>.<salt>".
		ok, refresh := validateHashedCookie(c.Value, cfg.LoginToken, cfg.LoginTimeout)
		if !ok {
			log.Printf("[authz] cookie validation failed: %q", c.Value)
			serveLoginAsset(w, "")
			return
		}
		if refresh {
			newVal := generateCookieValue(cfg.LoginToken, cfg.LoginTimeout)
			setCookie(w, newVal, cfg.LoginTimeout)
		}
		next.ServeHTTP(w, r)
	})
}

// validateHashedCookie checks a "<hash>.<ts>.<salt>" cookie value.
// Returns (valid, needsRefresh). needsRefresh is true when the remaining
// time is ≤ 1/4 of the timeout (sliding renewal).
func validateHashedCookie(cookieVal, loginToken string, loginTimeout int) (bool, bool) {
	parts := strings.SplitN(cookieVal, ".", 3)
	if len(parts) != 3 {
		return false, false
	}
	gotHash, tsStr, salt := parts[0], parts[1], parts[2]
	ts, err := strconv.ParseInt(tsStr, 10, 64)
	if err != nil {
		return false, false
	}
	// Re-derive hash and compare.
	expected := computeCookieHash(ts, salt, loginToken)
	// Compare only the hash portion (first 24 hex chars).
	expectedHash := strings.SplitN(expected, ".", 2)[0]
	if subtle.ConstantTimeCompare([]byte(gotHash), []byte(expectedHash)) != 1 {
		return false, false
	}
	// Check expiry.
	now := time.Now().Unix()
	age := now - ts
	if age >= int64(loginTimeout) {
		return false, false
	}
	// Refresh when ≤ 1/4 of timeout remains.
	remaining := int64(loginTimeout) - age
	needsRefresh := remaining <= int64(loginTimeout)/4
	return true, needsRefresh
}

// isPublicAuthPath reports whether path is reachable without a valid token
// (login/logout flow, favicon, logout script, proxy cache).
// When proxyPathPrefix is empty (caching disabled), the cache check is skipped
// so no path is accidentally treated as public.
func isPublicAuthPath(path string) bool {
	switch path {
	case "/__login", "/__logout", "/favicon.ico":
		return true
	}
	if proxyPathPrefix != "" && strings.HasPrefix(path, proxyPathPrefix) {
		return true
	}
	return strings.HasPrefix(path, "/__logout.")
}
