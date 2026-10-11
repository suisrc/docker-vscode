package pkg

import (
	"bytes"
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
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"path"
	"path/filepath"
	"strconv"
	"strings"
	"time"
)

//go:embed favicon.ico manager.html loading.html login.html logout.svc.js kvs.ini
var staticFS embed.FS

// MustAsset reads an embedded asset by name, failing fast at startup if missing.
// embed.FS is an in-memory read-only map; ReadFile just returns a slice over it,
// so there is no I/O and no benefit to pre-caching assets into package vars.
//
// When DEBUG=1 is set in the environment, assets are read from the current
// working directory instead of the embed. This allows live-editing assets
// (e.g. logout.svc.js, login.html) without recompiling.
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

// parseBackends builds a list of Backend entries from the selected [proxies]
// route table (or inline -n entries). Each entry is a [2]string{key, value}
// where key is the routing prefix (with optional & or ^ markers) and value is
// the backend URL. Inline -n entries carry a "proxies." key prefix which is
// stripped here; table entries are raw prefixes.
//
// Prefix markers:
//   - "^" prefix: regex pattern match
//   - "cc~" prefix: cached proxy backend (disk cache)
//   - "ws~" prefix: WebSocket-aware backend (upgrade requests detected)
//   - "http://" or "https://" prefix on the prefix itself: full-domain match
//   - trailing "[k=v]" on the prefix: query gate — the request must carry this
//     exact pair or the route is skipped (see splitQueryGate)
//   - "*" leading a prefix (per comma element): suffix match — the request path
//     must end with the rest (b.Suffix carries the literal suffix; a bare "*"
//     matches every path and is skipped with a warning)
//   - svc://<name> target: kvs-hosted service
//
// Markers may combine (e.g. "cc~/path"); they are stripped in any order.
//
// There is no implicit single-backend-to-service promotion: hosted services
// are addressed explicitly via svc://<name>.
// hasVerTemplate reports whether a route prefix carries the
// <verhash>/<version> placeholders.
func hasVerTemplate(s string) bool {
	return strings.Contains(s, "<verhash>") || strings.Contains(s, "<version>")
}

// ExpandVerTemplate expands the <verhash>/<version> placeholders of a route
// prefix from the service's resolved config (<verhash> → VersionHash,
// <version> → Version). ok=false when the service is unknown or a needed
// value has not been resolved yet — the route is then skipped until the
// service has been prepared ("没有启动就跳过，启动后再参与匹配").
func ExpandVerTemplate(tpl string, sc *ServiceConfig) (string, bool) {
	if sc == nil {
		return "", false
	}
	p := tpl
	if strings.Contains(p, "<verhash>") {
		if sc.VersionHash == "" {
			return "", false
		}
		p = strings.ReplaceAll(p, "<verhash>", sc.VersionHash)
	}
	if strings.Contains(p, "<version>") {
		if sc.Version == "" {
			return "", false
		}
		p = strings.ReplaceAll(p, "<version>", sc.Version)
	}
	return p, true
}

