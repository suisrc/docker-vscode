package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// GOverrideVersion is the per-service version override map set by
// /__restart?v=VERSION (key = service name). When an entry is non-empty,
// resolveVersion uses it instead of fetching version_latest_url. This avoids
// polluting os.Environ (KVS_SVC_<NAME>_VERSION) which could leak into the
// backend subprocess and cause unexpected behavior.
var (
	gOverrideMu       sync.Mutex
	gOverrideVersions = map[string]string{}
)

// SetOverrideVersion records a version override for the given service
// ("" = legacy single service).
func SetOverrideVersion(svc, version string) {
	gOverrideMu.Lock()
	defer gOverrideMu.Unlock()
	gOverrideVersions[svc] = version
}

// ClearOverrideVersion removes the version override of the given service.
func ClearOverrideVersion(svc string) {
	gOverrideMu.Lock()
	defer gOverrideMu.Unlock()
	delete(gOverrideVersions, svc)
}

// ClearServiceVersionEnv drops kvs's self-assigned KVS_SVC_<NAME>_VERSION so
// the version placeholder reads fresh on the next reload — but ONLY when a
// refill source exists (download / version_latest_url). Services without one
// keep their last known version: clearing would leave an empty value that
// expands bin_home into …/servers//bin, and "last known" is the best answer a
// service with no version source has. An externally pinned env (different
// value from the resolved one) is always preserved.
func ClearServiceVersionEnv(svc string) {
	sc := Services().Get(svc)
	if sc == nil || sc.Version == "" {
		return
	}
	if sc.Download == "" && sc.VersionLatestURL == "" {
		return // no refill path — keep the last known version
	}
	env := svcEnvKey(svc, "VERSION")
	if os.Getenv(env) == sc.Version {
		os.Unsetenv(env)
	}
}

// GetOverrideVersion returns the version override of the given service (""=none).
func GetOverrideVersion(svc string) string {
	gOverrideMu.Lock()
	defer gOverrideMu.Unlock()
	return gOverrideVersions[svc]
}

// gDebug is flag app is running by debug mode
var gDebug = os.Getenv("KVS_DEBUG") == "1"

// Config holds the gateway-level kvs configuration loaded from kvs.ini.
// Everything service-specific lives on ServiceConfig ([service.<name>]);
// endpoints that need "the" service derive it from the route table (first
// svc:// entry, see svcDefault in main.go) or take ?svc=<name>.
type Config struct {
	Proxies  []Backend
	Services map[string]*ServiceConfig // [service.<name>] sections, keyed by name

	CacheDir     string            // cache_dir — download + cc~ cache directory (top-level key)
	CachePath    string            // cache_path — /__cache/ external cache-proxy path prefix
	CacheHeaders map[string]string // [cache_headers] section: headers added to every cache response (CORS etc.)

	Home          string            // top-level home — gateway working directory (KVS_HOME)
	Port          string            // (KVS_PORT)
	CookieTknName string            // cookie_token_key, default "kvs"
	QueryTokenKey string            // query_token_key — when set, ?<key>=<token> grants access
	LoginAuthz    bool              // login_authz — enable auth redirect + logout button injection
	LoginToken    string            // login_token; when set, cookie value must match it
	LoginTimeout  int               // login_timeout (seconds); 0=session; >0=hashed+expiring; <0=error
	TokenTimeout  int               // token_timeout (seconds); default: 300
	UseSSL        bool              // use_ssl — enable HTTPS with a self-signed cert
	Headers       map[string]string // [headers] section: Xxx=Val → set/override; Xxx= → delete
	PathPublic    []string          // 免 authz 路径前缀清单：来自 [proxies] 里 ! 前缀路由（path_public 键已取消）

	// 反向代理调优（全部可省略；实现见 pkg/transport.go 与 pkg/wsproxy.go）
	ProxyIdleConnsPerHost   int    // proxy_idle_conns_per_host — 每后端空闲连接上限，默认 32
	ProxyMaxIdleConns       int    // proxy_max_idle_conns — 全局空闲连接上限，默认 256
	ProxyDisableCompression bool   // proxy_disable_compression — 代理不做透明 gzip，默认 true
	ProxyFlushInterval      int    // proxy_flush_interval — 刷出间隔(ms)，默认 100；0=不主动刷，-1=每写必刷
	WSProxy                 string // ws_proxy — auto|stdlib|off，WebSocket 升级快路径，默认 auto
	WSIdleTimeout           int    // ws_idle_timeout — WS 隧道空闲超时(秒)，0=关闭（默认）
	ReadTimeout             int    // read_timeout — 主 server 读完整请求(含 body)的秒数，0=不限，默认 60
	TunnelID                string // tunnel_id — relay 渠道 ID（KVS_TUNNEL_ID），目前仅 relay://vscode 消费；空=不门禁
}

// ServiceConfig holds one named service section ([service.<name>]). A service
// becomes a hosted backend when a route references it via svc://<name>: kvs
// then manages its lifecycle (probe → version resolve → download → extract →
// scripts → start/stop).
type ServiceConfig struct {
	Name string // section suffix, e.g. "def" in [service.def]

	Address string // address — backend address (http(s)://host:port or unix://path); reverse-assigned to KVS_SVC_<N>_ADDR/_HOST/_PORT/_SOCK
	Healthz string // healthz — liveness probe URL; "#@<code>" suffix requires that status, plain URL only requires connectivity

	Home             string // home — working directory; default KVS_HOME/<name>
	Version          string
	VersionBaseURL   string
	VersionLatestURL string
	VersionHashURL   string
	VersionHash      string
	Download         string
	DownloadInfo     string
	DownloadFieldURL string
	DownloadProxy    string
	BinHome          string
	OnceShell        string
	InitShell        string
	StopShell        string
	Command          string

	PackageExt string // resolved download package extension (cache filename suffix)
	InitError  string // non-fatal resolve error (e.g. version fetch); surfaced on the loading page

	// Extensions holds the service-provided extension handlers (registered via
	// RegisterSvcExt, routed as svc://<name>.<ext>); attached at config load.
	Extensions map[string]SvcExtHandler

	// vscode-specific service options (also present on other services but
	// only consumed by the VS Code flows). *Raw hold the unexpanded JSON
	// source values until resolveService expands them.
	VscAgentsCmd    string
	VscAgentCmds    map[string]string
	VscAgentCmdsRaw string
	VscAgentsDir    string
	VscAgentArgs    string
	VscLanguage     map[string]string
	VscLanguageRaw  string
	VscOpenURL      string

	Actions    map[string]string // per-service actions table (actions = {name: cmd} JSON)
	ActionsRaw string            // raw actions JSON before expansion

	// upstream_* — 发往上游的增强，生命周期跟随子进程：token 每次 Start 从
	// stdout 重新截取；headers/cookies 的值保留模板、每请求展开（所以
	// {KVS_SVC_<NAME>_UPSTREAM_TOKEN} 换代后引用立即同步为新值）。
	UpstreamTokenSpec    string // upstream_token 原始值 "<pattern>[<key>]"
	UpstreamTokenPattern string // 展开后的日志行前缀（到 token 位置为止）
	UpstreamTokenKey     string // [key] 标记的上游 query 参数名（空=未启用截取）
	UpstreamHeadersRaw   string // upstream_headrs 原始 JSON（"" 表示删除该头）
	UpstreamCookiesRaw   string // upstream_cookies 原始 JSON（"" 表示只删除同名 cookie）
	UpstreamHeaders      map[string]string
	UpstreamCookies      map[string]string

	upstreamToken atomic.Value // string — 最近一次 stdout 截取的 token

	EnvsDefaultRaw string // raw envs_default JSON: env name → default value (filled when the system var is empty)
}

