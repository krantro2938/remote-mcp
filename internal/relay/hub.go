package relay

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

// AgentConn is one connected agent.
type AgentConn struct {
	Name      string
	Hello     proto.Hello
	Connected time.Time
	Remote    string

	conn    *websocket.Conn
	wmu     sync.Mutex
	next    atomic.Uint64
	mu      sync.Mutex
	pending map[uint64]chan proto.Response
	closed  chan struct{}
}

var ErrAgentGone = errors.New("agent disconnected")

// Call sends a request to the agent and waits for the response.
func (a *AgentConn) Call(ctx context.Context, method string, params, out any, timeout time.Duration) error {
	raw, err := json.Marshal(params)
	if err != nil {
		return err
	}
	id := a.next.Add(1)
	ch := make(chan proto.Response, 1)
	a.mu.Lock()
	a.pending[id] = ch
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		delete(a.pending, id)
		a.mu.Unlock()
	}()

	data, _ := json.Marshal(proto.Envelope{Type: "req", Req: &proto.Request{ID: id, Method: method, Params: raw}})
	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	a.wmu.Lock()
	err = a.conn.Write(ctx, websocket.MessageText, data)
	a.wmu.Unlock()
	if err != nil {
		return fmt.Errorf("send to agent %q: %w", a.Name, err)
	}
	select {
	case resp := <-ch:
		if resp.Error != "" {
			return errors.New(resp.Error)
		}
		if out != nil && len(resp.Result) > 0 {
			return json.Unmarshal(resp.Result, out)
		}
		return nil
	case <-a.closed:
		return fmt.Errorf("%w while waiting for %s", ErrAgentGone, method)
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			return fmt.Errorf("agent %q did not answer %s within %s", a.Name, method, timeout)
		}
		return ctx.Err()
	}
}

// Hub tracks connected agents.
type Hub struct {
	log    *slog.Logger
	tokens map[string]string // token -> machine name

	mu     sync.Mutex
	agents map[string]*AgentConn
}

func NewHub(tokens map[string]string, log *slog.Logger) *Hub {
	return &Hub{log: log, tokens: tokens, agents: map[string]*AgentConn{}}
}

func (h *Hub) authenticate(r *http.Request) (string, bool) {
	tok, ok := strings.CutPrefix(r.Header.Get("Authorization"), "Bearer ")
	if !ok || tok == "" {
		return "", false
	}
	name, found := "", false
	for t, n := range h.tokens { // compare every token, constant time each
		if subtle.ConstantTimeCompare([]byte(t), []byte(tok)) == 1 {
			name, found = n, true
		}
	}
	return name, found
}

// ServeHTTP accepts agent WebSocket connections at /agent.
func (h *Hub) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	name, ok := h.authenticate(r)
	if !ok {
		h.log.Warn("agent auth failed", "remote", clientIP(r))
		time.Sleep(time.Second)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{CompressionMode: websocket.CompressionDisabled})
	if err != nil {
		return
	}
	conn.SetReadLimit(256 << 20)
	defer conn.CloseNow()

	ctx := r.Context()
	helloCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	_, data, err := conn.Read(helloCtx)
	cancel()
	if err != nil {
		return
	}
	var env proto.Envelope
	if json.Unmarshal(data, &env) != nil || env.Type != "hello" || env.Hello == nil {
		conn.Close(websocket.StatusPolicyViolation, "expected hello")
		return
	}

	a := &AgentConn{
		Name: name, Hello: *env.Hello, Connected: time.Now(), Remote: clientIP(r),
		conn: conn, pending: map[uint64]chan proto.Response{}, closed: make(chan struct{}),
	}
	h.mu.Lock()
	if old := h.agents[name]; old != nil {
		old.conn.Close(websocket.StatusGoingAway, "replaced by a new connection")
	}
	h.agents[name] = a
	h.mu.Unlock()
	h.log.Info("agent connected", "machine", name, "host", a.Hello.Hostname, "user", a.Hello.User,
		"backend", a.Hello.Backend, "version", a.Hello.Version, "remote", a.Remote)

	defer func() {
		close(a.closed)
		h.mu.Lock()
		if h.agents[name] == a {
			delete(h.agents, name)
		}
		h.mu.Unlock()
		h.log.Info("agent disconnected", "machine", name)
	}()

	// Keepalive: proxies and NATs drop idle connections.
	go func() {
		t := time.NewTicker(20 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-a.closed:
				return
			case <-t.C:
				pctx, cancel := context.WithTimeout(ctx, 15*time.Second)
				err := conn.Ping(pctx)
				cancel()
				if err != nil {
					conn.Close(websocket.StatusGoingAway, "ping timeout")
					return
				}
			}
		}
	}()

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return
		}
		var env proto.Envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case "resp":
			if env.Resp == nil {
				continue
			}
			a.mu.Lock()
			ch := a.pending[env.Resp.ID]
			a.mu.Unlock()
			if ch != nil {
				ch <- *env.Resp
			}
		case "hello":
			if env.Hello != nil {
				h.mu.Lock()
				a.Hello = *env.Hello
				h.mu.Unlock()
			}
		}
	}
}

// Get returns the named agent. With an empty name it returns the only
// connected agent, if exactly one is connected.
func (h *Hub) Get(name string) (*AgentConn, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if name != "" {
		if a := h.agents[name]; a != nil {
			return a, nil
		}
		for n, a := range h.agents { // be forgiving about case
			if strings.EqualFold(n, name) {
				return a, nil
			}
		}
		return nil, fmt.Errorf("machine %q is not connected (connected: %s)", name, h.namesLocked())
	}
	switch len(h.agents) {
	case 0:
		return nil, errors.New("no machine is connected. Start agent.exe on the target computer and check that it says \"connected\"")
	case 1:
		for _, a := range h.agents {
			return a, nil
		}
	}
	return nil, fmt.Errorf("several machines are connected (%s); pass the machine parameter", h.namesLocked())
}

func (h *Hub) namesLocked() string {
	if len(h.agents) == 0 {
		return "none"
	}
	names := make([]string, 0, len(h.agents))
	for n := range h.agents {
		names = append(names, n)
	}
	sort.Strings(names)
	return strings.Join(names, ", ")
}

func (h *Hub) List() []*AgentConn {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]*AgentConn, 0, len(h.agents))
	for _, a := range h.agents {
		out = append(out, a)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

func clientIP(r *http.Request) string {
	if xf := r.Header.Get("X-Forwarded-For"); xf != "" {
		ip, _, _ := strings.Cut(xf, ",")
		return strings.TrimSpace(ip)
	}
	return r.RemoteAddr
}
