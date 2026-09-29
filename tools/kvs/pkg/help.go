package pkg

import (
	"fmt"
	"os"
)

// HelpCommand prints usage information for all kvs commands.
func HelpCommand() {
	fmt.Print(`kvs — reverse proxy gateway (VS Code Server auto-deploy + auth + cache)

Usage:
  kvs <subcommand> [options]
  kvs [options]                     start the proxy service (embedded kvs.ini)
  kvs -c <config> [options]         start the proxy service with a config file
  kvs -n "<entries>" [options]      inline routes

Subcommands:
  kvs help                          show this help
  kvs demo [default|vscode|zcodex]  generate a sample kvs.ini config
  kvs mirror [vscode] [-version v] [-platform p]
                                    sync VS Code versions to S3-compatible storage
                                    app: vscode (default, only one supported)
                                    -version: version to sync (default latest)
                                    -platform: e.g. server-linux-x64-web (default)
                                    storage env: KVS_MIRROR_S3_PREFIX (required),
                                      KVS_MIRROR_S3_ACCESS/_SECRET/_REGION
  kvs syncto <src> <dst>            sync files between local and S3 (env-configured)
                                    kvs syncto /local/path s3:/bucket/key    upload
                                    kvs syncto s3:/bucket/key /local/path    download
                                    env: KVS_S3_PREFIX, KVS_S3_ACCESS,
                                         KVS_S3_SECRET, [KVS_S3_REGION]

Startup options:
  -c <path>                         config file path (optional; default: the
                                    embedded kvs.ini)
  -n ":name"                        use [proxies.<name>] as the active route table
  -n "prefix=url;prefix=url"        inline routes
                                    separate entries with ';', each: prefix=url
                                    example: -n "/healthz=text://OK:@now;/=http://127.0.0.1:8080"

Route formats ([proxies.<name>] section / -n argument):
  /=http://localhost:8080           HTTP reverse proxy
  /=unix:///var/run/app.sock        Unix socket reverse proxy
  /=file:///var/www/html            static file server
  /=text://Hello World              plain text response (@now replaced with current time)
  /=svc://<name>                    kvs-hosted service ([service.<name>] section):
                                    probe → download → extract → start, then proxy
  /=relay://<name>                  in-process relay: relay://vscode is the Remote-Server
                                    raw-byte relay (hosts /agent, clients /connect,
                                    pairing via ?tkn=); other names: zcode pairing WS relay
  /=api://<name>                    in-process handler registry (api://manager)
  ^/api/\d+=http://localhost:8080   ^ prefix: regex matching
  cc~/path=https://up.stream        cc prefix: cache proxied responses on disk
  ws~/path=ws://up:8080             ws prefix: WebSocket-aware route (upgrade fast path)
  /__healthz=text://OK:@now         health check endpoint

Config file sections:
  [proxies.<name>]                  named backend route tables; -n :<name> selects
                                    one, the first table in file order is the default
  [headers]                         request header rewrites (Xxx=Val sets; Xxx= removes)
  [service.<name>]                  named hosted services, started when a
                                    route references svc://<name>

Proxy tuning (optional, top-level keys):
  ws_proxy                        WebSocket upgrade handling: auto (raw-conn tunnel,
                                  kernel splice, keeps the pipelined first frame) |
                                  stdlib (previous behaviour). default auto
  ws_idle_timeout                 WS tunnel idle timeout in seconds (default 0=off);
                                  closes after ~2x this long with no traffic either way,
                                  or as soon as one side stops reading for max(timeout,30s)
                                  (a write stall would silently drop already-read bytes)
  proxy_idle_conns_per_host       idle connections kept per backend (default 32; Go default 2)
  proxy_max_idle_conns            global idle connection cap (default 256)
  proxy_disable_compression       pass Accept-Encoding through instead of gzip+gunzip
                                  inside the proxy (default true)
  proxy_flush_interval            streaming flush interval in ms (default 100; 0=never, -1=every write)
  read_timeout                    seconds to read a whole request incl. body (default 60;
                                  0=unlimited, for big uploads / slow clients)

Service env reverse-assignment (per [service.<name>], <NAME> = upper name):
  KVS_SVC_<NAME>_HOME               working dir (default {KVS_HOME}/<name>)
  KVS_SVC_<NAME>_ADDR               backend address (http(s):// or unix://)
  KVS_SVC_<NAME>_HOST/_PORT/_SOCK   derived from _ADDR (reverse-assigned)
  KVS_SVC_<NAME>_VERSION/_VERHASH   resolved version / hash (reverse-assigned)
  KVS_SVC_<NAME>_BIN_HOME           resolved bin dir (reverse-assigned)
  envs_default                      per-service JSON map (env name → default value):
                                    fills system env vars that are unset/empty,
                                    never overrides set values; processed last so
                                    the defaults can reference the vars above

Built-in endpoints:
  /__stats           runtime counters as JSON (WS tunnels splice/generic + bytes, relay
                     frames, cache hits, goroutines/heap); protected by the global auth
  /__version         resolved service version (plain text; ?svc=<name> selects one,
                     default: the table's first svc:// service)
  /__restart         re-prepare a service (?svc=<name> default as above, ?v=<version>)
  /__open_tkn        short-lived access token for curl/scripts
  /debug/pprof/      enabled separately by KVS_PPROF (loopback by default)

Environment variables:
  KVS_PORT                          listen port (default 8080, HTTPS=port+1 when SSL)
  KVS_USE_SSL                       enable self-signed HTTPS (default false)
  KVS_LOGIN_AUTHZ                   enable 401/403→login-page redirect (default false)
  KVS_LOGIN_TOKEN                   cookie validation value (empty disables validation)
  KVS_LOGIN_TIMEOUT                 cookie lifetime seconds (0=session, >0=hash+expiry)
  KVS_TOKEN_TIMEOUT                 temp token lifetime seconds issued by /__open_tkn (default 300)
  KVS_COOKIE_TOKEN_KEY              cookie name (default kvs)
  KVS_HOME                          gateway working directory (default /app/.kvs, reverse-assigned)
  KVS_PROXIES                       inline routes (same format as -n "p=u;p=u")
  KVS_SVC_<NAME>_*                  per-service settings, see the reverse-assignment table
                                    above and the [service.<name>] comments in kvs.ini
                                    (e.g. KVS_SVC_VSCODE_DOWNLOAD_BASE / _DOWNLOAD_PATH)
  KVS_PATH_PUBLIC                   |-separated paths reachable without auth
  KVS_CC_SED                        cache_sed value (cc~ body rewrite rules)
  KVS_PROXY_PATH                    proxy_path value (cache-proxy prefix)
  KVS_WS_PROXY                      ws_proxy value (auto|stdlib), see above
  KVS_WS_IDLE                       ws_idle_timeout value (seconds)
  KVS_PROXY_IDLE_CONNS              proxy_idle_conns_per_host value
  KVS_PROXY_MAX_IDLE                proxy_max_idle_conns value
  KVS_PROXY_DISABLE_GZIP            proxy_disable_compression value
  KVS_PROXY_FLUSH_MS                proxy_flush_interval value
  KVS_READ_TIMEOUT                  read_timeout value (seconds)
  KVS_PPROF                         1 (127.0.0.1:6060) | <port> | <addr> — enable pprof
  KVS_DEBUG                         1 — load assets from the working directory (live edit)
  KVS_MIRROR_S3_PREFIX              mirror: S3 storage prefix (e.g. https://oss.example.com/vsc)
  KVS_MIRROR_S3_ACCESS              mirror: S3 access key ID
  KVS_MIRROR_S3_SECRET              mirror: S3 secret access key
  KVS_MIRROR_S3_REGION              mirror: S3 region (default empty)

Examples:
  kvs                               start with the embedded kvs.ini (first table)
  kvs -c kvs.ini                    start with a config file in the working directory
  kvs -n :vscode                    start with the [proxies.vscode] table
  kvs -n "/=http://127.0.0.1:8080"  simple inline proxy
  KVS_LOGIN_TOKEN=secret kvs -n :vscode  start with auth enabled
`)
}

// DemoCommand writes a copy of the embedded kvs.ini template to ./kvs.ini in
// the current directory. The optional arg selects the template:
//
//	demo          (or "default")  → kvs.ini
//	demo vscode                   → kvs.ini (single template since the
//	                                multi-service rework; pick a table with -n)
//
// If kvs.ini already exists, it prints an error and exits non-zero so the
// operator's existing config is never overwritten.
func DemoCommand(args []string) {
	asset := "kvs.ini"
	if len(args) > 0 {
		switch args[0] {
		case "default", "vscode", "zcodex":
			if args[0] != "default" {
				fmt.Printf("note: the %q template was merged into kvs.ini; select the table with -n :%s\n", args[0], args[0])
			}
			asset = "kvs.ini"
		default:
			fmt.Fprintf(os.Stderr, "error: unknown demo template %q (use default, vscode or zcodex)\n", args[0])
			os.Exit(1)
		}
	}
	const dest = "kvs.ini"
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(os.Stderr, "error: %s already exists in the current directory\n", dest)
		os.Exit(1)
	}
	data := MustAsset(asset)
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", dest, err)
		os.Exit(1)
	}
	fmt.Printf("created %s from %s (%d bytes)\n", dest, asset, len(data))
}
