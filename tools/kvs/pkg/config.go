package pkg

import (
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"strconv"
	"strings"
	"time"
)

// GOverrideVersion is the version override set by /__restart?v=VERSION.
// When non-empty, resolveVersion uses it instead of fetching version_latest_url.
// This avoids polluting os.Environ (SVC_VERSION) which could leak into the
// backend subprocess and cause unexpected behavior.
var GOverrideVersion string

// gDebug is flag app is running by debug mode
var gDebug = os.Getenv("KVS_DEBUG") == "1"

// Config holds kvs configuration loaded from kvs.ini.
type Config struct {
	Proxies []Backend
	// [service] section
	SvcEnable           bool   // service enable
	SvcCheck            string // check — detect if backend already running (http/unix/file)
	SvcHome             string // home — working directory, injected as SVC_HOME
	SvcVersion          string // version — literal version, or empty to fetch from version_latest_url
	SvcVersionLatestURL string // version_latest_url — fetch version JSON, #field extracts a key
	SvcVersionHashURL   string // version_hash_url — fetch hash JSON, #field extracts a key, supports {SVC_VERSION}
	SvcVersionHash      string // resolved version hash → SVC_VERHASH
	SvcDownload         string // download — download URL (highest priority)
	SvcDownloadInfo     string // download_info — version info API URL (returns JSON)
	SvcDownloadFieldURL string // download_field_url — JSON field for URL in download_info
	SvcDownloadProxy    string // download_proxy — proxy URL (http/https/socks5) for downloads
	SvcCacheDir         string // cache_dir — cache directory
	SvcCacheSed         string // cache_sed — cc~ cache rewrite rules: file|old|new||... (via KVS_CC_SED)
	SvcProxyPath        string // proxy_path — /__cache/ path prefix
	SvcBinHome          string // bin_home — extracted bin directory, → SVC_BIN_HOME
	SvcOnceShell        string // once_shell — one-time script (file:// or sh -c), runs once per deploy, guarded by {bin_home}/__once__
	SvcInitShell        string // init_shell — startup script (file:// or sh -c), runs on every kvs startup
	SvcStopShell        string // stop_shell — shutdown script (file:// or sh -c), kvs-managed only
	SvcCommand          string // command — optional shell command to run as the backend subprocess

	VscAgentsCmd string            // vsc_agents_cmd — agent host command (env assignments + argv), kvs-managed subprocess
	VscAgentCmds map[string]string // vsc_agent_cmds — named agent host command presets (JSON map name→command)
	VscAgentsDir string            // vsc_agents_dir — directory scanned for *.json agent endpoint entries
	VscAgentArgs string            // SvcCommnand suffix, vscode agents connect config
	VscLanguage  map[string]string // vsc_language — lang→langpack mapping (e.g. zh-cn→zh-hans)

	Actions map[string]string // Actions maps the [actions] section: action name → one-shot command
	// top-level
	Port         string
	CookieName   string            // cookie, default "kvs"
	LoginAuthz   bool              // login_authz — enable auth redirect + logout button injection
	LoginToken   string            // login_token; when set, cookie value must match it
	LoginTimeout int               // login_timeout (seconds); 0=session; >0=hashed+expiring; <0=error
	UseSSL       bool              // use_ssl — enable HTTPS with a self-signed cert
	Headers      map[string]string // [headers] section: Xxx=Val → set/override; Xxx= → delete
	PathPublic   []string          // path_public — |-separated paths reachable without auth
	InitError    string            // non-fatal init error (e.g. version resolve failure); shown on loading page
}

