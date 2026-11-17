// Package proto defines the wire protocol between the relay and agents.
//
// The agent dials the relay over WebSocket and sends a Hello envelope first.
// After that the relay sends Request envelopes and the agent answers each with
// a Response envelope carrying the same ID. All payloads are JSON.
package proto

import "encoding/json"

const Version = "0.1.0"

// Envelope is the single message type sent over the WebSocket in both directions.
type Envelope struct {
	Type  string    `json:"type"` // "hello", "req", "resp"
	Hello *Hello    `json:"hello,omitempty"`
	Req   *Request  `json:"req,omitempty"`
	Resp  *Response `json:"resp,omitempty"`
}

type Request struct {
	ID     uint64          `json:"id"`
	Method string          `json:"method"`
	Params json.RawMessage `json:"params,omitempty"`
}

type Response struct {
	ID     uint64          `json:"id"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  string          `json:"error,omitempty"`
}

type Point struct {
	X int `json:"x"`
	Y int `json:"y"`
}

type Rect struct {
	X int `json:"x"`
	Y int `json:"y"`
	W int `json:"w"`
	H int `json:"h"`
}

type Monitor struct {
	Index   int  `json:"index"` // 1-based
	Bounds  Rect `json:"bounds"`
	Primary bool `json:"primary"`
}

// Hello is sent by the agent right after connecting, and returned by MethodInfo.
type Hello struct {
	Name     string    `json:"name"`
	Hostname string    `json:"hostname"`
	User     string    `json:"user"`
	OS       string    `json:"os"`
	Version  string    `json:"version"`
	Backend  string    `json:"backend"` // "windows" or "demo"
	Screen   Rect      `json:"screen"`  // virtual screen (all monitors)
	Monitors []Monitor `json:"monitors"`
}

// Methods understood by the agent.
const (
	MethodInfo       = "info"
	MethodScreenshot = "screenshot"
	MethodInput      = "input"
	MethodWindows    = "windows"
	MethodWindow     = "window"
	MethodRun        = "run"
	MethodJob        = "job"
	MethodReadFile   = "read_file"
	MethodWriteFile  = "write_file"
)

type ScreenshotParams struct {
	// Window selects a window by case-insensitive title substring (or exact handle).
	Window string `json:"window,omitempty"`
	Handle uint64 `json:"handle,omitempty"`
	// Monitor selects a monitor (1-based). 0 with no window means the whole virtual screen.
	Monitor int `json:"monitor,omitempty"`
	// Region, in absolute screen pixels, crops the capture.
	Region  *Rect `json:"region,omitempty"`
	DelayMs int   `json:"delay_ms,omitempty"`
}

type ScreenshotResult struct {
	PNG    []byte `json:"png"`    // full-resolution PNG
	Origin Point  `json:"origin"` // absolute screen position of the image's top-left pixel
	Width  int    `json:"width"`
	Height int    `json:"height"`
	Source string `json:"source"` // "screen", "monitor", "window:printwindow", "window:screen", "region"
	Title  string `json:"title,omitempty"`
	Handle uint64 `json:"handle,omitempty"`
}

type InputParams struct {
	// Action: move, click, down, up, drag, scroll, type, key
	Action string `json:"action"`
	X      int    `json:"x,omitempty"`
	Y      int    `json:"y,omitempty"`
	X2     int    `json:"x2,omitempty"`
	Y2     int    `json:"y2,omitempty"`
	HasXY  bool   `json:"has_xy,omitempty"`
	Button string `json:"button,omitempty"` // left, right, middle
	Clicks int    `json:"clicks,omitempty"` // 1 = single, 2 = double
	Amount int    `json:"amount,omitempty"` // scroll notches, positive = up/right
	Horiz  bool   `json:"horizontal,omitempty"`
	Text   string `json:"text,omitempty"`
	Keys   string `json:"keys,omitempty"` // e.g. "ctrl+shift+s", "enter", "alt+f4"
	Repeat int    `json:"repeat,omitempty"`
}

type InputResult struct {
	Cursor Point `json:"cursor"`
}

type Window struct {
	Handle     uint64 `json:"handle"`
	Title      string `json:"title"`
	PID        uint32 `json:"pid"`
	Process    string `json:"process,omitempty"`
	Rect       Rect   `json:"rect"`
	Minimized  bool   `json:"minimized"`
	Maximized  bool   `json:"maximized"`
	Foreground bool   `json:"foreground"`
}

type WindowsResult struct {
	Windows []Window `json:"windows"`
}

type WindowParams struct {
	Window string `json:"window,omitempty"`
	Handle uint64 `json:"handle,omitempty"`
	// Action: focus (default), maximize, minimize, restore, move, close
	Action string `json:"action,omitempty"`
	Rect   *Rect  `json:"rect,omitempty"` // for move
}

type RunParams struct {
	Command    string `json:"command"`
	Shell      string `json:"shell,omitempty"` // powershell (default on Windows), cmd, sh
	Cwd        string `json:"cwd,omitempty"`
	TimeoutSec int    `json:"timeout_sec,omitempty"`
	Background bool   `json:"background,omitempty"`
}

type RunResult struct {
	JobID      string `json:"job_id,omitempty"`
	Running    bool   `json:"running"`
	ExitCode   int    `json:"exit_code"`
	TimedOut   bool   `json:"timed_out,omitempty"`
	Output     string `json:"output"` // combined stdout+stderr, tail-truncated
	Truncated  bool   `json:"truncated,omitempty"`
	DurationMs int64  `json:"duration_ms"`
}

type JobParams struct {
	JobID string `json:"job_id"`
	Kill  bool   `json:"kill,omitempty"`
}

type ReadFileParams struct {
	Path     string `json:"path"`
	MaxBytes int    `json:"max_bytes,omitempty"`
	Offset   int64  `json:"offset,omitempty"`
}

type ReadFileResult struct {
	Content   []byte `json:"content"`
	Size      int64  `json:"size"`
	Truncated bool   `json:"truncated"`
	IsDir     bool   `json:"is_dir,omitempty"`
	Listing   string `json:"listing,omitempty"`
}

type WriteFileParams struct {
	Path    string `json:"path"`
	Content []byte `json:"content"`
	Append  bool   `json:"append,omitempty"`
	Mkdirs  bool   `json:"mkdirs,omitempty"`
}

type WriteFileResult struct {
	Size int64 `json:"size"`
}
