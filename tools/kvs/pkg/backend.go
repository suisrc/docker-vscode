package pkg

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/pem"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"
)

//go:embed favicon.ico loading.html login.html logout.vsc.js kvs.default.ini kvs.vscode.ini zlist.html
var staticFS embed.FS

// MustAsset reads an embedded asset by name, failing fast at startup if missing.
// embed.FS is an in-memory read-only map; ReadFile just returns a slice over it,
// so there is no I/O and no benefit to pre-caching assets into package vars.
//
// When DEBUG=1 is set in the environment, assets are read from the current
// working directory instead of the embed. This allows live-editing assets
// (e.g. logout.vsc.js, login.html) without recompiling.
func MustAsset(name string) []byte {
	if gDebug {
		if b, err := os.ReadFile(name); err == nil {
			return b
		}
		// Fall through to embed on error (e.g. file not found).
	}
	b, err := staticFS.ReadFile(name)
	if err != nil {
		log.Fatalf("embed asset %q: %v", name, err)
	}
	return b
}

// parseBackends builds a list of Backend entries from the ordered [proxies]
// section entries. Each entry is a [2]string{key, value} where key is the
// routing prefix (with optional & or ^ markers) and value is the backend URL.
//
// Prefix markers:
//   - "&" prefix: kvs-managed service backend (auto-deploy, loading page)
//   - "^" prefix: regex pattern match (cannot combine with "&")
//   - "cc~" prefix: cached proxy backend (disk cache)
//   - "ws~" prefix: WebSocket-aware backend (upgrade requests detected)
//   - "http://" or "https://" prefix on the prefix itself: full-domain match
//
// Markers may combine (e.g. "&cc~/" is a kvs-managed cached backend); they
// are stripped in any order.
//
// Service backends must be explicitly marked with "&"; there is no implicit
// single-backend-to-service promotion.
func parseBackends(entries [][2]string) []Backend {
	var backends []Backend
	for _, e := range entries {
		prefix := strings.TrimSpace(strings.TrimPrefix(e[0], "proxies."))
		urlStr := strings.TrimSpace(e[1])
		if prefix == "" || urlStr == "" {
			log.Printf("WARNING: empty prefix or url in proxies entry: %q=%q, skipping", prefix, urlStr)
			continue
		}

		// Strip prefix markers (in any order, so "&cc~/" etc. combine).
		var isService, isCache, isWSock bool
		for changed := true; changed; {
			changed = false
			switch {
			case strings.HasPrefix(prefix, "&"):
				prefix = prefix[1:]
				isService, changed = true, true
			case strings.HasPrefix(prefix, "cc~"):
				prefix = prefix[3:]
				isCache, changed = true, true
			case strings.HasPrefix(prefix, "ws~"):
				prefix = prefix[3:]
				isWSock, changed = true, true
			}
		}

		// Detect regex marker ("^" prefix).
		isRegex := strings.HasPrefix(prefix, "^")
		if isRegex {
			prefix = prefix[1:] // strip "^", keep the regex pattern
		}

		if !isRegex && !strings.HasPrefix(prefix, "/") &&
			!strings.HasPrefix(prefix, "http://") && !strings.HasPrefix(prefix, "https://") {
			prefix = "/" + prefix
		}

		if b := newBackend(prefix, urlStr); b != nil {
			b.IsService = isService
			b.IsRegex = isRegex
			b.IsCache = isCache
			b.IsWSock = isWSock
			backends = append(backends, *b)
		}
	}

	return backends
}

// newBackend creates a Backend by parsing scheme:// from rawURL.
func newBackend(prefix, rawURL string) *Backend {
	scheme, target, ok := strings.Cut(rawURL, "://")
	if !ok {
		log.Printf("WARNING: backend %q has no scheme, skipping", rawURL)
		return nil
	}
	scheme = strings.ToLower(scheme)
	log.Printf("backend %q → prefix=%q scheme=%q target=%q", rawURL, prefix, scheme, target)
	return &Backend{
		Prefix: prefix,
		Scheme: scheme,
		Target: target,
		RawURL: rawURL,
	}
}