func parseBackends(entries [][2]string) []Backend {
	var backends []Backend
	for _, e := range entries {
		prefix := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(e[0]), "proxies."))
		urlStr := strings.TrimSpace(e[1])
		if prefix == "" || urlStr == "" {
			log.Printf("WARNING: empty prefix or url in proxies entry: %q=%q, skipping", prefix, urlStr)
			continue
		}

		// Strip prefix markers (in any order, so "cc~ws~/" etc. combine).
		var isCache, isWSock, authFree bool
		for changed := true; changed; {
			changed = false
			switch {
			case strings.HasPrefix(prefix, "!"):
				prefix = prefix[1:]
				authFree, changed = true, true
			case strings.HasPrefix(prefix, "cc~"):
				prefix = prefix[3:]
				isCache, changed = true, true
			case strings.HasPrefix(prefix, "ws~"):
				prefix = prefix[3:]
				isWSock, changed = true, true
			}
		}

		// Trailing "[k=v]" query gate: extracted before the comma split so the
		// gate survives expansion. One gate covers every prefix the line
		// expands to.
		base, gateKey, gateVal := splitQueryGate(prefix)

		// Detect regex marker ("^" prefix): the pattern (incl. commas) is
		// used verbatim as one backend, no comma splitting.
		if isRegex := strings.HasPrefix(base, "^"); isRegex {
			if b := newBackend(base[1:], urlStr); b != nil {
				b.IsService = b.Scheme == "svc"
				if b.IsService {
					b.ServiceName, _, _ = SplitSvcTarget(b.Target)
				}
				b.IsRegex = true
				b.IsCache = isCache
				b.IsWSock = isWSock
				b.AuthFree = authFree
				b.QueryKey, b.QueryVal = gateKey, gateVal
				backends = append(backends, *b)
			}
			continue
		}

		// Comma-separated prefixes expand to one Backend per prefix, sharing
		// the same URL and markers (e.g. "/assets/,/favicon.ico=file://...").
		for _, p := range strings.Split(base, ",") {
			if p = strings.TrimSpace(p); p == "" {
				continue
			}
			// "*" 前导的后缀匹配（逐元素标记）：Source 保留含 * 的完整写法用于
			// 日志与展示，Suffix 存去 * 后的字面后缀。后缀元素不做 "/" 前缀补全
			// （*.webmanifest 的后缀就是 ".webmanifest"）；裸 "*" 会匹配所有
			// 路径，告警跳过。
			suffix := ""
			if strings.HasPrefix(p, "*") {
				if suffix = p[1:]; suffix == "" {
					log.Printf("WARNING: route %q: bare * matches every path, skipping", e[0])
					continue
				}
			} else if !strings.HasPrefix(p, "/") &&
				!strings.HasPrefix(p, "http://") && !strings.HasPrefix(p, "https://") {
				p = "/" + p
			}
			if b := newBackend(p, urlStr); b != nil {
				// svc://<name> targets are hosted services by definition —
				// the scheme itself is the explicit service marker.
				b.IsService = b.Scheme == "svc"
				if b.IsService {
					b.ServiceName, _, _ = SplitSvcTarget(b.Target)
				}
				b.IsCache = isCache
				b.IsWSock = isWSock
				b.AuthFree = authFree
				b.QueryKey, b.QueryVal = gateKey, gateVal
				b.Suffix = suffix
				// <verhash>/<version> route template: the prefix is matched
				// against the TARGET service's resolved VersionHash/Version
				// per request. svc:// only — other schemes have no service to
				// resolve from and match the placeholder text literally.
				b.VerTemplate = hasVerTemplate(p)
				if b.VerTemplate && b.Scheme != "svc" {
					log.Printf("WARNING: backend %q: <verhash>/<version> placeholders resolve only for svc:// targets, matching literally", b.RawURL)
					b.VerTemplate = false
				}
				backends = append(backends, *b)
			}
		}
	}

	return backends
}

// splitQueryGate extracts a trailing "[k=v]" query gate from a route prefix:
// the request's decoded query must carry this exact pair for the route to
// participate in dispatch (a miss falls through to later routes). The gate is
// only recognized when the prefix ends with "]" and the bracket content is
// exactly one "k=v" (first "=" splits, key non-empty, no comma) — anything
// else (regex char classes like [0-9], literal paths, several pairs) is left
// untouched as path text; content that carries "=" or "," but fails the rule
// gets a WARNING since it is likely a mistaken multi-pair gate. Single pair
// by design: two plain strings on Backend, dispatch is one comparison.
// Values are compared decoded: the config holds the literal value, the
// request side may URL-encode it.
func splitQueryGate(prefix string) (base, key, val string) {
	if !strings.HasSuffix(prefix, "]") {
		return prefix, "", ""
	}
	i := strings.LastIndexByte(prefix, '[')
	if i < 0 {
		return prefix, "", ""
	}
	inner := prefix[i+1 : len(prefix)-1]
	k, v, ok := strings.Cut(strings.TrimSpace(inner), "=")
	k = strings.TrimSpace(k)
	if !ok || k == "" {
		// 无 = 或空键：正则字符类、字面路径等，安静地按字面处理。
		return prefix, "", ""
	}
	if strings.Contains(inner, ",") {
		log.Printf("WARNING: route %q: query gate supports a single k=v only, treating prefix as literal path", prefix)
		return prefix, "", ""
	}
	return prefix[:i], k, strings.TrimSpace(v)
}

