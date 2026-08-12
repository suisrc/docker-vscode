package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"embed"
	"encoding/hex"
	"encoding/json"
	"encoding/pem"
	"fmt"
	"io"
	"log"
	"math/big"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

//go:embed favicon.ico loading.html login.html logout.vsc.js kvs.ini.example
var staticFS embed.FS

// gOverrideVersion is the version override set by /__restart?v=VERSION.
// When non-empty, resolveVersion uses it instead of fetching version_latest_url.
// This avoids polluting os.Environ (SVC_VERSION) which could leak into the
// backend subprocess and cause unexpected behavior.
var gOverrideVersion string

// mustAsset reads an embedded asset by name, failing fast at startup if missing.
// embed.FS is an in-memory read-only map; ReadFile just returns a slice over it,
// so there is no I/O and no benefit to pre-caching assets into package vars.
func mustAsset(name string) []byte {
	b, err := staticFS.ReadFile(name)
	if err != nil {
		log.Fatalf("embed asset %q: %v", name, err)
	}
	return b
}

// Config holds kvs configuration loaded from kvs.ini.
type Config struct {
	Proxies []Backend
	// [service] section
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
	SvcProxyPath        string // proxy_path — /__cache/ path prefix
	SvcBinHome          string // bin_home — extracted bin directory, → SVC_BIN_HOME
	SvcInitShell        string // init_shell — startup script (file:// or sh -c)
	SvcStopShell        string // stop_shell — shutdown script (file:// or sh -c), kvs-managed only
	SvcCommand          string // command — optional shell command to run as the backend subprocess
	// top-level
	Port         string
	CookieName   string            // cookie, default "kvs"
	LoginAuthz   bool              // login_authz — enable auth redirect + logout button injection
	LoginToken   string            // login_token; when set, cookie value must match it
	LoginTimeout int               // login_timeout (seconds); 0=session; >0=hashed+expiring; <0=error
	UseSSL       bool              // use_ssl — enable HTTPS with a self-signed cert
	Headers      map[string]string // [headers] section: Xxx=Val → set/override; Xxx= → delete
	InitError    string            // non-fatal init error (e.g. version resolve failure); shown on loading page
}

