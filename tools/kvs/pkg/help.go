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
  kvs -c <config> [options]        start the proxy service
  kvs -n "<entries>" [options]     inline routes, auto-appends -c default

Subcommands:
  kvs help                          show this help
  kvs demo                          generate a sample kvs.ini config
  kvs mirror -c <config> [version]  sync VS Code versions to S3-compatible storage
  kvs mirror -c default             sync the latest version with the built-in config
  kvs mirror -c default 1.130.0     sync a specific version

Startup options:
  -c <path>                         config file path (required)
  -c default                        use kvs.ini.example from embed
  -n "prefix=url;prefix=url"        inline [proxies] routes, auto-appends -c default
                                    separate entries with ';', each: prefix=url
                                    example: -n "/healthz=text://OK:@now;/=http://127.0.0.1:8080"

Route formats ([proxies] section / -n argument):
  /=http://localhost:8080           HTTP reverse proxy
  /=unix:///var/run/app.sock        Unix socket reverse proxy
  /=file:///var/www/html            static file server
  /=text://Hello World              plain text response (@now replaced with current time)
  &/=unix:///var/run/app.sock       & prefix: kvs-managed service (triggers auto-deploy)
  ^/api/\d+=http://localhost:8080   ^ prefix: regex matching
  /__healthz=text://OK:@now         health check endpoint

Config file sections:
  [proxies]          backend routes (required)
  [headers]          request header rewrites (Xxx=Val sets; Xxx= removes)
  [service]          service auto-deploy (download/extract/start)
  [actions]          named one-shot commands, run via /__agents/action/<name>
  [mirror]           S3 mirror sync config

Environment variables:
  KVS_PORT                          listen port (default 7080, HTTPS=port+1 when SSL)
  KVS_USESSL                        enable self-signed HTTPS (default false)
  KVS_LOGIN_AUTHZ                   enable 401/403→login-page redirect (default false)
  KVS_LOGIN_TOKEN                   cookie validation value (empty disables validation)
  KVS_LOGIN_TIMEOUT                 cookie lifetime seconds (0=session, >0=hash+expiry)
  KVS_COOKIE                        cookie name (default kvs)
  KVS_HOME                          service working directory (default /app/.vsc)
  KVS_SVC_SOCK_FILE                 backend socket path (default ./kvs.sock)
  KVS_VSCODE_VERSION                VS Code version to use (empty fetches the latest)
  KVS_VSCODE_DOWNLOAD_BASE          VS Code download base URL (default https://update.code.visualstudio.com)
  KVS_VSCODE_DOWNLOAD_PATH          download path prefix (default commit:, used to build the URL, [BASE_URL]/commit:[hash]/~)
  KVS_MIRROR_VSCODE_S3_PREFIX       mirror: S3 storage prefix (e.g. https://oss.example.com/vsc)
  KVS_MIRROR_VSCODE_S3_ACCESS       mirror: S3 access key ID
  KVS_MIRROR_VSCODE_S3_SECRET       mirror: S3 secret access key
  KVS_MIRROR_VSCODE_S3_REGION       mirror: S3 region (default empty)

Examples:
  kvs -c kvs.ini                    start with a config file
  kvs -c default                    start with the built-in default config
  kvs -n "/=http://127.0.0.1:8080"  simple proxy, auto-appends -c default
  KVS_LOGIN_TOKEN=secret kvs -c default  start with the default config and auth
`)
}

// DemoCommand writes a copy of the embedded kvs.ini.example to ./kvs.ini in
// the current directory. If kvs.ini already exists, it prints an error and
// exits non-zero so the operator's existing config is never overwritten.
func DemoCommand() {
	const dest = "kvs.ini"
	if _, err := os.Stat(dest); err == nil {
		fmt.Fprintf(os.Stderr, "error: %s already exists in the current directory\n", dest)
		os.Exit(1)
	}
	data := MustAsset("kvs.ini.example")
	if err := os.WriteFile(dest, data, 0o644); err != nil {
		fmt.Fprintf(os.Stderr, "error: write %s: %v\n", dest, err)
		os.Exit(1)
	}
	fmt.Printf("created %s (%d bytes)\n", dest, len(data))
}