// UpstreamToken returns the token captured from the service subprocess stdout
// ("" = none yet). Read per request by the upstream transport.
func (sc *ServiceConfig) UpstreamToken() string {
	if v := sc.upstreamToken.Load(); v != nil {
		return v.(string)
	}
	return ""
}

// SetUpstreamToken stores a freshly captured token and exports it as
// KVS_SVC_<NAME>_UPSTREAM_TOKEN, so upstream_headers/cookies templates that
// reference it expand to the new value from the next request on.
func (sc *ServiceConfig) SetUpstreamToken(tok string) {
	sc.upstreamToken.Store(tok)
	os.Setenv(svcEnvKey(sc.Name, "UPSTREAM_TOKEN"), tok)
}

// envSuffix returns the environment-variable name fragment for a service:
// upper-cased with non-alphanumeric characters folded to "_".
func envSuffix(name string) string {
	var b strings.Builder
	for _, c := range strings.ToUpper(name) {
		if c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' {
			b.WriteRune(c)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// svcEnvKey builds KVS_SVC_<NAME>_<SUFFIX> for a service.
func svcEnvKey(name, suffix string) string {
	return "KVS_SVC_" + envSuffix(name) + "_" + suffix
}

// Backend describes a single proxy target with its routing prefix.
type Backend struct {
	Source      string // routing Source, "/" for root
	Scheme      string // http, https, unix, file, text, relay, api, svc
	Target      string // host:port, socket path, dir path, literal text, relay/api/service name
	RawURL      string // original URL for logging
	PathMode    string // -/+ path mode: ""=forward as-is, "-"=strip source prefix onto target path, "+"=append full request path after target path; honored by http(s)/ws(s)/unix/svc/file, ignored with a warning by text/relay/api
	IsService   bool   // this backend is managed by kvs (svc:// target, or legacy & marker)
	ServiceName string // svc:// target's service name ("" for non-svc backends); carried through the hosted-service proxy so response modifiers can attribute themselves
	IsRegex     bool   // prefix is a regex pattern (^ prefix in [proxies])
	VerTemplate bool   // prefix carries <verhash>/<version> (svc:// only; expanded from the service config per request)
	IsCache     bool   // cc~ prefix: cache proxied responses on disk ({cache_dir:-/cache}/ccproxy/{scheme}:{host}/path)
	IsWSock     bool   // ws~ prefix (or ws://wss:// url scheme): WebSocket-aware route, upgrade requests detected and handled separately
	QueryKey    string // 前缀尾部 [k=v] query 门禁的参数名（""=无门禁）
	QueryVal    string // 门禁参数必须等于的解码字面值；单条件设计：两个字段零分配，dispatch 一次比较
	Suffix      string // "*" 后缀匹配：去 * 后的字面后缀（如 */code.webmanifest → /code.webmanifest；""=非后缀路由）；Source 保留含 * 的完整写法；路由过滤用，请求路径原样转发不改写
	AuthFree    bool   // 路由 ! 前缀：免 authz——登记进 PathPublic（仅前缀路由；!+* / 正则 / 全域名组合告警并忽略 !，仍要求鉴权）

	// svc 指向 svc:// 后端解析出的 ServiceConfig（其余 scheme 为 nil），代理
	// transport 与升级路径据此套用服务的 upstream_* 行为（头/Cookie 注入、
	// 401 token 重试）。非配置项，由 CreateBackendHandler 的 svc case 填充。
	svc *ServiceConfig
}

// iniFile holds parsed key-value pairs from kvs.ini.
// All entries are stored in a single ordered slice; the index map provides
// O(1) lookup by key. Keys are prefixed with their section:
//
//	"port"                    — top-level
//	"service.home"            — [service] section
//	"proxies./__healthz"      — [proxies] section
//	"headers.x-forwarded-port" — [headers] section
//
// Named sections ([proxies.zcodex], [service.def]…) keep their full section
// name as the prefix ("proxies.zcodex./__healthz"); table/service parsing uses
// the section-aware views below instead of the flat keys.
type iniFile struct {
	props [][2]string    // ordered [key, value] pairs preserving file order
	index map[string]int // key → index in props, for O(1) lookup

	secOrder []string               // section names in first-appearance order ("" = top-level)
	secKeys  map[string][][2]string // section name → ordered [raw key, value] pairs
}

// iniKey builds the lookup key from a section and key name.
// Top-level keys (section == "") have no prefix.
func iniKey(section, key string) string {
	if section == "" {
		return key
	}
	return section + "." + key
}

// get returns the value for key, or "" if not found.
func (ini *iniFile) get(key string) string {
	if i, ok := ini.index[key]; ok {
		return ini.props[i][1]
	}
	return ""
}

// set stores or updates key=value, maintaining the index.
func (ini *iniFile) set(key, value string) {
	if i, ok := ini.index[key]; ok {
		ini.props[i][1] = value
		return
	}
	ini.index[key] = len(ini.props)
	ini.props = append(ini.props, [2]string{key, value})
}

// setSection records a key/value pair inside a named section (raw key kept).
func (ini *iniFile) setSection(section, key, value string) {
	if ini.secKeys == nil {
		ini.secKeys = map[string][][2]string{}
	}
	if _, ok := ini.secKeys[section]; !ok {
		ini.secOrder = append(ini.secOrder, section)
	}
	ini.secKeys[section] = append(ini.secKeys[section], [2]string{key, value})
}

// hasPrefix returns all [key, value] pairs whose key starts with prefix,
// in file order.
func (ini *iniFile) hasPrefix(prefix string) [][2]string {
	var out [][2]string
	for _, kv := range ini.props {
		if strings.HasPrefix(kv[0], prefix) {
			out = append(out, kv)
		}
	}
	return out
}

// proxyTables lists the available route tables in file order: [proxies]
// (unnamed, reported as "") plus every [proxies.<name>].
func (ini *iniFile) proxyTables() []string {
	var names []string
	seen := map[string]bool{}
	for _, sec := range ini.secOrder {
		name := ""
		switch {
		case sec == "proxies":
			name = ""
		case strings.HasPrefix(sec, "proxies."):
			name = strings.TrimPrefix(sec, "proxies.")
		default:
			continue
		}
		if !seen[name] {
			seen[name] = true
			names = append(names, name)
		}
	}
	return names
}

// tableEntries returns the raw route entries of one table (file order).
func (ini *iniFile) tableEntries(table string) [][2]string {
	sec := "proxies"
	if table != "" {
		sec = "proxies." + table
	}
	return ini.secKeys[sec]
}

// serviceSections returns the named service sections in file order as
// (name, raw entries) pairs — every [service.<n>] section.
func (ini *iniFile) serviceSections() [][2]string {
	var out [][2]string
	seen := map[string]bool{}
	for _, sec := range ini.secOrder {
		if !strings.HasPrefix(sec, "service.") {
			continue
		}
		name := strings.TrimPrefix(sec, "service.")
		if seen[name] {
			log.Printf("WARNING: duplicate service section for %q, keeping the first", name)
			continue
		}
		seen[name] = true
		out = append(out, [2]string{name, sec})
	}
	return out
}

// resolveConfigPath resolves the kvs.ini path. The -c flag is required;
// there is no default fallback. Returns "" if -c was not provided.
func resolveConfigPath(flagPath string) string {
	if flagPath == "" {
		return ""
	}
	if fi, err := os.Stat(flagPath); err != nil || fi.IsDir() {
		return ""
	}
	return flagPath
}

// expandValue expands {VAR} and {VAR:-default} placeholders in v, looking the
// name up in the process environment (service fields are reverse-assigned via
// os.Setenv during resolveService, so they are visible here). This supports
// the template syntax used in kvs.ini:
//
//	{KVS_PORT:-8080}   → env KVS_PORT, or "8080" if unset/empty
//	{KVS_SVC_VSCODE_HOME} → any reverse-assigned service variable
//	{publisher}        → literal (all-lowercase braces are not placeholders)
//	${VAR} / $VAR      → standard os.ExpandEnv
func expandValue(v string, excludes ...string) string {
	// validPlaceholderName reports whether s is a bare identifier
	// (letters, digits, underscores), required before/inside {VAR} and
	// {VAR:-default} placeholders.
	validPlaceholderName := func(s string) bool {
		if s == "" {
			return false
		}
		for _, c := range s {
			if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
				return false
			}
		}
		return true
	}
	// First expand ${VAR} / $VAR via standard os.ExpandEnv.
	v = os.ExpandEnv(v)
	// Then expand {VAR} and {VAR:-default} placeholders.
	var sb strings.Builder
	i := 0
	for i < len(v) {
		if v[i] == '{' {
			end := strings.IndexByte(v[i:], '}')
			if end < 0 {
				sb.WriteByte(v[i])
				i++
				continue
			}
			expr := v[i+1 : i+end]
			// Check for :- default separator.
			name, def := expr, ""
			if idx := strings.Index(expr, ":-"); idx >= 0 {
				name, def = expr[:idx], expr[idx+2:]
			}
			// Only treat as a placeholder when the name is a valid
			// identifier; otherwise (e.g. JSON braces) emit '{' verbatim
			// and keep scanning so inner placeholders still expand.
			if !validPlaceholderName(name) {
				sb.WriteByte(v[i])
				i++
				continue
			}
			i += end + 1
			// pass: exclude strings is pass
			// 全小写名字不是 kvs 变量（约定变量名含大写）—— 花括号属于下游
			if slices.Contains(excludes, name) || name == strings.ToLower(name) {
				sb.WriteByte('{')
				sb.WriteString(name)
				sb.WriteByte('}')
				continue
			}
			val := os.Getenv(name)
			if val == "" {
				val = def
			}
			sb.WriteString(val)
		} else {
			sb.WriteByte(v[i])
			i++
		}
	}
	return sb.String()
}

// parseIni reads and parses an INI file into an iniFile.
//
// Format:
//   - Lines of the form key = value (whitespace trimmed).
//   - Empty lines and lines starting with # or ; are comments.
//   - [section] headers group keys ([headers], [proxies.<name>],
//     [service.<name>] are consumed via the section-aware views).
//   - Values support {VAR} and {VAR:-default} placeholders (expansion is
//     deferred to LoadInitConfig so the reverse-assigned KVS_SVC_<NAME>_*
//     variables are already visible).
//   - Multi-line values: trailing " \" continues to next line.
//   - Multi-line JSON: a value starting with "{" whose braces do not balance
//     on the same line keeps consuming lines (comment-only lines skipped)
//     until they balance; missing commas between members are repaired so
//     hand-written blocks stay valid JSON.
//   - Array values: key = [item1, item2, item3] → stored as []string.
//     Items are trimmed; quoted items ("..." / '...') are unquoted.
func parseIni(path string) (*iniFile, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return parseIniData(data, path)
}

// parseIniData parses INI content from a byte slice. source is used only for
// error messages (e.g. "kvs.ini:3: ..."). This allows loading config from the
// embedded kvs.ini without a temp file.
func parseIniData(data []byte, source string) (*iniFile, error) {
	ini := &iniFile{
		props:   make([][2]string, 0, 64),
		index:   make(map[string]int),
		secKeys: make(map[string][][2]string),
	}
	section := "" // current section, "" = top-level
	lines := strings.Split(string(data), "\n")

	// Pre-process: join lines ending with backslash (line continuation).
	// The backslash is removed and the next line (trimmed of leading
	// whitespace) is appended with a single space separator.
	var joined []string
	for _, l := range lines {
		trimmed := strings.TrimRight(l, " \t\r")
		// Continuation: the previous joined line must end with " \"
		// (space + backslash). A bare trailing "\" (e.g. Windows path)
		// is NOT treated as a continuation.
		if len(joined) > 0 && strings.HasSuffix(joined[len(joined)-1], " \\") {
			// Remove the " \" suffix, add a single space, then the
			// continuation line with leading whitespace collapsed.
			prev := strings.TrimSuffix(joined[len(joined)-1], " \\")
			cont := strings.TrimLeft(trimmed, " \t")
			joined[len(joined)-1] = prev + " " + cont
		} else {
			joined = append(joined, trimmed)
		}
	}

	store := func(section, k, v string) {
		ini.set(iniKey(section, strings.ToLower(k)), v)
		ini.setSection(section, k, v)
	}

	for lineNo := 0; lineNo < len(joined); lineNo++ {
		raw := joined[lineNo]
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		// Section header.
		if line[0] == '[' && line[len(line)-1] == ']' {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		// key = value — split at the first "=" outside [brackets] so a
		// "[k=v,…]" route gate stays on the key side.
		k, v, ok := cutKVOutsideBrackets(line)
		if !ok {
			return nil, fmt.Errorf("%s:%d: missing '=' in %q", source, lineNo+1, raw)
		}
		k = strings.TrimSpace(k)
		v = strings.TrimSpace(v)
		if k == "" {
			continue
		}
		// Quote handling: "value" and 'value' are stripped; mismatched
		// or dangling quotes are an error.
		if len(v) >= 2 {
			first, last := v[0], v[len(v)-1]
			if first == '"' || first == '\'' {
				// Opening quote: must have a matching close.
				if last != first {
					return nil, fmt.Errorf("%s:%d: unterminated quote %c in %q", source, lineNo+1, first, raw)
				}
				v = v[1 : len(v)-1]
			} else if last == '"' || last == '\'' {
				// Closing quote without opening.
				return nil, fmt.Errorf("%s:%d: dangling quote %c in %q", source, lineNo+1, last, raw)
			}
		} else if len(v) == 1 && (v[0] == '"' || v[0] == '\'') {
			return nil, fmt.Errorf("%s:%d: lone quote in %q", source, lineNo+1, raw)
		}
		// Multi-line JSON object: value starts with "{" and the braces do
		// not balance on this line. Consume lines until balanced (comment
		// lines skipped) so hand-written blocks like
		//   actions = {
		//       "name": "cmd"
		//   }
		// parse as one value.
		if strings.HasPrefix(v, "{") && !jsonBracesBalanced(v) {
			var block []string
			block = append(block, v)
			for lineNo++; lineNo < len(joined); lineNo++ {
				nl := strings.TrimSpace(joined[lineNo])
				if nl == "" || nl[0] == '#' || nl[0] == ';' {
					continue
				}
				block = append(block, nl)
				if jsonBracesBalanced(strings.Join(block, "\n")) {
					break
				}
			}
			v = repairJSONCommas(strings.Join(block, "\n"))
			store(section, k, v)
			continue
		}
		// Array value: [item1, item2, item3] — parse at read time and store
		// as a space-joined string. Variable expansion is done later by
		// loadInitConfig (the join is transparent to shell -c execution).
		if len(v) >= 2 && v[0] == '[' && v[len(v)-1] == ']' {
			items, err := parseArray(v, source, lineNo+1, raw)
			if err != nil {
				return nil, err
			}
			store(section, k, strings.Join(items, " "))
			continue
		}
		// Store with section prefix; variable expansion is deferred to
		// LoadInitConfig so reverse-assigned vars are visible.
		store(section, k, v)
	}
	return ini, nil
}

// jsonBracesBalanced reports whether every "{" in s has a matching "}",
// ignoring braces inside double-quoted strings (with backslash escapes).
func jsonBracesBalanced(s string) bool {
	depth := 0
	inStr := false
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			if c == '\\' {
				i++ // skip escaped character
			} else if c == '"' {
				inStr = false
			}
			continue
		}
		switch c {
		case '"':
			inStr = true
		case '{':
			depth++
		case '}':
			depth--
			if depth < 0 {
				return false
			}
		}
	}
	return depth == 0 && !inStr
}

