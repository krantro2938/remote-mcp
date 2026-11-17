package relay_test

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"image/png"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/krantro2938/remote-mcp/internal/agent"
	"github.com/krantro2938/remote-mcp/internal/relay"
)

const (
	agentToken = "agent-token-0123456789abcdef"
	mcpToken   = "mcp-token-0123456789abcdef"
	password   = "correct horse battery"
)

type bearer struct{ tok string }

func (b bearer) RoundTrip(r *http.Request) (*http.Response, error) {
	r = r.Clone(r.Context())
	r.Header.Set("Authorization", "Bearer "+b.tok)
	return http.DefaultTransport.RoundTrip(r)
}

type env struct {
	ts   *httptest.Server
	demo *agent.DemoBackend
	sess *mcp.ClientSession
	ctx  context.Context
	t    *testing.T
}

func setup(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	if os.Getenv("VERBOSE") != "" {
		log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}

	ts := httptest.NewUnstartedServer(nil)
	ts.Start()
	t.Cleanup(ts.Close)
	cfg := relay.Config{
		PublicURL: ts.URL, DataDir: t.TempDir(),
		AgentTokens: map[string]string{agentToken: "laptop"}, AdminPassword: password, MCPToken: mcpToken,
		CaptureTTL: time.Hour, LabeledTTL: time.Hour,
	}
	if err := cfg.Validate(); err != nil {
		t.Fatal(err)
	}
	srv, err := relay.New(cfg, log)
	if err != nil {
		t.Fatal(err)
	}
	ts.Config.Handler = srv.Handler()

	demo := agent.NewDemoBackend()
	ag := agent.New(agent.Config{Server: agent.NormalizeServer(strings.Replace(ts.URL, "http://", "ws://", 1)), Token: agentToken, Name: "laptop"}, demo)
	go ag.Run(ctx)
	deadline := time.Now().Add(5 * time.Second)
	for len(srv.Hub.List()) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("agent did not connect")
		}
		time.Sleep(20 * time.Millisecond)
	}

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "1"}, nil)
	sess, err := client.Connect(ctx, &mcp.StreamableClientTransport{
		Endpoint: ts.URL + "/mcp", HTTPClient: &http.Client{Transport: bearer{mcpToken}},
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { sess.Close() })
	return &env{ts: ts, demo: demo, sess: sess, ctx: ctx, t: t}
}

func (e *env) call(name string, args map[string]any) *mcp.CallToolResult {
	e.t.Helper()
	res, err := e.sess.CallTool(e.ctx, &mcp.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		e.t.Fatalf("%s: %v", name, err)
	}
	return res
}