// =============================================================================
// Backend Handlers — createBackendHandler dispatches to the right handler type.
// =============================================================================

// apiHandleMap is the registry for "api://" backends: handler name →
// http.Handler. Modules register their handlers via registerAPI at init
// time (e.g. zcode.go registers "zlist"); api://<name> looks the handler
// up here directly.
var apiHandleMap = map[string]http.Handler{}

// registerAPI adds a handler to the api:// registry.
func registerAPI(name string, h http.Handler) {
	apiHandleMap[name] = h
}

// CreateBackendHandler builds an http.Handler for the given backend.
// Supported schemes: http, https, ws, wss (reverse proxy), unix (reverse proxy),
// file (directory), text (literal).
//
// Backend markers:
//   - cc~ (b.IsCache): proxied GET responses are cached on disk under
//     {cache_dir:-/cache}/ccproxy/{scheme}:{host}/path
//   - ws~ / ws:// / wss:// (b.IsWSock): WebSocket-aware route — upgrade requests
//     are detected and handled through the upgrade-capable proxy transport.
func CreateBackendHandler(b Backend, cacheHeaders map[string]string, loginAuthz bool) http.Handler {
	switch b.Scheme {
	case "http", "https", "ws", "wss":
		// ws/wss backends are plain HTTP under the hood; mark them as
		// WebSocket routes so upgrade requests get the dedicated handling.
		if b.Scheme == "ws" || b.Scheme == "wss" {
			b.IsWSock = true
		}
		targetURL, err := url.Parse(b.RawURL)
		if err != nil {
			log.Fatalf("invalid backend URL %q: %v", b.RawURL, err)
		}
		rp := httputil.NewSingleHostReverseProxy(targetURL)
		origDirector := rp.Director
		rp.Director = func(req *http.Request) {
			origDirector(req)
			applyCacheHeaders(req, cacheHeaders)
		}
		if loginAuthz {
			rp.ModifyResponse = cacheResponseModifier()
		}
		if b.IsCache {
			return HandleCCBackend(b)
		}
		if b.IsWSock {
			return wsProxyHandler(b, rp)
		}
		return rp

	case "unix":
		socketPath := b.Target
		log.Printf("backend unix socket: %s", socketPath)
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "unix"
				applyCacheHeaders(req, cacheHeaders)
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		}
		if loginAuthz {
			rp.ModifyResponse = cacheResponseModifier()
		}
		return rp

	case "file":
		dir := b.Target
		log.Printf("backend file server: %s", dir)
		return http.StripPrefix(b.Prefix, http.FileServer(http.Dir(dir)))

	case "wsws":
		// In-process WebSocket relay backend (ws-to-ws, e.g. the zcode
		// pairing relay): the target is a logical relay name resolved by
		// zcodeGetRelay (optionally "/state-file"), not a host:port. The
		// relay is role-agnostic — the routing prefix (any prefix) is where
		// both desktop and web clients connect. Upgrade requests are handed
		// to the relay state machine; anything else is a 426.
		log.Printf("backend wsws relay: %s", b.Target)
		relay := zcodeGetRelay(b.Target)
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !isWebSocketUpgrade(r) {
				w.Header().Set("Upgrade", "websocket")
				http.Error(w, "websocket upgrade required", http.StatusUpgradeRequired)
				return
			}
			relay.Handle(w, r)
		})

	case "api":
		// In-process API backend: the target names a handler registered in
		// apiHandleMap; the response is rendered directly by Go code
		// (no subprocess). Handlers are registered via registerAPI from
		// their owning modules (e.g. zcode.go registers "zlist").
		if h, ok := apiHandleMap[b.Target]; ok {
			log.Printf("backend api handler: %s", b.Target)
			return h
		}
		log.Fatalf("unknown api handler %q in %q", b.Target, b.RawURL)
		return nil

	case "text":
		content := b.Target
		log.Printf("backend text: %d bytes", len(content))
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/plain; charset=utf-8")
			// @now is replaced with the current time on every request.
			body := strings.ReplaceAll(content, "@now", time.Now().Format(time.RFC3339))
			_, _ = w.Write([]byte(body))
		})

	default:
		log.Fatalf("unknown backend scheme %q in %q", b.Scheme, b.RawURL)
		return nil
	}
}