// Backend describes a single proxy target with its routing prefix.
type Backend struct {
	Prefix    string // routing prefix, "/" for root
	Scheme    string // http, https, unix, file, text
	Target    string // host:port, socket path, dir path, or literal text
	RawURL    string // original URL for logging
	IsService bool   // this backend is the one managed by kvs (auto-deploy, etc.)
	IsRegex   bool   // prefix is a regex pattern (^ prefix in [proxies])
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
func expandValue(v string, svcVars map[string]string) string {
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
			i += end + 1
			// Check for :- default separator.
			var name, def string
			if idx := strings.Index(expr, ":-"); idx >= 0 {
				name = expr[:idx]
				def = expr[idx+2:]
			} else {
				name = expr
			}
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
// error messages (e.g. "kvs.ini.example:3: ..."). This allows loading config
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
	// Priority: gOverrideVersion (/__restart?v=VERSION) > version (config) > versionLatestURL (fetched).
	if gOverrideVersion != "" {
		version = gOverrideVersion
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
	var result map[string]interface{}
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
// embedded kvs.ini.example without requiring a file on disk.
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
	if cfgPath == "" && flagValue("-n") != "" {
		cfgPath = "default"
	}

	if cfgPath == "default" {
		log.Printf("loading config: default (embedded kvs.ini.example)")
		ini, err := parseIniData(mustAsset("kvs.ini.example"), "kvs.ini.example")
		if err != nil {
			log.Fatalf("parse embedded config: %v", err)
		}
		return ini, "default"
	}

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

func loadInitConfig() Config {
	ini, _ := loadIni()

	// -n flag overrides [proxies] with inline "prefix=url;prefix=url" entries.
	// When -n is used, the user provides their own backends, so kvs skips the
	// service lifecycle (download/extract/start command, version resolution).
	useN := flagValue("-n") != ""

	// svcVars holds internal SVC_* variables, built incrementally as [service]
	// fields are resolved. These are available for {VAR} expansion in later
	// values within the same kvs.ini.
	svcVars := map[string]string{}

	proxyEntries := ini.hasPrefix("proxies.")
	if useN {
		proxyEntries = parseProxiesArg(flagValue("-n"))
	}
	if len(proxyEntries) == 0 {
		log.Fatal("config error: [proxies] section is required in kvs.ini")
	}

	var cfg Config

	// --- Resolve [service] fields in dependency order ---

	// 1. home → SVC_HOME (loaded first, available for later references)
	cfg.SvcHome = expandValue(svcStr(ini, "home", ""), svcVars)
	svcVars["SVC_HOME"] = cfg.SvcHome
	_ = os.Setenv("SVC_HOME", cfg.SvcHome)

	if !useN {
		// 1b. version_base_url → SVC_VERSION_BASE_URL (before version_latest_url etc.)
		svcVersionBaseURL := expandValue(svcStr(ini, "version_base_url", ""), svcVars)
		svcVars["SVC_VERSION_BASE_URL"] = svcVersionBaseURL
		_ = os.Setenv("SVC_VERSION_BASE_URL", svcVersionBaseURL)

		// 2. version / version_latest_url / version_hash_url → SVC_VERSION, SVC_VERHASH
		cfg.SvcVersion = expandValue(svcStr(ini, "version", ""), svcVars)
		cfg.SvcVersionLatestURL = expandValue(svcStr(ini, "version_latest_url", ""), svcVars)
		cfg.SvcVersionHashURL = strings.ReplaceAll(svcStr(ini, "version_hash_url", ""), "{SVC_VERSION_BASE_URL}", svcVersionBaseURL)

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

		// 3. download — re-expand with svcVars (may reference {SVC_VERSION_HASH})
		cfg.SvcDownload = expandValue(svcStr(ini, "download", ""), svcVars)

		// 4. download_info / download_field_url
		cfg.SvcDownloadInfo = expandValue(svcStr(ini, "download_info", ""), svcVars)
		cfg.SvcDownloadFieldURL = expandValue(svcStr(ini, "download_field_url", "url"), svcVars)
		cfg.SvcDownloadProxy = expandValue(svcStr(ini, "download_proxy", ""), svcVars)

		// 5. cache_dir → expand with svcVars
		cfg.SvcCacheDir = expandValue(svcStr(ini, "cache_dir", ""), svcVars)

		// 6. bin_home → SVC_BIN_HOME
		cfg.SvcBinHome = expandValue(svcStr(ini, "bin_home", ""), svcVars)
		svcVars["SVC_BIN_HOME"] = cfg.SvcBinHome
		_ = os.Setenv("SVC_BIN_HOME", cfg.SvcBinHome)

		// 7. Other fields
		cfg.SvcCheck = expandValue(svcStr(ini, "check", ""), svcVars)
		cfg.SvcProxyPath = expandValue(svcStr(ini, "proxy_path", ""), svcVars)
		cfg.SvcInitShell = expandValue(svcStr(ini, "init_shell", ""), svcVars)
		cfg.SvcStopShell = expandValue(svcStr(ini, "stop_shell", ""), svcVars)
		cfg.SvcCommand = expandValue(svcStr(ini, "command", ""), svcVars)
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

	return cfg
}

// parseBackends builds a list of Backend entries from the ordered [proxies]
// section entries. Each entry is a [2]string{key, value} where key is the
// routing prefix (with optional & or ^ markers) and value is the backend URL.
//
// Prefix markers:
//   - "&" prefix: kvs-managed service backend (auto-deploy, loading page)
//   - "^" prefix: regex pattern match (cannot combine with "&")
//   - "http://" or "https://" prefix on the prefix itself: full-domain match
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

		// Detect service marker ("&" prefix) and strip it for routing.
		isService := strings.HasPrefix(prefix, "&")
		if isService {
			prefix = strings.TrimPrefix(prefix, "&")
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
// Service Preparation — download, extract, fixup before starting KVS_SVC_COMMAND
// =============================================================================

// serviceState tracks lazy preparation of the service backend. Preparation is
// triggered on the first request to the service prefix (not at startup).
// `preparing` covers the whole lifecycle (from trigger to finish) so requests
// never proxy to the backend before it is fully ready (avoids 502).
// On failure, the state is reset so the next request retries.
type serviceState struct {
	mu        sync.Mutex
	preparing bool   // true from first trigger until finish() — gates proxying
	status    string // current status message while preparing; "" when idle
	done      bool   // preparation finished (successfully or not)
	err       error  // preparation error; nil on success
}

// begin marks preparation as in progress (called once at trigger time).
// Returns false if preparation is already in progress or already succeeded.
func (s *serviceState) begin() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.preparing || (s.done && s.err == nil) {
		return false // already preparing or already succeeded
	}
	s.preparing = true
	s.done = false
	s.err = nil
	s.status = ""
	return true
}

// setPreparing updates the human-readable status while preparing.
func (s *serviceState) setPreparing(status string) {
	s.mu.Lock()
	s.status = status
	s.mu.Unlock()
}

// finish marks preparation done, storing err (nil on success).
func (s *serviceState) finish(err error) {
	s.mu.Lock()
	s.preparing = false
	s.status = ""
	s.done = true
	s.err = err
	s.mu.Unlock()
}

// active reports whether preparation is in progress.
func (s *serviceState) active() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.preparing
}

// getStatus returns the current status message (empty when idle).
func (s *serviceState) getStatus() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.status
}

// result returns (done, err) — whether preparation finished and any error.
func (s *serviceState) result() (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.done, s.err
}

// reset clears the preparation state so the next request re-triggers
// download/extract/start. Used by /__restart.
func (s *serviceState) reset() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.preparing = false
	s.done = false
	s.err = nil
	s.status = ""
}

// serveLoadingPage writes the preparation loading page (loading.html), injecting
// the current status into the __STATUS__ placeholder.
func serveLoadingPage(w http.ResponseWriter, status string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-cache")
	w.WriteHeader(http.StatusServiceUnavailable)
	msg := status
	if msg == "" {
		msg = "Preparing Application"
	}
	html := strings.Replace(string(mustAsset("loading.html")), "__STATUS__", msg, 1)
	_, _ = w.Write([]byte(html))
}

// prepareService downloads (if needed), extracts, and runs fixups for the backend
// service. It blocks until preparation is complete. srvState tracks status for the
// loading page.
//
// New flow:
//  1. If check is set and the backend is already reachable → skip (already running)
//  2. If bin_home exists and is non-empty → skip (already extracted)
//  3. Resolve download URL (download has priority; otherwise download_info + field)
//  4. Follow redirects to get .ext → SVC_PACKAGE_EXT
//  5. Download to cache_dir/cache/version/{SVC_VERSION}_{SVC_VERSION_HASH}.{ext}
//  6. Extract tarball to bin_home
//  7. Run init_shell
//
// Returns (managed, error). managed=false means the backend is an external service
// already alive (detected via check) — kvs must NOT start/stop it or touch its
// socket. managed=true means kvs owns the backend lifecycle.
func prepareService(cfg Config, srvState *serviceState) (bool, error) {
	// 0. If config init failed (e.g. version resolve error), fail fast so the
	// loading page shows the error instead of crashing the whole program.
	if cfg.InitError != "" {
		return false, fmt.Errorf("%s", cfg.InitError)
	}

	// 1. Check if backend is already running (external/system service).
	if cfg.SvcCheck != "" {
		if isBackendAlive(cfg.SvcCheck) {
			log.Printf("[prepare] backend already alive: %s (external service, not managed by kvs)", cfg.SvcCheck)
			return false, nil
		}
		// If check is unix:// and the socket file exists but is dead, it's a
		// stale socket left by a previous kvs-managed crash — safe to remove.
		if scheme, target, ok := strings.Cut(cfg.SvcCheck, "://"); ok && scheme == "unix" {
			if _, err := os.Stat(target); err == nil {
				log.Printf("[prepare] removing stale unix socket: %s", target)
				_ = os.Remove(target)
			}
		}
	}

	// 2. If bin_home exists and is non-empty → skip extraction.
	if cfg.SvcBinHome != "" {
		if installed, _ := isServiceInstalled(cfg.SvcBinHome); installed {
			log.Printf("[prepare] bin_home already exists: %s", cfg.SvcBinHome)
			// Still run init_shell? No — init_shell runs once per deploy.
			return true, nil
		}
	}

	// 3. Resolve download URL.
	// Build an HTTP client that uses download_proxy if configured.
	dlClient := buildDownloadClient(cfg.SvcDownloadProxy)
	if cfg.SvcDownloadProxy != "" {
		log.Printf("[prepare] using download proxy: %s", cfg.SvcDownloadProxy)
	}

	downloadURL := cfg.SvcDownload
	if downloadURL == "" && cfg.SvcDownloadInfo != "" {
		// Fetch download_info JSON and extract URL field.
		var err error
		downloadURL, err = resolveDownloadInfo(dlClient, cfg.SvcDownloadInfo, cfg.SvcDownloadFieldURL)
		if err != nil {
			return false, fmt.Errorf("download_info: %w", err)
		}
	}
	if downloadURL == "" {
		return true, nil // nothing to download → nothing to deploy (but kvs may still start command)
	}
	if cfg.SvcBinHome == "" {
		return false, fmt.Errorf("bin_home is required when download is set")
	}

	// 4. Follow redirects to resolve .ext → SVC_PACKAGE_EXT.
	ext, err := resolveExtFromURL(dlClient, downloadURL)
	if err != nil {
		return false, fmt.Errorf("resolve extension: %w", err)
	}
	_ = os.Setenv("SVC_PACKAGE_EXT", ext)
	log.Printf("[prepare] package ext: %s", ext)

	// 5. Compute cache path and download if not cached.
	verName := cfg.SvcVersion
	if cfg.SvcVersionHash != "" {
		verName = cfg.SvcVersion + "_" + cfg.SvcVersionHash
	}
	cachePath := filepath.Join(cfg.SvcCacheDir, "cache", "version", verName+"."+ext)

	if _, err := os.Stat(cachePath); err != nil {
		srvState.setPreparing("Downloading Application: " + downloadURL)
		log.Printf("[prepare] downloading: %s → %s", downloadURL, cachePath)
		// Progress callback updates the loading page status.
		onProgress := func(written, total int64) {
			if total > 0 {
				pct := written * 100 / total
				srvState.setPreparing(fmt.Sprintf("Downloading Application: %s [%d%%]", downloadURL, pct))
			}
		}
		if err := downloadFile(dlClient, downloadURL, cachePath, onProgress); err != nil {
			return false, fmt.Errorf("download: %w", err)
		}
		log.Printf("[prepare] download complete: %s", cachePath)
	} else {
		log.Printf("[prepare] download cached: %s", cachePath)
	}

	// 6. Extract to bin_home.
	srvState.setPreparing("Extracting Application: " + cfg.SvcBinHome)
	log.Printf("[prepare] extracting: %s → %s", cachePath, cfg.SvcBinHome)
	if err := extractTarball(cachePath, cfg.SvcBinHome); err != nil {
		return false, fmt.Errorf("extract: %w", err)
	}
	log.Printf("[prepare] extract complete: %s", cfg.SvcBinHome)

	// 7. Run init_shell if set.
	if cfg.SvcInitShell != "" {
		srvState.setPreparing("Running startup script: " + cfg.SvcInitShell)
		log.Printf("[prepare] running startup script: %s", cfg.SvcInitShell)
		if err := runServiceStartup(cfg.SvcInitShell); err != nil {
			return false, fmt.Errorf("init_shell: %w", err)
		}
		log.Printf("[prepare] startup script complete")
	}

	return true, nil
}

// isBackendAlive checks if the backend at the given URL is reachable.
// Supports http://, https:// (GET request), unix:// (socket dial), file:// (file exists).
func isBackendAlive(checkURL string) bool {
	scheme, target, ok := strings.Cut(checkURL, "://")
	if !ok {
		return false
	}
	switch scheme {
	case "http", "https":
		client := &http.Client{Timeout: 5 * time.Second}
		resp, err := client.Get(checkURL)
		if err != nil {
			return false
		}
		resp.Body.Close()
		return true
	case "unix":
		conn, err := net.Dial("unix", target)
		if err != nil {
			return false
		}
		conn.Close()
		return true
	case "file":
		_, err := os.Stat(target)
		return err == nil
	}
	return false
}

// buildDownloadClient creates an *http.Client for service downloads.
// If proxyURL is non-empty, it supports http://, https://, and socks5:// proxies.
// Uses http.DefaultTransport as the base (optimal default settings for CDN),
// only overriding the proxy. A generous timeout allows large tarballs.
func buildDownloadClient(proxyURL string) *http.Client {
	tr := http.DefaultTransport.(*http.Transport).Clone()
	if proxyURL != "" {
		u, err := url.Parse(proxyURL)
		if err != nil {
			log.Printf("[prepare] invalid download_proxy %q: %v, ignoring", proxyURL, err)
		} else {
			tr.Proxy = http.ProxyURL(u)
		}
	}
	return &http.Client{
		Transport:     tr,
		Timeout:       30 * time.Minute,
		CheckRedirect: redirectLimit(10),
	}
}

// resolveDownloadInfo fetches a JSON API and extracts the URL field.
func resolveDownloadInfo(client *http.Client, infoURL, field string) (string, error) {
	resp, err := client.Get(infoURL)
	if err != nil {
		return "", fmt.Errorf("fetch: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", fmt.Errorf("HTTP %d", resp.StatusCode)
	}
	var result map[string]interface{}
	if err := json.NewDecoder(resp.Body).Decode(&result); err != nil {
		return "", fmt.Errorf("decode JSON: %w", err)
	}
	if field == "" {
		field = "url"
	}
	urlStr, _ := result[field].(string)
	if urlStr == "" {
		return "", fmt.Errorf("field %q not found or empty in response", field)
	}
	return urlStr, nil
}

// isServiceInstalled reports whether dir exists, is a directory, and is non-empty.
func isServiceInstalled(dir string) (ok bool, reason string) {
	fi, err := os.Stat(dir)
	if err != nil || !fi.IsDir() {
		return false, ""
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return false, "dir exists but is not readable"
	}
	if len(entries) == 0 {
		return false, "dir exists but is empty"
	}
	return true, ""
}

// resolveExtFromURL follows redirects on urlStr and extracts the file extension
// from the final URL's basename (e.g. .tar.gz).
//
// First tries to extract the extension from the URL path directly (no network
// request). If the URL path has no recognizable extension (e.g. a short
// redirect URL), falls back to a HEAD request with a short timeout.
func resolveExtFromURL(client *http.Client, urlStr string) (string, error) {
	// Fast path: extract extension from the URL path without a network request.
	if ext := extractExt(urlStr); ext != "tar.gz" || strings.Contains(filepath.Base(urlStr), ".tar.gz") {
		log.Printf("[prepare] ext from URL path: %s", ext)
		return ext, nil
	}

	// Slow path: HEAD request to follow redirects and get the final URL.
	// Use a short timeout so a slow proxy/server doesn't block for minutes.
	headClient := *client
	headClient.Timeout = 30 * time.Second
	resp, err := headClient.Head(urlStr)
	if err != nil {
		// HEAD failed — default to tar.gz rather than failing the whole prepare.
		log.Printf("[prepare] HEAD %s failed: %v, defaulting ext to tar.gz", urlStr, err)
		return "tar.gz", nil
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		log.Printf("[prepare] HEAD %s returned %d (using final URL for extension)", urlStr, resp.StatusCode)
	}
	finalURL := ""
	if resp.Request != nil {
		finalURL = resp.Request.URL.String()
	}
	if finalURL == "" {
		finalURL = urlStr
	}
	return extractExt(finalURL), nil
}

// extractExt returns the extension portion of a basename without the leading
// dot, e.g. "x.tar.gz" → "tar.gz". The leading dot is intentionally omitted so
// callers control it in the template (e.g. "${HASH}.{ext}" → "hash.tar.gz").
func extractExt(base string) string {
	base = filepath.Base(base)
	// Strip query/fragment.
	if i := strings.IndexAny(base, "?#"); i >= 0 {
		base = base[:i]
	}
	dotIdx := strings.Index(base, ".")
	if dotIdx < 0 {
		log.Printf("[prepare] no extension found in %q, defaulting to tar.gz", base)
		return "tar.gz" // sensible default for compressed tarballs
	}
	return base[dotIdx+1:]
}

// writerFunc adapts a function to the io.Writer interface.
type writerFunc func(p []byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// downloadFile downloads urlStr to destPath using an atomic temp+rename strategy.
// onProgress (if non-nil) is called periodically with bytes written and total.
func downloadFile(client *http.Client, urlStr, destPath string, onProgress func(written, total int64)) error {
	resp, err := client.Get(urlStr)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("HTTP %d", resp.StatusCode)
	}

	total := resp.ContentLength
	if err := os.MkdirAll(filepath.Dir(destPath), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(destPath), ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	success := false
	defer func() {
		if !success {
			_ = os.Remove(tmpName)
		}
	}()

	// Use io.TeeReader + a counting writer for progress, with a 64KB buffer.
	var written int64
	var lastUpdate time.Time
	counter := writerFunc(func(p []byte) (int, error) {
		written += int64(len(p))
		if onProgress != nil && time.Since(lastUpdate) >= time.Second {
			onProgress(written, total)
			lastUpdate = time.Now()
		}
		return tmp.Write(p)
	})
	buf := make([]byte, 64*1024)
	_, err = io.CopyBuffer(counter, resp.Body, buf)
	if err != nil {
		tmp.Close()
		return err
	}
	// Final progress update.
	if onProgress != nil {
		onProgress(written, total)
	}
	if err := tmp.Close(); err != nil {
		return err
	}

	// Verify downloaded size against Content-Length (when provided).
	if resp.ContentLength > 0 && written != resp.ContentLength {
		return fmt.Errorf("download truncated: got %d bytes, expected %d", written, resp.ContentLength)
	}

	if err := os.Rename(tmpName, destPath); err != nil {
		return err
	}
	success = true
	return nil
}

// extractTarball extracts a .tar.gz (or .tgz) archive into destDir, creating
// destDir if needed. It strips the top-level directory from archive entries
// so the contents land directly in destDir.
func extractTarball(tarPath, destDir string) error {
	f, err := os.Open(tarPath)
	if err != nil {
		return err
	}
	defer f.Close()

	gz, err := gzip.NewReader(f)
	if err != nil {
		return fmt.Errorf("gzip: %w", err)
	}
	defer gz.Close()

	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return err
	}

	tr := tar.NewReader(gz)
	var stripPrefix string
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return fmt.Errorf("tar: %w", err)
		}

		// Determine common prefix to strip from the first entry.
		// When the first entry has no '/' (rare: bare file), stripPrefix stays
		// empty and no stripping occurs — all names are used as-is.
		if stripPrefix == "" {
			if idx := strings.Index(hdr.Name, "/"); idx >= 0 {
				stripPrefix = hdr.Name[:idx+1]
			}
		}

		rel := strings.TrimPrefix(hdr.Name, stripPrefix)
		if rel == "" || rel == "." {
			continue
		}
		// Skip macOS metadata.
		if strings.HasPrefix(filepath.Base(rel), "._") {
			continue
		}

		target := filepath.Join(destDir, rel)
		// Safety: ensure target stays within destDir (reject path traversal).
		if !isPathWithin(target, destDir) {
			log.Printf("[prepare] WARNING: skipping path traversal attempt: %s", hdr.Name)
			continue
		}

		switch hdr.Typeflag {
		case tar.TypeDir:
			if err := os.MkdirAll(target, os.FileMode(hdr.Mode)); err != nil {
				return err
			}
		case tar.TypeReg:
			if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
				return err
			}
			out, err := os.OpenFile(target, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, os.FileMode(hdr.Mode))
			if err != nil {
				return err
			}
			if _, err := io.Copy(out, io.LimitReader(tr, hdr.Size)); err != nil {
				out.Close()
				return err
			}
			if err := out.Close(); err != nil {
				return err
			}
		case tar.TypeSymlink:
			// Create symlink if supported; skip on error.
			if err := os.Symlink(hdr.Linkname, target); err != nil {
				log.Printf("[prepare] symlink skipped: %s → %s (%v)", target, hdr.Linkname, err)
			}
		}
	}
	return nil
}