func textOf(r *mcp.CallToolResult) string {
	var parts []string
	for _, c := range r.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			parts = append(parts, tc.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func imageOf(t *testing.T, r *mcp.CallToolResult) (w, h int) {
	t.Helper()
	for _, c := range r.Content {
		if ic, ok := c.(*mcp.ImageContent); ok {
			img, err := png.Decode(bytes.NewReader(ic.Data))
			if err != nil {
				t.Fatalf("image does not decode: %v", err)
			}
			return img.Bounds().Dx(), img.Bounds().Dy()
		}
	}
	t.Fatalf("no image in result: %s", textOf(r))
	return
}

var idRe = regexp.MustCompile(`capture_id=([0-9a-f]{24})`)

func TestEndToEnd(t *testing.T) {
	e := setup(t)

	// Tool list is complete.
	tools, err := e.sess.ListTools(e.ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(tools.Tools) != 17 {
		t.Errorf("expected 17 tools, got %d", len(tools.Tools))
	}

	out := textOf(e.call("list_machines", nil))
	if !strings.Contains(out, "laptop") || !strings.Contains(out, "1920x1080") {
		t.Fatalf("list_machines: %s", out)
	}

	// Full screenshot is downscaled to 1400 wide.
	r := e.call("screenshot", map[string]any{})
	if r.IsError {
		t.Fatal(textOf(r))
	}
	w, h := imageOf(t, r)
	if w != 1400 || h != 788 {
		t.Fatalf("view size %dx%d, want 1400x788", w, h)
	}
	shotID := idRe.FindStringSubmatch(textOf(r))[1]

	// Clicking at view (700,394) must land at screen (960,540).
	r = e.call("click", map[string]any{"x": 700, "y": 394})
	if r.IsError {
		t.Fatal(textOf(r))
	}
	if ev := e.demo.Events(); ev[len(ev)-1] != "click  960,540 x1" {
		t.Fatalf("click event = %q", ev[len(ev)-1])
	}

	// Window capture: origin (100,80), 1400x850 -> fits in 1400, so scale 1.
	r = e.call("screenshot", map[string]any{"window": "vivado", "label": "fig1_vivado"})
	w, h = imageOf(t, r)
	if w != 1400 || h != 850 || !strings.Contains(textOf(r), `label="fig1_vivado"`) {
		t.Fatalf("window shot %dx%d: %s", w, h, textOf(r))
	}
	// Coordinates now refer to the window image: (10,10) -> screen (110,90).
	r = e.call("click", map[string]any{"x": 10, "y": 10, "double": true, "screenshot": true, "wait_ms": 10})
	if ev := e.demo.Events(); ev[len(ev)-1] != "click  110,90 x2" {
		t.Fatalf("click event = %q", ev[len(ev)-1])
	}
	if w, _ := imageOf(t, r); w != 1400 { // follow-up screenshot re-captures the window
		t.Fatalf("follow-up shot width %d", w)
	}

	// Explicit capture_id still maps through the old full-screen shot.
	e.call("click", map[string]any{"x": 0, "y": 0, "capture_id": shotID, "button": "right"})
	if ev := e.demo.Events(); ev[len(ev)-1] != "click right 0,0 x1" {
		t.Fatalf("click event = %q", ev[len(ev)-1])
	}

	// Zoom into the full-screen shot: view (700,394,140,79) -> 191x108 full-res crop at (960,540)ish.
	r = e.call("zoom", map[string]any{"capture_id": shotID, "x": 700, "y": 394, "w": 140, "h": 79, "label": "zoomed"})
	w, h = imageOf(t, r)
	if w < 185 || w > 195 || h < 104 || h > 112 {
		t.Fatalf("zoom size %dx%d", w, h)
	}
	if !strings.Contains(textOf(r), "screen position (960,540)") && !strings.Contains(textOf(r), "screen position (959,539)") {
		t.Fatalf("zoom origin: %s", textOf(r))
	}
	// Clicking (0,0) in the zoom lands at its origin.
	e.call("click", map[string]any{"x": 0, "y": 0})
	if ev := e.demo.Events(); !strings.HasPrefix(ev[len(ev)-1], "click  96") {
		t.Fatalf("click after zoom = %q", ev[len(ev)-1])
	}

	e.call("drag", map[string]any{"x": 0, "y": 0, "x2": 100, "y2": 50, "capture_id": shotID})
	e.call("type_text", map[string]any{"text": "Привет, Vivado!"})
	e.call("press_keys", map[string]any{"keys": "ctrl+s"})
	e.call("scroll", map[string]any{"amount": -5})
	ev := e.demo.Events()
	want := []string{"drag 0,0 -> 137,69", "type Привет, Vivado!", "key ctrl+s"}
	for i, wnt := range want {
		if got := ev[len(ev)-4+i]; got != wnt {
			t.Errorf("event %d = %q, want %q", i, got, wnt)
		}
	}

	out = textOf(e.call("list_windows", nil))
	if !strings.Contains(out, "vivado.exe") || !strings.Contains(out, "Notepad") {
		t.Fatalf("list_windows: %s", out)
	}
	r = e.call("window", map[string]any{"window": "notepad", "action": "maximize", "screenshot": true})
	if !strings.Contains(textOf(r), "[maximized]") {
		t.Fatalf("window: %s", textOf(r))
	}

	// Commands.
	r = e.call("run_command", map[string]any{"command": "echo hello; echo oops >&2; exit 3", "shell": "sh"})
	if !r.IsError || !strings.Contains(textOf(r), "exit code 3") || !strings.Contains(textOf(r), "hello") || !strings.Contains(textOf(r), "oops") {
		t.Fatalf("run: %s", textOf(r))
	}
	r = e.call("run_command", map[string]any{"command": "sleep 5", "shell": "sh", "timeout_sec": 1})
	if !strings.Contains(textOf(r), "TIMED OUT") {
		t.Fatalf("timeout: %s", textOf(r))
	}
	r = e.call("run_command", map[string]any{"command": "sleep 1; echo done", "shell": "sh", "background": true})
	jobID := regexp.MustCompile(`job ([0-9a-f]{12})`).FindStringSubmatch(textOf(r))
	if jobID == nil {
		t.Fatalf("background: %s", textOf(r))
	}
	time.Sleep(1500 * time.Millisecond)
	if out := textOf(e.call("job_status", map[string]any{"job_id": jobID[1]})); !strings.Contains(out, "exit code 0") || !strings.Contains(out, "done") {
		t.Fatalf("job_status: %s", out)
	}

	// Files.
	dir := t.TempDir()
	p := filepath.Join(dir, "sub", "run.tcl")
	e.call("write_file", map[string]any{"path": p, "content": "open_project uart.xpr\n", "mkdirs": true})
	if out := textOf(e.call("read_file", map[string]any{"path": p})); out != "open_project uart.xpr\n" {
		t.Fatalf("read_file: %q", out)
	}
	if out := textOf(e.call("read_file", map[string]any{"path": filepath.Join(dir, "sub")})); !strings.Contains(out, "run.tcl") {
		t.Fatalf("read dir: %q", out)
	}

	// Errors are tool errors, not protocol errors.
	r = e.call("screenshot", map[string]any{"window": "does-not-exist"})
	if !r.IsError {
		t.Fatal("expected tool error")
	}
	r = e.call("screenshot", map[string]any{"machine": "nope"})
	if !r.IsError || !strings.Contains(textOf(r), "not connected") {
		t.Fatalf("unknown machine: %s", textOf(r))
	}

	// Captures, labels, gallery and zip.
	out = textOf(e.call("label_capture", map[string]any{"capture_id": shotID, "label": "fig0 full screen"}))
	if !strings.Contains(out, shotID) {
		t.Fatal(out)
	}
	out = textOf(e.call("list_captures", map[string]any{"labeled_only": true}))
	for _, l := range []string{"fig0 full screen", "fig1_vivado", "zoomed"} {
		if !strings.Contains(out, l) {
			t.Errorf("list_captures misses %q: %s", l, out)
		}
	}
	resp, _ := http.Get(e.ts.URL + "/c/" + shotID + ".png")
	if resp.StatusCode != 200 || resp.Header.Get("Content-Type") != "image/png" {
		t.Fatalf("capture download: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp, _ = http.Get(e.ts.URL + "/gallery")
	if resp.StatusCode != 401 {
		t.Fatalf("gallery without auth: %d", resp.StatusCode)
	}
	req, _ := http.NewRequest("GET", e.ts.URL+"/gallery.zip", nil)
	req.SetBasicAuth("me", password)
	resp, err = http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("zip: %v %d", err, resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	zr, err := zip.NewReader(bytes.NewReader(data), int64(len(data)))
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, f := range zr.File {
		names = append(names, f.Name)
	}
	if strings.Join(names, ",") != "fig0_full_screen.png,fig1_vivado.png,zoomed.png" {
		t.Fatalf("zip entries: %v", names)
	}
}

func TestAgentAuth(t *testing.T) {
	e := setup(t)
	req, _ := http.NewRequest("GET", e.ts.URL+"/agent", nil)
	req.Header.Set("Authorization", "Bearer wrong-token-0123456789")
	resp, err := http.DefaultClient.Do(req)
	if err != nil || resp.StatusCode != 401 {
		t.Fatalf("expected 401, got %v %v", resp.StatusCode, err)
	}
}

// TestOAuthFlow walks through what claude.ai does when adding a custom connector.
func TestOAuthFlow(t *testing.T) {
	e := setup(t)
	base := e.ts.URL
	noRedirect := &http.Client{CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }}

	// 1. Unauthenticated MCP request -> 401 pointing at resource metadata.
	resp, _ := http.Post(base+"/mcp", "application/json", strings.NewReader(`{}`))
	if resp.StatusCode != 401 || !strings.Contains(resp.Header.Get("WWW-Authenticate"), "/.well-known/oauth-protected-resource") {
		t.Fatalf("401 challenge: %d %q", resp.StatusCode, resp.Header.Get("WWW-Authenticate"))
	}
	getJSON := func(u string) map[string]any {
		resp, err := http.Get(u)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("GET %s: %v %v", u, err, resp.StatusCode)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return m
	}
	prm := getJSON(base + "/.well-known/oauth-protected-resource/mcp")
	if prm["resource"] != base+"/mcp" {
		t.Fatalf("resource metadata: %v", prm)
	}
	asm := getJSON(base + "/.well-known/oauth-authorization-server")
	if asm["registration_endpoint"] != base+"/register" {
		t.Fatalf("as metadata: %v", asm)
	}

	// 2. Dynamic client registration.
	redirect := "https://claude.ai/api/mcp/auth_callback"
	resp, _ = http.Post(base+"/register", "application/json", strings.NewReader(`{"client_name":"Claude","redirect_uris":["`+redirect+`"]}`))
	var reg map[string]any
	json.NewDecoder(resp.Body).Decode(&reg)
	resp.Body.Close()
	clientID, _ := reg["client_id"].(string)
	if resp.StatusCode != 201 || clientID == "" {
		t.Fatalf("register: %d %v", resp.StatusCode, reg)
	}
	if resp, _ := http.Post(base+"/register", "application/json", strings.NewReader(`{"redirect_uris":["http://evil.example/cb"]}`)); resp.StatusCode != 400 {
		t.Fatalf("http redirect accepted: %d", resp.StatusCode)
	}

	// 3. Authorization with PKCE.
	verifier := "verifier-0123456789-0123456789-0123456789-abc"
	sum := sha256.Sum256([]byte(verifier))
	q := url.Values{
		"response_type": {"code"}, "client_id": {clientID}, "redirect_uri": {redirect},
		"code_challenge": {base64.RawURLEncoding.EncodeToString(sum[:])}, "code_challenge_method": {"S256"},
		"state": {"xyz"}, "resource": {base + "/mcp"},
	}
	resp, _ = http.Get(base + "/authorize?" + q.Encode())
	page, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(page), "Allow remote control?") {
		t.Fatalf("authorize page: %d", resp.StatusCode)
	}
	bad := url.Values{}
	for k, v := range q {
		bad[k] = v
	}
	bad.Set("password", "wrong")
	resp, _ = noRedirect.PostForm(base+"/authorize", bad)
	page, _ = io.ReadAll(resp.Body)
	if resp.StatusCode != 200 || !strings.Contains(string(page), "Wrong password") {
		t.Fatalf("wrong password: %d", resp.StatusCode)
	}
	good := url.Values{}
	for k, v := range q {
		good[k] = v
	}
	good.Set("password", password)
	resp, _ = noRedirect.PostForm(base+"/authorize", good)
	loc, _ := url.Parse(resp.Header.Get("Location"))
	if resp.StatusCode != 302 || !strings.HasPrefix(loc.String(), redirect) || loc.Query().Get("state") != "xyz" {
		t.Fatalf("authorize redirect: %d %s", resp.StatusCode, loc)
	}
	code := loc.Query().Get("code")

	// 4. Token exchange; wrong verifier fails, right one succeeds, code is single-use.
	tokenReq := func(v url.Values) (int, map[string]any) {
		resp, err := http.PostForm(base+"/token", v)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		var m map[string]any
		json.NewDecoder(resp.Body).Decode(&m)
		return resp.StatusCode, m
	}
	ex := url.Values{"grant_type": {"authorization_code"}, "code": {code}, "client_id": {clientID}, "redirect_uri": {redirect}, "code_verifier": {"wrong"}}
	if st, _ := tokenReq(ex); st != 400 {
		t.Fatalf("wrong verifier accepted: %d", st)
	}
	// The failed attempt consumed the code (single use), so authorize again.
	resp, _ = noRedirect.PostForm(base+"/authorize", good)
	loc, _ = url.Parse(resp.Header.Get("Location"))
	ex.Set("code", loc.Query().Get("code"))
	ex.Set("code_verifier", verifier)
	st, tok := tokenReq(ex)
	access, _ := tok["access_token"].(string)
	refresh, _ := tok["refresh_token"].(string)
	if st != 200 || access == "" || refresh == "" {
		t.Fatalf("token: %d %v", st, tok)
	}
	if st, _ := tokenReq(ex); st != 400 {
		t.Fatalf("code reuse accepted: %d", st)
	}

	// 5. The access token works for MCP.
	client := mcp.NewClient(&mcp.Implementation{Name: "oauth-test", Version: "1"}, nil)
	sess, err := client.Connect(e.ctx, &mcp.StreamableClientTransport{Endpoint: base + "/mcp", HTTPClient: &http.Client{Transport: bearer{access}}}, nil)
	if err != nil {
		t.Fatalf("connect with access token: %v", err)
	}
	res, err := sess.CallTool(e.ctx, &mcp.CallToolParams{Name: "list_machines"})
	if err != nil || !strings.Contains(textOf(res), "laptop") {
		t.Fatalf("call with access token: %v %s", err, textOf(res))
	}
	sess.Close()

	// 6. Refresh rotates the refresh token.
	st, tok2 := tokenReq(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}})
	if st != 200 || tok2["access_token"] == access {
		t.Fatalf("refresh: %d %v", st, tok2)
	}
	if st, _ := tokenReq(url.Values{"grant_type": {"refresh_token"}, "refresh_token": {refresh}, "client_id": {clientID}}); st != 400 {
		t.Fatalf("old refresh token still valid: %d", st)
	}
}