// repairJSONCommas inserts missing commas between members of a hand-written
// JSON object: inside an object, a string immediately followed by another
// string (newlines/whitespace only in between) gets a "," inserted. Values
// like {"a": "x"\n"b": "y"} become valid JSON. Strings are respected, so
// braces or colons inside values never trigger a repair.
func repairJSONCommas(s string) string {
	type frame struct {
		obj bool // true = object, false = array
	}
	var stack []frame
	var out strings.Builder
	inStr := false
	prev := byte(0) // last significant char outside strings
	isWS := func(c byte) bool { return c == ' ' || c == '\t' || c == '\r' || c == '\n' }
	for i := 0; i < len(s); i++ {
		c := s[i]
		if inStr {
			out.WriteByte(c)
			if c == '\\' && i+1 < len(s) {
				i++
				out.WriteByte(s[i])
			} else if c == '"' {
				inStr = false
				prev = '"'
			}
			continue
		}
		switch c {
		case '"':
			// Missing comma between object members: the previous significant
			// char is the closing quote of a value and this quote opens the
			// next key.
			if len(stack) > 0 && stack[len(stack)-1].obj && prev == '"' {
				out.WriteByte(',')
				out.WriteByte('\n')
			}
			inStr = true
			out.WriteByte(c)
		case '{':
			stack = append(stack, frame{obj: true})
			out.WriteByte(c)
			prev = c
		case '[':
			stack = append(stack, frame{obj: false})
			out.WriteByte(c)
			prev = c
		case '}', ']':
			if len(stack) > 0 {
				stack = stack[:len(stack)-1]
			}
			out.WriteByte(c)
			prev = c
		default:
			out.WriteByte(c)
			if !isWS(c) {
				prev = c
			}
		}
	}
	return out.String()
}

