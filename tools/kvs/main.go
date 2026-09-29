package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"kvs/pkg"
)

func main() {
	// Subcommand: "help" / "-h" / "--help" — print usage.
	if len(os.Args) > 1 && (os.Args[1] == "help" || os.Args[1] == "-h" || os.Args[1] == "--help") {
		pkg.HelpCommand()
		return
	}

	// Subcommand: "demo" — write the embedded kvs.ini template to ./kvs.ini.
	if len(os.Args) > 1 && os.Args[1] == "demo" {
		pkg.DemoCommand(os.Args[2:])
		return
	}

	// Subcommand: "mirror" — sync vscode versions to S3-compatible storage.
	if len(os.Args) > 1 && os.Args[1] == "mirror" {
		pkg.MirrorCommand(os.Args[2:])
		return
	}

	// Subcommand: "syncto" — generic local <-> S3 file sync (env-configured).
	if len(os.Args) > 1 && os.Args[1] == "syncto" {
		pkg.SynctoCommand(os.Args[2:])
		return
	}

	cfg := pkg.LoadInitConfig()

	// Apply cache_dir as the proxy cache root (disk storage).
	// The cache route is registered only when cache_dir is configured AND
	// proxy_path is non-empty; otherwise the prefix stays "" so the cache
	// route is not registered and auth checks don't treat every path as public.
	if cfg.CacheDir != "" {
		pkg.SetCacheConfig(cfg.CacheDir, cfg.ProxyPath)
	}
	// cc~ marked backends always have disk caching available, defaulting to
	// /cache when cache_dir is not configured.
	pkg.SetCacheDir(cfg.CacheDir)
	// svc:// hosted services: download cache root + register the named
	// services of this config (handlers resolve them at mux build time).
	pkg.Services().SetCacheDir(cfg.CacheDir)
	pkg.Services().Register(cfg.Services)
	// zcode relay state file lives under the gateway home ({home}/zcodex.json).
	pkg.SetZcodeHome(cfg.Home)
	// 应用中心内置卡（zcode 数据文件夹/工作目录、版本固定）持久化在 zcodex.json：
	// 先回填进程 env 和版本覆盖，再重载一次配置，让命令行展开和 envs_default
	// 按覆盖值生效。
	if pkg.ManagerReapplyOverrides() {
		newCfg := pkg.LoadInitConfig()
		pkg.Services().Register(newCfg.Services)
		cfg = newCfg
	}
	// cache_sed: rewrite cc~ cached bodies (file|old|new||... rules).
	pkg.SetCacheSed(cfg.CacheSed)
	// 反向代理调优 + WebSocket 升级快路径（见 pkg/transport.go / pkg/wsproxy.go）。
	pkg.SetProxyTuning(cfg.ProxyIdleConnsPerHost, cfg.ProxyMaxIdleConns, cfg.ProxyDisableCompression, cfg.ProxyFlushInterval)
	pkg.SetWSTunnelOptions(cfg.WSProxy, cfg.WSIdleTimeout)
	// 主 HTTP server 的 ReadTimeout（读完整请求含 body 的秒数；0=不限，默认 60）。
	pkg.SetServerReadTimeout(cfg.ReadTimeout)
	// 可选 pprof（KVS_PPROF=1 | <port> | <addr>），默认关闭。
	pkg.StartPProfIfEnabled()

	// Build (prefix → handler) routes for each backend. Hosted svc:// services
	// get their own lazy preparation flow through the service manager (loading
	// page while download/extract/start is in progress).
	//
	// The table is REBUILT after every config reload (/__restart, manager
	// apply): handlers pin reload-dependent state (svc:// addresses, file://
	// dirs expanded from bin_home, headers), so a version change must rebuild
	// them. The dispatch loop reads the latest table per request; templated
	// (<verhash>/<version>) matching is expanded per request regardless.
	type route struct {
		prefix    string
		re        *regexp.Regexp // compiled regex for ^-prefixed backends, nil otherwise
		handler   http.Handler
		svc       string // svc:// service name ("" = not a hosted service)
		exact     bool   // ws~ backends: request path must equal prefix exactly (no prefix match)
		templated bool   // prefix carries <verhash>/<version>, expanded per request
	}
	buildRoutes := func(c pkg.Config) []route {
		pkg.ManagerResetSvcMounts() // 嗅探表跟随当前路由表整体重建
		rts := make([]route, len(c.Proxies))
		for i, b := range c.Proxies {
			var re *regexp.Regexp
			if b.IsRegex {
				compiled, err := regexp.Compile(b.Source)
				if err != nil {
					log.Fatalf("invalid regex %q: %v", b.Source, err)
				}
				re = compiled
			}
			rts[i] = route{prefix: b.Source, re: re, handler: pkg.CreateBackendHandler(b, c.Headers, c.LoginAuthz), svc: svcBackendName(b), exact: pkg.ExactPathMatch(b), templated: b.VerTemplate}
			// 应用中心（api://manager）嗅探：登记 svc:// 托管服务的挂载路径，
			// 新建内置应用对话框据此列出当前可用的 zcode/vscode 服务。
			// <verhash>/<version> 模板路由不参与嗅探（挂载点应是真实路径）。
			if name := svcBackendName(b); name != "" && !b.VerTemplate {
				pkg.ManagerSvcMount(name, b.Source)
			}
			// ws~ + 隧道类目标（http/https/ws/wss/unix）现在按前缀匹配；前缀为 "/" 时
			// 会匹配所有路径，把其后声明的路由全部遮蔽。旧版这类路由是精确匹配（基本
			// 不可用），行为有变，显式告警以免静默踩坑。
			if b.IsWSock && !pkg.ExactPathMatch(b) && b.Source == "/" {
				log.Printf("WARNING: ws~ route %q (%s) 前缀为 /，按前缀匹配会遮蔽其后声明的所有路由", b.Source, b.RawURL)
			}
		}
		return rts
	}
	var routesPtr atomic.Value // []route — swapped after every config reload
	routesPtr.Store(buildRoutes(cfg))

	// svcPick resolves the target service of a global endpoint: ?svc=<name>
	// selects explicitly; without it, the first service in route-table order
	// whose config provides the capability answers. There is NO default-service
	// concept — the scan only asks "which service provides this feature".
	svcPick := func(r *http.Request, pred func(*pkg.ServiceConfig) bool) *pkg.ServiceConfig {
		if name := r.URL.Query().Get("svc"); name != "" {
			return pkg.Services().Get(name)
		}
		for _, b := range cfg.Proxies {
			name := svcBackendName(b)
			if name == "" {
				continue
			}
			if sc := pkg.Services().Get(name); sc != nil && pred(sc) {
				return sc
			}
		}
		return nil
	}
	agentsProvided := func(sc *pkg.ServiceConfig) bool { return sc.VscAgentsCmd != "" || sc.VscAgentsDir != "" }

	// setCookie writes a cookie with the given value and MaxAge.
	setCookie := func(w http.ResponseWriter, value string, maxAge int) {
		http.SetCookie(w, &http.Cookie{
			Name:     cfg.CookieTknName,
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
			pkg.ServeLoginAsset(w, r, "")
			return
		}
		if err := r.ParseForm(); err != nil {
			http.Error(w, "invalid form", http.StatusBadRequest)
			return
		}
		tkn := strings.TrimSpace(r.PostFormValue("token"))
		if tkn == "" || subtle.ConstantTimeCompare([]byte(tkn), []byte(cfg.LoginToken)) != 1 {
			// Wrong password: re-render the login page with an error message.
			// We do NOT redirect — the browser address bar stays at /__login
			// (the form action), but the login page JS restores it to the
			// original URL via history.replaceState(document.referrer) so the
			// next correct submission has a valid Referer.
			pkg.ServeLoginAsset(w, r, "Invalid access token, please try again")
			return
		}
		// Generate cookie value based on login_timeout mode.
		cookieVal := pkg.GenerateTokenValue(cfg.LoginToken, cfg.LoginTimeout)
		maxAge := 0 // session lifetime
		if cfg.LoginTimeout > 0 {
			maxAge = cfg.LoginTimeout
		}
		setCookie(w, cookieVal, maxAge)
		back := pkg.SafeReferer(r.Referer(), r.Host)
		log.Printf("login ok, cookie %s set, reloading: %s", cfg.CookieTknName, back)
		http.Redirect(w, r, back, http.StatusSeeOther)
	})

	// /__logout – clears the token cookie. Returns a minimal JSON response
	// (not a page) so it can be called via fetch from the logout button script.
	// The caller is responsible for reloading/redirecting after logout.
	mux.HandleFunc("/__logout", func(w http.ResponseWriter, r *http.Request) {
		setCookie(w, "", -1)
		log.Printf("logout: cleared cookie %s", cfg.CookieTknName)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"success":true,"message":"logout success"}`))
	})

	// /__stats – 运行计数（JSON）：WS 隧道（splice/generic、字节数）、中继帧、
	// 缓存命中、goroutine/堆。挂在这里即受全局鉴权保护（login_token 非空时需
	// 带 Cookie 或令牌）。
	mux.HandleFunc("/__stats", pkg.StatsHandler)

	// /__version – returns the resolved service application version as plain
	// text. Falls back to "0.0.0" when no version is resolved (e.g. no service
	// backend, version resolution failed, or version not yet loaded).
	// ?svc=<name> selects a specific named service (default: the table's
	// first svc:// service).
	mux.HandleFunc("/__version", func(w http.ResponseWriter, r *http.Request) {
		// ?svc=<name> 显式选择；缺省时按路由表顺序取第一个已解析版本的服务。
		v := ""
		if sc := svcPick(r, func(sc *pkg.ServiceConfig) bool { return sc.Version != "" }); sc != nil {
			v = sc.Version
		}
		if v == "" {
			v = "0.0.0"
		}
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, v)
	})

	// /__restart – kills the backend subprocess and resets the service state so
	// the next request re-triggers version detection, download, extract, and
	// start. Requires authentication (not in isPublicAuthPath). Only works when
	// a hosted (svc://) service backend exists and was kvs-managed.
	// /__restart            → restart the primary service
	// /__restart?svc=name   → restart a specific named service
	// /__restart?v=1.32.1   → restart with a specific version override
	restartHandler := func(w http.ResponseWriter, r *http.Request) {
		// Extract optional version: query (?v=1.32.1) is the primary method.
		overrideVersion := r.URL.Query().Get("v")
		svcName := r.URL.Query().Get("svc")
		if svcName == "" || pkg.Services().Get(svcName) == nil {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusOK)
			fmt.Fprintln(w, `<!DOCTYPE html><html><body><h3>Service required: /__restart?svc=&lt;name&gt;</h3><a href="/">Go Home</a></body></html>`)
			return
		}

		// Set or clear the version override BEFORE reloading config.
		// This must happen even if the backend is not running, so that the
		// version takes effect on the next preparation cycle. Overrides are
		// tracked per service.
		if overrideVersion != "" {
			log.Printf("[restart:%s] overriding version to %s", svcName, overrideVersion)
			pkg.SetOverrideVersion(svcName, overrideVersion)
		} else {
			// No version specified → clear any previous override so
			// resolveVersion falls back to version_latest_url. Also drop the
			// self-assigned KVS_SVC_<NAME>_VERSION env (it would otherwise be
			// read back by the version placeholder and re-pin the old value).
			if old := pkg.GetOverrideVersion(svcName); old != "" {
				log.Printf("[restart:%s] clearing version override (was %s)", svcName, old)
				pkg.ClearOverrideVersion(svcName)
			}
			pkg.ClearServiceVersionEnv(svcName)
		}

		// Reload config so download/bin_home are re-expanded with the new (or
		// cleared) version. If version resolve fails (e.g. 404), LoadInitConfig
		// stores the error in InitError instead of crashing. PrepareService
		// will surface it on the loading page as a download failure.
		newCfg := pkg.LoadInitConfig()
		pkg.Services().Register(newCfg.Services)

		// Agent branch: /__restart?agent=<file> — restart the SERVICE backend
		// (command) with bridge args appended to it, derived from the agent entry file.
		if agentFile := r.URL.Query().Get("agent"); agentFile != "" {
			sc := pkg.Services().Get(svcName)
			if sc == nil || sc.Command == "" {
				http.Error(w, "command not configured", http.StatusBadRequest)
				return
			}
			args := pkg.AgentBridgeArgsFromFile(filepath.Join(sc.VscAgentsDir, agentFile+".json"))
			if args == "" {
				http.Error(w, "agent entry not found or invalid: "+agentFile, http.StatusNotFound)
				return
			}
			// Apply the bridge args to the service's own config.
			sc.VscAgentArgs = args
		}

		cfg = newCfg                         // always update cfg so route changes (if any) take effect
		routesPtr.Store(buildRoutes(newCfg)) // handlers pin reload-dependent state — rebuild

		// Kill the running service backend (if any) and reset state.
		pkg.Services().StopOne(svcName, true)
		pkg.Services().State(svcName).Reset()

		// If config reload failed (e.g. version hash 404), report the error
		// but don't crash — the loading page will show it on next access.
		var initErr string
		if sc := pkg.Services().Get(svcName); sc != nil {
			initErr = sc.InitError
		}
		if initErr != "" {
			if st := pkg.Services().State(svcName); st != nil {
				st.Finish(fmt.Errorf("%s", initErr))
			}
			log.Printf("[restart] config reload error: %s", initErr)
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusInternalServerError)
			fmt.Fprintf(w, `<!DOCTYPE html><html><body><h3>Restart failed: %s</h3><p><a href="/">Go Home</a> | <a href="/__restart?svc=%s">Retry Restart</a></p></body></html>`, initErr, url.QueryEscape(svcName))
			return
		}

		log.Printf("[restart:%s] service reset, will re-prepare on next request", svcName)
		// Redirect to home so the browser reloads and re-triggers preparation.
		http.Redirect(w, r, "/", http.StatusSeeOther)
	}
	mux.HandleFunc("/__restart", restartHandler)

	// 应用中心内置卡保存（zcode 数据文件夹/工作目录）后的应用动作：env 已由
	// manager 写入进程，这里重载配置（命令行按新 env 重新展开）并复位服务状态，
	// 下次访问按新值重新准备 —— 与 /__restart 同一套机制。
	pkg.SetManagerApplyHook(func(svcName string) {
		if pkg.Services().Get(svcName) == nil {
			return
		}
		newCfg := pkg.LoadInitConfig()
		pkg.Services().Register(newCfg.Services)
		cfg = newCfg
		routesPtr.Store(buildRoutes(newCfg)) // 版本/地址变更后路由跟随
		pkg.Services().StopOne(svcName, true)
		pkg.Services().State(svcName).Reset()
		if sc := pkg.Services().Get(svcName); sc != nil && sc.InitError != "" {
			if st := pkg.Services().State(svcName); st != nil {
				st.Finish(fmt.Errorf("%s", sc.InitError))
			}
		}
		log.Printf("[manager:%s] env applied, service will re-prepare on next request", svcName)
	})

	//======================================================================================

	// /__open_tkn – returns a short-lived access token for API/script use.
	// Requires a valid login cookie (NOT in isPublicAuthPath), so it cannot be
	// abused to mint tokens anonymously.
	mux.HandleFunc("/__open_tkn", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if cfg.TokenTimeout <= 0 {
			_, _ = io.WriteString(w, "") // no config temp token
			return
		}
		_, _ = io.WriteString(w, pkg.GenerateTokenValue(cfg.LoginToken, cfg.TokenTimeout))
	})

	// /__open_url  - use application open current windows url
	mux.HandleFunc("/__open_url", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		openURL := ""
		if sc := svcPick(r, func(sc *pkg.ServiceConfig) bool { return sc.VscOpenURL != "" }); sc != nil {
			openURL = sc.VscOpenURL
		}
		if openURL == "" || !strings.Contains(openURL, "://") {
			// disable by vscode app
			_, _ = io.WriteString(w, "error:disable open in application")
			return
		}
		if cfg.TokenTimeout <= 0 || cfg.QueryTokenKey == "" {
			// Go back directly
			_, _ = io.WriteString(w, openURL)
			return
		}
		tkn := pkg.GenerateTokenValue(cfg.LoginToken, cfg.TokenTimeout)
		if strings.Contains(openURL, "?") {
			_, _ = io.WriteString(w, openURL+"&"+cfg.QueryTokenKey+"="+tkn)
			return
		}
		_, _ = io.WriteString(w, openURL+"?"+cfg.QueryTokenKey+"="+tkn)
	})

	agentvs := &pkg.Process{Name: "agentvs"}

	// /__agents/status — returns { running, command, cmds } for the Agents
	// dialog, read from the table's default service. command reflects the
	// current (possibly body-replaced) vsc_agents_cmd; cmds is the
	// vsc_agent_cmds preset map (name→command), shown as quick-fill buttons
	// when the command box is empty.
	mux.HandleFunc("/__agents/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		resp := map[string]any{
			"running": agentvs.Running(),
		}
		if sc := svcPick(r, agentsProvided); sc != nil {
			resp["command"] = sc.VscAgentsCmd
			resp["cmds"] = sc.VscAgentCmds
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/__agents/entries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		dir := ""
		if sc := svcPick(r, agentsProvided); sc != nil {
			dir = sc.VscAgentsDir
		}
		resp := pkg.ListAgentEntries(dir)
		_ = json.NewEncoder(w).Encode(resp)
	})

	// /__agents/start — launch the agent host (409 if already running).
	// Optional JSON body {"command": "..."} REPLACES the service's
	// vsc_agents_cmd (the new value is kept for subsequent
	// status/start/restart); an empty or missing body starts the currently
	// configured command.
	mux.HandleFunc("/__agents/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if agentvs.Running() {
			http.Error(w, "agents already running", http.StatusConflict)
			return
		}
		sc := svcPick(r, agentsProvided)
		if r.Body != nil {
			var req struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err == nil {
				if c := strings.TrimSpace(req.Command); c != "" && sc != nil && c != sc.VscAgentsCmd {
					log.Printf("[agents] command replaced: %s", c)
					sc.VscAgentsCmd = c
				}
			}
		}
		if sc == nil || sc.VscAgentsCmd == "" {
			http.Error(w, "vsc_agents_cmd not configured", http.StatusBadRequest)
			return
		}
		if err := agentvs.Start("", sc.VscAgentsCmd); err != nil {
			http.Error(w, "failed to start agents: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true}`)
	})

	// /__agents/stop — terminate the agent host process group.
	mux.HandleFunc("/__agents/stop", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if !agentvs.Running() {
			http.Error(w, "agents not running", http.StatusConflict)
			return
		}
		agentvs.Stop()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true}`)
	})

	// /__agents/restart — restart the agent host (without bridge args; to
	// reload with a specific entry use POST /__restart?agent=<file>).
	// Optional JSON body {"command": "..."} REPLACES the service's
	// vsc_agents_cmd, same as /__agents/start.
	mux.HandleFunc("/__agents/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		sc := svcPick(r, agentsProvided)
		if r.Body != nil {
			var req struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err == nil {
				if c := strings.TrimSpace(req.Command); c != "" && sc != nil && c != sc.VscAgentsCmd {
					log.Printf("[agents] command replaced: %s", c)
					sc.VscAgentsCmd = c
				}
			}
		}
		if sc == nil || sc.VscAgentsCmd == "" {
			http.Error(w, "vsc_agents_cmd not configured", http.StatusBadRequest)
			return
		}
		if ok := agentvs.Restart(sc.VscAgentsCmd); !ok {
			http.Error(w, "failed to start agents", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true}`)
	})

	// /__agents/action/<name> — run the actions entry <name> from the default
	// service's [service.<name>] section. kvs stays generic:
	// pkg.RunAgentAction executes the value (file:// via shebang, otherwise
	// sh -c) and blocks until it exits. Any method works, so a browser URL or
	// plain curl triggers it. An unknown name returns 404 without revealing
	// the configured actions.
	mux.HandleFunc("/__agents/action/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/__agents/action/")
		if name == "" || strings.Contains(name, "/") {
			http.Error(w, "action name required", http.StatusBadRequest)
			return
		}
		sc := svcPick(r, func(sc *pkg.ServiceConfig) bool {
			_, ok := sc.Actions[name]
			return ok
		})
		if sc == nil {
			http.Error(w, "unknown action", http.StatusNotFound)
			return
		}
		cmd, known := sc.Actions[name]
		if !known {
			// Deliberately does not echo the configured action names: the
			// action table is server-side only.
			http.Error(w, "unknown action", http.StatusNotFound)
			return
		}
		if cmd == "" {
			http.Error(w, "action "+name+" has no command", http.StatusBadRequest)
			return
		}
		if err := pkg.RunAgentAction(name, cmd); err != nil {
			log.Printf("[agents] action %s failed: %s: %v", name, cmd, err)
			http.Error(w, "action failed: "+err.Error(), http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		fmt.Fprintf(w, `{"success":true,"action":%q}`, name)
	})

	//======================================================================================
	// /favicon.ico – a minimal inline SVG favicon (blue rounded square with "C")
	// so browsers don't log 404s for it. Modern browsers accept image/svg+xml.
	mux.HandleFunc("/favicon.ico", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/svg+xml")
		w.Header().Set("Cache-Control", "public, max-age=86400")
		pkg.ServeStaticAsset(w, "favicon.ico")
	})

	// Intercept VS Code NLS requests for translation remapping. The prefix
	// comes from KVS_SVC_VSCODE_NLS_URL — the same variable the [service.vsc]
	// once_shell template uses; the fallback matches the ini default
	// {KVS_SVC_VSCODE_NLS_URL:-/__cache/vscode/nls/}. A /__ prefix serves NLS
	// through kvs itself; any other value (e.g. an external URL) needs no
	// local route.
	// NLS（KVS_SVC_VSCODE_NLS_URL）不再在 main 绑定：由路由表达，
	// 如 /__cache/vscode/nls/=svc://vscode.nls（svc 服务扩展）。

	// cache proxy – {proxy_path}/{scheme}:{host}/path → {scheme}://{host}/path
	// Only registered when both cache_dir (disk storage) and proxy_path (route
	// prefix) are configured. Either one empty disables the cache proxy route.
	if pkg.CachePathPrefix() != "" {
		mux.HandleFunc(pkg.CachePathPrefix(), pkg.HandleCache)
	}

	// /__logout.vsc.js – VS Code logout-button script injected into proxied HTML.
	// Named .vsc because it targets the VS Code activity-bar toolbar; other apps
	// can get their own script (e.g. /__logout.xxx.js) later.
	mux.HandleFunc("/__logout.vsc.js", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/javascript; charset=utf-8")
		w.Header().Set("Cache-Control", "no-cache")
		_, _ = w.Write(pkg.MustAsset("logout.vsc.js"))
	})

	// All other requests: dispatch to backends. The service backend is
	// prepared lazily on first access: while preparing, its prefix returns the
	// loading page; once done (success), it proxies normally; on error, 500.
	// Non-service backends are always reachable.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		// Dispatch to the first matching route (latest table — rebuilt after
		// every config reload so version changes are followed immediately).
		for _, rt := range routesPtr.Load().([]route) {
			// Match: regex backends use regexp.Match; ws~ backends require
			// the path to equal the prefix exactly; others use prefix match.
			if rt.re != nil {
				if !rt.re.MatchString(r.URL.Path) {
					continue
				}
			} else if rt.templated {
				// <verhash>/<version> 前缀按目标服务当前配置展开后做前缀匹配
				// （仅前缀匹配，不支持正则；模板仅对 svc:// 有效）；服务未
				// 准备（版本未解析）时跳过。
				p, ok := pkg.ExpandVerTemplate(rt.prefix, pkg.Services().Get(rt.svc))
				if !ok || !strings.HasPrefix(r.URL.Path, p) {
					continue
				}
			} else if rt.exact {
				if r.URL.Path != rt.prefix {
					continue
				}
			} else {
				if !strings.HasPrefix(r.URL.Path, rt.prefix) {
					continue
				}
			}
			if rt.svc != "" {
				// Hosted svc:// service: per-service preparation state and
				// subprocess, triggered lazily on the first request.
				st := pkg.Services().State(rt.svc)
				sc := pkg.Services().Get(rt.svc)
				// Finished with error? Return it as 500 (no re-trigger, so
				// the client gets the final error instead of looping on the
				// 503 loading page). /__restart resets the state to retry.
				if done, err := st.Result(); done && err != nil {
					http.Error(w, "service "+rt.svc+" preparation failed: "+err.Error(), http.StatusInternalServerError)
					return
				}
				// Trigger preparation if not already running or succeeded.
				if st.Begin() {
					go func(sc *pkg.ServiceConfig, st *pkg.ServiceState) {
						managed, err := pkg.PrepareService(sc, st)
						if err == nil && managed && sc.Command != "" {
							// Start the backend subprocess after a successful prepare.
							if err := pkg.Services().StartProc(sc); err != nil {
								log.Printf("[prepare:%s] start backend: %v", sc.Name, err)
							}
						}
						st.Finish(err)
						if err != nil {
							log.Printf("[prepare:%s] service preparation failed: %v", sc.Name, err)
						}
					}(sc, st)
				}
				// Preparing? Show loading page.
				if st.Active() {
					pkg.ServeLoadingPage(w, st.GetStatus())
					return
				}
				// Finished: proxy on success, 500 on error.
				if done, err := st.Result(); done {
					if err != nil {
						http.Error(w, "service "+rt.svc+" preparation failed: "+err.Error(), http.StatusInternalServerError)
						return
					}
					rt.handler.ServeHTTP(w, r)
					return
				}
				// not yet active (goroutine just scheduled) — loading page.
				pkg.ServeLoadingPage(w, st.GetStatus())
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
		finalHandler = pkg.AuthMiddleware(mux, cfg, setCookie)
	}

	servers := pkg.BuildServers(cfg.Port, cfg.UseSSL, finalHandler)

	// Graceful shutdown on SIGINT/SIGTERM.
	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)

	// Start HTTP servers first so the loading page is available during preparation.
	for _, srv := range servers {
		srv := srv
		go func() {
			var err error
			if srv.IsTLS {
				err = srv.Server.ListenAndServeTLS("", "")
			} else {
				err = srv.Server.ListenAndServe()
			}
			if err != nil && err != http.ErrServerClosed {
				log.Fatalf("server on %s: %v", srv.Server.Addr, err)
			}
		}()
	}

	log.Printf("proxy starting: %s", pkg.ServerAddrs(servers))
	log.Printf("cookie name: %s", cfg.CookieTknName)
	if cfg.LoginToken != "" {
		mode := "session"
		if cfg.LoginTimeout > 0 {
			mode = fmt.Sprintf("expiring (%ds, auto-renew at 1/4)", cfg.LoginTimeout)
		}
		log.Printf("cookie auth: enabled (cookie %s, mode: %s)", cfg.CookieTknName, mode)
	}
	log.Printf("backends: %d", len(cfg.Proxies))
	for i, b := range cfg.Proxies {
		marker := ""
		if b.IsService {
			marker += " (service)"
		}
		if b.IsCache {
			marker += " (cc cache)"
		}
		if b.IsWSock {
			marker += " (ws serve)"
		}
		log.Printf("  route[%d] %s → %s://%s%s", i, b.Source, b.Scheme, b.Target, marker)
	}
	if len(cfg.Headers) > 0 {
		log.Printf("proxy headers: %v", cfg.Headers)
	}

	// Hosted svc:// services start lazily: the first request to their route
	// triggers preparation (probe → download → extract → start), see the
	// service route handler above.

	<-sigCh
	log.Printf("shutdown signal received, draining… (press Ctrl+C again to force quit)")
	go func() {
		<-sigCh
		log.Printf("forced exit on second signal")
		os.Exit(1)
	}()
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Server.Shutdown(shutdownCtx)
	}
	// Agent host is kvs-managed: terminate it on kvs exit.
	agentvs.Stop()
	// Hosted svc:// services: stop kvs-managed subprocesses (with stop_shell).
	// External services (alive when probed, never started by kvs) are never
	// killed or touched by kvs on shutdown.
	pkg.Services().StopAll()
	// 应用中心/中继状态文件：退出前同步落盘，避免防抖窗口内的修改丢失。
	pkg.FlushNow()
}

// svcBackendName returns the service name behind a svc:// backend ("" otherwise).
func svcBackendName(b pkg.Backend) string {
	if b.Scheme == "svc" {
		name, _, _ := pkg.SplitSvcTarget(b.Target) // svc://name.ext → name
		return name
	}
	return ""
}
