package pkg

// cacherec.go — cache_rec：按 KVS_CC_REC_<名称> 环境变量做命中即短路（不发上游、不写缓存）。
// 共用小工具见 cache.go。

import (
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"sort"
	"strconv"
	"strings"
)

// =============================================================================
// cache_rec — 命中即短路：不发上游请求，直接返回规则指定的响应
//
// 规则来源：环境变量 KVS_CC_REC_<名称>，一个变量一条规则，全局生效（不挂在某条
// 路由上，两条缓存入口 /__cache/ 与路由 cc~ 前缀都生效，先于缓存查找判断）：
//
//	upHost|<匹配>[,匹配…]|status[|content-text[|content-type]]
//
//   - upHost:  代理后端的主机（与 cache_sed 的 host 同一套匹配：逗号列表任一命中、
//              空=任意、大小写与端口无关、支持一个 * 通配）
//   - 匹配:    对上游 path 的子串匹配；多个用 , 分隔且**必须全部满足**（AND）——
//              与 cache_sed 的文件名列表（任一命中即可）语义相反
//   - status:  返回的状态码（100-599）
//   - content-text / content-type: 响应正文与类型，类型默认 text/plain。
//     正文以 file:// 开头时不当作字面量，而是读取该路径的文件内容返回
//     （每次命中都重读，改文件即生效；读不到回 500 并记日志）。
//
// 命中时：上游不会被请求，也不读写缓存条目；响应带 X-Cache: REC 便于在浏览器里
// 辨认，日志打 [cache] cache_rec: <名称> 命中 …。多条规则按名称排序，第一条命中即用。
// =============================================================================

// recEnvPrefix marks the environment variables that carry cache_rec rules.
const recEnvPrefix = "KVS_CC_REC_"

// recRuleFormat documents the rule value format (used in logs/docs).
const recRuleFormat = "upHost|path[,paths…]|status[|content-text[|content-type]]"

// recBodyFileScheme marks a content-text value that names a file to read
// instead of carrying the body literally: file:///abs/path or file://rel/path
// (same convention as the file:// routes / once_shell values).
const recBodyFileScheme = "file://"

// cacheRecRule is one parsed rule (one KVS_CC_REC_<NAME> variable).
type cacheRecRule struct {
	name        string   // rule name (env suffix) — group label for logs
	hosts       []string // upHost list; empty = any host (same matching as sed hosts)
	paths       []string // upstream-path substrings; ALL must occur (AND)
	status      int      // response status (100-599)
	body        string   // literal response body (verbatim; empty = no body)
	bodyFile    string   // content-text was file://… → read this path per request
	contentType string   // response Content-Type (default text/plain)
}

// recRules holds the installed rules; nil = feature disabled.
var recRules []cacheRecRule

// SetCacheRecEnv scans the process environment for KVS_CC_REC_<NAME> rule
// variables and installs them, replacing any previously installed set. Called
// from main after config load (so [service.*] envs_default values are already
// in the environment) and again after every config reload.
func SetCacheRecEnv() { parseCacheRecEnv(os.Environ()) }

// parseCacheRecEnv installs one rule per KVS_CC_REC_<NAME> variable, in
// name-sorted order (deterministic first-match-wins). Bad values are skipped
// with a warning; unrelated variables are ignored.
func parseCacheRecEnv(env []string) {
	type entry struct{ name, val string }
	var items []entry
	for _, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, recEnvPrefix) || strings.TrimSpace(val) == "" {
			continue
		}
		items = append(items, entry{strings.TrimPrefix(name, recEnvPrefix), val})
	}
	if len(items) == 0 {
		recRules = nil
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	var rules []cacheRecRule
	for _, it := range items {
		if it.name == "" {
			log.Printf("[cache] cache_rec: skip %s<name> (empty rule name)", recEnvPrefix)
			continue
		}
		rule, err := parseCacheRecRule(it.name, it.val)
		if err != nil {
			log.Printf("[cache] cache_rec: skip %s%s: %v", recEnvPrefix, it.name, err)
			continue
		}
		rules = append(rules, rule)
	}
	if len(rules) == 0 {
		recRules = nil
		log.Printf("[cache] cache_rec: configured but no valid rule, disabled")
		return
	}
	recRules = rules
	log.Printf("[cache] cache_rec: %d rule(s) enabled", len(rules))
	for _, r := range rules {
		log.Printf("[cache] cache_rec: %s: host=%s paths=%s → %d %s body=%s",
			r.name, hostList(r.hosts), strings.Join(r.paths, ","), r.status, r.contentType, r.bodyDesc())
	}
}