// parseArray parses "[item1, item2, item3]" into a []string.
// The outer brackets are already verified by the caller; v includes them.
// Items are split on commas, trimmed, and unquoted (if wrapped in " or ').
// Commas inside quotes are preserved. An empty array "[]" yields []string{}.
func parseArray(v, path string, lineNo int, raw string) ([]string, error) {
	inner := strings.TrimSpace(v[1 : len(v)-1])
	if inner == "" {
		return []string{}, nil
	}
	var items []string
	var cur strings.Builder
	inQuote := byte(0) // 0 = not in quote, otherwise the quote char
	for i := 0; i < len(inner); i++ {
		c := inner[i]
		if inQuote != 0 {
			cur.WriteByte(c)
			if c == inQuote {
				inQuote = 0
			}
			continue
		}
		switch c {
		case '"', '\'':
			inQuote = c
			cur.WriteByte(c)
		case ',':
			items = append(items, strings.TrimSpace(cur.String()))
			cur.Reset()
		default:
			cur.WriteByte(c)
		}
	}
	// Last item (or unterminated quote).
	last := strings.TrimSpace(cur.String())
	if last != "" || len(items) > 0 {
		items = append(items, last)
	}
	if inQuote != 0 {
		return nil, fmt.Errorf("%s:%d: unterminated quote %c in array %q", path, lineNo, inQuote, raw)
	}
	// Unquote items.
	for i, s := range items {
		items[i] = unquoteValue(s)
	}
	return items, nil
}

// unquoteValue strips surrounding " or ' from s if present.
func unquoteValue(s string) string {
	if len(s) >= 2 {
		if (s[0] == '"' && s[len(s)-1] == '"') || (s[0] == '\'' && s[len(s)-1] == '\'') {
			return s[1 : len(s)-1]
		}
	}
	return s
}

// strProp returns the string value for a top-level key, falling back to
// def when the key is absent or empty.
func strProp(ini *iniFile, key, def string) string {
	if v := ini.get(key); v != "" {
		return v
	}
	return def
}

// parseBool returns the boolean value for s, falling back to def when s is
// empty. Accepts 1/0, true/false (case-insensitive).
func parseBool(s string, def bool) bool {
	if s == "" {
		return def
	}
	return s == "1" || strings.EqualFold(s, "true")
}

// parseIntDefault 解析整数配置项：空值或非法值都返回 def（只告警，不中断启动）。
func parseIntDefault(s string, def int) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return def
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Printf("WARNING: invalid integer %q, using default %d", s, def)
		return def
	}
	return n
}

// parseTimeout parses a login_timeout value (seconds). Empty or "0" → 0
// (session lifetime). Negative values cause a fatal error at startup.
func parseTimeout(s string) int {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0
	}
	n, err := strconv.Atoi(s)
	if err != nil {
		log.Fatalf("invalid login_timeout %q: must be an integer (seconds)", s)
	}
	if n < 0 {
		log.Fatalf("invalid login_timeout %q: must be >= 0 (0=session, >0=expiring)", s)
	}
	return n
}

