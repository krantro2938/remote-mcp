package relay

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html/template"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"
)

// OAuth is a minimal OAuth 2.1 authorization server for a single owner, which
// is what claude.ai custom connectors need: dynamic client registration,
// authorization code + PKCE (S256), and refresh tokens. The "login" is one
// admin password.
type OAuth struct {
	issuer   string // e.g. https://relay.example.com
	resource string // issuer + "/mcp"
	password string
	path     string
	log      *slog.Logger

	mu    sync.Mutex
	state oauthState
	codes map[string]authCode
	fails []time.Time
}

type oauthClient struct {
	ID           string    `json:"id"`
	Name         string    `json:"name"`
	RedirectURIs []string  `json:"redirect_uris"`
	Created      time.Time `json:"created"`
}

type oauthToken struct {
	Kind     string    `json:"kind"` // access, refresh
	ClientID string    `json:"client_id"`
	Expires  time.Time `json:"expires"`
}

type oauthState struct {
	Clients map[string]*oauthClient `json:"clients"`
	Tokens  map[string]*oauthToken  `json:"tokens"` // sha256(token) -> token
}

type authCode struct {
	clientID    string
	redirectURI string
	challenge   string
	expires     time.Time
}

const (
	accessTTL  = 24 * time.Hour
	refreshTTL = 90 * 24 * time.Hour
)

func NewOAuth(publicURL, password, dataDir string, log *slog.Logger) (*OAuth, error) {
	o := &OAuth{
		issuer:   strings.TrimSuffix(publicURL, "/"),
		password: password,
		path:     filepath.Join(dataDir, "oauth.json"),
		log:      log,
		codes:    map[string]authCode{},
		state:    oauthState{Clients: map[string]*oauthClient{}, Tokens: map[string]*oauthToken{}},
	}
	o.resource = o.issuer + "/mcp"
	if data, err := os.ReadFile(o.path); err == nil {
		_ = json.Unmarshal(data, &o.state)
		if o.state.Clients == nil {
			o.state.Clients = map[string]*oauthClient{}
		}
		if o.state.Tokens == nil {
			o.state.Tokens = map[string]*oauthToken{}
		}
	}
	return o, nil
}

func (o *OAuth) saveLocked() {
	now := time.Now()
	for h, t := range o.state.Tokens {
		if now.After(t.Expires) {
			delete(o.state.Tokens, h)
		}
	}
	data, _ := json.MarshalIndent(o.state, "", "  ")
	tmp := o.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err == nil {
		_ = os.Rename(tmp, o.path)
	}
}

func hashToken(t string) string {
	s := sha256.Sum256([]byte(t))
	return hex.EncodeToString(s[:])
}

func randomToken() string {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	return base64.RawURLEncoding.EncodeToString(b)
}

// VerifyAccess reports whether tok is a valid access token.
func (o *OAuth) VerifyAccess(tok string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	t := o.state.Tokens[hashToken(tok)]
	return t != nil && t.Kind == "access" && time.Now().Before(t.Expires)
}

// ResourceMetadataURL is advertised in WWW-Authenticate on 401 responses.
func (o *OAuth) ResourceMetadataURL() string {
	return o.issuer + "/.well-known/oauth-protected-resource"
}

func (o *OAuth) Register(mux *http.ServeMux) {
	mux.HandleFunc("GET /.well-known/oauth-protected-resource", o.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-protected-resource/", o.protectedResource)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server", o.serverMetadata)
	mux.HandleFunc("GET /.well-known/oauth-authorization-server/", o.serverMetadata)
	mux.HandleFunc("GET /.well-known/openid-configuration", o.serverMetadata)
	mux.HandleFunc("POST /register", o.register)
	mux.HandleFunc("GET /authorize", o.authorizeForm)
	mux.HandleFunc("POST /authorize", o.authorizeSubmit)
	mux.HandleFunc("POST /token", o.token)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func oauthError(w http.ResponseWriter, code int, errCode, desc string) {
	writeJSON(w, code, map[string]string{"error": errCode, "error_description": desc})
}

func (o *OAuth) protectedResource(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, 200, map[string]any{
		"resource":                 o.resource,
		"authorization_servers":    []string{o.issuer},
		"bearer_methods_supported": []string{"header"},
		"resource_name":            "remote-mcp",
	})
}

func (o *OAuth) serverMetadata(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	writeJSON(w, 200, map[string]any{
		"issuer":                                o.issuer,
		"authorization_endpoint":                o.issuer + "/authorize",
		"token_endpoint":                        o.issuer + "/token",
		"registration_endpoint":                 o.issuer + "/register",
		"response_types_supported":              []string{"code"},
		"grant_types_supported":                 []string{"authorization_code", "refresh_token"},
		"code_challenge_methods_supported":      []string{"S256"},
		"token_endpoint_auth_methods_supported": []string{"none", "client_secret_post", "client_secret_basic"},
		"scopes_supported":                      []string{"mcp"},
	})
}