// isPathWithin reports whether target is destDir itself or nested inside it,
// guarding against path traversal in extracted archives.
func isPathWithin(target, destDir string) bool {
	cleanDest := filepath.Clean(destDir)
	cleanTarget := filepath.Clean(target)
	return cleanTarget == cleanDest ||
		strings.HasPrefix(cleanTarget, cleanDest+string(os.PathSeparator))
}

// runServiceStartup executes KVS_SVC_INITSHELL. If it starts with "file://", the
// referenced file is made executable and run directly (the kernel reads its #!
// shebang). Otherwise, the string is run via sh -c.
func runServiceStartup(cmd string) error {
	var c *exec.Cmd
	if filePath, ok := strings.CutPrefix(cmd, "file://"); ok {
		if err := os.Chmod(filePath, 0o755); err != nil {
			return fmt.Errorf("chmod startup script file: %w", err)
		}
		c = exec.Command(filePath)
	} else {
		c = exec.Command("sh", "-c", cmd)
	}
	c.Stdout = os.Stdout
	c.Stderr = os.Stderr
	return c.Run()
}

// normalizeProxyPath ensures p starts and ends with '/'.
// Empty or "/" input defaults to "/__cache/".
func normalizeProxyPath(p string) string {
	p = strings.TrimSpace(p)
	if p == "" {
		return "" // empty = cache proxy disabled
	}
	if p == "/" {
		return "/__cache/" // bare "/" is ambiguous, use default
	}
	if !strings.HasPrefix(p, "/") {
		p = "/" + p
	}
	if !strings.HasSuffix(p, "/") {
		p = p + "/"
	}
	return p
}

// helpCommand prints usage information for all kvs commands.
func helpCommand() {
	fmt.Print(`kvs — 反向代理网关 (VS Code Server 自动部署 + 鉴权 + 缓存)

用法:
  kvs <subcommand> [options]
  kvs -c <config> [options]        启动代理服务
  kvs -n "<entries>" [options]     内联路由，自动补充 -c default

子命令:
  kvs help                          显示本帮助信息
  kvs demo                          生成示例配置文件 kvs.ini
  kvs mirror -c <config> [version]  同步 VS Code 版本到 S3 兼容存储
  kvs mirror -c default             使用内置默认配置同步 latest 版本
  kvs mirror -c default 1.130.0     同步指定版本

启动选项:
  -c <path>                         指定配置文件 (必填)
  -c default                        使用 embed 中的 kvs.ini.example
  -n "prefix=url;prefix=url"        内联 [proxies] 路由，自动补充 -c default
                                    用 ; 分割多条，每条格式: prefix=url
                                    示例: -n "/healthz=text://OK:@now;/=http://127.0.0.1:8080"

路由格式 ([proxies] 段 / -n 参数):
  /=http://localhost:8080           HTTP 反向代理
  /=unix:///var/run/app.sock        Unix socket 反向代理
  /=file:///var/www/html            静态文件服务
  /=text://Hello World              纯文本响应 (@now 替换为当前时间)
  &/=unix:///var/run/app.sock       & 前缀: kvs 托管服务 (触发自动部署)
  ^/api/\d+=http://localhost:8080   ^ 前缀: 正则匹配
  /__healthz=text://OK:@now         健康检查端点

配置文件段:
  [proxies]         后端路由 (必填)
  [headers]         请求头改写 (Xxx=Val 设置; Xxx= 删除)
  [service]         服务自动部署 (download/extract/start)
  [mirror]          S3 镜像同步配置

环境变量:
  KVS_PORT                         监听端口 (默认 7080，SSL 时 HTTPS=port+1)
  KVS_USESSL                       启用自签名 HTTPS (默认 false)
  KVS_LOGIN_AUTHZ                  启用 401/403→登录页重定向 (默认 false)
  KVS_LOGIN_TOKEN                  Cookie 校验值 (留空则不校验)
  KVS_LOGIN_TIMEOUT                Cookie 有效期秒数 (0=session, >0=哈希+过期)
  KVS_COOKIE                       Cookie 名 (默认 kvs)
  KVS_HOME                         服务工作目录 (默认 /app/.vsc)
  KVS_SVC_SOCK_FILE                后端 socket 路径 (默认 ./kvs.sock)
  KVS_VSCODE_VERSION               VS Code 指定版本 (留空则获取最新版本)
  KVS_VSCODE_DOWNLOAD_BASE         VS Code 下载基础 URL (默认 https://update.code.visualstudio.com)
  KVS_VSCODE_DOWNLOAD_PATH         下载路径前缀 (默认 commit:，用于拼接URL， [BASE_URL]/commit:[hash]/~)
  KVS_MIRROR_VSCODE_S3_PREFIX      mirror: S3 存储前缀 (如 https://oss.example.com/vsc)
  KVS_MIRROR_VSCODE_S3_ACCESS      mirror: S3 access key ID
  KVS_MIRROR_VSCODE_S3_SECRET      mirror: S3 secret access key
  KVS_MIRROR_VSCODE_S3_REGION      mirror: S3 region (默认空)

示例:
  kvs -c kvs.ini                    用配置文件启动
  kvs -c default                    用内置默认配置启动
  kvs -n "/=http://127.0.0.1:8080"  简单代理，自动补充 -c default
  KVS_LOGIN_TOKEN=secret kvs -c default 带鉴权的默认配置启动
`)
}