// resolveVersion resolves the application version and its hash for one
// service (the results are reverse-assigned to KVS_SVC_<NAME>_VERSION /
// _VERHASH by resolveService).
//
//   - If a version override is registered for svcKey (/__restart?v=VERSION),
//     it is used directly.
//   - Else if version is non-empty, it is used directly.
//   - If version is empty, versionLatestURL is fetched. The URL may contain a
//     "#field" suffix; if present, the response is treated as JSON and the named
//     field is extracted. Otherwise the response body (trimmed) is the version.
//   - If versionHashURL is empty, the version hash equals the version.
//   - If versionHashURL is set, it is fetched after VERSION substitution
//     (the {VERSION} brace form and the bare VERSION token, e.g.
//     .../api/versions/VERSION/...) and the same "#field" extraction applies.
//
// Returns (version, versionHash, error).
func resolveVersion(svcKey, version, versionLatestURL, versionHashURL string) (string, string, error) {
	// Step 1: Resolve the version.
	// Priority: /__restart override > version (config) > versionLatestURL (fetched).
	if v := GetOverrideVersion(svcKey); v != "" {
		version = v
		log.Printf("[version %s] version=%s (from override)", svcKey, version)
	} else if version == "" && versionLatestURL != "" {
		v, err := fetchJSONField(versionLatestURL)
		if err != nil {
			return "", "", fmt.Errorf("fetch version %w", err)
		}
		version = v
	}
	if version == "" {
		return "", "", fmt.Errorf("version is empty and version_latest_url is not configured")
	}
	log.Printf("[version %s] version=%s", svcKey, version)

	// Step 2: Resolve the version hash.
	if versionHashURL == "" {
		log.Printf("[version %s] version_hash_url not set, hash=version=%s", svcKey, version)
		return version, version, nil
	}

	// Substitute VERSION in the hash URL: the {VERSION} brace form and the
	// bare VERSION token (e.g. .../api/versions/VERSION/...).
	hashURL := strings.ReplaceAll(versionHashURL, "{VERSION}", version)
	hashURL = strings.ReplaceAll(hashURL, "VERSION", version)
	vh, err := fetchJSONField(hashURL)
	if err != nil {
		return "", "", fmt.Errorf("fetch version hash: %w", err)
	}
	if vh == "" {
		vh = version
		log.Printf("[version %s] version hash empty, falling back to version=%s", svcKey, version)
	}
	log.Printf("[version %s] version hash=%s", svcKey, vh)
	return version, vh, nil
}

// fetchJSONField fetches a URL and extracts a value from the response.
// If the URL contains a "#field" fragment, the response is treated as JSON and
// the named field is extracted. Otherwise, the response body (trimmed) is
// returned as-is.
func fetchJSONField(rawURL string) (string, error) {
	// Split URL and #field fragment.
	field := ""
	urlStr := rawURL
	if idx := strings.Index(rawURL, "#"); idx >= 0 {
		urlStr = rawURL[:idx]
		field = rawURL[idx+1:]
	}

	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(urlStr)
	if err != nil {
		return "", fmt.Errorf("fetch %s: %w", urlStr, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("%s returned HTTP %d", urlStr, resp.StatusCode)
	}

	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return "", fmt.Errorf("read %s: %w", urlStr, err)
	}

	// No field specified — return the raw body (trimmed).
	if field == "" {
		return strings.TrimSpace(string(body)), nil
	}

	// Extract the named field from JSON.
	var result map[string]any
	if err := json.Unmarshal(body, &result); err != nil {
		return "", fmt.Errorf("decode JSON from %s: %w", urlStr, err)
	}
	val, _ := result[field].(string)
	return val, nil
}

// flagValue returns the value of a -X flag (supports both "-X val" and "-X=val").
// Returns "" if the flag is absent.
func flagValue(name string) string {
	for i, arg := range os.Args[1:] {
		if arg == name && i+1 < len(os.Args)-1 {
			return os.Args[i+2]
		}
		if val, ok := strings.CutPrefix(arg, name+"="); ok {
			return val
		}
	}
	return ""
}

// cutKVOutsideBrackets splits "key=value" at the first "=" that is not inside
// [brackets]. Route prefixes may end in a "[k=v,…]" query gate whose "=" must
// stay on the key side — both for kvs.ini lines and -n inline entries. When
// every "=" sits inside an unclosed "[" (a key with an unbalanced bracket and
// an "=" — the pre-gate parser handled those by splitting at the first "="),
// fall back to exactly that so the whole config never fails to load.
func cutKVOutsideBrackets(s string) (string, string, bool) {
	depth := 0
	for i := 0; i < len(s); i++ {
		switch s[i] {
		case '[':
			depth++
		case ']':
			if depth > 0 {
				depth--
			}
		case '=':
			if depth == 0 {
				return s[:i], s[i+1:], true
			}
		}
	}
	return strings.Cut(s, "=")
}

