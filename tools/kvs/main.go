package main

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
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

	// Subcommand: "demo" — generate a starter kvs.ini from the embedded example.
	if len(os.Args) > 1 && os.Args[1] == "demo" {
		pkg.DemoCommand()
		return
	}

	// Subcommand: "mirror" — sync vscode versions to S3-compatible storage.
	if len(os.Args) > 1 && os.Args[1] == "mirror" {
		pkg.MirrorCommand(os.Args[2:])
		return
	}

	cfg := pkg.LoadInitConfig()

	// Apply cache_dir as the proxy cache root (disk storage).
	// The cache route is registered only when cache_dir is configured AND
	// proxy_path is non-empty; otherwise the prefix stays "" so the cache
	// route is not registered and auth checks don't treat every path as public.
	if cfg.SvcCacheDir != "" {
		pkg.SetCacheConfig(cfg.SvcCacheDir, cfg.SvcProxyPath)
	}
	// cc~ marked backends always have disk caching available, defaulting to
	// /cache when cache_dir is not configured.
	pkg.SetCacheDir(cfg.SvcCacheDir)
	// zcode relay state file lives under the service home ({home}/zcoded.json).
	pkg.SetZcodeHome(cfg.SvcHome)
	// cache_sed: rewrite cc~ cached bodies (file|old|new||... rules).
	pkg.SetCacheSed(cfg.SvcCacheSed)

	// Service preparation state (for showing loading page during download/extract).
	srvState := &pkg.ServiceState{}

	// Build (prefix → handler) routes for each backend. The service backend's
	// prefix is recorded separately so the loading page is only shown for it
	// during preparation (non-service backends stay reachable).
	type route struct {
		prefix    string
		re        *regexp.Regexp // compiled regex for ^-prefixed backends, nil otherwise
		handler   http.Handler
		isService bool
		exact     bool // ws~ backends: request path must equal prefix exactly (no prefix match)
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
		routes[i] = route{prefix: b.Prefix, re: re, handler: pkg.CreateBackendHandler(b, cfg.Headers, cfg.LoginAuthz), isService: b.IsService, exact: b.IsWSock}
		if b.IsService {
			servicePrefix = b.Prefix
		}
	}

	service := &pkg.Process{Name: "service"}
	vagents := &pkg.Process{Name: "vagents"}
	// vmodels := &pkg.Process{Name: "vmodels"}

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
			pkg.ServeLoginAsset(w, "")
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
			pkg.ServeLoginAsset(w, "Invalid access token, please try again")
			return
		}
		// Generate cookie value based on login_timeout mode.
		cookieVal := pkg.GenerateCookieValue(cfg.LoginToken, cfg.LoginTimeout)
		maxAge := 0 // session lifetime
		if cfg.LoginTimeout > 0 {
			maxAge = cfg.LoginTimeout
		}
		setCookie(w, cookieVal, maxAge)
		back := pkg.SafeReferer(r.Referer(), r.Host)
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

	// /__version – returns the resolved service application version as plain
	// text. Falls back to "0.0.0" when no version is resolved (e.g. no service
	// backend, version resolution failed, or version not yet loaded).
	mux.HandleFunc("/__version", func(w http.ResponseWriter, r *http.Request) {
		v := cfg.SvcVersion
		if v == "" {
			v = "0.0.0"
		}
		// if vh := cfg.SvcVersionHash; vh != "" {
		// 	v = v + ", " + vh
		// }
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		_, _ = io.WriteString(w, v)
	})

	// /__restart – kills the backend subprocess and resets the service state so
	// the next request re-triggers version detection, download, extract, and
	// start. Requires authentication (not in isPublicAuthPath). Only works when
	// a service backend exists and was kvs-managed.
	// /__restart        → simple restart, clears any version override
	// /__restart?v=1.32.1 → restart with a specific version override
	// /__restart/1.32.1 → same, path-style
	restartHandler := func(w http.ResponseWriter, r *http.Request) {
		// Extract optional version: query (?v=1.32.1) is the primary method.
		// overrideVersion := r.PathValue("v")
		overrideVersion := r.URL.Query().Get("v")

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
			pkg.GOverrideVersion = overrideVersion
		} else {
			// No version specified → clear any previous override so
			// resolveVersion falls back to version_latest_url.
			if pkg.GOverrideVersion != "" {
				log.Printf("[restart] clearing version override (was %s)", pkg.GOverrideVersion)
				pkg.GOverrideVersion = ""
			}
		}

		// Reload config so download/bin_home are re-expanded with the new (or
		// cleared) version. If version resolve fails (e.g. 404), LoadInitConfig
		// stores the error in cfg.InitError instead of crashing. PrepareService
		// will surface it on the loading page as a download failure.
		newCfg := pkg.LoadInitConfig()

		// Agent branch: /__restart?agent=<file> — restart the SERVICE backend
		// (command) with bridge args appended to it, derived from the agent entry file.
		if agentFile := r.URL.Query().Get("agent"); agentFile != "" {
			if cfg.SvcCommand == "" {
				http.Error(w, "command not configured", http.StatusBadRequest)
				return
			}
			if agentFile == "" {
				newCfg.VscAgentArgs = ""
			} else if args := pkg.AgentBridgeArgsFromFile(filepath.Join(cfg.VscAgentsDir, agentFile+".json")); args == "" {
				http.Error(w, "agent entry not found or invalid: "+agentFile, http.StatusNotFound)
				return
			} else {
				newCfg.VscAgentArgs = args
			}
		}

		cfg = newCfg // always update cfg so InitError (if any) is visible to prepareService

		// Kill the running backend (if any) and reset state.
		hadBackend := service.Running()
		service.Stop()
		if hadBackend {
			// Run stop_shell if configured.
			if cfg.SvcStopShell != "" {
				log.Printf("[restart] running stop_shell: %s", cfg.SvcStopShell)
				if err := pkg.RunServiceStartup(cfg.SvcStopShell); err != nil {
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
		srvState.Reset()

		// If config reload failed (e.g. version hash 404), report the error
		// but don't crash — the loading page will show it on next access.
		if newCfg.InitError != "" {
			srvState.Finish(fmt.Errorf("%s", newCfg.InitError))
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
	// mux.HandleFunc("/__restart/{v}", restartHandler)

	//======================================================================================

	// /__agents/status — returns { running, command, cmds } for the Agents
	// dialog. command reflects the current (possibly body-replaced)
	// vsc_agents_cmd; cmds is the vsc_agent_cmds preset map (name→command),
	// shown as quick-fill buttons when the command box is empty.
	mux.HandleFunc("/__agents/status", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		resp := map[string]any{
			"running": vagents.Running(),
			"command": cfg.VscAgentsCmd,
			"cmds":    cfg.VscAgentCmds,
		}
		_ = json.NewEncoder(w).Encode(resp)
	})

	mux.HandleFunc("/__agents/entries", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		resp := pkg.ListAgentEntries(cfg.VscAgentsDir)
		_ = json.NewEncoder(w).Encode(resp)
	})

	// /__agents/start — launch the agent host (409 if already running).
	// Optional JSON body {"command": "..."} REPLACES cfg.VscAgentsCmd (the
	// new value is kept for subsequent status/start/restart); an empty or
	// missing body starts the currently configured command.
	mux.HandleFunc("/__agents/start", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if vagents.Running() {
			http.Error(w, "agents already running", http.StatusConflict)
			return
		}
		if r.Body != nil {
			var req struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err == nil {
				if c := strings.TrimSpace(req.Command); c != "" && c != cfg.VscAgentsCmd {
					log.Printf("[agents] command replaced: %s", c)
					cfg.VscAgentsCmd = c
				}
			}
		}
		if cfg.VscAgentsCmd == "" {
			http.Error(w, "vsc_agents_cmd not configured", http.StatusBadRequest)
			return
		}
		if err := vagents.Start("", cfg.VscAgentsCmd); err != nil {
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
		if !vagents.Running() {
			http.Error(w, "agents not running", http.StatusConflict)
			return
		}
		vagents.Stop()
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true}`)
	})

	// /__agents/restart — restart the agent host (without bridge args; to
	// reload with a specific entry use POST /__restart?agent=<file>).
	// Optional JSON body {"command": "..."} REPLACES cfg.VscAgentsCmd, same
	// as /__agents/start.
	mux.HandleFunc("/__agents/restart", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			http.Error(w, "POST required", http.StatusMethodNotAllowed)
			return
		}
		if r.Body != nil {
			var req struct {
				Command string `json:"command"`
			}
			if err := json.NewDecoder(io.LimitReader(r.Body, 64*1024)).Decode(&req); err == nil {
				if c := strings.TrimSpace(req.Command); c != "" && c != cfg.VscAgentsCmd {
					log.Printf("[agents] command replaced: %s", c)
					cfg.VscAgentsCmd = c
				}
			}
		}
		if cfg.VscAgentsCmd == "" {
			http.Error(w, "vsc_agents_cmd not configured", http.StatusBadRequest)
			return
		}
		if ok := vagents.Restart(cfg.VscAgentsCmd); !ok {
			http.Error(w, "failed to start agents", http.StatusInternalServerError)
			return
		}
		w.WriteHeader(http.StatusOK)
		_, _ = io.WriteString(w, `{"success":true}`)
	})

	// /__agents/action/<name> — run the [actions] entry <name> from kvs.ini.
	// kvs stays generic: pkg.RunAgentAction executes the value (file:// via
	// shebang, otherwise sh -c) and blocks until it exits. Any method works, so
	// a browser URL or plain curl triggers it. An unknown name returns 404
	// without revealing the configured actions.
	mux.HandleFunc("/__agents/action/", func(w http.ResponseWriter, r *http.Request) {
		name := strings.TrimPrefix(r.URL.Path, "/__agents/action/")
		if name == "" || strings.Contains(name, "/") {
			http.Error(w, "action name required", http.StatusBadRequest)
			return
		}
		cmd, known := cfg.Actions[name]
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

	// Intercept VS Code NLS requests for translation remapping.
	if nlsPathPrefix := os.Getenv("SVC_VSCODE_NLS_URL"); strings.HasPrefix(nlsPathPrefix, "/__") {
		mux.HandleFunc(nlsPathPrefix, func(w http.ResponseWriter, r *http.Request) {
			pkg.VscodeNlsHandle(w, r, nlsPathPrefix, cfg.SvcHome, cfg.SvcBinHome, cfg.VscLanguage)
		})
	}

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
		// Intercept VS Code web-extension-resource requests，SvcVersionHash change on restart
		if cfg.SvcVersionHash != "" && cfg.SvcHome != "" {
			prePath := "/stable-" + cfg.SvcVersionHash + "/web-extension-resource/"
			if strings.HasPrefix(r.URL.Path, prePath) {
				pkg.VscodeExtHandle(w, r, prePath, cfg.SvcHome)
				return
			}
		}
		// Dispatch to the first matching route.
		for _, rt := range routes {
			// Match: regex backends use regexp.Match; ws~ backends require
			// the path to equal the prefix exactly; others use prefix match.
			if rt.re != nil {
				if !rt.re.MatchString(r.URL.Path) {
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
			if rt.isService {
				// Trigger preparation if not already running or succeeded.
				if srvState.Begin() {
					go func() {
						managed, err := pkg.PrepareService(cfg, srvState)
						if err == nil && managed && cfg.SvcCommand != "" {
							// Clean up stale unix socket files left by a previous crash.
							for _, b := range cfg.Proxies {
								if b.IsService && b.Scheme == "unix" {
									_ = os.Remove(b.Target)
								}
							}
							// Start the backend subprocess after a successful prepare.
							service.Start("", cfg.SvcCommand+cfg.VscAgentArgs)
						}
						srvState.Finish(err)
						if err != nil {
							log.Printf("[prepare] service preparation failed: %v", err)
						}
					}()
				}
				// Preparing? Show loading page.
				if srvState.Active() {
					pkg.ServeLoadingPage(w, srvState.GetStatus())
					return
				}
				// Finished: proxy on success, 500 on error.
				if done, err := srvState.Result(); done {
					if err != nil {
						http.Error(w, "service preparation failed: "+err.Error(), http.StatusInternalServerError)
						return
					}
					rt.handler.ServeHTTP(w, r)
					return
				}
				// not yet active (goroutine just scheduled) — loading page.
				pkg.ServeLoadingPage(w, srvState.GetStatus())
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
			marker += " (service)"
		}
		if b.IsCache {
			marker += " (cc cache)"
		}
		if b.IsWSock {
			marker += " (ws)"
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
		service.Start("", cfg.SvcCommand+cfg.VscAgentArgs)
	}

	<-sigCh
	log.Printf("shutdown signal received, draining…")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	for _, srv := range servers {
		_ = srv.Server.Shutdown(shutdownCtx)
	}
	// Agent host is kvs-managed: terminate it on kvs exit.
	vagents.Stop()
	// Only kvs-managed backends (started via startBackend) are cleaned up.
	// External/system services (detected via check, no proc) are never
	// killed or touched by kvs on shutdown.
	if service.Running() {
		service.Stop()
		// Run stop_shell if configured (kvs-managed backend only).
		if cfg.SvcStopShell != "" {
			log.Printf("[shutdown] running stop_shell: %s", cfg.SvcStopShell)
			if err := pkg.RunServiceStartup(cfg.SvcStopShell); err != nil {
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