// demoCommand writes a copy of the embedded kvs.ini.example to ./kvs.ini in
// the current directory. If kvs.ini already exists, it prints an error and
// exits non-zero so the operator's existing config is never overwritten.
func demoCommand() {
	const dest = "kvs.ini"
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(os.Stderr, "error: %s already exists in the current directory\n", dest)
		os.Exit(1)
	}
	data := mustAsset("kvs.ini.example")
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", dest, err)
		os.Exit(1)
	}
	fmt.Printf("created %s (%d bytes)\n", dest, len(data))
}

func main() {
	// Subcommand: "help" / "-h" / "--help" — print usage.
	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help") {
		helpCommand()
		return
	}

	// Subcommand: "demo" — generate a starter kvs.ini from the embedded example.
	if len(os.Args) > 1 && os.Args[1] == "demo" {
		demoCommand()
		return
	}

	// Subcommand: "mirror" — sync vscode versions to S3-compatible storage.
	if len(os.Args) > 1 && os.Args[1] == "mirror" {
		mirrorCommand(os.Args[2:])
		return
	}

	cfg := loadInitConfig()

	// Apply cache_dir as the proxy cache root (disk storage).
	// proxyPathPrefix is set only when cache_dir is configured AND proxy_path
	// is non-empty; otherwise it stays "" so the cache route is not registered
	// and isPublicAuthPath won't accidentally treat every path as public
	// (empty prefix matches all).
	if cfg.SvcCacheDir != "" {
		cacheOverride = filepath.Join(cfg.SvcCacheDir, "cache", "ccproxy")
		proxyPathPrefix = normalizeProxyPath(cfg.SvcProxyPath)
	}

	// Service preparation state (for showing loading page during download/extract).
	srvState := &serviceState{}

	// Build (prefix → handler) routes for each backend. The service backend's
	// prefix is recorded separately so the loading page is only shown for it
	// during preparation (non-service backends stay reachable).
	type route struct {
		prefix    string
		re        *regexp.Regexp // compiled regex for ^-prefixed backends, nil otherwise
		handler   http.Handler
		isService bool
	}
	routes := make([]route, len(cfg.Proxies))
	servicePrefix := "" // prefix of the kvs-managed service backend, "" if none
	for i, b := range cfg.Proxies {
		var re *regexp.Regexp
		if b.IsRegex {
			compiled, err := regexp.Compile(b.Prefix)
			if err != nil {
				log.Fatalf("invalid regex %q: %v", b.Prefix, err)
			}
			re = compiled
		}
		routes[i] = route{prefix: b.Prefix, re: re, handler: createBackendHandler(b, cfg.Headers, cfg.LoginAuthz), isService: b.IsService}
		if b.IsService {
			servicePrefix = b.Prefix
		}
	}

	// serviceCmd holds the backend subprocess; started either eagerly (no
	// service backend) or lazily after preparation completes (service backend).
	// Uses a pointer so the /__restart handler can set it to nil after killing.
	serviceCmd := (*exec.Cmd)(nil)

	// setCookie writes a cookie with the given value and MaxAge.
	setCookie := func(w http.ResponseWriter, value string, maxAge int) {
		http.SetCookie(w, &http.Cookie{
			Name:     cfg.CookieName,
			Value:    value,
			Path:     "/",
			MaxAge:   maxAge,
			HttpOnly: true,
			SameSite: http.SameSiteLaxMode,
		})
	}

	mux := http.NewServeMux()

	// /__login – serves login page (GET) or processes form (POST).
	mux.HandleFunc("/__login", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			serveStaticAsset(w, "login.html")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		tkn := strings.TrimSpace(r.PostFormValue("token"))
		if tkn == "" || subtle.ConstantTimeCompare([]byte(tkn), []byte(cfg.LoginToken)) != 1 {
			serveStaticAsset(w, "login.html")
			return
		}
		// Generate cookie value based on login_timeout mode.
		cookieVal := generateCookieValue(cfg.LoginToken, cfg.LoginTimeout)
		maxAge := 0 // session lifetime
		if cfg.LoginTimeout > 0 {
			maxAge = cfg.LoginTimeout
		}
		setCookie(w, cookieVal, maxAge)
		back := safeReferer(r.Referer(), r.Host)
		log.Printf("login ok, cookie %s set, reloading: %s", cfg.CookieName, back)
		http.Redirect(w, r, back, http.StatusSeeOther)
	})

	// /__logout – clears the token cookie. Returns a minimal JSON response
	// (not a page) so it can be called via fetch from the logout button script.
	// The caller is responsible for reloading/redirecting after logout.
	mux.HandleFunc("/__logout", func(w http.ResponseWriter, r *http.Request) {
		setCookie(w, "", -1)
		log.Printf("logout: cleared cookie %s", cfg.CookieName)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"logout success"}`))
	})

	// /__restart – kills the backend subprocess and resets the service state so
	// the next request re-triggers version detection, download, extract, and
	// start. Requires authentication (not in isPublicAuthPath). Only works when
	// a service backend exists and was kvs-managed.
	// /__restart        → simple restart, clears any version override
	// /__restart?v=1.32.1 → restart with a specific version override
	// /__restart/1.32.1 → same, path-style
	restartHandler := func(w http.ResponseWriter, r *http.Request) {
		// Extract optional version: path value (/__restart/1.32.1) or query (?v=1.32.1)
		overrideVersion := r.PathValue("v")
		if overrideVersion == "" {
			overrideVersion = r.URL.Query().Get("v")
		}

		if servicePrefix == "" {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `<!DOCTYPE html><html><body><h3>No service backend configured.</h3><a href="/">Go Home</a></body></html>`)
			return
		}

		// Set or clear the global version override BEFORE reloading config.
		// This must happen even if the backend is not running, so that the
		// version takes effect on the next preparation cycle.
		if overrideVersion != "" {
			log.Printf("[restart] overriding version to %s", overrideVersion)
			gOverrideVersion = overrideVersion
		} else {
			// No version specified → clear any previous override so
			// resolveVersion falls back to version_latest_url.
			if gOverrideVersion != "" {
				log.Printf("[restart] clearing version override (was %s)", gOverrideVersion)
				gOverrideVersion = ""
			}
		}

		// Reload config so download/bin_home are re-expanded with the new (or
		// cleared) version. If version resolve fails (e.g. 404), loadInitConfig
		// stores the error in cfg.InitError instead of crashing. prepareService
		// will surface it on the loading page as a download failure.
		newCfg := loadInitConfig()
		cfg = newCfg // always update cfg so InitError (if any) is visible to prepareService

		// Kill the running backend (if any) and reset state.
		if serviceCmd != nil && serviceCmd.Process != nil {
			log.Printf("[restart] killing backend process group")
			killProcessGroup(serviceCmd)
			// Run stop_shell if configured.
			if cfg.SvcStopShell != "" {
				log.Printf("[restart] running stop_shell: %s", cfg.SvcStopShell)
				if err := runServiceStartup(cfg.SvcStopShell); err != nil {
					log.Printf("[restart] stop_shell error: %v", err)
				}
			}
		}
		// Clean up unix socket files.
		for _, b := range cfg.Proxies {
			if b.IsService && b.Scheme == "unix" {
				_ = os.Remove(b.Target)
			}
		}
		// Reset state so next request re-triggers preparation.
		serviceCmd = nil
		srvState.reset()

		// If config reload failed (e.g. version hash 404), report the error
		// but don't crash — the loading page will show it on next access.
		if newCfg.InitError != "" {
			srvState.finish(fmt.Errorf("%s", newCfg.InitError))
			log.Printf("[restart] config reload error: %s", newCfg.InitError)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `<!DOCTYPE html><html><body><h3>Restart failed: %s</h3><p><a href="/">Go Home</a> | <a href="/__restart">Retry Restart</a></p></body></html>`, newCfg.InitError)
			return
		}

		log.Printf("[restart] service reset, will re-prepare on next request")
		// Redirect to home so the browser reloads and re-triggers preparation.
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
	mux.HandleFunc("/__restart", restartHandler)
	mux.HandleFunc("/__restart/{v}", restartHandler)

	// /favicon.ico – a minimal inline SVG favicon (blue rounded square with "C")
	// so browsers don't log 404s for it. Modern browsers accept image/svg+xml.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		serveStaticAsset(w, "favicon.ico")
	})

	// cache proxy – {proxy_path}/{scheme}:{host}/path → {scheme}://{host}/path
	// Only registered when both cache_dir (disk storage) and proxy_path (route
	// prefix) are configured. Either one empty disables the cache proxy route.
	if proxyPathPrefix != "" {
		mux.HandleFunc(proxyPathPrefix, handleCache)
	}

	// /__logout.vsc.js – VS Code logout-button script injected into proxied HTML.
	// Named .vsc because it targets the VS Code activity-bar toolbar; other apps
	// can get their own script (e.g. /__logout.xxx.js) later.
	mux.HandleFunc("/__logout.vsc.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(mustAsset("logout.vsc.js"))
	})

	// All other requests: dispatch to backends. The service backend is
	// prepared lazily on first access: while preparing, its prefix returns the
	// loading page; once done (success), it proxies normally; on error, 500.
	// Non-service backends are always reachable.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		for _, rt := range routes {
			// Match: regex backends use regexp.Match; others use prefix match.
			if rt.re != nil {
				if !rt.re.MatchString(r.URL.Path) {
					continue
				}
			} else {
				if !strings.HasPrefix(r.URL.Path, rt.prefix) {
					continue
				}
			}
			if rt.isService {
				// Trigger preparation if not already running or succeeded.
				if srvState.begin() {
					go func() {
						managed, err := prepareService(cfg, srvState)
						if err == nil && managed && cfg.SvcCommand != "" {
							// Clean up stale unix socket files left by a previous crash.
							for _, b := range cfg.Proxies {
								if b.IsService && b.Scheme == "unix" {
									_ = os.Remove(b.Target)
								}
							}
							// Start the backend subprocess after a successful prepare.
							if c := startBackend(cfg.SvcCommand); c != nil {
								serviceCmd = c
							}
						}
						srvState.finish(err)
						if err != nil {
							log.Printf("[prepare] service preparation failed: %v", err)
						}
					}()
				}
				// Preparing? Show loading page.
				if srvState.active() {
					serveLoadingPage(w, srvState.getStatus())
					return
				}
				// Finished: proxy on success, 500 on error.
				if done, err := srvState.result(); done {
					if err != nil {
						http.Error(w, "service preparation failed: "+err.Error(), http.StatusInternalServerError)
						return
					}
					rt.handler.ServeHTTP(w, r)
					return
				}
				// not yet active (goroutine just scheduled) — loading page.
				serveLoadingPage(w, srvState.getStatus())
				return
			}
			rt.handler.ServeHTTP(w, r)
			return
		}
		http.Error(w, "no backend matched", http.StatusBadGateway)
	})

	// When token is set, wrap the mux with a token-checking middleware:
	// requests carrying a cookie whose value equals the configured token pass
	// through; all others get 401. The login/logout endpoints and other kvs
	// internal assets are always reachable so the user can authenticate.
	finalHandler := http.Handler(mux)
	if cfg.LoginToken != "" {
		finalHandler = authMiddleware(mux, cfg, setCookie)
	}

	servers := buildServers(cfg.Port, cfg.UseSSL, finalHandler)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP servers first so the loading page is available during preparation.
	for _, srv := range servers {
		go func(s *http.Server, isTLS bool) {
			var err error
			if isTLS {
				err = s.ListenAndServeTLS("", "")
			} else {
				err = s.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				log.Fatalf("server on %s: %v", s.Addr, err)
			}
		}(srv.server, srv.isTLS)
	}

	log.Printf("proxy starting: %s", strings.Join(serverAddrs(servers), ", "))
	log.Printf("cookie name: %s", cfg.CookieName)
	if cfg.LoginToken != "" {
		mode := "session"
		if cfg.LoginTimeout > 0 {
			mode = fmt.Sprintf("expiring (%ds, auto-renew at 1/4)", cfg.LoginTimeout)
		}
		log.Printf("cookie auth: enabled (cookie %s, mode: %s)", cfg.CookieName, mode)
	}
	log.Printf("backends: %d", len(cfg.Proxies))
	for i, b := range cfg.Proxies {
		marker := ""
		if b.IsService {
			marker = " (service)"
		}
		log.Printf("  route[%d] %s → %s://%s%s", i, b.Prefix, b.Scheme, b.Target, marker)
	}
	if len(cfg.Headers) > 0 {
		log.Printf("proxy headers: %v", cfg.Headers)
	}

	// Start backend subprocess if KVS_SVC_COMMAND is configured. When a service
	// backend exists, the subprocess is started lazily after preparation
	// completes (see the service route handler above); otherwise it starts now.
	if cfg.SvcCommand != "" && servicePrefix == "" {
		serviceCmd = startBackend(cfg.SvcCommand)
	}

	<-sigCh
	log.Printf("shutdown signal received, draining…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.server.Shutdown(shutdownCtx)
	}
	if serviceCmd != nil {
		// Only kvs-managed backends (started via startBackend) are cleaned up.
		// External/system services (detected via check, serviceCmd stays nil)
		// are never killed or touched by kvs on shutdown.
		killProcessGroup(serviceCmd)
		// Run stop_shell if configured (kvs-managed backend only).
		if cfg.SvcStopShell != "" {
			log.Printf("[shutdown] running stop_shell: %s", cfg.SvcStopShell)
			if err := runServiceStartup(cfg.SvcStopShell); err != nil {
				log.Printf("[shutdown] stop_shell error: %v", err)
			}
		}
		for _, b := range cfg.Proxies {
			if b.IsService && b.Scheme == "unix" {
				_ = os.Remove(b.Target)
			}
		}
	}
}

// serverInstance binds an *http.Server with its TLS flag.
type serverInstance struct {
	server *http.Server
	isTLS  bool
}

// buildServers constructs one (plain HTTP) or two (HTTP + HTTPS) servers with
// sensible timeouts and the self-signed cert when SSL is enabled.
func buildServers(port string, useSSL bool, mux http.Handler) []serverInstance {
	base := newCacheServer(":"+port, mux, nil)
	if !useSSL {
		return []serverInstance{{server: base, isTLS: false}}
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
	return []serverInstance{
		{server: base, isTLS: false},
		{server: httpsSrv, isTLS: true},
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

func serverAddrs(servers []serverInstance) []string {
	out := make([]string, 0, len(servers))
	for _, s := range servers {
		scheme := "http"
		if s.isTLS {
			scheme = "https"
		}
		out = append(out, scheme+"://"+s.server.Addr)
	}
	return out
}

// safeReferer returns a same-origin redirect target, falling back to "/".
// It accepts both relative paths and absolute URLs: when the referer is an
// absolute URL on the same host (r.Host), only the path+query is returned so
// the redirect stays same-origin. Cross-origin or unparseable referers fall
// back to "/". This preserves query strings (e.g. ?folder=/wsc) that would
// otherwise be lost.
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

// =============================================================================
// Cache — handles proxy_path endpoints
// =============================================================================

// allowedCacheSchemes restricts the cache to web schemes to mitigate SSRF.
var allowedCacheSchemes = map[string]bool{"http": true, "https": true}

// cacheTransport is a shared transport for the cache with a response
// header timeout so a slow/hung upstream cannot hold connections indefinitely.
var cacheTransport = &http.Transport{
	Proxy:                 http.ProxyFromEnvironment,
	DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
	ResponseHeaderTimeout: 30 * time.Second,
	IdleConnTimeout:       90 * time.Second,
	MaxIdleConns:          100,
	MaxIdleConnsPerHost:   10,
}

// =============================================================================
// Proxy Cache
// =============================================================================

// --- cache cache init (sync.Once, disk-only cache) ---

var (
	cacheOnce       sync.Once
	cacheRoot       string // resolved cache root at first cache request; route only registered when non-empty
	cacheOverride   string // set from SvcCacheDir in main()
	proxyPathPrefix string // normalized proxy_path prefix (e.g. "/__cache/"), set in main()
)

// initCache resolves the cache root from the cache_dir config. The proxy_path route
// is only registered when cache_dir is set, so the root is always non-empty
// here; this just stores it for the cache handlers.
func initCache() {
	cacheRoot = cacheOverride
	log.Printf("[cache] cache root: %s", cacheRoot)
}

// redirectLimit is a CheckRedirect policy shared by cache and download clients.
func redirectLimit(max int) func(*http.Request, []*http.Request) error {
	return func(req *http.Request, via []*http.Request) error {
		if len(via) >= max {
			return fmt.Errorf("too many redirects")
		}
		return nil
	}
}

// cacheClient is used for manual upstream fetches when caching.
var cacheClient = &http.Client{
	Transport:     cacheTransport,
	Timeout:       5 * time.Minute,
	CheckRedirect: redirectLimit(10),
}

// cacheHeaderWhitelist lists headers preserved in cache metadata.
var cacheHeaderWhitelist = map[string]bool{
	"content-type":        true,
	"content-length":      true,
	"content-encoding":    true,
	"cache-control":       true,
	"etag":                true,
	"last-modified":       true,
	"content-disposition": true,
}

// cacheMeta holds the cached HTTP status and a subset of response headers.
type cacheMeta struct {
	Status  int
	Headers map[string][]string
}

// cachePaths returns the body and meta file paths for a cache entry.
//
// Cache layout mirrors the URL structure:
//
//	{KVS_SVC_PROXYPATH}/{scheme}:{host}/path/to/file.js       → body
//	{KVS_SVC_PROXYPATH}/{scheme}:{host}/path/to/file.js_.json  → metadata
//
// The rest path is cleaned and stripped of leading "/" to stay within
// the cache root.  Requests for the root path use "__index" as filename.
func cachePaths(scheme, host, rest string) (bodyPath, metaPath string) {
	// Clean and make relative to prevent directory traversal.
	p := strings.TrimPrefix(filepath.Clean(rest), "/")
	if p == "" || p == "." {
		p = "__index"
	}
	base := filepath.Join(cacheRoot, scheme+":"+host, p)
	return base, base + "_.json"
}

// readCacheMeta reads and parses a cache metadata file from disk.
func readCacheMeta(path string) (*cacheMeta, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var m cacheMeta
	if err := json.Unmarshal(data, &m); err != nil {
		return nil, err
	}
	return &m, nil
}

// writeCacheMeta atomically writes cache metadata as JSON.
func writeCacheMeta(path string, m *cacheMeta) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, data, 0o644)
}

// =============================================================================
// Cache Handler
// =============================================================================

// handleCache proxies {proxy_path}/[...] → target URL.
//
// Format:  {proxy_path}/[cc~]{scheme}:{host}[/path][?query]
//
//	cc~              — optional cache marker: check cache, write on MISS
//	{scheme}:        — optional scheme (http, https); defaults to https
//	{host}           — upstream host[:port]
func handleCache(w http.ResponseWriter, r *http.Request) {
	cacheOnce.Do(initCache)

	p := strings.TrimPrefix(r.URL.Path, proxyPathPrefix)
	if p == "" || p == "/" {
		http.Error(w, "missing domain/path", http.StatusBadRequest)
		return
	}

	// Detect cc~ cache prefix.
	cacheable := strings.HasPrefix(p, "cc~")
	if cacheable {
		p = p[3:]
	}
	// Only cache safe GET responses.
	if r.Method != http.MethodGet {
		cacheable = false
	}

	// Parse scheme:host/rest from the path.
	scheme, host, rest, err := parseCachePath(p)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	targetURL := buildTargetURL(scheme, host, rest, r.URL.RawQuery)

	log.Printf("[cache] %s %s → %s (cacheable=%v)", r.Method, r.URL.Path, targetURL, cacheable)

	if !cacheable {
		// Plain passthrough — no cache.
		handlePassThroughCache(w, r, targetURL)
		return
	}

	// Try cache lookup; on MISS fetch, stream and store.
	bodyPath, metaPath := cachePaths(scheme, host, rest)
	if serveFromCache(w, bodyPath, metaPath, targetURL) {
		return
	}
	handleCachedCache(w, r, targetURL, bodyPath, metaPath)
}

// parseCachePath extracts scheme, host and rest from the cache path segment.
// Format: scheme:host/rest or host/rest.  Returns an error string for bad input.
func parseCachePath(p string) (scheme, host, rest string, err error) {
	slashIdx := strings.Index(p, "/")
	if slashIdx >= 0 {
		rest = p[slashIdx:]
		p = p[:slashIdx]
	} else {
		rest = "/"
	}

	if idx := strings.Index(p, ":"); idx >= 0 {
		scheme = strings.ToLower(p[:idx])
		host = p[idx+1:]
	} else {
		scheme = "https"
		host = p
	}

	if host == "" {
		return "", "", "", fmt.Errorf("missing host")
	}
	if !allowedCacheSchemes[scheme] {
		return "", "", "", fmt.Errorf("unsupported scheme")
	}
	return scheme, host, rest, nil
}

// buildTargetURL constructs the full upstream URL with a strings.Builder.
func buildTargetURL(scheme, host, rest, rawQuery string) string {
	var b strings.Builder
	b.Grow(len(scheme) + 3 + len(host) + len(rest) + len(rawQuery) + 1)
	b.WriteString(scheme)
	b.WriteString("://")
	b.WriteString(host)
	b.WriteString(rest)
	if rawQuery != "" {
		b.WriteByte('?')
		b.WriteString(rawQuery)
	}
	return b.String()
}

// serveFromCache tries to serve a response from cache. Returns true on HIT.
func serveFromCache(w http.ResponseWriter, bodyPath, metaPath, targetURL string) bool {
	meta, err := readCacheMeta(metaPath)
	if err != nil {
		return false
	}
	body, err := os.ReadFile(bodyPath)
	if err != nil {
		return false
	}
	log.Printf("[cache] cache HIT  %s ← %s (%d bytes)", targetURL, bodyPath, len(body))
	w.Header().Set("X-Cache", "HIT")
	for k, vs := range meta.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.WriteHeader(meta.Status)
	_, _ = w.Write(body)
	return true
}

// handleCachedCache fetches the upstream, streams the response to both the
// client and a cache file, and writes cache metadata on success.
func handleCachedCache(w http.ResponseWriter, r *http.Request, targetURL, bodyPath, metaPath string) {
	log.Printf("[cache] cache MISS (will cache) %s", targetURL)

	// cacheClient already enforces a 5m timeout; reuse the request context
	// so client disconnects cancel the upstream fetch.
	req, err := http.NewRequestWithContext(r.Context(), "GET", targetURL, nil)
	if err != nil {
		http.Error(w, "bad target URL", http.StatusBadRequest)
		return
	}
	for k, vs := range r.Header {
		switch strings.ToLower(k) {
		case "accept", "accept-encoding", "accept-language", "user-agent":
			for _, v := range vs {
				req.Header.Add(k, v)
			}
		}
	}

	resp, err := cacheClient.Do(req)
	if err != nil {
		log.Printf("[cache] fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	// Non-2xx: stream through without caching.
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		log.Printf("[cache] upstream returned %d, not caching", resp.StatusCode)
		for k, vs := range resp.Header {
			for _, v := range vs {
				w.Header().Add(k, v)
			}
		}
		w.Header().Set("X-Cache", "MISS")
		w.WriteHeader(resp.StatusCode)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Collect whitelisted headers for cache metadata.
	meta := &cacheMeta{
		Status:  resp.StatusCode,
		Headers: make(map[string][]string),
	}
	for k, vs := range resp.Header {
		if cacheHeaderWhitelist[strings.ToLower(k)] {
			meta.Headers[k] = vs
		}
	}

	// Set response headers before writing body.
	for k, vs := range meta.Headers {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)

	// Ensure cache directory exists.
	if err := os.MkdirAll(filepath.Dir(bodyPath), 0o755); err != nil {
		log.Printf("[cache] mkdir cache FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}

	// Stream to client + temp file simultaneously.
	tmp, err := os.CreateTemp(filepath.Dir(bodyPath), ".tmp-*")
	if err != nil {
		log.Printf("[cache] create temp FAIL: %v", err)
		_, _ = io.Copy(w, resp.Body)
		return
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(tmpName)
		}
	}()

	n, copyErr := io.Copy(io.MultiWriter(w, tmp), resp.Body)
	_ = tmp.Close()
	if copyErr != nil {
		log.Printf("[cache] copy error: %v", copyErr)
		return
	}

	// Atomic rename temp → final body.
	if err := os.Rename(tmpName, bodyPath); err != nil {
		log.Printf("[cache] rename cache FAIL: %s → %v", bodyPath, err)
		return
	}
	committed = true

	// Write metadata.
	if err := writeCacheMeta(metaPath, meta); err != nil {
		log.Printf("[cache] write meta FAIL: %s → %v", metaPath, err)
	} else {
		log.Printf("[cache] CACHED %s → %s (%d bytes)", targetURL, bodyPath, n)
	}
}

// handlePassThroughCache proxies the request to the upstream without caching.
func handlePassThroughCache(w http.ResponseWriter, r *http.Request, targetURL string) {
	log.Printf("[cache] passthrough %s", targetURL)

	req, err := http.NewRequestWithContext(r.Context(), r.Method, targetURL, r.Body)
	if err != nil {
		http.Error(w, "bad target URL", http.StatusBadRequest)
		return
	}
	// Forward original headers; net/http strips hop-by-hop on the wire.
	req.Header = r.Header.Clone()

	resp, err := cacheClient.Do(req)
	if err != nil {
		log.Printf("[cache] fetch error: %v", err)
		http.Error(w, "upstream fetch failed", http.StatusBadGateway)
		return
	}
	defer resp.Body.Close()

	for k, vs := range resp.Header {
		for _, v := range vs {
			w.Header().Add(k, v)
		}
	}
	w.Header().Set("X-Cache", "MISS")
	w.WriteHeader(resp.StatusCode)
	_, _ = io.Copy(w, resp.Body)
}

// atomicWriteFile writes data to path via a temp file + rename for crash safety.
func atomicWriteFile(path string, data []byte, perm os.FileMode) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	cleanup := func() { _ = os.Remove(tmpName) }
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		cleanup()
		return err
	}
	if err := tmp.Close(); err != nil {
		cleanup()
		return err
	}
	if err := os.Chmod(tmpName, perm); err != nil {
		cleanup()
		return err
	}
	return os.Rename(tmpName, path)
}

// =============================================================================
// Backend Handlers — createBackendHandler dispatches to the right handler type.
// =============================================================================

// createBackendHandler builds an http.Handler for the given backend.
// Supported schemes: http, https, unix (reverse proxy), file (directory), text (literal).
func createBackendHandler(b Backend, cacheHeaders map[string]string, loginAuthz bool) http.Handler {
	switch b.Scheme {
	case "http", "https":
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
		body := mustAsset("login.html")
		r.Body = io.NopCloser(bytes.NewReader(body))
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

// startBackend runs a command directly as a subprocess in its own process group.
// Note: command is split with strings.Fields, so arguments cannot contain spaces.
func startBackend(cmdStr string) *exec.Cmd {
	parts := strings.Fields(cmdStr)
	if len(parts) == 0 {
		return nil
	}
	cmd := exec.Command(parts[0], parts[1:]...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		log.Fatalf("start backend command: %v", err)
	}
	log.Printf("backend command started (pid %d)", cmd.Process.Pid)
	return cmd
}

// killProcessGroup sends SIGTERM to the process group of the given command.
func killProcessGroup(cmd *exec.Cmd) {
	if cmd == nil || cmd.Process == nil {
		return
	}
	pgid, err := syscall.Getpgid(cmd.Process.Pid)
	if err != nil {
		log.Printf("get pgid: %v", err)
		return
	}
	if err := syscall.Kill(-pgid, syscall.SIGTERM); err != nil {
		log.Printf("kill backend pgid %d: %v", pgid, err)
	} else {
		log.Printf("sent SIGTERM to backend process group %d", pgid)
	}
}

// serveStaticAsset writes an embedded asset with the given content type.
func serveStaticAsset(w http.ResponseWriter, name string) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_, _ = w.Write(mustAsset(name))
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
func authMiddleware(next http.Handler, cfg Config, setCookie func(http.ResponseWriter, string, int)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isPublicAuthPath(r.URL.Path) {
			next.ServeHTTP(w, r)
			return
		}
		c, err := r.Cookie(cfg.CookieName)
		if err != nil || c.Value == "" {
			serveStaticAsset(w, "login.html")
			return
		}

		if cfg.LoginTimeout <= 0 {
			// Plain mode: direct comparison.
			if subtle.ConstantTimeCompare([]byte(c.Value), []byte(cfg.LoginToken)) != 1 {
				serveStaticAsset(w, "login.html")
				return
			}
			next.ServeHTTP(w, r)
			return
		}

		// Hashed mode: parse "<hash>.<ts>.<salt>".
		ok, refresh := validateHashedCookie(c.Value, cfg.LoginToken, cfg.LoginTimeout)
		if !ok {
			log.Printf("[authz] cookie validation failed: %q", c.Value)
			serveStaticAsset(w, "login.html")
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

// ------------------------------------------------------------------------------
// 将应用镜像同步s3服务器上， 这个是为 vscode 定制的功能

// mirrorConfig holds the [mirror] section settings.
type mirrorConfig struct {
	S3Prefix    string // s3_prefix — e.g. https://oss.example.com/vsc
	S3Access    string // s3_access — access key ID
	S3Secret    string // s3_secret — secret access key
	S3Region    string // s3_region — region (default empty)
	VSCPlatform string // vsc_platform — e.g. server-linux-x64-web (default: server-linux-x64-web)
	VSCBaseURL  string // vsc_base_url — API base URL (default: https://update.code.visualstudio.com)
	VSCDownload string // vsc_download — download URL template, supports {name} and {hash}; empty=use API returned url
	CacheDir    string // cache_dir — from [service] section, for caching tarballs
}

// loadMirrorConfig reads the [mirror] section from the config file
// specified by -c. Values are expanded via expandValue (supports {VAR} and
// {VAR:-default} syntax), requiring SVC_HOME to be resolved first from
// [service].home so that cache_dir = {SVC_HOME}/cache expands correctly.
func loadMirrorConfig() mirrorConfig {
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
	if cfgPath == "" {
		log.Fatal("mirror: config file required: use -c <path> or -c default")
	}

	var ini *iniFile
	if cfgPath == "default" {
		log.Printf("mirror: loading config: default (embedded kvs.ini.example)")
		var err error
		ini, err = parseIniData(mustAsset("kvs.ini.example"), "kvs.ini.example")
		if err != nil {
			log.Fatalf("mirror: parse embedded config: %v", err)
		}
	} else {
		var err error
		ini, err = parseIni(cfgPath)
		if err != nil {
			log.Fatalf("mirror: parse config: %v", err)
		}
	}
	// Build svcVars: resolve SVC_HOME first so {SVC_HOME} can be expanded
	// in cache_dir and other [service] fields.
	svcVars := map[string]string{}
	svcHome := expandValue(ini.get("service.home"), svcVars)
	svcVars["SVC_HOME"] = svcHome
	_ = os.Setenv("SVC_HOME", svcHome)

	mc := mirrorConfig{
		S3Prefix:    expandValue(ini.get("mirror.s3_prefix"), svcVars),
		S3Access:    expandValue(ini.get("mirror.s3_access"), svcVars),
		S3Secret:    expandValue(ini.get("mirror.s3_secret"), svcVars),
		S3Region:    expandValue(ini.get("mirror.s3_region"), svcVars),
		VSCPlatform: expandValue(ini.get("mirror.vsc_platform"), svcVars),
		VSCBaseURL:  expandValue(ini.get("mirror.vsc_base_url"), svcVars),
		VSCDownload: expandValue(ini.get("mirror.vsc_download"), svcVars),
		CacheDir:    expandValue(ini.get("service.cache_dir"), svcVars),
	}
	if mc.VSCPlatform == "" {
		mc.VSCPlatform = "server-linux-x64-web"
	}
	if mc.VSCBaseURL == "" {
		mc.VSCBaseURL = "https://update.code.visualstudio.com"
	}
	return mc
}

// mirrorCommand implements `kvs mirror -c <config> [version]`.
//
//   - `kvs mirror -c kvs.ini`          → sync the latest version
//   - `kvs mirror -c kvs.ini 1.132.1`  → sync a specific version
//
// latest mode: first fetch /api/latest/ to get the version name, then
// fetch /api/versions/{name}/ to get the full metadata (url, hash, etc.)
//
// S3 layout:
//   - {s3_prefix}/stable/{hash}/vscode-server-linux-x64-web.tar.gz   → tarball
//   - {s3_prefix}/api/latest/server-linux-x64-web/stable            → latest JSON (latest only)
//   - {s3_prefix}/api/versions/{name}/server-linux-x64-web/stable   → version JSON
func mirrorCommand(args []string) {
	if len(args) == 0 {
		fmt.Fprintf(os.Stderr, "usage: kvs mirror -c <config> [version]\n")
		os.Exit(1)
	}
	// Extract version from args, skipping -c <config> / -c=<config>.
	version := "latest"
	for i := 0; i < len(args); i++ {
		if args[i] == "-c" {
			i++ // skip config path
			continue
		}
		if strings.HasPrefix(args[i], "-c=") {
			continue
		}
		version = args[i]
		break
	}

	mc := loadMirrorConfig()
	if mc.S3Prefix == "" {
		fmt.Fprintf(os.Stderr, "mirror: s3_prefix is not configured, mirror disabled\n")
		os.Exit(1)
	}
	log.Printf("[mirror] s3_prefix=%s", mc.S3Prefix)

	client := buildDownloadClient("")

	// Step 1: Fetch version metadata JSON.
	// latest mode: first fetch /api/latest/ to get the version name, then
	// fetch /api/versions/{name}/ to get the full metadata (url, hash, etc.)
	// specified version: directly fetch /api/versions/{version}/
	var apiURL string
	if version == "latest" {
		latestAPI := mc.VSCBaseURL + "/api/latest/" + mc.VSCPlatform + "/stable"
		log.Printf("[mirror] fetching latest: %s", latestAPI)
		resp, err := client.Get(latestAPI)
		if err != nil {
			log.Fatalf("[mirror] fetch latest: %v", err)
		}
		if resp.StatusCode != http.StatusOK {
			resp.Body.Close()
			log.Fatalf("[mirror] latest API returned HTTP %d", resp.StatusCode)
		}
		latestJSON, err := io.ReadAll(resp.Body)
		resp.Body.Close()
		if err != nil {
			log.Fatalf("[mirror] read latest: %v", err)
		}
		log.Printf("[mirror] latest response: %s", string(latestJSON))
		var latestMeta map[string]interface{}
		if err := json.Unmarshal(latestJSON, &latestMeta); err != nil {
			log.Fatalf("[mirror] parse latest JSON: %v", err)
		}
		latestName, _ := latestMeta["name"].(string)
		if latestName == "" {
			log.Fatalf("[mirror] missing name in latest response")
		}
		log.Printf("[mirror] latest version name: %s", latestName)
		apiURL = mc.VSCBaseURL + "/api/versions/" + latestName + "/" + mc.VSCPlatform + "/stable"
	} else {
		apiURL = mc.VSCBaseURL + "/api/versions/" + version + "/" + mc.VSCPlatform + "/stable"
	}
	log.Printf("[mirror] fetching %s", apiURL)
	resp, err := client.Get(apiURL)
	if err != nil {
		log.Fatalf("[mirror] fetch metadata: %v", err)
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		log.Fatalf("[mirror] metadata API returned HTTP %d", resp.StatusCode)
	}
	metaJSON, err := io.ReadAll(resp.Body)
	resp.Body.Close()
	if err != nil {
		log.Fatalf("[mirror] read metadata: %v", err)
	}
	log.Printf("[mirror] metadata: %s", string(metaJSON))

	// Parse metadata JSON.
	var meta map[string]interface{}
	if err := json.Unmarshal(metaJSON, &meta); err != nil {
		log.Fatalf("[mirror] parse metadata JSON: %v", err)
	}
	downloadURL, _ := meta["url"].(string)
	metaVersion, _ := meta["version"].(string) // commit hash
	metaName, _ := meta["name"].(string)       // version name e.g. 1.132.1
	if downloadURL == "" || metaVersion == "" {
		log.Fatalf("[mirror] missing url or version in metadata")
	}
	log.Printf("[mirror] version=%s hash=%s url=%s", metaName, metaVersion, downloadURL)

	// If vsc_download is configured, use it to build the download URL.
	if mc.VSCDownload != "" {
		url := mc.VSCDownload
		url = strings.ReplaceAll(url, "{name}", metaName)
		url = strings.ReplaceAll(url, "{hash}", metaVersion)
		downloadURL = url
		log.Printf("[mirror] using vsc_download: %s", downloadURL)
	}

	// Step 2: Download the tarball (use cache_dir if configured).
	var tarballPath string
	if mc.CacheDir != "" {
		tarballPath = filepath.Join(mc.CacheDir, "cache", "version", metaName+"_"+metaVersion+".tar.gz")
		if _, err := os.Stat(tarballPath); err == nil {
			log.Printf("[mirror] tarball cached: %s", tarballPath)
		} else {
			log.Printf("[mirror] downloading tarball: %s", downloadURL)
			var lastDlPct int64 = -1
			if err := downloadFile(client, downloadURL, tarballPath, func(written, total int64) {
				if total > 0 {
					pct := written * 100 / total
					if pct != lastDlPct {
						lastDlPct = pct
						fmt.Fprintf(os.Stderr, "\r[mirror] download progress: %d%%", pct)
					}
				}
			}); err != nil {
				fmt.Fprintln(os.Stderr)
				log.Fatalf("[mirror] download tarball: %v", err)
			}
			fmt.Fprintln(os.Stderr)
			log.Printf("[mirror] download complete: %s", tarballPath)
		}
	} else {
		tmpFile, err := os.CreateTemp("", "vscode-mirror-*.tar.gz")
		if err != nil {
			log.Fatalf("[mirror] create temp file: %v", err)
		}
		tarballPath = tmpFile.Name()
		defer os.Remove(tarballPath)
		log.Printf("[mirror] downloading tarball: %s", downloadURL)
		var lastDlPct2 int64 = -1
		if err := downloadFile(client, downloadURL, tarballPath, func(written, total int64) {
			if total > 0 {
				pct := written * 100 / total
				if pct != lastDlPct2 {
					lastDlPct2 = pct
					fmt.Fprintf(os.Stderr, "\r[mirror] download progress: %d%%", pct)
				}
			}
		}); err != nil {
			fmt.Fprintln(os.Stderr)
			log.Fatalf("[mirror] download tarball: %v", err)
		}
		fmt.Fprintln(os.Stderr)
		log.Printf("[mirror] download complete")
	}

	// Step 3: Upload tarball to {s3_prefix}/stable/{hash}/{vsc_platform}.tar.gz
	tarballURL := mc.S3Prefix + "/stable/" + metaVersion + "/vscode-" + mc.VSCPlatform + ".tar.gz"
	log.Printf("[mirror] uploading tarball to %s", tarballURL)
	var lastUpPct int64 = -1
	if err := s3UploadFile(mc, tarballURL, tarballPath, "application/gzip", func(written, total int64) {
		if total > 0 {
			pct := written * 100 / total
			if pct != lastUpPct {
				lastUpPct = pct
				fmt.Fprintf(os.Stderr, "\r[mirror] upload progress: %d%%", pct)
			}
		}
	}); err != nil {
		fmt.Fprintln(os.Stderr)
		log.Fatalf("[mirror] upload tarball: %v", err)
	}
	fmt.Fprintln(os.Stderr)
	log.Printf("[mirror] tarball uploaded")

	// Upload version tag file: {s3_prefix}/stable/{hash}/{name}
	// Content is the original metadata JSON.
	if metaName != "" {
		tagURL := mc.S3Prefix + "/stable/" + metaVersion + "/" + metaName
		log.Printf("[mirror] uploading version tag to %s", tagURL)
		if err := s3UploadBytes(mc, tagURL, metaJSON, "application/json", nil); err != nil {
			log.Fatalf("[mirror] upload version tag: %v", err)
		}
		log.Printf("[mirror] version tag uploaded")
	}

	// Remove "url" field from metadata JSON before uploading to S3.
	// The cache server does not provide a download url; the download path is
	// constructed from {s3_prefix}/stable/{hash}/vscode-{platform}.tar.gz
	delete(meta, "url")
	cachedJSON, err := json.Marshal(meta)
	if err != nil {
		log.Fatalf("[mirror] marshal cached JSON: %v", err)
	}

	// Step 4: Upload latest JSON (latest mode only).
	if version == "latest" {
		latestURL := mc.S3Prefix + "/api/latest/" + mc.VSCPlatform + "/stable"
		log.Printf("[mirror] uploading latest JSON to %s", latestURL)
		if err := s3UploadBytes(mc, latestURL, cachedJSON, "application/json", nil); err != nil {
			log.Fatalf("[mirror] upload latest JSON: %v", err)
		}
		log.Printf("[mirror] latest JSON uploaded")
	}

	// Step 5: Upload version JSON.
	versionURL := mc.S3Prefix + "/api/versions/" + metaName + "/" + mc.VSCPlatform + "/stable"
	log.Printf("[mirror] uploading version JSON to %s", versionURL)
	if err := s3UploadBytes(mc, versionURL, cachedJSON, "application/json", nil); err != nil {
		log.Fatalf("[mirror] upload version JSON: %v", err)
	}
	log.Printf("[mirror] version JSON uploaded")

	log.Printf("[mirror] done: version %s (hash %s)", metaName, metaVersion)
}

// progressReader wraps an io.Reader and reports read progress via onProgress.
type progressReader struct {
	r          *bytes.Reader
	total      int64
	written    int64
	onProgress func(written, total int64)
}

func (pr *progressReader) Read(p []byte) (int, error) {
	n, err := pr.r.Read(p)
	pr.written += int64(n)
	if pr.onProgress != nil && n > 0 {
		pr.onProgress(pr.written, pr.total)
	}
	return n, err
}

// s3UploadFile uploads a local file to an S3-compatible endpoint.
// onProgress (if non-nil) is called periodically with bytes uploaded and total.
func s3UploadFile(mc mirrorConfig, s3URL, localPath, contentType string, onProgress func(written, total int64)) error {
	data, err := os.ReadFile(localPath)
	if err != nil {
		return err
	}
	return s3UploadBytes(mc, s3URL, data, contentType, onProgress)
}

// s3UploadBytes uploads data to an S3-compatible endpoint using AWS Signature V4.
// onProgress (if non-nil) is called periodically with bytes uploaded and total.
func s3UploadBytes(mc mirrorConfig, s3URL string, data []byte, contentType string, onProgress func(written, total int64)) error {
	u, err := url.Parse(s3URL)
	if err != nil {
		return fmt.Errorf("parse S3 URL: %w", err)
	}

	host := u.Host
	objectKey := u.Path
	region := mc.S3Region
	if region == "" {
		region = "us-east-1"
	}

	now := time.Now().UTC()
	dateStamp := now.Format("20060102")
	amzDate := now.Format("20060102T150405Z")
	payloadHash := sha256Hex(data)

	// Canonical request.
	// Content-Type must be included in the signature if it's sent as a header.
	canonicalURI := s3EncodePath(objectKey)
	canonicalHeaders := fmt.Sprintf("content-type:%s\nhost:%s\nx-amz-content-sha256:%s\nx-amz-date:%s\n",
		contentType, host, payloadHash, amzDate)
	signedHeaders := "content-type;host;x-amz-content-sha256;x-amz-date"
	canonicalRequest := fmt.Sprintf("PUT\n%s\n%s\n%s\n%s\n%s",
		canonicalURI, u.RawQuery, canonicalHeaders, signedHeaders, payloadHash)

	// String to sign.
	credentialScope := fmt.Sprintf("%s/%s/s3/aws4_request", dateStamp, region)
	stringToSign := fmt.Sprintf("AWS4-HMAC-SHA256\n%s\n%s\n%s",
		amzDate, credentialScope, sha256Hex([]byte(canonicalRequest)))

	// Signing key + signature.
	signingKey := s3SignKey(mc.S3Secret, dateStamp, region)
	signature := hex.EncodeToString(hmacSHA256(signingKey, []byte(stringToSign)))

	authorization := fmt.Sprintf("AWS4-HMAC-SHA256 Credential=%s/%s, SignedHeaders=%s, Signature=%s",
		mc.S3Access, credentialScope, signedHeaders, signature)

	// HTTP request.
	body := io.Reader(bytes.NewReader(data))
	if onProgress != nil {
		body = &progressReader{r: bytes.NewReader(data), total: int64(len(data)), onProgress: onProgress}
	}
	req, err := http.NewRequest("PUT", s3URL, body)
	if err != nil {
		return err
	}
	req.Host = host
	req.ContentLength = int64(len(data))
	req.Header.Set("x-amz-content-sha256", payloadHash)
	req.Header.Set("x-amz-date", amzDate)
	req.Header.Set("Content-Type", contentType)
	req.Header.Set("Authorization", authorization)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("S3 upload: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		body, _ := io.ReadAll(resp.Body)
		return fmt.Errorf("S3 upload failed: HTTP %d: %s", resp.StatusCode, string(body))
	}
	return nil
}

// s3SignKey derives the AWS SigV4 signing key.
func s3SignKey(secret, dateStamp, region string) []byte {
	kDate := hmacSHA256([]byte("AWS4"+secret), []byte(dateStamp))
	kRegion := hmacSHA256(kDate, []byte(region))
	kService := hmacSHA256(kRegion, []byte("s3"))
	return hmacSHA256(kService, []byte("aws4_request"))
}

// hmacSHA256 returns HMAC-SHA256(key, data).
func hmacSHA256(key, data []byte) []byte {
	h := hmac.New(sha256.New, key)
	h.Write(data)
	return h.Sum(nil)
}

// sha256Hex returns the hex-encoded SHA-256 hash of data.
func sha256Hex(data []byte) string {
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// s3EncodePath URI-encodes each path segment but keeps slashes.
// For S3 SigV4, the canonical URI must be the URI-encoded path where
// each path segment is encoded but "/" separators are preserved.
func s3EncodePath(path string) string {
	if path == "" {
		return "/"
	}
	var sb strings.Builder
	// Handle leading slash.
	if path[0] == '/' {
		sb.WriteByte('/')
		path = path[1:]
	}
	for i, seg := range strings.Split(path, "/") {
		if i > 0 {
			sb.WriteByte('/')
		}
		sb.WriteString(url.PathEscape(seg))
	}
	return sb.String()
}