// parseProxiesArg parses the -n flag value into [][2]string pairs matching the
// format returned by ini.tableEntries. The value uses ";" to separate
// entries, each of the form "prefix=url".
//
// Example: "/__healthz=text://OK:@now;/=http://127.0.0.1:8080"
//
// → [["proxies./__healthz", "text://OK:@now"], ["proxies./", "http://127.0.0.1:8080"]]
func parseProxiesArg(s string) [][2]string {
	var entries [][2]string
	for _, part := range strings.Split(s, ";") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		k, v, ok := cutKVOutsideBrackets(part)
		if !ok || strings.TrimSpace(k) == "" {
			log.Printf("WARNING: skipping invalid -n entry %q (expected prefix=url)", part)
			continue
		}
		entries = append(entries, [2]string{"proxies." + strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return entries
}

// loadIni resolves the config source from the -c flag and returns the parsed
// INI file and the source label (for logging).
//
// -c is optional and takes a FILE PATH only (no preset names): absent → the
// embedded kvs.ini; present → that file (fatal when missing). The route table
// is always selected with -n :<name> (or inline -n / KVS_PROXIES entries);
// with no -n the first [proxies.<name>] table in file order is used.
func loadIni() (*iniFile, string) {
	// No -c → the embedded kvs.ini.
	if cfgPath := flagValue("-c"); cfgPath == "" {
		log.Printf("loading config: embed kvs.ini")
		ini, err := parseIniData(MustAsset("kvs.ini"), "kvs.ini")
		if err != nil {
			log.Fatalf("parse embedded config: %v", err)
		}
		return ini, "embed"
	} else if resolved := resolveConfigPath(cfgPath); resolved != "" {
		log.Printf("loading config: %s", resolved)
		ini, err := parseIni(resolved)
		if err != nil {
			log.Fatalf("parse config: %v", err)
		}
		return ini, resolved
	} else {
		log.Fatalf("config file not found: %s", cfgPath)
		return nil, ""
	}
}

// LoadInitConfig loads kvs.ini and resolves the full Config.
func LoadInitConfig() Config {
	ini, source := loadIni()

	// Route source selection, highest priority first:
	//   1. -n ":name"        → named [proxies.<name>] table
	//   2. -n "p=u;p=u"      → inline routes
	//   3. KVS_PROXIES env   → inline routes
	//   4. otherwise         → first table in file order
	table := ""
	var proxyEntries [][2]string
	nFlag := flagValue("-n")
	switch {
	case strings.HasPrefix(nFlag, ":"):
		table = strings.TrimPrefix(nFlag, ":")
	case nFlag != "" && strings.Contains(nFlag, "="):
		proxyEntries = parseProxiesArg(nFlag)
	case nFlag != "":
		table = nFlag
	case os.Getenv("KVS_PROXIES") != "":
		proxyEntries = parseProxiesArg(os.Getenv("KVS_PROXIES"))
	default:
		tables := ini.proxyTables()
		if len(tables) == 0 {
			log.Fatal("config error: [proxies] section is required in kvs.ini")
		}
		table = tables[0]
	}
	if proxyEntries == nil {
		proxyEntries = ini.tableEntries(table)
		if len(proxyEntries) == 0 {
			log.Fatalf("config error: route table [proxies.%s] not found or empty (available: %v)", table, ini.proxyTables())
		}
	}
	log.Printf("config source: %s, route table: %q (available: %v)", source, table, ini.proxyTables())

	var cfg Config
	cfg.Services = map[string]*ServiceConfig{}

	// --- Top-level home / port with reverse env assignment -----------------
	// home = {KVS_HOME:-/app/.kvs}: when KVS_HOME is unset the resolved value
	// is written back so service defaults (KVS_HOME/<name>) and later
	// expansions see it. Same for KVS_PORT.
	cfg.Home = expandValue(strProp(ini, "home", ""))
	if cfg.Home == "" {
		cfg.Home = "/app/.kvs"
	}
	if os.Getenv("KVS_HOME") == "" {
		os.Setenv("KVS_HOME", cfg.Home)
		log.Printf("KVS_HOME not set, assigned to %s", cfg.Home)
	} else {
		cfg.Home = os.Getenv("KVS_HOME")
	}
	cfg.Port = expandValue(strProp(ini, "port", "8080"))
	if os.Getenv("KVS_PORT") == "" && cfg.Port != "" {
		os.Setenv("KVS_PORT", cfg.Port)
	}

	// --- cache_dir / cache_path (top-level keys) ---------------------------
	cfg.CacheDir = expandValue(strProp(ini, "cache_dir", ""))
	if cfg.CacheDir != "" {
		os.Setenv("CACHE_DIR", cfg.CacheDir)
	}
	cfg.CachePath = expandValue(strProp(ini, "cache_path", ""))

	// --- Named service sections (raw) --------------------------------------
	sections := map[string]*ServiceConfig{}
	serviceOrder := make([]string, 0, 4)
	for _, kv := range ini.serviceSections() {
		sc := &ServiceConfig{Name: kv[0]}
		for _, e := range ini.secKeys[kv[1]] {
			sc.setRaw(e[0], e[1])
		}
		sections[sc.Name] = sc
		serviceOrder = append(serviceOrder, sc.Name)
	}

	// --- Referenced services: svc://<name> targets of the selected table ---
	// A [service.<name>] section is only parsed when the selected table
	// references it via svc://<name>; every other section stays dormant (no
	// env reverse-assignment, no version fetch). Referenced sections must be
	// resolved BEFORE the proxy values are expanded, because routes reference
	// the reverse-assigned KVS_SVC_<NAME>_* vars.
	var svcRefs []string
	seenRef := map[string]bool{}
	for _, e := range proxyEntries {
		target := strings.TrimSpace(e[1])
		target = strings.TrimLeft(target, "-+")
		if name, ok := strings.CutPrefix(target, "svc://"); ok {
			// svc://<name>.<ext> 引用的仍是服务 <name>（扩展随服务解析）
			if name = strings.TrimSpace(name); name != "" {
				if svcName, _, _ := SplitSvcTarget(name); svcName != "" && !seenRef[svcName] {
					seenRef[svcName] = true
					svcRefs = append(svcRefs, svcName)
				}
			}
		}
	}
	for _, name := range svcRefs {
		if _, ok := sections[name]; !ok {
			log.Fatalf("config error: route references svc://%s but no [service.%s] section exists", name, name)
		}
		cfg.Services[name] = sections[name]
	}
	if len(svcRefs) > 0 {
		log.Printf("services in use: %v (available: %v)", svcRefs, serviceOrder)
	}
	for _, name := range svcRefs {
		cfg.resolveService(cfg.Services[name])
		cfg.Services[name].Extensions = svcExtensions(name) // svc://<name>.<ext> 扩展
	}

	// --- Expand [headers] and proxy routes (after all KVS_SVC_* vars) ----
	headerEntries := ini.hasPrefix("headers.")
	for i, kv := range headerEntries {
		headerEntries[i][1] = expandValue(kv[1])
	}

	// Expand the selected route table's targets now that every service env
	// var (KVS_SVC_<NAME>_*) is reverse-assigned.
	for i, kv := range proxyEntries {
		proxyEntries[i][1] = expandValue(kv[1])
	}

	// Build []proxyEntry from the expanded pairs.
	cfg.Proxies = parseBackends(proxyEntries)
	if len(cfg.Proxies) == 0 {
		log.Fatal("config error: [proxies] parsed to zero backends")
	}

	// Build headers map from the expanded headers.* pairs.
	cfg.Headers = make(map[string]string, len(headerEntries))
	for _, kv := range headerEntries {
		cfg.Headers[strings.TrimPrefix(kv[0], "headers.")] = kv[1]
	}

	// [cache_headers]: 附加到缓存响应的头（CORS 等）。空值等于「不设置」。
	// 键在 ini 里被折叠为小写，写回响应时由 net/http 规范化为标准大小写。
	cacheHeaderEntries := ini.hasPrefix("cache_headers.")
	cfg.CacheHeaders = make(map[string]string, len(cacheHeaderEntries))
	for _, kv := range cacheHeaderEntries {
		if v := expandValue(kv[1]); v != "" {
			cfg.CacheHeaders[strings.TrimPrefix(kv[0], "cache_headers.")] = v
		}
	}

	// Top-level fields (always resolved, even in -n mode).
	cfg.CookieTknName = expandValue(strProp(ini, "cookie_token_key", "kvs"))
	cfg.QueryTokenKey = expandValue(strProp(ini, "query_token_key", ""))
	cfg.LoginAuthz = parseBool(expandValue(strProp(ini, "login_authz", "")), false)
	cfg.LoginToken = expandValue(strProp(ini, "login_token", ""))
	cfg.LoginTimeout = parseTimeout(expandValue(strProp(ini, "login_timeout", "0")))
	cfg.TokenTimeout = parseTimeout(expandValue(strProp(ini, "token_timeout", "300")))
	cfg.UseSSL = parseBool(expandValue(strProp(ini, "use_ssl", "")), false)
	// 反向代理调优（默认值见 pkg/transport.go / pkg/wsproxy.go）
	cfg.ProxyIdleConnsPerHost = parseIntDefault(expandValue(strProp(ini, "proxy_idle_conns_per_host", "")), 0)
	cfg.ProxyMaxIdleConns = parseIntDefault(expandValue(strProp(ini, "proxy_max_idle_conns", "")), 0)
	cfg.ProxyDisableCompression = parseBool(expandValue(strProp(ini, "proxy_disable_compression", "")), true)
	cfg.ProxyFlushInterval = parseIntDefault(expandValue(strProp(ini, "proxy_flush_interval", "")), 100)
	cfg.WSProxy = expandValue(strProp(ini, "ws_proxy", ""))
	cfg.WSIdleTimeout = parseIntDefault(expandValue(strProp(ini, "ws_idle_timeout", "")), 0)
	cfg.ReadTimeout = parseIntDefault(expandValue(strProp(ini, "read_timeout", "")), 60)
	cfg.TunnelID = strings.TrimSpace(expandValue(strProp(ini, "tunnel_id", "")))

	// 免 authz 清单由路由派生（path_public 配置键已取消）：[proxies] 里 !
	// 前缀的路由，前缀/后缀分别登记。collectAuthFree 见下。
	collectAuthFree(&cfg)

	return cfg
}

// collectAuthFree derives the auth-exempt list from the !-prefixed routes of
// the proxy table (path_public key removed). PREFIX routes only: combining !
// with a suffix (*), regex or full-domain route is rejected with a warning
// and the marker is ignored — the route keeps requiring auth.
func collectAuthFree(cfg *Config) {
	for _, b := range cfg.Proxies {
		if !b.AuthFree {
			continue
		}
		switch {
		case b.Suffix != "":
			log.Printf("WARNING: backend %q: auth-free marker (!) does not support suffix routes, exemption ignored (auth still required)", b.RawURL)
		case b.IsRegex || strings.HasPrefix(b.Source, "http://") || strings.HasPrefix(b.Source, "https://"):
			log.Printf("WARNING: backend %q: auth-free marker (!) does not support regex/full-domain routes, exemption ignored (auth still required)", b.RawURL)
		default:
			cfg.PathPublic = append(cfg.PathPublic, b.Source)
		}
	}
}

// setRaw stores a raw section value on the service (lower-cased key, as the
// ini parser folds keys).
func (sc *ServiceConfig) setRaw(key, value string) {
	switch strings.ToLower(key) {
	case "address":
		sc.Address = value
	case "healthz":
		sc.Healthz = value
	case "home":
		sc.Home = value
	case "version":
		sc.Version = value
	case "version_base_url":
		sc.VersionBaseURL = value
	case "version_latest_url":
		sc.VersionLatestURL = value
	case "version_hash_url":
		sc.VersionHashURL = value
	case "download":
		sc.Download = value
	case "download_info":
		sc.DownloadInfo = value
	case "download_field_url":
		sc.DownloadFieldURL = value
	case "download_proxy":
		sc.DownloadProxy = value
	case "bin_home":
		sc.BinHome = value
	case "once_shell":
		sc.OnceShell = value
	case "init_shell":
		sc.InitShell = value
	case "stop_shell":
		sc.StopShell = value
	case "upstream_token":
		sc.UpstreamTokenSpec = value
	case "upstream_headrs", "upstream_headers":
		// headrs 为用户既有的键名拼写，upstream_headers 一并接受。
		sc.UpstreamHeadersRaw = value
	case "upstream_cookies":
		sc.UpstreamCookiesRaw = value
	case "command":
		sc.Command = value
	case "vsc_agents_cmd":
		sc.VscAgentsCmd = value
	case "vsc_agent_cmds":
		sc.VscAgentCmdsRaw = value
	case "vsc_agents_dir":
		sc.VscAgentsDir = value
	case "vsc_agent_args":
		sc.VscAgentArgs = value
	case "vsc_language":
		sc.VscLanguageRaw = value
	case "vsc_open_url":
		sc.VscOpenURL = value
	case "actions":
		sc.ActionsRaw = value
	case "envs_default":
		sc.EnvsDefaultRaw = value
	}
}

// resolveService expands one named service section in dependency order and
// reverse-assigns the KVS_SVC_<NAME>_* environment variables so later values
// (healthz, bin_home, command, proxy routes) can reference them:
//
//	KVS_SVC_<N>_HOME      env or {KVS_HOME}/<name>
//	KVS_SVC_<N>_ADDR      resolved address (also _HOST/_PORT or _SOCK)
//	KVS_SVC_<N>_VERSION   resolved version (from override/config/latest_url)
//	KVS_SVC_<N>_VERHASH   resolved version hash (defaults to version)
//	KVS_SVC_<N>_BIN_HOME  resolved bin_home
func (c *Config) resolveService(sc *ServiceConfig) {
	name := envSuffix(sc.Name)
	envKey := func(suffix string) string { return "KVS_SVC_" + name + "_" + suffix }
	logIf := func(format string, args ...any) {
		log.Printf("[service "+sc.Name+"] "+format, args...)
	}

	// 1. home → KVS_SVC_<N>_HOME (default {KVS_HOME}/<name>), resolved value
	//    written back for later placeholders.
	sc.Home = expandValue(sc.Home)
	if sc.Home == "" {
		sc.Home = filepath.Join(c.Home, sc.Name)
		logIf("home not set, defaulting to %s", sc.Home)
	}
	os.Setenv(envKey("HOME"), sc.Home)

	// 2. address → reverse-assign ADDR + HOST/PORT (http) or SOCK (unix).
	sc.Address = expandValue(sc.Address)
	if sc.Address == "" {
		sc.Address = os.Getenv(envKey("ADDR"))
	}
	if sc.Address != "" {
		os.Setenv(envKey("ADDR"), sc.Address)
		if u, err := url.Parse(sc.Address); err == nil {
			switch u.Scheme {
			case "http", "https":
				if h := u.Hostname(); h != "" {
					os.Setenv(envKey("HOST"), h)
				}
				if p := u.Port(); p != "" {
					os.Setenv(envKey("PORT"), p)
				}
			case "unix":
				// url.Parse eats the first path segment as a host
				// ("unix://../../x" → host "..", path "/../x"), breaking
				// relative socket paths — cut the scheme manually, the same
				// way the backend and probe paths do.
				if _, sock, ok := strings.Cut(sc.Address, "://"); ok && sock != "" {
					os.Setenv(envKey("SOCK"), sock)
				}
			}
		}
		logIf("address: %s", sc.Address)
	}

	// 3. healthz — liveness probe; falls back to the address (connectivity).
	sc.Healthz = expandValue(sc.Healthz)
	if sc.Healthz == "" {
		sc.Healthz = sc.Address
	}

	// 4. version chain → resolve → reverse-assign VERSION/VERHASH.
	sc.Version = expandValue(sc.Version)
	sc.VersionBaseURL = expandValue(sc.VersionBaseURL)
	os.Setenv(envKey("DOWNLOAD_BASE"), sc.VersionBaseURL)
	sc.VersionLatestURL = expandValue(sc.VersionLatestURL)
	// The VERSION token is substituted by resolveVersion, not here — keep the
	// {VERSION} brace form intact for it.
	sc.VersionHashURL = expandValue(sc.VersionHashURL, "VERSION")

	sc.Download = expandValue(sc.Download)
	// The chain also runs on a version OVERRIDE alone (manager card / __restart
	// pin): services without download/latest_url config must still honor
	// "start this version next time" — the override switches bin_home etc.
	if sc.Download != "" || sc.VersionLatestURL != "" || GetOverrideVersion(sc.Name) != "" {
		version, vh, err := resolveVersion(sc.Name, sc.Version, sc.VersionLatestURL, sc.VersionHashURL)
		if err != nil {
			sc.InitError = fmt.Sprintf("resolve version %v", err)
			log.Printf("WARNING: [service %s] %s", sc.Name, sc.InitError)
		} else {
			sc.Version, sc.VersionHash = version, vh
		}
	}
	if sc.VersionHash == "" {
		sc.VersionHash = sc.Version
	}
	// Never write an empty version back: KVS_SVC_<NAME>_VERSION is the last
	// known good value that bin_home etc. expand from — an empty overwrite
	// yields …/servers//bin style paths. Empty means "no source this pass",
	// not "forget the version".
	if sc.Version == "" {
		if v := os.Getenv(envKey("VERSION")); v != "" {
			sc.Version, sc.VersionHash = v, v
			log.Printf("[version %s] no source this pass, keeping last known version %s", sc.Name, v)
		}
	}
	if sc.Version != "" {
		os.Setenv(envKey("VERSION"), sc.Version)
		os.Setenv(envKey("VERHASH"), sc.VersionHash)
	}

	// Substitute the bare VERSION/VERHASH tokens in the download URL. VERHASH
	// goes first — its name contains VERSION as a substring. This covers
	// .../commit:VERHASH/... and .../api/versions/VERSION/... style URLs.
	sc.Download = strings.ReplaceAll(sc.Download, "VERHASH", sc.VersionHash)
	sc.Download = strings.ReplaceAll(sc.Download, "VERSION", sc.Version)
	if sc.Download != "" {
		logIf("download: %s", sc.Download)
	} else {
		logIf("download url: empty")
	}

	// 5. download_info / download_field_url / download_proxy.
	sc.DownloadInfo = expandValue(sc.DownloadInfo)
	sc.DownloadFieldURL = expandValue(sc.DownloadFieldURL)
	if sc.DownloadFieldURL == "" {
		sc.DownloadFieldURL = "url"
	}
	sc.DownloadProxy = expandValue(sc.DownloadProxy)

	// 6. bin_home → KVS_SVC_<N>_BIN_HOME (default: service home), so later
	//    values (command, scripts, routes) can reference it.
	sc.BinHome = expandValue(sc.BinHome)
	if strings.ContainsRune(sc.BinHome, '{') {
		sc.BinHome = expandValue(sc.BinHome)
	}
	if sc.BinHome == "" {
		sc.BinHome = sc.Home
	}
	os.Setenv(envKey("BIN_HOME"), sc.BinHome)

	// 7. scripts and command (double brace pass for nested placeholders).
	sc.OnceShell = expandValue(sc.OnceShell)
	sc.InitShell = expandValue(sc.InitShell)
	sc.StopShell = expandValue(sc.StopShell)
	sc.Command = expandValue(sc.Command)
	if strings.ContainsRune(sc.Command, '{') {
		sc.Command = expandValue(sc.Command)
	}
	if sc.Command != "" {
		logIf("command: %s", sc.Command)
	}

	// 8. vscode-specific options and the actions table.
	if raw := sc.VscLanguageRaw; raw != "" {
		// NOT expanded via expandValue: the {…} JSON braces would be
		// mistaken for {VAR} placeholders and destroyed.
		var m map[string]string
		if err := json.Unmarshal([]byte(raw), &m); err != nil {
			log.Printf("WARNING: [service %s] invalid vsc_language JSON: %v", sc.Name, err)
		} else {
			sc.VscLanguage = m
		}
	}
	sc.VscAgentsCmd = expandValue(sc.VscAgentsCmd)
	if raw := sc.VscAgentCmdsRaw; raw != "" {
		// Values carry {KVS_SVC_*_HOME}/{KVS_SVC_*_BIN_HOME} placeholders, so
		// expand FIRST (JSON keys are not valid identifiers and pass through
		// verbatim), then unmarshal.
		var m map[string]string
		if err := json.Unmarshal([]byte(expandValue(raw)), &m); err != nil {
			log.Printf("WARNING: [service %s] invalid vsc_agent_cmds JSON: %v", sc.Name, err)
		} else {
			sc.VscAgentCmds = m
		}
	}
	sc.VscAgentsDir = expandValue(sc.VscAgentsDir)
	sc.VscAgentArgs = expandValue(sc.VscAgentArgs)
	sc.VscOpenURL = expandValue(sc.VscOpenURL)
	if raw := sc.ActionsRaw; raw != "" {
		m := map[string]string{}
		if err := json.Unmarshal([]byte(expandValue(raw)), &m); err != nil {
			log.Printf("WARNING: [service %s] invalid actions JSON: %v", sc.Name, err)
		} else {
			sc.Actions = m
		}
	}

	// 9. envs_default — fill UNSET/empty system env vars with defaults. Runs
	// LAST so the defaults can reference every reverse-assigned service
	// variable ({KVS_SVC_<NAME>_*}); already-set values are never overridden.
	// The values land in the process environment, so the backend subprocess
	// and the once/init/stop scripts inherit them.
	if raw := sc.EnvsDefaultRaw; raw != "" {
		// Expand FIRST (JSON keys pass through verbatim), then apply — same
		// pattern as vsc_agent_cmds/actions above.
		var m map[string]string
		if err := json.Unmarshal([]byte(expandValue(raw)), &m); err != nil {
			log.Printf("WARNING: [service %s] invalid envs_default JSON: %v", sc.Name, err)
		} else {
			for name, def := range m {
				if def == "" {
					continue
				}
				if os.Getenv(name) == "" {
					os.Setenv(name, def)
					logIf("env default: %s=%s", name, def)
				}
			}
		}
	}

	// 10. upstream_* — 上游增强（子进程生命周期）。headers/cookies 只解析 JSON
	// 结构，值保留模板、每请求展开（token 占位符随截取即时生效）；token 的
	// pattern 现在展开（ADDR/HOST 此时已定），[key] 非法即告警停用。env 里的
	// 上一代 token 回填进 sc：配置重载不丢 token（子进程还活着，token 仍有效）。
	sc.UpstreamHeaders = parseUpstreamMap(sc.Name, "upstream_headrs", sc.UpstreamHeadersRaw)
	sc.UpstreamCookies = parseUpstreamMap(sc.Name, "upstream_cookies", sc.UpstreamCookiesRaw)
	sc.UpstreamTokenPattern, sc.UpstreamTokenKey = splitUpstreamTokenSpec(expandValue(sc.UpstreamTokenSpec))
	if sc.UpstreamTokenSpec != "" && sc.UpstreamTokenKey == "" {
		log.Printf("WARNING: [service %s] upstream_token %q: expected \"<log prefix>[<query key>]\" with an identifier in [], capture disabled", sc.Name, sc.UpstreamTokenSpec)
	}
	if t := os.Getenv(envKey("UPSTREAM_TOKEN")); t != "" {
		sc.upstreamToken.Store(t)
	}
}

// splitUpstreamTokenSpec parses an upstream_token value "<pattern>[<key>]":
// the bracketed suffix names the upstream query param the captured token is
// appended as, the prefix is the log-line pattern matched (the marker also
// shows where the token sits in the line). Bracket form avoids the &-ambiguity
// of multi-param query strings in the pattern. Returns empty strings when the
// spec is malformed (caller warns) — an empty pattern is rejected too, since
// it would match every log line.
func splitUpstreamTokenSpec(spec string) (pattern, key string) {
	if !strings.HasSuffix(spec, "]") {
		return "", ""
	}
	i := strings.LastIndexByte(spec, '[')
	if i <= 0 {
		return "", ""
	}
	key = spec[i+1 : len(spec)-1]
	if key == "" {
		return "", ""
	}
	for _, c := range key {
		if !(c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9') {
			return "", ""
		}
	}
	return spec[:i], key
}

// parseUpstreamMap parses an upstream_headrs/upstream_cookies JSON object
// (string values only; "" means "delete"). Parse errors disable the feature
// with a warning instead of failing the whole config load.
func parseUpstreamMap(name, label, raw string) map[string]string {
	raw = strings.TrimSpace(raw)
	if raw == "" || raw == "{}" {
		return nil
	}
	m := map[string]string{}
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		log.Printf("WARNING: [service %s] %s: invalid JSON (%v), upstream injection disabled", name, label, err)
		return nil
	}
	return m
}