// newBackend creates a Backend by parsing scheme:// from rawURL.
// A leading "-" or "+" on rawURL is the http(s) path mode marker:
//   - "-": replace — strip the routing prefix from the request path and
//     join the remainder onto the target's own path
//   - "+": append — the full request path (prefix included) is appended
//     after the target's own path
//   - none: the request path is forwarded unchanged
//
// Only http(s)/ws(s)/unix/svc/file honor the marker; text/relay/api ignore
// it with a warning.
func newBackend(prefix, rawURL string) *Backend {
	var mode string
	if strings.HasPrefix(rawURL, "-") || strings.HasPrefix(rawURL, "+") {
		mode, rawURL = rawURL[:1], rawURL[1:]
	}
	scheme, target, ok := strings.Cut(rawURL, "://")
	if !ok {
		log.Printf("WARNING: backend %q has no scheme, skipping", rawURL)
		return nil
	}
	scheme = strings.ToLower(scheme)
	if mode != "" && (scheme == "text" || scheme == "relay" || scheme == "api") {
		// These targets never rewrite request paths: text:// serves one
		// literal body, relay:// is path-agnostic once dispatched, and api://
		// handlers are written against the strip-prefix mount contract.
		log.Printf("WARNING: backend %q: %s:// targets do not support -/+ path modes, ignoring %q", rawURL, scheme, mode)
		mode = ""
	}
	return &Backend{
		Source:   prefix,
		Scheme:   scheme,
		Target:   target,
		RawURL:   rawURL,
		PathMode: mode,
	}
}

// =============================================================================
// Backend Handlers — createBackendHandler dispatches to the right handler type.
// =============================================================================

// apiHandleMap is the registry for "api://" backends: handler name →
// http.Handler. Modules register their handlers via registerAPI at init
// time (e.g. manager.go registers "manager"); api://<name> looks the handler
// up here directly.
var apiHandleMap = map[string]http.Handler{}

// registerAPI adds a handler to the api:// registry.
func registerAPI(name string, h http.Handler) {
	apiHandleMap[name] = h
}

// SvcExtHandler is one hosted-service extension handler (svc://<name>.<ext>).
// It receives the service config resolved for the CURRENT request (the
// version hash and dirs may change across config reloads) and the route
// prefix it is mounted at (expanded per request for templated routes) so the
// handler can self-locate in the raw request path.
type SvcExtHandler func(w http.ResponseWriter, r *http.Request, sc *ServiceConfig, mount string)

// svcExtRegistry holds the service-provided extension handlers:
// service name → extension name → handler. Modules register via
// RegisterSvcExt at init time (e.g. vscode.go provides "ext").
var svcExtRegistry = map[string]map[string]SvcExtHandler{}

// RegisterSvcExt attaches an extension handler to a named hosted service.
func RegisterSvcExt(svc, ext string, h SvcExtHandler) {
	m := svcExtRegistry[svc]
	if m == nil {
		m = map[string]SvcExtHandler{}
		svcExtRegistry[svc] = m
	}
	m[ext] = h
}

// svcExtensions returns the registered extension map of a service (nil when
// none — attached onto ServiceConfig.Extensions at config load).
func svcExtensions(svc string) map[string]SvcExtHandler {
	return svcExtRegistry[svc]
}

// SplitSvcTarget splits an svc:// target into service name and optional
// extension: "vscode.ext" → ("vscode", "ext", true); "zcode" → ("zcode",
// "", false). The extension is the part after the LAST dot.
func SplitSvcTarget(target string) (svc, ext string, hasExt bool) {
	if i := strings.LastIndexByte(target, '.'); i > 0 && i < len(target)-1 {
		return target[:i], target[i+1:], true
	}
	return target, "", false
}