// bodyDesc describes the rule's body source for logs: "literal(<n>字节)" or
// "file:<path>" (never the body text itself — it may be large).
func (r *cacheRecRule) bodyDesc() string {
	if r.bodyFile != "" {
		return "file:" + r.bodyFile
	}
	if r.body == "" {
		return "none"
	}
	return fmt.Sprintf("literal(%d字节)", len(r.body))
}

// parseCacheRecRule parses one rule value:
// upHost|path[,paths…]|status[|content-text[|content-type]].
func parseCacheRecRule(name, spec string) (cacheRecRule, error) {
	fields := strings.Split(spec, "|")
	if len(fields) < 3 || len(fields) > 5 {
		return cacheRecRule{}, fmt.Errorf("want %s, got %d field(s)", recRuleFormat, len(fields))
	}
	paths := splitList(fields[1])
	if len(paths) == 0 {
		return cacheRecRule{}, errors.New("empty path list")
	}
	status, err := strconv.Atoi(strings.TrimSpace(fields[2]))
	if err != nil || status < 100 || status > 599 {
		return cacheRecRule{}, fmt.Errorf("bad status %q (want 100-599)", strings.TrimSpace(fields[2]))
	}
	rule := cacheRecRule{
		name:        name,
		hosts:       splitList(fields[0]), // 空 = 任意主机
		paths:       paths,
		status:      status,
		contentType: "text/plain",
	}
	if len(fields) >= 4 {
		// file:// 开头 = 读该文件内容作为正文（每次命中重读）；否则字面量，
		// 原样保留（正文里的空格有意义）。
		if strings.HasPrefix(fields[3], recBodyFileScheme) {
			rule.bodyFile = strings.TrimPrefix(fields[3], recBodyFileScheme)
		} else {
			rule.body = fields[3]
		}
	}
	if len(fields) == 5 && strings.TrimSpace(fields[4]) != "" {
		rule.contentType = strings.TrimSpace(fields[4])
	}
	return rule, nil
}

// recMatchingRule returns the first installed rule (name order) whose host list
// admits the upstream host and whose path patterns ALL occur in the upstream
// path; nil when nothing matches.
func recMatchingRule(upstreamHost, path string) *cacheRecRule {
	for i := range recRules {
		rule := &recRules[i]
		if !hostMatch(rule.hosts, upstreamHost) {
			continue
		}
		hit := true
		for _, p := range rule.paths {
			if !strings.Contains(path, p) { // AND：任一不满足即整条不命中
				hit = false
				break
			}
		}
		if hit {
			return rule
		}
	}
	return nil
}

// recBody resolves a rule's response body: a literal, or the current content of
// the configured file (re-read per request, so editing the file takes effect
// immediately). A listed file that cannot be read is a deployment error — the
// request gets a 500 instead of a silently empty/incorrect body.
func recBody(rule *cacheRecRule) (string, error) {
	if rule.bodyFile == "" {
		return rule.body, nil
	}
	data, err := os.ReadFile(rule.bodyFile)
	if err != nil {
		return "", err
	}
	return string(data), nil
}

// serveCacheRec writes a rule's synthetic response: upstream is never contacted
// and no cache entry is touched. 204/304/1xx carry no body by definition, so a
// configured content-text is dropped for those statuses.
func serveCacheRec(w http.ResponseWriter, r *http.Request, rule *cacheRecRule, upstreamHost, path string) {
	body, err := recBody(rule)
	if err != nil {
		log.Printf("[cache] cache_rec: %s hit %s%s, but read file error: %v",
			rule.name, upstreamHost, path, err)
		http.Error(w, "cache_rec body file unreadable: "+rule.bodyFile, http.StatusInternalServerError)
		return
	}
	src := fmt.Sprintf("%d bytes", len(body))
	if rule.bodyFile != "" {
		src = "file " + rule.bodyFile
	}
	log.Printf("[cache] cache_rec: %s hit %s%s → %d (%s, %s)",
		rule.name, upstreamHost, path, rule.status, rule.contentType, src)
	w.Header().Set("Content-Type", rule.contentType)
	w.Header().Set("X-Cache", "REC")
	noBody := rule.status == http.StatusNoContent || rule.status == http.StatusNotModified || rule.status < 200
	if body != "" && !noBody {
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
	}
	w.WriteHeader(rule.status)
	if body != "" && !noBody {
		_, _ = io.WriteString(w, body)
	}
}