// Backend describes a single proxy target with its routing prefix.
type Backend struct {
	Source    string // routing Source, "/" for root
	Scheme    string // http, https, unix, file, text
	Target    string // host:port, socket path, dir path, or literal text
	RawURL    string // original URL for logging
	IsService bool   // this backend is the one managed by kvs (auto-deploy, etc.)
	IsRegex   bool   // prefix is a regex pattern (^ prefix in [proxies])
	IsCache   bool   // cc~ prefix: cache proxied responses on disk ({cache_dir:-/cache}/ccproxy/{scheme}:{host}/path)
	IsWSock   bool   // ws~ prefix (or ws://wss:// url scheme): WebSocket-aware route, upgrade requests detected and handled separately
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
// Array values (key = [a, b, c]) are parsed at read time and stored as a
// space-joined string; the original items are kept in arrays for callers
// that need the []string form.
type iniFile struct {
	props [][2]string    // ordered [key, value] pairs preserving file order
	index map[string]int // key → index in props, for O(1) lookup
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

// expandValue expands {VAR} and {VAR:-default} placeholders in v.
// Lookup order: os environment variables first, then the svcVars map (internal
// SVC_* variables). This supports the template syntax used in kvs.ini:
//
//	{KVS_PORT:-7080}   → env KVS_PORT, or "7080" if unset/empty
//	{SVC_HOME}         → internal SVC_HOME variable
//	${VAR} / $VAR      → standard os.ExpandEnv (legacy, still supported)
//
// A brace expression is only treated as a placeholder when the name is a
// valid identifier (letters/digits/underscores). Anything else (e.g. JSON
// like `{ "commit": "{SVC_VERSION_HASH}" }`) is emitted verbatim, though
// inner valid placeholders are still expanded.
func expandValue(v string, svcVars map[string]string) string {
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
			// Lookup: svcVars first (internal), then os env.
			val, ok := svcVars[name]
			if !ok {
				val = os.Getenv(name)
			}
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
//   - [section] headers group keys; [headers], [proxies], [service] are special.
//   - Values support {VAR} and {VAR:-default} placeholders (expanded later by
//     loadInitConfig, not here, so internal SVC_* vars are available).
//   - Multi-line values: trailing " \" continues to next line.
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
// error messages (e.g. "kvs.default.ini:3: ..."). This allows loading config
// from embedded assets (via -c default) without a temp file.
func parseIniData(data []byte, source string) (*iniFile, error) {
	ini := &iniFile{
		props: make([][2]string, 0, 64),
		index: make(map[string]int),
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

	for lineNo, raw := range joined {
		line := strings.TrimSpace(raw)
		if line == "" || line[0] == '#' || line[0] == ';' {
			continue
		}
		// Section header.
		if line[0] == '[' && line[len(line)-1] == ']' {
			section = strings.TrimSpace(line[1 : len(line)-1])
			continue
		}
		// key = value
		k, v, ok := strings.Cut(line, "=")
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
		// Array value: [item1, item2, item3] — parse at read time and store
		// as a space-joined string. Variable expansion is done later by
		// loadInitConfig (the join is transparent to shell -c execution).
		if len(v) >= 2 && v[0] == '[' && v[len(v)-1] == ']' {
			items, err := parseArray(v, source, lineNo+1, raw)
			if err != nil {
				return nil, err
			}
			ini.set(iniKey(section, strings.ToLower(k)), strings.Join(items, " "))
			continue
		}
		// Store with section prefix; variable expansion is deferred to
		// loadInitConfig so internal SVC_* vars are available.
		ini.set(iniKey(section, strings.ToLower(k)), v)
	}
	return ini, nil
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

// svcStr returns the string value for a [service] key, falling back to
// def when the key is absent or empty.
func svcStr(ini *iniFile, key, def string) string {
	if v := ini.get("service." + key); v != "" {
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

// resolveVersion resolves the application version and its hash.
//
//   - If version is non-empty, it is used directly as SVC_VERSION.
//   - If version is empty, versionLatestURL is fetched. The URL may contain a
//     "#field" suffix; if present, the response is treated as JSON and the named
//     field is extracted. Otherwise the response body (trimmed) is the version.
//   - If versionHashURL is empty, SVC_VERSION_HASH = SVC_VERSION.
//   - If versionHashURL is set, it is fetched (after {SVC_VERSION} substitution)
//     and the same "#field" extraction applies.
//
// Returns (version, versionHash, error).
func resolveVersion(version, versionLatestURL, versionHashURL string) (string, string, error) {
	// Step 1: Resolve SVC_VERSION.
	// Priority: GOverrideVersion (/__restart?v=VERSION) > version (config) > versionLatestURL (fetched).
	if GOverrideVersion != "" {
		version = GOverrideVersion
		log.Printf("[version] SVC_VERSION=%s (from override)", version)
	} else if version == "" && versionLatestURL != "" {
		v, err := fetchJSONField(versionLatestURL)
		if err != nil {
			return "", "", fmt.Errorf("fetch version: %w", err)
		}
		version = v
	}
	if version == "" {
		return "", "", fmt.Errorf("version is empty and version_latest_url is not configured")
	}
	log.Printf("[version] SVC_VERSION=%s", version)

	// Step 2: Resolve SVC_VERSION_HASH.
	if versionHashURL == "" {
		log.Printf("[version] version_hash_url not set, SVC_VERSION_HASH=SVC_VERSION=%s", version)
		return version, version, nil
	}

	// Substitute {SVC_VERSION} in the hash URL.
	hashURL := strings.ReplaceAll(versionHashURL, "{SVC_VERSION}", version)
	vh, err := fetchJSONField(hashURL)
	if err != nil {
		return "", "", fmt.Errorf("fetch version hash: %w", err)
	}
	if vh == "" {
		vh = version
		log.Printf("[version] version_hash empty, falling back to SVC_VERSION=%s", version)
	}
	log.Printf("[version] SVC_VERSION_HASH=%s", vh)
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

// parseProxiesArg parses the -n flag value into [][2]string pairs matching the
// format returned by ini.hasPrefix("proxies."). The value uses ";" to separate
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
		k, v, ok := strings.Cut(part, "=")
		if !ok || strings.TrimSpace(k) == "" {
			log.Printf("WARNING: skipping invalid -n entry %q (expected prefix=url)", part)
			continue
		}
		entries = append(entries, [2]string{"proxies." + strings.TrimSpace(k), strings.TrimSpace(v)})
	}
	return entries
}

// loadIni parses the -c flag value and returns the parsed INI file plus the
// resolved source label (for logging). The special value "default" loads the
// embedded kvs.default.ini without requiring a file on disk.
func loadIni() (*iniFile, string) {
	cfgPath := ""
	for i, arg := range os.Args[1:] {
		if arg == "-c" && i+1 < len(os.Args)-1 {
			cfgPath = os.Args[i+2]
			break
		}
		if val, ok := strings.CutPrefix(arg, "-c="); ok {
			cfgPath = val
			break
		}
	}

	// -n present without -c: auto-use default config.
	if cfgPath == "" && (flagValue("-n") != "" || os.Getenv("KVS_PROXIES") != "") {
		if os.Getenv("KVS_SVC_ENABLE") == "" {
			os.Setenv("KVS_SVC_ENABLE", "false")
		}
		cfgPath = "default"
	}
	// switch -c by embed fs
	switch cfgPath {
	case "default":
		log.Printf("loading config: default (embedded kvs.default.ini)")
		ini, err := parseIniData(MustAsset("kvs.default.ini"), "kvs.default.ini")
		if err != nil {
			log.Fatalf("parse embedded config: %v", err)
		}
		return ini, "default"
	case "vscode":
		log.Printf("loading config: vscode (embedded kvs.vscode.ini)")
		ini, err := parseIniData(MustAsset("kvs.vscode.ini"), "kvs.vscode.ini")
		if err != nil {
			log.Fatalf("parse embedded config: %v", err)
		}
		return ini, "vscode"
	case "zcoded":
		if os.Getenv("KVS_SVC_ENABLE") == "" {
			os.Setenv("KVS_SVC_ENABLE", "false")
		}
		if os.Getenv("KVS_PATH_PUBLIC") == "" {
			os.Setenv("KVS_PATH_PUBLIC", "/ws|/remote/v4|/api/v1/client/configs")
		}
		if os.Getenv("KVS_CC_SED") == "" {
			// KVS_CC_SED='index-*.js|overrideUrl:void 0|overrideUrl:`wss://>host</ws`'
			os.Setenv("KVS_CC_SED", "src-*.js|`wss://zcode.z.ai/ws`|`wss://>host</ws`||src-*.js|`/api/v1/client/configs`,ff(e).origin|`/api/v1/client/configs`")
		}
		if os.Getenv("KVS_PROXIES") == "" {
			os.Setenv("KVS_PROXIES", "ws~/ws=wsws://zcode;cc~/api/v1/=https://zcode.z.ai/api/v1/;cc~/remote/v4=https://zcode.z.ai/remote/v4;/=api://zlist")
		}
		// read other config from kvs.default.ini
		log.Printf("loading config: zcoded (embedded kvs.default.ini)")
		ini, err := parseIniData(MustAsset("kvs.default.ini"), "kvs.default.ini")
		if err != nil {
			log.Fatalf("parse embedded config: %v", err)
		}
		return ini, "zcoded"
	}
	// -c by local fs
	resolved := resolveConfigPath(cfgPath)
	if resolved == "" {
		if cfgPath != "" {
			log.Fatalf("config file not found: %s", cfgPath)
		}
		log.Fatal("config file required: use -c <path> or -c default")
	}
	log.Printf("loading config: %s", resolved)
	ini, err := parseIni(resolved)
	if err != nil {
		log.Fatalf("parse config: %v", err)
	}
	return ini, resolved
}

// LoadInitConfig loads kvs.ini and resolves the full Config.
func LoadInitConfig() Config {
	ini, _ := loadIni()

	// -n flag overrides [proxies] with inline "prefix=url;prefix=url" entries.
	// When -n is used, the user provides their own backends, so kvs skips the
	// service lifecycle (download/extract/start command, version resolution).
	useNext := flagValue("-n")
	if useNext == "" {
		useNext = os.Getenv("KVS_PROXIES")
	}

	// svcVars holds internal SVC_* variables, built incrementally as [service]
	// fields are resolved. These are available for {VAR} expansion in later
	// values within the same kvs.ini.
	svcVars := map[string]string{}

	var proxyEntries [][2]string
	if useNext != "" {
		proxyEntries = parseProxiesArg(useNext)
	} else {
		proxyEntries = ini.hasPrefix("proxies.")
	}
	if len(proxyEntries) == 0 {
		log.Fatal("config error: [proxies] section is required in kvs.ini")
	}

	var cfg Config

	// --- Resolve [service] fields in dependency order ---
	cfg.SvcEnable = parseBool(expandValue(strProp(ini, "service.enable", ""), svcVars), true)

	// 1. home → SVC_HOME (loaded first, available for later references)
	cfg.SvcHome = expandValue(svcStr(ini, "home", ""), svcVars)
	svcVars["SVC_HOME"] = cfg.SvcHome
	_ = os.Setenv("SVC_HOME", cfg.SvcHome)

	// 1b. cache_dir → expanded with svcVars. Parsed even in -n mode so
	// cc~ marked backends can honor a configured cache directory.
	cfg.SvcCacheDir = expandValue(svcStr(ini, "cache_dir", ""), svcVars)

	// 1c. cache_sed → cc~ 缓存内容替换规则 (file|old|new||...)。与 cache_dir
	// 一样在 -n 模式下也解析，使 cc~ 标记的后端也能做缓存内容替换。
	cfg.SvcCacheSed = expandValue(svcStr(ini, "cache_sed", ""), svcVars)

	if cfg.SvcEnable {
		// 1b. version_base_url → SVC_VERSION_BASE_URL (before version_latest_url etc.)
		svcVersionBaseURL := expandValue(svcStr(ini, "version_base_url", ""), svcVars)
		svcVars["SVC_VERSION_BASE_URL"] = svcVersionBaseURL
		_ = os.Setenv("SVC_VERSION_BASE_URL", svcVersionBaseURL)

		// 1c. （SVC_VSCODE_NLS_URL:-/__cache/vscode/nls/}
		if _, exist := os.LookupEnv("SVC_VSCODE_NLS_URL"); !exist {
			os.Setenv("SVC_VSCODE_NLS_URL", "/__cache/vscode/nls/")
		}

		// 2. version / version_latest_url / version_hash_url → SVC_VERSION, SVC_VERHASH
		cfg.SvcVersion = expandValue(svcStr(ini, "version", ""), svcVars)
		cfg.SvcVersionLatestURL = expandValue(svcStr(ini, "version_latest_url", ""), svcVars)
		cfg.SvcVersionHashURL = strings.ReplaceAll(svcStr(ini, "version_hash_url", ""), "{SVC_VERSION_BASE_URL}", svcVersionBaseURL)

		// 3. download — re-expand with svcVars (may reference {SVC_VERSION_HASH})
		if download := expandValue(svcStr(ini, "download", ""), svcVars); download != "" {
			var err error
			cfg.SvcVersion, cfg.SvcVersionHash, err = resolveVersion(cfg.SvcVersion, cfg.SvcVersionLatestURL, cfg.SvcVersionHashURL)
			if err != nil {
				cfg.InitError = fmt.Sprintf("resolve version: %v", err)
				log.Printf("WARNING: %s", cfg.InitError)
			}
			svcVars["SVC_VERSION"] = cfg.SvcVersion
			svcVars["SVC_VERSION_HASH"] = cfg.SvcVersionHash
			_ = os.Setenv("SVC_VERSION", cfg.SvcVersion)
			_ = os.Setenv("SVC_VERSION_HASH", cfg.SvcVersionHash)

			download = strings.ReplaceAll(download, "SVC_VERSION_HASH", cfg.SvcVersionHash)
			download = strings.ReplaceAll(download, "SVC_VERSION", cfg.SvcVersion)
			cfg.SvcDownload = download
			log.Printf("backend download url: %s", cfg.SvcDownload)
		} else {
			svcVars["SVC_VERSION"] = cfg.SvcVersion
			svcVars["SVC_VERSION_HASH"] = cfg.SvcVersionHash
			_ = os.Setenv("SVC_VERSION", cfg.SvcVersion)
			_ = os.Setenv("SVC_VERSION_HASH", cfg.SvcVersionHash)
		}

		// 4. download_info / download_field_url
		cfg.SvcDownloadInfo = expandValue(svcStr(ini, "download_info", ""), svcVars)
		cfg.SvcDownloadFieldURL = expandValue(svcStr(ini, "download_field_url", "url"), svcVars)
		cfg.SvcDownloadProxy = expandValue(svcStr(ini, "download_proxy", ""), svcVars)

		// 6. bin_home → SVC_BIN_HOME
		cfg.SvcBinHome = expandValue(svcStr(ini, "bin_home", ""), svcVars)
		svcVars["SVC_BIN_HOME"] = cfg.SvcBinHome
		_ = os.Setenv("SVC_BIN_HOME", cfg.SvcBinHome)

		// 7. Other fields
		if checkURL := os.Getenv("KVS_SVC_CHECK_URL"); checkURL != "" {
			cfg.SvcCheck = checkURL
		} else {
			cfg.SvcCheck = expandValue(svcStr(ini, "check", ""), svcVars)
		}
		cfg.SvcProxyPath = expandValue(svcStr(ini, "proxy_path", ""), svcVars)
		cfg.SvcOnceShell = expandValue(svcStr(ini, "once_shell", ""), svcVars)
		cfg.SvcInitShell = expandValue(svcStr(ini, "init_shell", ""), svcVars)
		cfg.SvcStopShell = expandValue(svcStr(ini, "stop_shell", ""), svcVars)
		cfg.SvcCommand = expandValue(svcStr(ini, "command", ""), svcVars)
		if strings.ContainsRune(cfg.SvcCommand, '{') {
			cfg.SvcCommand = expandValue(cfg.SvcCommand, svcVars)
		}

		// 7b. vsc_language — lang→langpack JSON map (e.g. {"zh-cn":"zh-hans"}).
		//     NOT expanded via expandValue: the {…} JSON braces would be
		//     mistaken for {VAR} placeholders and destroyed.
		if raw := ini.get("service.vsc_language"); raw != "" {
			var m map[string]string
			if err := json.Unmarshal([]byte(raw), &m); err != nil {
				log.Printf("WARNING: invalid vsc_language JSON: %v", err)
			} else {
				cfg.VscLanguage = m
			}
		}
		cfg.VscAgentsCmd = expandValue(svcStr(ini, "vsc_agents_cmd", ""), svcVars)
		// vsc_agent_cmds — named command presets (JSON map). Values carry
		// {SVC_HOME}/{SVC_BIN_HOME} placeholders, so expand FIRST (the JSON
		// keys like "vscode" are not valid identifiers and pass through
		// verbatim), then unmarshal.
		if raw := ini.get("service.vsc_agent_cmds"); raw != "" {
			var m map[string]string
			if err := json.Unmarshal([]byte(expandValue(raw, svcVars)), &m); err != nil {
				log.Printf("WARNING: invalid vsc_agent_cmds JSON: %v", err)
			} else {
				cfg.VscAgentCmds = m
			}
		}
		cfg.VscAgentsDir = expandValue(svcStr(ini, "vsc_agents_dir", ""), svcVars)
		cfg.VscAgentArgs = expandValue(svcStr(ini, "vsc_agent_args", ""), svcVars)

		// 7c. [actions] — named one-shot commands (name → command). Loaded
		//     generically: kvs has no idea what any action does, it just runs
		//     the command. Placed after bin_home so {SVC_BIN_HOME} expands.
		cfg.Actions = make(map[string]string)
		for _, kv := range ini.hasPrefix("actions.") {
			name := strings.TrimPrefix(kv[0], "actions.")
			cfg.Actions[name] = strings.TrimSpace(expandValue(kv[1], svcVars))
		}
	} else {
		log.Printf("[service] disable, pass service config.")
	}

	// 8. Expand [proxies] and [headers] now that all SVC_* vars are set.
	for i, kv := range proxyEntries {
		proxyEntries[i][1] = expandValue(kv[1], svcVars)
	}
	headerEntries := ini.hasPrefix("headers.")
	for i, kv := range headerEntries {
		headerEntries[i][1] = expandValue(kv[1], svcVars)
	}

	// Build []proxyEntry from the expanded proxies.* pairs.
	cfg.Proxies = parseBackends(proxyEntries)
	if len(cfg.Proxies) == 0 {
		log.Fatal("config error: [proxies] parsed to zero backends")
	}

	// Build headers map from the expanded headers.* pairs.
	cfg.Headers = make(map[string]string, len(headerEntries))
	for _, kv := range headerEntries {
		cfg.Headers[strings.TrimPrefix(kv[0], "headers.")] = kv[1]
	}

	// Top-level fields (always resolved, even in -n mode).
	cfg.Port = expandValue(strProp(ini, "port", "7080"), svcVars)
	cfg.CookieName = expandValue(strProp(ini, "cookie", "kvs"), svcVars)
	cfg.LoginAuthz = parseBool(expandValue(strProp(ini, "login_authz", ""), svcVars), false)
	cfg.LoginToken = expandValue(strProp(ini, "login_token", ""), svcVars)
	cfg.LoginTimeout = parseTimeout(expandValue(strProp(ini, "login_timeout", "0"), svcVars))
	cfg.UseSSL = parseBool(expandValue(strProp(ini, "use_ssl", ""), svcVars), false)
	// path_public — |-separated list of paths reachable without auth
	// (e.g. /api/v1/public|/remote/v4). Non-empty entries are kept as-is;
	// prefix matching is applied by the auth middleware.
	if raw := expandValue(strProp(ini, "path_public", ""), svcVars); raw != "" {
		for _, p := range strings.Split(raw, "|") {
			if p = strings.TrimSpace(p); p != "" {
				cfg.PathPublic = append(cfg.PathPublic, p)
			}
		}
	}

	return cfg
}
