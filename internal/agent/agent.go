// Package agent implements the remote-control agent that runs on the target
// machine. It dials out to the relay over WebSocket, so the target needs no
// open ports, and executes requests coming from the relay.
package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	"image/png"
	"log"
	"net/http"
	"os"
	"os/user"
	"runtime"
	"strings"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

// Backend is the platform-specific part: screen, input and windows.
type Backend interface {
	Name() string
	Displays() (screen proto.Rect, monitors []proto.Monitor, err error)
	// Capture returns the image and the absolute screen position of its top-left pixel.
	Capture(p proto.ScreenshotParams) (img *image.RGBA, origin proto.Point, source, title string, handle uint64, err error)
	Input(p proto.InputParams) (proto.Point, error)
	Windows() ([]proto.Window, error)
	Window(p proto.WindowParams) (*proto.Window, error)
}

type Config struct {
	Server string // wss://host/agent
	Token  string
	Name   string
	// ReadOnly disables input, window changes, run and write_file.
	ReadOnly bool
}

type Agent struct {
	cfg     Config
	backend Backend
	jobs    *jobTable

	wmu  sync.Mutex
	conn *websocket.Conn
}

func New(cfg Config, b Backend) *Agent {
	return &Agent{cfg: cfg, backend: b, jobs: newJobTable()}
}

// NormalizeServer turns "https://host", "host" or "wss://host" into "wss://host/agent".
func NormalizeServer(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "/")
	switch {
	case strings.HasPrefix(s, "https://"):
		s = "wss://" + strings.TrimPrefix(s, "https://")
	case strings.HasPrefix(s, "http://"):
		s = "ws://" + strings.TrimPrefix(s, "http://")
	case strings.HasPrefix(s, "wss://"), strings.HasPrefix(s, "ws://"):
	default:
		s = "wss://" + s
	}
	if !strings.HasSuffix(s, "/agent") {
		s += "/agent"
	}
	return s
}

// Run connects to the relay and serves requests until ctx is cancelled,
// reconnecting with backoff whenever the connection drops.
func (a *Agent) Run(ctx context.Context) error {
	backoff := time.Second
	for {
		start := time.Now()
		err := a.session(ctx)
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Since(start) > time.Minute {
			backoff = time.Second
		}
		log.Printf("disconnected: %v (reconnecting in %s)", err, backoff)
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff *= 2
		if backoff > 30*time.Second {
			backoff = 30 * time.Second
		}
	}
}

func (a *Agent) hello() proto.Hello {
	h := proto.Hello{
		Name:    a.cfg.Name,
		OS:      runtime.GOOS + "/" + runtime.GOARCH,
		Version: proto.Version,
		Backend: a.backend.Name(),
	}
	h.Hostname, _ = os.Hostname()
	if u, err := user.Current(); err == nil {
		h.User = u.Username
	}
	if scr, mons, err := a.backend.Displays(); err == nil {
		h.Screen, h.Monitors = scr, mons
	}
	return h
}

func (a *Agent) session(ctx context.Context) error {
	dialCtx, cancel := context.WithTimeout(ctx, 20*time.Second)
	defer cancel()
	hdr := http.Header{}
	hdr.Set("Authorization", "Bearer "+a.cfg.Token)
	conn, resp, err := websocket.Dial(dialCtx, a.cfg.Server, &websocket.DialOptions{HTTPHeader: hdr})
	if err != nil {
		if resp != nil && resp.StatusCode == http.StatusUnauthorized {
			return fmt.Errorf("relay rejected the agent token (401)")
		}
		return err
	}
	conn.SetReadLimit(256 << 20)
	defer conn.CloseNow()

	a.wmu.Lock()
	a.conn = conn
	a.wmu.Unlock()

	h := a.hello()
	if err := a.send(ctx, proto.Envelope{Type: "hello", Hello: &h}); err != nil {
		return err
	}
	log.Printf("connected to %s as %q (%dx%d, %d monitor(s))", a.cfg.Server, a.cfg.Name, h.Screen.W, h.Screen.H, len(h.Monitors))

	for {
		_, data, err := conn.Read(ctx)
		if err != nil {
			return err
		}
		var env proto.Envelope
		if err := json.Unmarshal(data, &env); err != nil || env.Type != "req" || env.Req == nil {
			continue
		}
		go a.handle(ctx, *env.Req)
	}
}

func (a *Agent) send(ctx context.Context, env proto.Envelope) error {
	data, err := json.Marshal(env)
	if err != nil {
		return err
	}
	a.wmu.Lock()
	defer a.wmu.Unlock()
	if a.conn == nil {
		return errors.New("not connected")
	}
	wctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	return a.conn.Write(wctx, websocket.MessageText, data)
}