// =============================================================================
// cache_sed：按 KVS_CC_SED_<名称> 环境变量改写 cc~ 缓存正文
// cache_sed — rewrite cc~ cached response bodies on write
// =============================================================================

// sedMode is the cache_sed processing mode decided once at cache-write time
// and persisted in the metadata, so serve time never re-derives it.
type sedMode string

const (
	sedModeNone sedMode = "none" // rules did not touch the body — upstream bytes stored & served as-is (rules 1/2/3)
	sedModeOnce sedMode = "once" // once-only rewrite, stored gzip, served as-is (rule 4)
	sedModeEach sedMode = "each" // once baked in + each rewrites per request; stored plain (rule 5)
)

// sedMeta is the per-file rewrite plan persisted in the cache metadata.
//   - Mode: how the stored body was processed (see sedMode).
//   - SrcGzip: only for mode=each — the upstream origin was gzip/br
//     encoded, so every response is re-compressed as gzip after the
//     per-request rewrite (rule 5); plain origins stay plain.
//   - Each: the request-dependent rules (old→new, new keeps the ">host<"
//     placeholder) that matched this file at write time; serve time uses
//     this list instead of the runtime config.
type sedMeta struct {
	Mode    sedMode       `json:"mode"`
	SrcGzip bool          `json:"src_gzip,omitempty"`
	Each    []sedEachRule `json:"each,omitempty"`
}

// sedEachRule is one persisted old→new pair (new may contain ">host<").
type sedEachRule struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// sedEnvPrefix marks the environment variables that carry cache_sed rules:
// KVS_CC_SED_<名称> = old|new|file[,files…][|host[,hosts…]]. The suffix is the
// rule's name — one variable per rule, so related rules are naturally grouped
// by their own variable instead of sharing a single packed config value.
const sedEnvPrefix = "KVS_CC_SED_"

// cacheSedRule is one parsed rule (one KVS_CC_SED_<NAME> variable).
//   - old:   literal content to find; '|' cannot appear in it
//   - new:   replacement content; ">host<" expands to the request Host per
//     request (that is what makes the rule an "each" rule)
//   - files: comma list of file patterns, matched with globMatch (one '*' wildcard)
//   - hosts: optional comma list of 来源主机 (the proxied backend's host), see
//     hostMatch; empty = any host. NOT the front-end request Host — that one
//     only feeds the ">host<" expansion in new.
type cacheSedRule struct {
	name  string // rule name (env suffix) — group label for logs
	old   string
	new   string
	files []string
	hosts []string
}

// sedRules holds the installed rules; nil = feature disabled.
var sedRules []cacheSedRule

// SetCacheSedEnv scans the process environment for KVS_CC_SED_<NAME> rule
// variables and installs them, replacing any previously installed set. Each
// variable is one rule (the suffix is its name/group label). Called from main
// after config load, so [service.*] envs_default values are already in the
// environment. No matching variable disables the feature.
func SetCacheSedEnv() { parseCacheSedEnv(os.Environ()) }

// parseCacheSedEnv installs one rule per KVS_CC_SED_<NAME> variable, in
// name-sorted order so rule order is deterministic across restarts. Values
// that fail to parse are skipped with a warning; unrelated variables ignored.
func parseCacheSedEnv(env []string) {
	type entry struct{ name, val string }
	var items []entry
	for _, kv := range env {
		name, val, ok := strings.Cut(kv, "=")
		if !ok || !strings.HasPrefix(name, sedEnvPrefix) || strings.TrimSpace(val) == "" {
			continue
		}
		items = append(items, entry{strings.TrimPrefix(name, sedEnvPrefix), val})
	}
	if len(items) == 0 {
		sedRules = nil
		return
	}
	sort.Slice(items, func(i, j int) bool { return items[i].name < items[j].name })

	var rules []cacheSedRule
	for _, it := range items {
		if it.name == "" {
			log.Printf("[cache] cache_sed: skip %s<name> (empty rule name)", sedEnvPrefix)
			continue
		}
		rule, err := parseCacheSedRule(it.name, it.val)
		if err != nil {
			log.Printf("[cache] cache_sed: skip %s%s: %v", sedEnvPrefix, it.name, err)
			continue
		}
		rules = append(rules, rule)
	}
	if len(rules) == 0 {
		sedRules = nil
		log.Printf("[cache] cache_sed: configured but no valid rule, disabled")
		return
	}
	sedRules = rules
	log.Printf("[cache] cache_sed: %d rule(s) enabled", len(rules))
	for _, r := range rules {
		log.Printf("[cache] cache_sed: %s: %q → %q files=%s hosts=%s",
			r.name, r.old, r.new, strings.Join(r.files, ","), hostList(r.hosts))
	}
}

