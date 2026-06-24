package main

import (
	"bytes"
	"context"
	"embed"
	"flag"
	"html/template"
	"io"
	"log"
	"net"
	"net/http"
	"net/http/httputil"
	"net/url"
	"os"
	"strings"
)

//go:embed login.html logout.html
var templateFS embed.FS

var (
	tmplLogin  *template.Template
	tmplLogout *template.Template
)

// Config holds proxy configuration.
type Config struct {
	BackendURL  string
	ProxyPort   string
	TokenCookie string // cookie name, default "vscode-tkn"
}

func loadConfig() Config {
	backend := os.Getenv("BACKEND_URL")
	port := os.Getenv("PROXY_PORT")
	if port == "" {
		port = "7080"
	}
	cookie := os.Getenv("TOKEN_COOKIE")
	if cookie == "" {
		cookie = "vscode-tkn"
	}

	flag.StringVar(&backend, "backend", backend, "Backend service URL")
	flag.StringVar(&port, "port", port, "Proxy listen port")
	flag.StringVar(&cookie, "cookie", cookie, "Token cookie name")
	flag.Parse()

	if backend == "" {
		log.Fatal("BACKEND_URL is required (set via env or -backend flag)")
	}
	return Config{
		BackendURL:  backend,
		ProxyPort:   port,
		TokenCookie: cookie,
	}
}

func main() {
	cfg := loadConfig()

	var err error
	tmplLogin, err = template.ParseFS(templateFS, "login.html")
	if err != nil {
		log.Fatalf("parse login.html: %v", err)
	}
	tmplLogout, err = template.ParseFS(templateFS, "logout.html")
	if err != nil {
		log.Fatalf("parse logout.html: %v", err)
	}

	backendURL, err := url.Parse(cfg.BackendURL)
	if err != nil {
		log.Fatalf("invalid BACKEND_URL: %v", err)
	}

	var proxy *httputil.ReverseProxy

	if backendURL.Scheme == "unix" {
		// Unix socket backend: unix:///var/run/vscode.sock
		socketPath := backendURL.Path
		if socketPath == "" {
			// unix:///var/run/vscode.sock → Path = "/var/run/vscode.sock"
			socketPath = backendURL.Host
		}
		log.Printf("using unix socket: %s", socketPath)

		proxy = &httputil.ReverseProxy{
			Director: func(req *http.Request) {
				req.URL.Scheme = "http"
				req.URL.Host = "unix"
			},
			Transport: &http.Transport{
				DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
					var d net.Dialer
					return d.DialContext(ctx, "unix", socketPath)
				},
			},
		}
	} else {
		proxy = httputil.NewSingleHostReverseProxy(backendURL)
	}

	// Pre-render login page for ModifyResponse (can't stream template there).
	loginHTML := renderLoginHTML()

	// Custom error handler: intercept 401/403 and show login page.
	proxy.ModifyResponse = func(r *http.Response) error {
		if r.StatusCode == http.StatusUnauthorized || r.StatusCode == http.StatusForbidden {
			if r.Body != nil {
				r.Body.Close()
			}
			r.StatusCode = http.StatusOK
			r.Header = make(http.Header)
			r.Header.Set("Content-Type", "text/html; charset=utf-8")
			r.Body = io.NopCloser(bytes.NewReader(loginHTML))
			r.ContentLength = int64(len(loginHTML))
			return nil
		}
		return nil
	}

	mux := http.NewServeMux()

	// /__login – serves login page (GET) or processes form (POST).
	mux.HandleFunc("/__login", func(w http.ResponseWriter, r *http.Request) {
		switch r.Method {
		case http.MethodPost:
			r.ParseForm()
			tkn := strings.TrimSpace(r.PostFormValue("token"))
			if tkn == "" {
				serveLoginHTML(w)
				return
			}
			http.SetCookie(w, &http.Cookie{
				Name:     cfg.TokenCookie,
				Value:    tkn,
				Path:     "/",
				HttpOnly: true,
				SameSite: http.SameSiteLaxMode,
			})
			// Reload the page that triggered the login.
			back := r.Referer()
			if back == "" {
				back = "/"
			}
			log.Printf("login ok, cookie %s=%s, reloading: %s", cfg.TokenCookie, tkn, back)
			http.Redirect(w, r, back, http.StatusSeeOther)
		default:
			serveLoginHTML(w)
		}
	})

	// /__logout – clears the token cookie.
	mux.HandleFunc("/__logout", func(w http.ResponseWriter, r *http.Request) {
		http.SetCookie(w, &http.Cookie{
			Name:     cfg.TokenCookie,
			Value:    "",
			Path:     "/",
			MaxAge:   -1,
			HttpOnly: true,
		})
		log.Printf("logout: cleared cookie %s", cfg.TokenCookie)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		tmplLogout.Execute(w, nil)
	})

	// All other requests go through the reverse proxy.
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		proxy.ServeHTTP(w, r)
	})

	addr := ":" + cfg.ProxyPort
	log.Printf("proxy starting on %s → %s", addr, cfg.BackendURL)
	log.Printf("token cookie: %s", cfg.TokenCookie)
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

func renderLoginHTML() []byte {
	var buf bytes.Buffer
	if err := tmplLogin.Execute(&buf, nil); err != nil {
		panic("render login: " + err.Error())
	}
	return buf.Bytes()
}

func serveLoginHTML(w http.ResponseWriter) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	tmplLogin.Execute(w, nil)
}