// unixSyntheticTarget labels unix-socket requests for rewriteProxyRequest:
// the transport dials the socket directly, the URL only carries scheme/host.
var unixSyntheticTarget = &url.URL{Scheme: "http", Host: "unix"}

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
	// Route mount as written in the ini (cc~/ws~ markers included) — every
	// scheme case logs "backend <mount> → <resolved target>" exactly once
	// per (re)build, so reloads stay debuggable from the log alone.
	mount := b.Source
	if b.IsCache {
		mount = "cc~" + mount
	}
	if b.IsWSock {
		mount = "ws~" + mount
	}
	if len(b.QueryKey) > 0 {
		mount += "[" + b.QueryKey + "=" + b.QueryVal + "]"
	}
	// Hosted-service name for self-attribution (injected logout script's
	// ?svc= query): set by parseBackends, carried through the svc:// proxy.
	svcName := b.ServiceName
	switch b.Scheme {
	case "http", "https", "ws", "wss":
		log.Printf("backend %s → %s", mount, b.RawURL)
		// ws/wss backends are plain HTTP under the hood; mark them as
		// WebSocket routes so upgrade requests get the dedicated handling.
		if b.Scheme == "ws" || b.Scheme == "wss" {
			b.IsWSock = true
		}
		targetURL, err := url.Parse(b.RawURL)
		if err != nil {
			log.Fatalf("invalid backend URL %q: %v", b.RawURL, err)
		}
		// Path handling per the -/+/none marker on the target URL:
		//   none → request path forwarded unchanged (target path ignored);
		//   -    → replace: strip the routing prefix, join onto target path;
		//   +    → append: full request path joined after target path.
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				rewriteProxyRequest(req, b, targetURL, cacheHeaders)
			},
			Transport:     upstreamAwareTransport(sharedProxyTransport(), b),
			FlushInterval: proxyFlushInterval,
			BufferPool:    sharedProxyBufferPool,
		}
		if loginAuthz {
			rp.ModifyResponse = cacheResponseModifier(svcName)
		}
		// Upgrade requests take the raw-conn tunnel (pkg/wsproxy.go); every
		// other request keeps using the standard ReverseProxy.
		up := &wsUpstream{b: b, target: targetURL, hdrs: cacheHeaders, mods: rp.ModifyResponse, svc: b.svc}
		fast := wsUpgradeHandler(up, rp)
		if b.IsCache {
			// A cc~ route must never swallow an upgrade (the cache path uses a
			// plain http.Client, which cannot upgrade): split by request kind.
			return upgradeAwareHandler(fast, HandleCCBackend(b))
		}
		return fast

	case "unix":
		socketPath := b.Target
		log.Printf("backend %s → unix://%s", mount, socketPath)
		// Path modes go through the same rewriteProxyRequest as http(s)
		// targets: "" forwards the full path, "-" strips the mount prefix,
		// "+" appends after the (empty) target path — i.e. keeps it whole.
		rp := &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				rewriteProxyRequest(req, b, unixSyntheticTarget, cacheHeaders)
			},
			Transport:     upstreamAwareTransport(unixProxyTransport(socketPath), b),
			FlushInterval: proxyFlushInterval,
			BufferPool:    sharedProxyBufferPool,
		}
		if loginAuthz {
			rp.ModifyResponse = cacheResponseModifier(svcName)
		}
		up := &wsUpstream{b: b, target: unixSyntheticTarget, hdrs: cacheHeaders, mods: rp.ModifyResponse, svc: b.svc}
		fast := wsUpgradeHandler(up, rp)
		if b.IsCache {
			return upgradeAwareHandler(fast, HandleCCBackend(b))
		}
		return fast

	case "file":
		dir := b.Target
		log.Printf("backend %s → file://%s", mount, dir)
		fs := precompressedFileServer(dir, http.FileServer(http.Dir(dir)))
		if b.PathMode == "+" {
			// "+": the mount prefix stays in the file layout (dir + full path).
			return fs
		}
		// "" and "-": strip the mount prefix before mapping into dir,
		// + .gz/.br pre-compressed sibling support.
		return http.StripPrefix(b.Source, fs)

	case "svc":
		// kvs-hosted service backend: the target names a [service.<name>]
		// section. The inner reverse proxy points at the service's resolved
		// address (http/https/unix); preparation (download/extract/start) is
		// orchestrated by the route loop in main.go, which only reaches this
		// handler once the service is ready.
		//
		// svc://<name>.<ext> dispatches to the service's own extension handler
		// (registered via RegisterSvcExt) instead of the reverse proxy — e.g.
		// svc://vscode.ext serves the VS Code web-extension resources. The
		// service config is resolved per request (version hash changes across
		// reloads); the handler locates itself in the raw request path.
		name, ext, hasExt := SplitSvcTarget(b.Target)
		sc := Services().Get(name)
		if sc == nil || sc.Address == "" {
			log.Fatalf("backend svc://%s: service not configured or address missing (in %q)", name, b.RawURL)
		}
		if hasExt {
			h, known := sc.Extensions[ext]
			if !known {
				log.Fatalf("backend svc://%s: unknown extension %q (in %q)", name, ext, b.RawURL)
			}
			log.Printf("backend %s → svc://%s.%s (service extension)", mount, name, ext)
			return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				cur := Services().Get(name)
				if cur == nil {
					http.NotFound(w, r)
					return
				}
				mount := b.Source
				if b.VerTemplate {
					if p, ok := ExpandVerTemplate(b.Source, cur); ok {
						mount = p
					}
				}
				h(w, r, cur, mount)
			})
		}
		scheme, target, ok := strings.Cut(sc.Address, "://")
		scheme = strings.ToLower(scheme)
		if !ok || scheme == "svc" {
			log.Fatalf("backend svc://%s: invalid address %q", name, sc.Address)
		}
		nb := b
		nb.Scheme, nb.Target, nb.RawURL = scheme, target, sc.Address
		nb.IsService = false // lifecycle handled by the service manager
		nb.ServiceName = svcName
		nb.svc = sc
		log.Printf("backend %s → svc://%s (hosted service, address %s)", mount, name, sc.Address)
		return CreateBackendHandler(nb, cacheHeaders, loginAuthz)

	case "relay":
		// relay://<target> 进程内中继：按 target 到公共注册表分流（RelayHandler
		// —— 预置 "vscode" 与 "zcode"）。未注册 target 是配置错误，启动即报错。
		// -/+ 路径模式不适用（解析期已拒绝）。
		h, ok := RelayHandler(b.Target, b.Source)
		if !ok {
			log.Fatalf("unknown relay %q in %q", b.Target, b.RawURL)
		}
		log.Printf("backend %s → relay://%s", mount, b.Target)
		return h

	case "api":
		// In-process API backend: the target names a handler registered in
		// apiHandleMap; the response is rendered directly by Go code
		// (no subprocess). Handlers are registered via registerAPI from
		// their owning modules (e.g. manager.go registers "manager").
		if h, ok := apiHandleMap[b.Target]; ok {
			// Handlers are written against the root ("/"), so they are mounted
			// at the route prefix by stripping it from the request path — the
			// same semantics as file:// backends. This lets an api:// service
			// live at any path (e.g. /admin=api://manager serves the page at
			// /admin and its API at /admin/__manager*); root mounts ("/") are
			// passed through untouched. Template mounts (<verhash>/<version>)
			// skip the strip: the expanded prefix is only known per request,
			// and such handlers locate themselves in the raw path. -/+ path
			// modes do not apply (rejected at parse time).
			if !b.VerTemplate && b.Source != "" && b.Source != "/" {
				h = http.StripPrefix(b.Source, h)
			}
			log.Printf("backend %s → api://%s", mount, b.Target)
			return h
		}
		log.Fatalf("unknown api handler %q in %q", b.Target, b.RawURL)
		return nil

	case "text":
		content := b.Target
		// RawURL is the literal content — never log it, log the size instead.
		log.Printf("backend %s → text (%d bytes)", mount, len(content))
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