// applyCacheHeaders rewrites request headers according to the [headers] section.
// Xxx=Val → set/override header Xxx; Xxx= → delete header Xxx.
func applyCacheHeaders(req *http.Request, hdrs map[string]string) {
	if len(hdrs) == 0 {
		return
	}
	for name, val := range hdrs {
		if val == "" {
			req.Header.Del(name)
		} else {
			req.Header.Set(name, val)
		}
	}
}

// cacheResponseModifier returns the composed response modifier applied to all
// reverse-proxy backends: replace 401/403 with the login page, then inject the
// logout-button script into recognised HTML apps.
func cacheResponseModifier() func(*http.Response) error {
	return chainModifiers(authRedirectModifier(), injectLogoutButton)
}

// authRedirectModifier returns a ModifyResponse that replaces 401/403 from the
// upstream with the pre-rendered login page, so the browser shows login instead
// of an error. Shared by http/https and unix reverse proxies.
func authRedirectModifier() func(*http.Response) error {
	return func(r *http.Response) error {
		if r.StatusCode != http.StatusUnauthorized && r.StatusCode != http.StatusForbidden {
			return nil
		}
		if r.Body != nil {
			r.Body.Close()
		}
		r.StatusCode = http.StatusOK
		r.Header = make(http.Header)
		r.Header.Set("Content-Type", "text/html; charset=utf-8")
		body := strings.Replace(string(MustAsset("login.html")), "{{ERROR}}", "", 1)
		r.Body = io.NopCloser(bytes.NewReader([]byte(body)))
		r.ContentLength = int64(len(body))
		return nil
	}
}

// chainModifiers runs the given response modifiers in order, stopping at the
// first error. This lets us compose auth-redirect and logout-button injection.
//
// NOTE: authRedirectModifier must run before injectLogoutButton — when the
// upstream returns 401/403, authRedirectModifier replaces the body with
// login.html (which contains no app fingerprints), so injectLogoutButton is
// a no-op. Running in the reverse order would inject into the error page before
// it gets replaced.
func chainModifiers(mods ...func(*http.Response) error) func(*http.Response) error {
	return func(r *http.Response) error {
		for _, m := range mods {
			if m == nil {
				continue
			}
			if err := m(r); err != nil {
				return err
			}
		}
		return nil
	}
}

// appDetector describes how to recognise a proxied app and which logout script
// to inject into its HTML. Adding support for a new proxied app is just a matter
// of appending an entry here (plus the script + its route).
type appDetector struct {
	fingerprint []byte // substring searched for in the response body
	scriptTag   []byte // <script src="..."> appended when the fingerprint matches
}

// appDetectors lists the apps whose logout-button scripts are injected into
// recognised HTML pages. Each entry pairs a fingerprint (a byte substring
// unique to that app's HTML) with the <script> tag to inject.
var appDetectors = []appDetector{
	{
		// VS Code Server web workbench — the meta id is present in workbench.html.
		fingerprint: []byte(`vscode-workbench-web-configuration`),
		scriptTag:   []byte(`<script src="/__logout.vsc.js"></script>`),
	},
}

// injectLogoutButton inspects proxied HTML document responses and, when the body
// matches a known app fingerprint, appends that app's logout-button script tag.
// Unrecognised pages pass through untouched, so non-VS-Code backends are not
// polluted with a useless script.
//
// Only top-level HTML documents (Content-Type: text/html, GET) are inspected,
// so static assets, API calls and Server-Sent-Events are unaffected.
func injectLogoutButton(r *http.Response) error {
	if r.Request == nil || r.Request.Method != http.MethodGet {
		return nil
	}
	// Only inspect top-level navigations; skip iframes / fetches.
	if dest := r.Request.Header.Get("Sec-Fetch-Dest"); dest != "" && dest != "document" {
		return nil
	}
	ct := r.Header.Get("Content-Type")
	if !strings.HasPrefix(ct, "text/html") {
		return nil
	}

	body, err := io.ReadAll(r.Body)
	if err != nil {
		return err
	}

	// Detect the app by fingerprint; inject only on a match.
	if tag := matchAppScript(body); tag != nil {
		setResponseBody(r, injectScript(body, tag))
		return nil
	}
	// No match — restore body as-is.
	r.Body = io.NopCloser(bytes.NewReader(body))
	return nil
}

