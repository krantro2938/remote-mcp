// Package relay is the public server: agents connect to it over WebSocket and
// MCP clients (claude.ai, Claude Code) connect to its /mcp endpoint.
package relay

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

type Config struct {
	PublicURL     string            // https://relay.example.com (no trailing slash)
	ListenAddr    string            // 127.0.0.1:8080
	DataDir       string            // captures and OAuth state
	AgentTokens   map[string]string // token -> machine name
	AdminPassword string            // OAuth consent + gallery
	MCPToken      string            // optional static bearer token for /mcp
	MCPSecretPath string            // optional unauthenticated path, e.g. "/mcp-<random>"
	CaptureTTL    time.Duration
	LabeledTTL    time.Duration
}

// ConfigFromEnv reads the configuration from environment variables.
func ConfigFromEnv() (Config, error) {
	c := Config{
		PublicURL:     strings.TrimSuffix(strings.TrimSpace(os.Getenv("PUBLIC_URL")), "/"),
		ListenAddr:    envOr("LISTEN_ADDR", "127.0.0.1:8080"),
		DataDir:       envOr("DATA_DIR", "/var/lib/remote-mcp"),
		AdminPassword: os.Getenv("ADMIN_PASSWORD"),
		MCPToken:      os.Getenv("MCP_TOKEN"),
		MCPSecretPath: strings.TrimSpace(os.Getenv("MCP_SECRET_PATH")),
		AgentTokens:   map[string]string{},
	}
	var err error
	if c.CaptureTTL, err = time.ParseDuration(envOr("CAPTURE_TTL", "72h")); err != nil {
		return c, fmt.Errorf("CAPTURE_TTL: %w", err)
	}
	if c.LabeledTTL, err = time.ParseDuration(envOr("LABELED_TTL", "720h")); err != nil {
		return c, fmt.Errorf("LABELED_TTL: %w", err)
	}
	// AGENT_TOKENS="laptop=TOKEN1,labpc=TOKEN2"
	for _, part := range strings.Split(os.Getenv("AGENT_TOKENS"), ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		name, tok, ok := strings.Cut(part, "=")
		if !ok {
			return c, fmt.Errorf("AGENT_TOKENS entry %q must look like name=token", part)
		}
		c.AgentTokens[strings.TrimSpace(tok)] = strings.TrimSpace(name)
	}
	return c, c.Validate()
}

func envOr(k, def string) string {
	if v := strings.TrimSpace(os.Getenv(k)); v != "" {
		return v
	}
	return def
}

func (c Config) Validate() error {
	u, err := url.Parse(c.PublicURL)
	if c.PublicURL == "" || err != nil || u.Host == "" || (u.Scheme != "https" && u.Scheme != "http") {
		return errors.New("PUBLIC_URL must be set to the relay's public URL, e.g. https://relay.example.com")
	}
	if len(c.AgentTokens) == 0 {
		return errors.New("AGENT_TOKENS must contain at least one name=token pair")
	}
	for tok, name := range c.AgentTokens {
		if len(tok) < 20 {
			return fmt.Errorf("agent token for %q is too short (use at least 20 random characters, e.g. openssl rand -hex 24)", name)
		}
	}
	if len(c.AdminPassword) < 10 {
		return errors.New("ADMIN_PASSWORD must be at least 10 characters")
	}
	if c.MCPToken != "" && len(c.MCPToken) < 20 {
		return errors.New("MCP_TOKEN must be at least 20 characters")
	}
	if c.MCPSecretPath != "" && (!strings.HasPrefix(c.MCPSecretPath, "/") || len(c.MCPSecretPath) < 24) {
		return errors.New("MCP_SECRET_PATH must start with / and be at least 24 characters long")
	}
	return nil
}

type Server struct {
	cfg   Config
	log   *slog.Logger
	Hub   *Hub
	Caps  *CaptureStore
	OAuth *OAuth
	tools *Tools
}

func New(cfg Config, log *slog.Logger) (*Server, error) {
	if err := os.MkdirAll(cfg.DataDir, 0o750); err != nil {
		return nil, err
	}
	caps, err := NewCaptureStore(cfg.DataDir+"/captures", log)
	if err != nil {
		return nil, err
	}
	oa, err := NewOAuth(cfg.PublicURL, cfg.AdminPassword, cfg.DataDir, log)
	if err != nil {
		return nil, err
	}
	hub := NewHub(cfg.AgentTokens, log)
	return &Server{cfg: cfg, log: log, Hub: hub, Caps: caps, OAuth: oa, tools: NewTools(hub, caps, cfg.PublicURL, log)}, nil
}