func (a *Agent) handle(ctx context.Context, req proto.Request) {
	start := time.Now()
	result, err := a.dispatch(ctx, req)
	resp := proto.Response{ID: req.ID}
	if err != nil {
		resp.Error = err.Error()
		log.Printf("%-11s %s -> error: %v", req.Method, summarize(req), err)
	} else {
		resp.Result, err = json.Marshal(result)
		if err != nil {
			resp.Error = "marshal result: " + err.Error()
		}
		log.Printf("%-11s %s (%s)", req.Method, summarize(req), time.Since(start).Round(time.Millisecond))
	}
	if err := a.send(ctx, proto.Envelope{Type: "resp", Resp: &resp}); err != nil {
		log.Printf("send response: %v", err)
	}
}

// summarize produces a short, human-readable log line so the person sitting at
// the machine can see what is being done.
func summarize(req proto.Request) string {
	s := string(req.Params)
	if len(s) > 120 {
		s = s[:120] + "…"
	}
	return s
}

func decode[T any](raw json.RawMessage) (T, error) {
	var v T
	if len(raw) == 0 {
		return v, nil
	}
	err := json.Unmarshal(raw, &v)
	return v, err
}

var errReadOnly = errors.New("agent is running in read-only mode")

func (a *Agent) dispatch(ctx context.Context, req proto.Request) (any, error) {
	switch req.Method {
	case proto.MethodInfo:
		return a.hello(), nil

	case proto.MethodScreenshot:
		p, err := decode[proto.ScreenshotParams](req.Params)
		if err != nil {
			return nil, err
		}
		if p.DelayMs > 0 {
			time.Sleep(time.Duration(min(p.DelayMs, 30000)) * time.Millisecond)
		}
		img, origin, source, title, handle, err := a.backend.Capture(p)
		if err != nil {
			return nil, err
		}
		if isBlack(img) {
			source += ":black"
		}
		var buf bytes.Buffer
		enc := png.Encoder{CompressionLevel: png.BestSpeed}
		if err := enc.Encode(&buf, img); err != nil {
			return nil, err
		}
		b := img.Bounds()
		return proto.ScreenshotResult{
			PNG: buf.Bytes(), Origin: origin, Width: b.Dx(), Height: b.Dy(),
			Source: source, Title: title, Handle: handle,
		}, nil

	case proto.MethodInput:
		if a.cfg.ReadOnly {
			return nil, errReadOnly
		}
		p, err := decode[proto.InputParams](req.Params)
		if err != nil {
			return nil, err
		}
		cur, err := a.backend.Input(p)
		if err != nil {
			return nil, err
		}
		return proto.InputResult{Cursor: cur}, nil

	case proto.MethodWindows:
		ws, err := a.backend.Windows()
		if err != nil {
			return nil, err
		}
		return proto.WindowsResult{Windows: ws}, nil

	case proto.MethodWindow:
		p, err := decode[proto.WindowParams](req.Params)
		if err != nil {
			return nil, err
		}
		if a.cfg.ReadOnly && p.Action != "" && p.Action != "focus" {
			return nil, errReadOnly
		}
		return a.backend.Window(p)

	case proto.MethodRun:
		if a.cfg.ReadOnly {
			return nil, errReadOnly
		}
		p, err := decode[proto.RunParams](req.Params)
		if err != nil {
			return nil, err
		}
		return a.jobs.run(ctx, p)

	case proto.MethodJob:
		p, err := decode[proto.JobParams](req.Params)
		if err != nil {
			return nil, err
		}
		if p.Kill && a.cfg.ReadOnly {
			return nil, errReadOnly
		}
		return a.jobs.status(p)

	case proto.MethodReadFile:
		p, err := decode[proto.ReadFileParams](req.Params)
		if err != nil {
			return nil, err
		}
		return readFile(p)

	case proto.MethodWriteFile:
		if a.cfg.ReadOnly {
			return nil, errReadOnly
		}
		p, err := decode[proto.WriteFileParams](req.Params)
		if err != nil {
			return nil, err
		}
		return writeFile(p)
	}
	return nil, fmt.Errorf("unknown method %q", req.Method)
}

// isBlack reports whether nearly all sampled pixels are pure black, which is
// what Windows returns when the session is locked or an RDP window is minimized.
func isBlack(img *image.RGBA) bool {
	b := img.Bounds()
	if b.Empty() {
		return true
	}
	total, black := 0, 0
	stepX := max(1, b.Dx()/64)
	stepY := max(1, b.Dy()/64)
	for y := b.Min.Y; y < b.Max.Y; y += stepY {
		for x := b.Min.X; x < b.Max.X; x += stepX {
			i := img.PixOffset(x, y)
			if img.Pix[i] < 4 && img.Pix[i+1] < 4 && img.Pix[i+2] < 4 {
				black++
			}
			total++
		}
	}
	return total > 0 && black*100/total >= 99
}