// injectScript inserts tag into body before the last </body> (or appends if none).
func injectScript(body, tag []byte) []byte {
	const closeBody = "</body>"
	if idx := bytes.LastIndex(body, []byte(closeBody)); idx >= 0 {
		return bytes.Join([][]byte{body[:idx], tag, body[idx:]}, nil)
	}
	return append(body, tag...)
}

// matchAppScript returns the logout-script tag for the first app whose
// fingerprint is found in body, or nil if no app matches.
func matchAppScript(body []byte) []byte {
	for _, d := range appDetectors {
		if bytes.Contains(body, d.fingerprint) {
			return d.scriptTag
		}
	}
	return nil
}

// setResponseBody replaces the response body with b, fixing Content-Length and
// clearing Transfer-Encoding so clients see a consistent payload.
func setResponseBody(r *http.Response, b []byte) {
	r.Body = io.NopCloser(bytes.NewReader(b))
	r.ContentLength = int64(len(b))
	r.Header.Set("Content-Length", strconv.Itoa(len(b)))
	r.Header.Del("Transfer-Encoding")
	// If the transport already decompressed, drop Content-Encoding so the
	// client doesn't try to decompress an already-decompressed body.
	if r.Uncompressed {
		r.Header.Del("Content-Encoding")
	}
}

// generateSelfSignedCert creates a self-signed TLS certificate valid for 10 years.
func generateSelfSignedCert() (tls.Certificate, error) {
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		return tls.Certificate{}, err
	}

	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		return tls.Certificate{}, err
	}

	// Allow a small clock-skew window so the cert is valid immediately.
	notBefore := time.Now().Add(-time.Hour)
	tmpl := &x509.Certificate{
		SerialNumber: serial,
		Subject: pkix.Name{
			CommonName:   "CodeAuth",
			Organization: []string{"Self-Signed CodeAuth"},
		},
		NotBefore:             notBefore,
		NotAfter:              notBefore.Add(10 * 365 * 24 * time.Hour),
		KeyUsage:              x509.KeyUsageKeyEncipherment | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		BasicConstraintsValid: true,
	}

	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		return tls.Certificate{}, err
	}

	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		return tls.Certificate{}, err
	}
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	return tls.X509KeyPair(certPEM, keyPEM)
}

// =============================================================================
// WebSocket-aware proxying (ws~ marked routes)
// =============================================================================

// isWebSocketUpgrade reports whether the client requests a WebSocket
// connection: Upgrade: websocket plus a Connection header containing the
// "upgrade" token (Connection may list multiple tokens).
func isWebSocketUpgrade(r *http.Request) bool {
	if !strings.EqualFold(r.Header.Get("Upgrade"), "websocket") {
		return false
	}
	for _, v := range r.Header.Values("Connection") {
		for _, tok := range strings.Split(v, ",") {
			if strings.EqualFold(strings.TrimSpace(tok), "upgrade") {
				return true
			}
		}
	}
	return false
}

// wsProxyHandler wraps the reverse proxy of a ws~ marked backend. Upgrade
// requests are detected and logged as WS routes (routed through the
// hijack-capable ReverseProxy transport, which transparently tunnels the
// 101 Switching Protocols handshake and the bidirectional frames); all other
// requests fall through as regular HTTP proxying.
func wsProxyHandler(b Backend, rp http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			log.Printf("[ws] upgrade %s%s → %s (scheme=%s)", r.Host, r.URL.Path, b.RawURL, b.Scheme)
		}
		rp.ServeHTTP(w, r)
	})
}