// Handler returns the complete HTTP handler.
func (s *Server) Handler() http.Handler {
	mcpServer := s.tools.Server()
	mcpHandler := mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return mcpServer },
		&mcp.StreamableHTTPOptions{SessionTimeout: 2 * time.Hour})

	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintln(w, "remote-mcp relay. MCP endpoint: "+s.cfg.PublicURL+"/mcp")
	})
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) { fmt.Fprintln(w, "ok") })
	mux.Handle("/agent", s.Hub)
	mux.Handle("/mcp", s.requireAuth(mcpHandler))
	if s.cfg.MCPSecretPath != "" {
		mux.Handle(s.cfg.MCPSecretPath, mcpHandler)
	}
	s.OAuth.Register(mux)
	mux.HandleFunc("GET /c/{file}", s.serveCapture)
	mux.HandleFunc("GET /gallery", s.basicAuth(s.gallery))
	mux.HandleFunc("GET /gallery.zip", s.basicAuth(s.galleryZip))
	mux.HandleFunc("GET /download/", s.basicAuth(s.downloads))
	return logRequests(s.log, mux)
}

// downloads serves DATA_DIR/downloads (agent.exe and per-machine agent.json
// files written by deploy/docker-entrypoint.sh), so the agent can be fetched from any browser.
func (s *Server) downloads(w http.ResponseWriter, r *http.Request) {
	dir := filepath.Join(s.cfg.DataDir, "downloads")
	name := strings.TrimPrefix(r.URL.Path, "/download/")
	w.Header().Set("Cache-Control", "no-store")
	if name == "" {
		entries, _ := os.ReadDir(dir)
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		fmt.Fprint(w, `<!doctype html><meta charset="utf-8"><title>remote-mcp downloads</title><body style="font-family:system-ui;margin:24px"><h1 style="font-size:20px">Downloads</h1><ul>`)
		for _, e := range entries {
			if !e.IsDir() {
				fmt.Fprintf(w, `<li><a href="/download/%s">%s</a></li>`, url.PathEscape(e.Name()), template.HTMLEscapeString(e.Name()))
			}
		}
		fmt.Fprint(w, `</ul><p>Put <code>agent.json</code> (renamed from agent-&lt;machine&gt;.json) next to <code>agent.exe</code> and double-click agent.exe.</p></body>`)
		return
	}
	if strings.ContainsAny(name, `/\`) || strings.HasPrefix(name, ".") {
		http.NotFound(w, r)
		return
	}
	path := filepath.Join(dir, name)
	if _, err := os.Stat(path); err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, name))
	http.ServeFile(w, r, path)
}

func (s *Server) requireAuth(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
		if ok && tok != "" {
			if s.cfg.MCPToken != "" && subtle.ConstantTimeCompare([]byte(tok), []byte(s.cfg.MCPToken)) == 1 {
				next.ServeHTTP(w, r)
				return
			}
			if s.OAuth.VerifyAccess(tok) {
				next.ServeHTTP(w, r)
				return
			}
		}
		w.Header().Set("WWW-Authenticate", fmt.Sprintf(`Bearer resource_metadata="%s"`, s.OAuth.ResourceMetadataURL()))
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "invalid_token", "error_description": "missing or invalid access token"})
	})
}

func (s *Server) basicAuth(h http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		_, pw, ok := r.BasicAuth()
		if !ok || subtle.ConstantTimeCompare([]byte(pw), []byte(s.cfg.AdminPassword)) != 1 {
			if ok {
				time.Sleep(time.Second)
			}
			w.Header().Set("WWW-Authenticate", `Basic realm="remote-mcp gallery (any user name, admin password)"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		h(w, r)
	}
}

// serveCapture serves /c/<id>.png. The 96-bit random id acts as the secret.
func (s *Server) serveCapture(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimSuffix(r.PathValue("file"), ".png")
	data, err := s.Caps.ReadPNG(id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "image/png")
	w.Header().Set("Cache-Control", "private, max-age=86400")
	w.Header().Set("X-Robots-Tag", "noindex")
	if r.URL.Query().Get("download") != "" {
		name := id
		if c, err := s.Caps.Get(id); err == nil && c.Label != "" {
			name = unsafeName.ReplaceAllString(c.Label, "_")
		}
		w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s.png"`, name))
	}
	_, _ = w.Write(data)
}