// sedRuleFormat documents the rule value format (used in logs/docs).
const sedRuleFormat = "old|new|file[,files…][|host[,hosts…]]"

// parseCacheSedRule parses one rule value: old|new|file[,files…][|host[,hosts…]].
// Only the file and host fields are comma-separated; old/new are taken
// verbatim, so a replacement targeting minified JS may contain commas.
func parseCacheSedRule(name, spec string) (cacheSedRule, error) {
	fields := strings.Split(spec, "|")
	if len(fields) < 3 || len(fields) > 4 {
		return cacheSedRule{}, fmt.Errorf("want %s, got %d field(s)", sedRuleFormat, len(fields))
	}
	if fields[0] == "" {
		return cacheSedRule{}, errors.New("empty old")
	}
	files := splitList(fields[2])
	if len(files) == 0 {
		return cacheSedRule{}, errors.New("empty file list")
	}
	var hosts []string
	if len(fields) == 4 {
		hosts = splitList(fields[3])
	}
	return cacheSedRule{name: name, old: fields[0], new: fields[1], files: files, hosts: hosts}, nil
}

// sedMatchingRules returns the rules that apply to a cache entry: at least one
// files pattern matches name AND the host list admits the 来源主机
// (targetHost — the proxied backend's host, not the request Host). Host
// filtering, like the once/each mode itself, is a WRITE-time decision — the
// resulting plan is persisted, so a later HIT replays what was recorded (the
// ">host<" expansions then follow the serving request Host, as before).
func sedMatchingRules(name, upstreamHost string) []cacheSedRule {
	var out []cacheSedRule
	for _, rule := range sedRules {
		if !hostMatch(rule.hosts, upstreamHost) {
			continue
		}
		for _, p := range rule.files {
			if globMatch(p, name, false) {
				out = append(out, rule)
				break
			}
		}
	}
	return out
}

// sedApply expands ">host<" in replacements and substitutes old→new for
// rules whose old value occurs in the body.
func sedApply(rules []cacheSedRule, payload []byte, host string) ([]byte, bool) {
	s := string(payload)
	changed := false
	for _, rule := range rules {
		if !strings.Contains(s, rule.old) {
			continue
		}
		newVal := strings.ReplaceAll(rule.new, ">host<", host)
		s = strings.ReplaceAll(s, rule.old, newVal)
		changed = true
		log.Printf("[cache] cache_sed: apply %q → %q", rule.old, newVal)
	}
	if !changed {
		return payload, false
	}
	return []byte(s), true
}