// joinProxyPaths joins a target base path and a (sub)path with a single
// "/" separator; either side may be empty or "/".
func joinProxyPaths(base, sub string) string {
	if base == "" || base == "/" {
		return sub
	}
	if sub == "" || sub == "/" {
		return base
	}
	return strings.TrimSuffix(base, "/") + "/" + strings.TrimPrefix(sub, "/")
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
// logout-button script into recognised HTML apps. svcName attributes the
// injected script to the backend's hosted service (?svc= on the script URL) —
// empty for non-svc backends.
func cacheResponseModifier(svcName string) func(*http.Response) error {
	return chainModifiers(authRedirectModifier(), injectLogoutButtonFor(svcName))
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
	scriptPath  string // script URL appended as <script src="...[?svc=]"> on fingerprint match
}

// appDetectors lists the apps whose logout-button scripts are injected into
// recognised HTML pages. Each entry pairs a fingerprint (a byte substring
// unique to that app's HTML) with the script to inject. The ?svc= query (the
// backend's hosted-service name) is appended by matchAppScript so the script
// talks to its own service instead of letting the server pick one arbitrarily.
var appDetectors = []appDetector{
	{
		// VS Code Server web workbench — the meta id is present in workbench.html.
		fingerprint: []byte(`vscode-workbench-web-configuration`),
		scriptPath:  "/__logout.svc.js",
	},
}

// injectLogoutButtonFor inspects proxied HTML document responses and, when the
// body matches a known app fingerprint, appends that app's logout-button script
// tag. svcName is the backend's hosted-service name ("" for non-svc backends)
// and is appended to the script URL as ?svc=. Unrecognised pages pass through
// untouched, so non-VS-Code backends are not polluted with a useless script.
//
// Only top-level HTML documents (Content-Type: text/html, GET) are inspected,
// so static assets, API calls and Server-Sent-Events are unaffected.
func injectLogoutButtonFor(svcName string) func(*http.Response) error {
	return func(r *http.Response) error {
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
		if tag := matchAppScript(body, svcName); tag != nil {
			setResponseBody(r, injectScript(body, tag))
			return nil
		}
		// No match — restore body as-is.
		r.Body = io.NopCloser(bytes.NewReader(body))
		return nil
	}
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
// fingerprint is found in body, or nil if no app matches. The tag carries the
// backend's service name as ?svc= so the script targets its own service.
func matchAppScript(body []byte, svcName string) []byte {
	for _, d := range appDetectors {
		if bytes.Contains(body, d.fingerprint) {
			tag := `<script src="` + d.scriptPath
			if svcName != "" {
				tag += `?svc=` + url.QueryEscape(svcName)
			}
			return []byte(tag + `"></script>`)
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

// rewriteProxyRequest applies the backend's routing rules to req. It is shared
// by the ReverseProxy Director and the raw-conn WS upgrade path
// (pkg/wsproxy.go) so both send byte-identical upstream requests.
func rewriteProxyRequest(req *http.Request, b Backend, targetURL *url.URL, cacheHeaders map[string]string) {
	// ws/wss 只是 http/https 的别名：http.Transport 只认 http/https，
	// 不映射的话 ws:// 路由会得到 `unsupported protocol scheme "ws"`。
	switch targetURL.Scheme {
	case "ws":
		req.URL.Scheme = "http"
	case "wss":
		req.URL.Scheme = "https"
	default:
		req.URL.Scheme = targetURL.Scheme
	}
	req.URL.Host = targetURL.Host
	basePath := targetURL.EscapedPath()
	switch b.PathMode {
	case "-":
		// 后缀路由没有可剥的前缀（Source 是匹配模式不是路径前缀），
		// TrimPrefix 自然空转 → 转发完整请求路径。
		req.URL.Path = joinProxyPaths(basePath, strings.TrimPrefix(req.URL.Path, b.Source))
	case "+":
		req.URL.Path = joinProxyPaths(basePath, req.URL.Path)
	}
	req.URL.RawPath = ""
	if targetURL.RawQuery != "" && req.URL.RawQuery == "" {
		req.URL.RawQuery = targetURL.RawQuery
	}
	applyCacheHeaders(req, cacheHeaders)
}

// upstreamAwareTransport wraps the proxy transport with the service's
// upstream_* behaviors (header/cookie injection, 401 token retry) when the
// backend carries a resolved ServiceConfig — plain transports otherwise.
func upstreamAwareTransport(base http.RoundTripper, b Backend) http.RoundTripper {
	if serviceHasUpstream(b.svc) {
		return &svcUpstreamTransport{base: base, sc: b.svc}
	}
	return base
}

// upgradeAwareHandler splits upgrade requests from plain ones: WebSocket
// upgrades go to the upgrade-capable handler, everything else to plain.
func upgradeAwareHandler(upgrade, plain http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgrade(r) {
			upgrade.ServeHTTP(w, r)
			return
		}
		plain.ServeHTTP(w, r)
	})
}

// ExactPathMatch reports whether a backend's routing prefix must match the
// request path exactly instead of by prefix. In-process endpoints (the relay,
// api:// handlers) are single resources where a wider match would be
// wrong; tunnel-capable schemes (http/https/ws/wss/unix) match by prefix, so a
// ws~ marked route can cover a whole subtree.
func ExactPathMatch(b Backend) bool {
	if !b.IsWSock {
		return false
	}
	return b.Scheme != "http" && b.Scheme != "https" && b.Scheme != "ws" && b.Scheme != "wss" && b.Scheme != "unix"
}

// precompressedFileServer wraps a file handler with pre-compressed asset
// support: for a request path <a>, when the plain file and a sibling
// <a>.br or <a>.gz exist on disk, the pre-compressed variant is served
// instead. br wins over gz. The variant is only selected when the
// request's Accept-Encoding lists the matching coding, and the response
// carries Content-Encoding + Vary headers so caches do not mix variants.
func precompressedFileServer(dir string, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			next.ServeHTTP(w, r)
			return
		}
		upath := r.URL.Path
		if upath == "" || strings.HasSuffix(upath, "/") {
			next.ServeHTTP(w, r)
			return
		}
		clean := path.Clean("/" + upath)[1:] // strip leading "/" for fs paths
		if clean == "" {
			next.ServeHTTP(w, r)
			return
		}
		accept := r.Header.Get("Accept-Encoding")
		// Preference: br > gz. Serve a variant only when both the plain
		// file and the pre-compressed sibling exist (the variant optimizes
		// <a>; it is not a standalone asset).
		for _, v := range []struct{ ext, coding string }{{".br", "br"}, {".gz", "gzip"}, {".zs", "zstd"}} {
			if !acceptsEncoding(accept, v.coding) {
				continue
			}
			full := filepath.Join(dir, filepath.FromSlash(clean))
			if _, err := os.Stat(full); err != nil {
				break // plain file missing; let the next handler 404 it
			}
			comp := full + v.ext
			fi, err := os.Stat(comp)
			if err != nil || fi.IsDir() {
				continue
			}
			f, err := os.Open(comp)
			if err != nil {
				continue
			}
			w.Header().Set("Content-Encoding", v.coding)
			w.Header().Set("Vary", "Accept-Encoding")
			// Guess a content type from the original extension, not .br/.gz.
			http.ServeContent(w, r, clean, fi.ModTime(), f)
			f.Close()
			return
		}
		next.ServeHTTP(w, r)
	})
}

// acceptsEncoding reports whether the Accept-Encoding header value lists
// the given coding (case-insensitive token match; "*" matches anything).
func acceptsEncoding(accept, coding string) bool {
	for _, part := range strings.Split(accept, ",") {
		tok := strings.TrimSpace(part)
		if i := strings.IndexByte(tok, ';'); i >= 0 {
			tok = tok[:i]
		}
		tok = strings.TrimSpace(tok)
		if strings.EqualFold(tok, coding) || tok == "*" {
			return true
		}
	}
	return false
}