var galleryTmpl = template.Must(template.New("g").Funcs(template.FuncMap{
	"fmt": func(t time.Time) string { return t.Local().Format("2006-01-02 15:04:05") },
}).Parse(`<!doctype html><html><head><meta charset="utf-8"><title>remote-mcp captures</title>
<meta name="viewport" content="width=device-width,initial-scale=1">
<style>
body{font-family:system-ui,sans-serif;margin:24px;background:#fafafa;color:#222}
h1{font-size:20px;margin:0 0 4px} .bar{margin:0 0 18px;color:#555;font-size:14px} a{color:#0b5cad}
.grid{display:grid;grid-template-columns:repeat(auto-fill,minmax(300px,1fr));gap:16px}
.card{background:#fff;border:1px solid #e3e3e3;border-radius:10px;overflow:hidden}
.card img{width:100%;display:block;background:#eee;aspect-ratio:16/10;object-fit:contain}
.meta{padding:8px 10px;font-size:13px;line-height:1.5}.label{font-weight:600;font-size:14px}
.muted{color:#777}
</style></head><body>
<h1>Captures</h1>
<p class="bar">{{len .Caps}} stored · <a href="/gallery.zip">download all labeled as zip</a> · <a href="/gallery.zip?all=1">download everything as zip</a>{{if .LabeledOnly}} · <a href="/gallery">show all</a>{{else}} · <a href="/gallery?labeled=1">labeled only</a>{{end}}</p>
<div class="grid">
{{range .Caps}}<div class="card"><a href="/c/{{.ID}}.png" target="_blank"><img loading="lazy" src="/c/{{.ID}}.png"></a>
<div class="meta"><div class="label">{{if .Label}}{{.Label}}{{else}}<span class="muted">unlabeled</span>{{end}}</div>
{{.Machine}} · {{.Width}}×{{.Height}} · {{fmt .Created}}<br>
<span class="muted">{{.ID}}</span> · <a href="/c/{{.ID}}.png?download=1">download</a></div></div>
{{end}}</div></body></html>`))

func (s *Server) gallery(w http.ResponseWriter, r *http.Request) {
	labeled := r.URL.Query().Get("labeled") != ""
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_ = galleryTmpl.Execute(w, map[string]any{"Caps": s.Caps.List(r.URL.Query().Get("machine"), labeled), "LabeledOnly": labeled})
}

func (s *Server) galleryZip(w http.ResponseWriter, r *http.Request) {
	caps := s.Caps.List(r.URL.Query().Get("machine"), r.URL.Query().Get("all") == "")
	w.Header().Set("Content-Type", "application/zip")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="captures-%s.zip"`, time.Now().Format("20060102-1504")))
	if err := s.Caps.WriteZip(w, caps); err != nil {
		s.log.Error("zip", "err", err)
	}
}

// RunCleanup periodically deletes old captures.
func (s *Server) RunCleanup(ctx context.Context) {
	t := time.NewTicker(time.Hour)
	defer t.Stop()
	for {
		if n := s.Caps.Cleanup(s.cfg.CaptureTTL, s.cfg.LabeledTTL); n > 0 {
			s.log.Info("deleted old captures", "count", n)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

type statusWriter struct {
	http.ResponseWriter
	code int
}

func (w *statusWriter) WriteHeader(code int) { w.code = code; w.ResponseWriter.WriteHeader(code) }

// Unwrap lets http.ResponseController reach Flush/Hijack on the real writer
// (needed for SSE streaming and WebSocket upgrades).
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *statusWriter) Flush() {
	if f, ok := w.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func logRequests(log *slog.Logger, h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/agent" { // long-lived; the hub logs it
			h.ServeHTTP(w, r)
			return
		}
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w, code: 200}
		h.ServeHTTP(sw, r)
		if r.URL.Path == "/healthz" {
			return
		}
		path := r.URL.Path
		if strings.HasPrefix(path, "/c/") {
			path = "/c/…"
		}
		log.Debug("http", "method", r.Method, "path", path, "status", sw.code, "dur", time.Since(start).Round(time.Millisecond), "remote", clientIP(r))
		if sw.code == http.StatusUnauthorized {
			log.Info("http unauthorized", "method", r.Method, "path", path, "remote", clientIP(r))
		}
	})
}