// sedPipeline implements the cache_sed rules 1-5 for a cache MISS in one
// pass. It returns the bytes to STORE on disk, the bytes to SEND to the
// client, the plan to persist in the metadata (Mode=none ⇒ Sed stays nil)
// and whether the response is gzip framed.
//
//	rules 1/3, unsupported encoding, decode/encode failure:
//		passthrough — store=send=original bytes, headers untouched;
//	rule 4 (once only): store=send=gzip(rewritten) — HIT serves as-is;
//	rule 5 (once + each): store=rewritten plain; send is the each-rewritten
//		copy, gzip framed when the origin was gzip/br (HIT replays this).
func sedPipeline(rules []cacheSedRule, name string, data []byte, srcEnc string, host string) (store, send []byte, plan sedMeta, sendGzip bool) {
	// Rule 1: file name not matched — pass through untouched.
	if len(rules) == 0 {
		return data, data, sedMeta{Mode: sedModeNone}, false
	}

	// Rule 2: decode per upstream Content-Encoding. Unsupported encodings
	// and decode failures degrade to passthrough.
	var payload []byte
	encoded := false // origin was gzip/br — rule 5 response framing
	switch srcEnc {
	case "":
		payload = data
	case "gzip":
		p, err := gzipDecode(data)
		if err != nil {
			log.Printf("[cache] cache_sed: %s gzip decode FAIL: %v (passthrough)", name, err)
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		payload, encoded = p, true
	case "br":
		if !brotliAvailable() {
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		p, err := brotliDecompress(data)
		if err != nil {
			log.Printf("[cache] cache_sed: %s brotli decode FAIL: %v (passthrough)", name, err)
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		payload, encoded = p, true
	default:
		// Other encodings are not handled — equivalent to passthrough.
		log.Printf("[cache] cache_sed: %s upstream encoding %q unsupported (passthrough)", name, srcEnc)
		return data, data, sedMeta{Mode: sedModeNone}, false
	}

	// Split rules into once (host-independent) and each (">host<" placeholder).
	var onceRules, eachRules []cacheSedRule
	for _, r := range rules {
		if strings.Contains(r.new, ">host<") {
			eachRules = append(eachRules, r)
		} else {
			onceRules = append(onceRules, r)
		}
	}

	// Rule 3: no rule content occurs in the payload — passthrough.
	s := string(payload)
	anyHit := false
	for _, r := range onceRules {
		if strings.Contains(s, r.old) {
			anyHit = true
			break
		}
	}
	if !anyHit {
		for _, r := range eachRules {
			if strings.Contains(s, r.old) {
				anyHit = true
				break
			}
		}
	}
	if !anyHit {
		log.Printf("[cache] cache_sed: %s: no rule content matched (passthrough)", name)
		return data, data, sedMeta{Mode: sedModeNone}, false
	}

	// Rule 4: apply ALL once rules first — the result is baked into the
	// stored body for both the once and each paths.
	payload, onceChanged := sedApply(onceRules, payload, host)

	// Rule 5 selection: each rules still matching the post-once payload.
	var matched []sedEachRule
	for _, r := range eachRules {
		if strings.Contains(string(payload), r.old) {
			matched = append(matched, sedEachRule{Old: r.old, New: r.new})
		}
	}
	if len(matched) == 0 {
		// Rule 4: once only — compress to gzip, store and serve as-is.
		gz, err := gzipEncode(payload)
		if err != nil {
			log.Printf("[cache] cache_sed: %s gzip encode FAIL: %v (passthrough)", name, err)
			return data, data, sedMeta{Mode: sedModeNone}, false
		}
		log.Printf("[cache] cache_sed: %s: mode=once, stored gzip (%d → %d bytes)", name, len(data), len(gz))
		return gz, gz, sedMeta{Mode: sedModeOnce}, true
	}

	// Rule 5: store plain (once result baked in); the response is rewritten
	// per request and gzip framed when the origin was gzip/br.
	log.Printf("[cache] cache_sed: %s: mode=each (%d rule(s), once=%v), stored plain (%d bytes)", name, len(matched), onceChanged, len(payload))
	plan = sedMeta{Mode: sedModeEach, SrcGzip: encoded, Each: matched}
	send = sedApplyEach(matched, payload, host)
	if encoded {
		gz, gerr := gzipEncode(send)
		if gerr == nil {
			return payload, gz, plan, true
		}
		log.Printf("[cache] cache_sed: %s gzip encode FAIL: %v (serving plain)", name, gerr)
		plan.SrcGzip = false // keep MISS and HIT framing in agreement
	}
	return payload, send, plan, false
}

// sedApplyEach applies persisted each rules (old→new, ">host<" expanded
// with the current request Host) against a plain stored body.
func sedApplyEach(each []sedEachRule, payload []byte, host string) []byte {
	rules := make([]cacheSedRule, len(each))
	for i, r := range each {
		rules[i] = cacheSedRule{old: r.Old, new: r.New}
	}
	out, _ := sedApply(rules, payload, host)
	return out
}

// sedServeFromMeta builds the HIT response body from the stored bytes,
// driven solely by the persisted plan and the metadata headers (never by
// sniffing bytes):
//
//	mode=each  → apply the persisted Each rules with the current request
//	             Host; gzip framed when SrcGzip (rule 5 replay);
//	otherwise  → stored bytes are final; framing is whatever the metadata
//	             headers record (none = upstream original, once = our gzip).
func sedServeFromMeta(meta *cacheMeta, body []byte, host string) ([]byte, bool) {
	sm := meta.Sed
	if sm == nil || sm.Mode != sedModeEach || len(sm.Each) == 0 {
		return body, isGzipMeta(meta)
	}
	out := sedApplyEach(sm.Each, body, host)
	if sm.SrcGzip {
		gz, err := gzipEncode(out)
		if err != nil {
			log.Printf("[cache] cache_sed gzip encode FAIL: %v (serving plain)", err)
			return out, false
		}
		return gz, true
	}
	// Plain origin: never compress — echo the framing the origin used.
	return out, false
}