func (o *OAuth) register(w http.ResponseWriter, r *http.Request) {
	var req struct {
		ClientName   string   `json:"client_name"`
		RedirectURIs []string `json:"redirect_uris"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&req); err != nil {
		oauthError(w, 400, "invalid_client_metadata", "invalid JSON")
		return
	}
	if len(req.RedirectURIs) == 0 {
		oauthError(w, 400, "invalid_redirect_uri", "redirect_uris required")
		return
	}
	for _, u := range req.RedirectURIs {
		pu, err := url.Parse(u)
		if err != nil || pu.Scheme == "" || pu.Fragment != "" {
			oauthError(w, 400, "invalid_redirect_uri", "bad redirect uri "+u)
			return
		}
		// Only https, or http to localhost (for local MCP clients).
		if pu.Scheme != "https" && !(pu.Scheme == "http" && (pu.Hostname() == "localhost" || pu.Hostname() == "127.0.0.1")) {
			oauthError(w, 400, "invalid_redirect_uri", "redirect uri must be https or http://localhost")
			return
		}
	}
	c := &oauthClient{ID: newCaptureID(), Name: req.ClientName, RedirectURIs: req.RedirectURIs, Created: time.Now().UTC()}
	o.mu.Lock()
	if len(o.state.Clients) > 200 { // forget the oldest registrations
		var oldest *oauthClient
		for _, cl := range o.state.Clients {
			if oldest == nil || cl.Created.Before(oldest.Created) {
				oldest = cl
			}
		}
		delete(o.state.Clients, oldest.ID)
	}
	o.state.Clients[c.ID] = c
	o.saveLocked()
	o.mu.Unlock()
	o.log.Info("oauth client registered", "client", c.Name, "redirects", c.RedirectURIs)
	writeJSON(w, 201, map[string]any{
		"client_id":                  c.ID,
		"client_id_issued_at":        c.Created.Unix(),
		"client_name":                c.Name,
		"redirect_uris":              c.RedirectURIs,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
	})
}

var authorizeTmpl = template.Must(template.New("a").Parse(`<!doctype html>
<html><head><meta charset="utf-8"><meta name="viewport" content="width=device-width,initial-scale=1">
<title>remote-mcp – authorize</title>
<style>
body{font-family:system-ui,sans-serif;background:#f4f4f5;display:flex;min-height:100vh;align-items:center;justify-content:center;margin:0}
form{background:#fff;padding:28px 32px;border-radius:12px;box-shadow:0 2px 12px #0001;max-width:380px;width:100%}
h1{font-size:19px;margin:0 0 6px}p{color:#555;font-size:14px;line-height:1.45}
input[type=password]{width:100%;box-sizing:border-box;padding:10px;font-size:15px;border:1px solid #ccc;border-radius:8px}
button{margin-top:14px;width:100%;padding:10px;font-size:15px;border:0;border-radius:8px;background:#111;color:#fff;cursor:pointer}
.err{color:#b00020}
</style></head><body>
<form method="post" action="/authorize">
<h1>Allow remote control?</h1>
<p><b>{{.ClientName}}</b> wants to connect to this relay. It will be able to see and control every machine connected to it.</p>
{{if .Error}}<p class="err">{{.Error}}</p>{{end}}
<input type="password" name="password" placeholder="Admin password" autofocus required>
{{range $k, $v := .Hidden}}<input type="hidden" name="{{$k}}" value="{{$v}}">{{end}}
<button type="submit">Allow</button>
</form></body></html>`))

func (o *OAuth) validateAuthRequest(v url.Values) (*oauthClient, string) {
	if v.Get("response_type") != "code" {
		return nil, "response_type must be code"
	}
	if v.Get("code_challenge") == "" || v.Get("code_challenge_method") != "S256" {
		return nil, "PKCE with S256 is required"
	}
	o.mu.Lock()
	c := o.state.Clients[v.Get("client_id")]
	o.mu.Unlock()
	if c == nil {
		return nil, "unknown client_id (remove and re-add the connector)"
	}
	ru := v.Get("redirect_uri")
	if ru == "" && len(c.RedirectURIs) == 1 {
		ru = c.RedirectURIs[0]
	}
	for _, allowed := range c.RedirectURIs {
		if allowed == ru {
			return c, ""
		}
	}
	return nil, "redirect_uri is not registered for this client"
}

var authParams = []string{"response_type", "client_id", "redirect_uri", "code_challenge", "code_challenge_method", "state", "scope", "resource"}

func (o *OAuth) renderForm(w http.ResponseWriter, v url.Values, c *oauthClient, msg string) {
	hidden := map[string]string{}
	for _, k := range authParams {
		if v.Get(k) != "" {
			hidden[k] = v.Get(k)
		}
	}
	name := c.Name
	if name == "" {
		name = "An MCP client"
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("X-Frame-Options", "DENY")
	w.Header().Set("Cache-Control", "no-store")
	_ = authorizeTmpl.Execute(w, map[string]any{"ClientName": name, "Hidden": hidden, "Error": msg})
}

func (o *OAuth) authorizeForm(w http.ResponseWriter, r *http.Request) {
	v := r.URL.Query()
	c, msg := o.validateAuthRequest(v)
	if c == nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	o.renderForm(w, v, c, "")
}

// tooManyFailures implements a simple global lockout: 10 wrong passwords in
// 15 minutes blocks further attempts until the window passes.
func (o *OAuth) tooManyFailures() bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	cut := time.Now().Add(-15 * time.Minute)
	kept := o.fails[:0]
	for _, t := range o.fails {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	o.fails = kept
	return len(o.fails) >= 10
}

func (o *OAuth) authorizeSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", 400)
		return
	}
	v := r.PostForm
	c, msg := o.validateAuthRequest(v)
	if c == nil {
		http.Error(w, msg, http.StatusBadRequest)
		return
	}
	if o.tooManyFailures() {
		o.renderForm(w, v, c, "Too many wrong passwords. Try again in 15 minutes.")
		return
	}
	if subtle.ConstantTimeCompare([]byte(v.Get("password")), []byte(o.password)) != 1 {
		o.mu.Lock()
		o.fails = append(o.fails, time.Now())
		o.mu.Unlock()
		o.log.Warn("oauth wrong password", "remote", clientIP(r))
		time.Sleep(time.Second)
		o.renderForm(w, v, c, "Wrong password.")
		return
	}
	ru := v.Get("redirect_uri")
	if ru == "" {
		ru = c.RedirectURIs[0]
	}
	code := randomToken()
	o.mu.Lock()
	for k, ac := range o.codes {
		if time.Now().After(ac.expires) {
			delete(o.codes, k)
		}
	}
	o.codes[code] = authCode{clientID: c.ID, redirectURI: ru, challenge: v.Get("code_challenge"), expires: time.Now().Add(5 * time.Minute)}
	o.mu.Unlock()
	o.log.Info("oauth authorized", "client", c.Name, "remote", clientIP(r))

	target, _ := url.Parse(ru)
	q := target.Query()
	q.Set("code", code)
	if st := v.Get("state"); st != "" {
		q.Set("state", st)
	}
	q.Set("iss", o.issuer)
	target.RawQuery = q.Encode()
	http.Redirect(w, r, target.String(), http.StatusFound)
}

func (o *OAuth) issueLocked(clientID string) map[string]any {
	access, refresh := randomToken(), randomToken()
	now := time.Now()
	o.state.Tokens[hashToken(access)] = &oauthToken{Kind: "access", ClientID: clientID, Expires: now.Add(accessTTL)}
	o.state.Tokens[hashToken(refresh)] = &oauthToken{Kind: "refresh", ClientID: clientID, Expires: now.Add(refreshTTL)}
	o.saveLocked()
	return map[string]any{
		"access_token":  access,
		"token_type":    "Bearer",
		"expires_in":    int(accessTTL.Seconds()),
		"refresh_token": refresh,
		"scope":         "mcp",
	}
}

func (o *OAuth) token(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Access-Control-Allow-Origin", "*")
	if err := r.ParseForm(); err != nil {
		oauthError(w, 400, "invalid_request", "bad form")
		return
	}
	v := r.PostForm
	clientID := v.Get("client_id")
	if clientID == "" {
		clientID, _, _ = r.BasicAuth()
	}
	switch v.Get("grant_type") {
	case "authorization_code":
		o.mu.Lock()
		ac, ok := o.codes[v.Get("code")]
		delete(o.codes, v.Get("code")) // single use
		o.mu.Unlock()
		switch {
		case !ok || time.Now().After(ac.expires):
			oauthError(w, 400, "invalid_grant", "code is invalid or expired")
			return
		case ac.clientID != clientID:
			oauthError(w, 400, "invalid_grant", "code was issued to another client")
			return
		case v.Get("redirect_uri") != "" && v.Get("redirect_uri") != ac.redirectURI:
			oauthError(w, 400, "invalid_grant", "redirect_uri mismatch")
			return
		}
		sum := sha256.Sum256([]byte(v.Get("code_verifier")))
		if base64.RawURLEncoding.EncodeToString(sum[:]) != ac.challenge {
			oauthError(w, 400, "invalid_grant", "PKCE verification failed")
			return
		}
		o.mu.Lock()
		resp := o.issueLocked(clientID)
		o.mu.Unlock()
		writeJSON(w, 200, resp)

	case "refresh_token":
		h := hashToken(v.Get("refresh_token"))
		o.mu.Lock()
		t := o.state.Tokens[h]
		if t == nil || t.Kind != "refresh" || time.Now().After(t.Expires) || (clientID != "" && t.ClientID != clientID) {
			o.mu.Unlock()
			oauthError(w, 400, "invalid_grant", "refresh token is invalid or expired")
			return
		}
		delete(o.state.Tokens, h) // rotate
		resp := o.issueLocked(t.ClientID)
		o.mu.Unlock()
		writeJSON(w, 200, resp)

	default:
		oauthError(w, 400, "unsupported_grant_type", "use authorization_code or refresh_token")
	}
}
