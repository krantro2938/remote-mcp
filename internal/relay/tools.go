package relay

import (
	"context"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/krantro2938/remote-mcp/internal/proto"
)

const instructions = `Remote control of computers that run the remote-mcp agent (usually Windows).

Workflow:
1. list_machines to see what is connected (pass "machine" when more than one is).
2. screenshot (optionally window="Vivado" to capture just that window, even if covered) to see the screen.
3. Coordinates for click/drag/scroll/move_mouse/zoom are pixels in the LAST IMAGE YOU WERE SHOWN for that machine
   (or pass capture_id to use another image). The relay converts them to real screen pixels, so never rescale yourself.
4. Set screenshot=true on an action to get a fresh screenshot of the same target afterwards.
5. Use zoom to read small text at full resolution or to crop a figure.
6. For report figures pass label (e.g. "fig2_rtl_schematic") to screenshot/zoom or use label_capture.
   The user downloads labeled captures from the gallery (list_captures prints the links).

Prefer run_command (PowerShell) for anything scriptable, e.g. Vivado in batch/Tcl mode
(vivado -mode batch -source script.tcl), and use the GUI mainly for the screenshots themselves.
Commands longer than ~60s should use background=true and job_status.
A completely black screenshot means the Windows session is locked or its RDP window is minimized.`

type Tools struct {
	hub       *Hub
	caps      *CaptureStore
	publicURL string
	log       *slog.Logger
}

func NewTools(hub *Hub, caps *CaptureStore, publicURL string, log *slog.Logger) *Tools {
	return &Tools{hub: hub, caps: caps, publicURL: strings.TrimSuffix(publicURL, "/"), log: log}
}

func text(format string, args ...any) *mcp.CallToolResult {
	return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(format, args...)}}}
}

func ptr[T any](v T) *T { return &v }

var readOnly = &mcp.ToolAnnotations{ReadOnlyHint: true, OpenWorldHint: ptr(false)}
var acting = &mcp.ToolAnnotations{DestructiveHint: ptr(true), OpenWorldHint: ptr(false)}

// Server builds the MCP server with all tools.
func (t *Tools) Server() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: "remote-mcp", Title: "Remote control", Version: proto.Version},
		&mcp.ServerOptions{Instructions: instructions})

	mcp.AddTool(s, &mcp.Tool{Name: "list_machines", Annotations: readOnly,
		Description: "List connected machines with their screen size, monitors, user and agent version."}, t.listMachines)
	mcp.AddTool(s, &mcp.Tool{Name: "screenshot", Annotations: readOnly,
		Description: "Take a screenshot of the whole screen, one monitor, or one window (window capture works even when the window is covered). Returns a downscaled image; the full-resolution original is stored on the relay. Coordinates in later actions refer to this image."}, t.screenshot)
	mcp.AddTool(s, &mcp.Tool{Name: "zoom", Annotations: readOnly,
		Description: "Crop a rectangle out of a previous capture at full resolution, to read small text or to make a cropped figure for a report. x/y/w/h are in the coordinates of that capture's image. Creates a new capture; coordinates in later actions refer to the zoomed image."}, t.zoom)
	mcp.AddTool(s, &mcp.Tool{Name: "click", Annotations: acting,
		Description: "Click at x,y (image coordinates of the last screenshot/zoom, unless screen_coords=true)."}, t.click)
	mcp.AddTool(s, &mcp.Tool{Name: "drag", Annotations: acting,
		Description: "Press the mouse at x,y, move to x2,y2 and release (image coordinates of the last screenshot/zoom)."}, t.drag)
	mcp.AddTool(s, &mcp.Tool{Name: "scroll", Annotations: acting,
		Description: "Scroll the mouse wheel at x,y (or where the cursor is). amount>0 scrolls up/right, <0 down/left; one unit is one wheel notch."}, t.scroll)
	mcp.AddTool(s, &mcp.Tool{Name: "move_mouse", Annotations: acting,
		Description: "Move the mouse pointer to x,y without clicking (e.g. to show a tooltip)."}, t.moveMouse)
	mcp.AddTool(s, &mcp.Tool{Name: "type_text", Annotations: acting,
		Description: "Type text into the focused control. Any Unicode (incl. Cyrillic) works; \\n presses Enter, \\t presses Tab."}, t.typeText)
	mcp.AddTool(s, &mcp.Tool{Name: "press_keys", Annotations: acting,
		Description: "Press a key or key combination, e.g. \"enter\", \"ctrl+s\", \"ctrl+shift+esc\", \"alt+f4\", \"f6\", \"down\". Use repeat to press it several times."}, t.pressKeys)
	mcp.AddTool(s, &mcp.Tool{Name: "list_windows", Annotations: readOnly,
		Description: "List visible top-level windows with title, process, position and state (top-most first)."}, t.listWindows)
	mcp.AddTool(s, &mcp.Tool{Name: "window", Annotations: acting,
		Description: "Bring a window to the front or change it: action focus (default), maximize, minimize, restore, move (with x,y,w,h in screen pixels) or close. Select it by title substring / process name or handle."}, t.window)
	mcp.AddTool(s, &mcp.Tool{Name: "run_command", Annotations: acting,
		Description: "Run a command on the machine (PowerShell by default on Windows; shell can be cmd). Returns exit code and combined output (last 64 KiB). Use background=true for long commands (e.g. Vivado synthesis) and poll with job_status."}, t.runCommand)
	mcp.AddTool(s, &mcp.Tool{Name: "job_status", Annotations: acting,
		Description: "Get the status and latest output of a background command, or kill it."}, t.jobStatus)
	mcp.AddTool(s, &mcp.Tool{Name: "read_file", Annotations: readOnly,
		Description: "Read a text file (or list a directory) on the machine. Images (.png/.jpg) are returned as images."}, t.readFile)
	mcp.AddTool(s, &mcp.Tool{Name: "write_file", Annotations: acting,
		Description: "Write a text file on the machine (e.g. a Tcl script for Vivado). Creates or overwrites unless append=true."}, t.writeFile)
	mcp.AddTool(s, &mcp.Tool{Name: "label_capture", Annotations: &mcp.ToolAnnotations{IdempotentHint: true, OpenWorldHint: ptr(false)},
		Description: "Give a capture a name for the report (e.g. \"fig3_simulation_waveform\"). Labeled captures are kept 30 days and appear in the gallery download."}, t.labelCapture)
	mcp.AddTool(s, &mcp.Tool{Name: "list_captures", Annotations: readOnly,
		Description: "List stored captures with their labels and download links, plus the gallery and zip links for the user."}, t.listCaptures)
	return s
}

// ---------- inputs ----------

type MachineIn struct {
	Machine string `json:"machine,omitempty" jsonschema:"machine name from list_machines; may be omitted when only one is connected"`
}

type ScreenshotIn struct {
	Machine string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Window  string `json:"window,omitempty" jsonschema:"capture only this window: case-insensitive title substring or process name, e.g. Vivado"`
	Handle  uint64 `json:"handle,omitempty" jsonschema:"capture the window with this handle (from list_windows)"`
	Monitor int    `json:"monitor,omitempty" jsonschema:"capture only this monitor (1-based); default is all monitors"`
	Label   string `json:"label,omitempty" jsonschema:"save the capture under this name for the report, e.g. fig1_block_design"`
	MaxSize int    `json:"max_size,omitempty" jsonschema:"longest side of the returned image in pixels (default 1400, max 2560)"`
	DelayMs int    `json:"delay_ms,omitempty" jsonschema:"wait this long before capturing (e.g. for menus/animations)"`
}

type ZoomIn struct {
	Machine   string `json:"machine,omitempty" jsonschema:"machine name (only used to find the latest capture)"`
	CaptureID string `json:"capture_id,omitempty" jsonschema:"capture to zoom into; default is the last image shown for the machine"`
	X         int    `json:"x" jsonschema:"left edge, in that capture's image coordinates"`
	Y         int    `json:"y" jsonschema:"top edge"`
	W         int    `json:"w" jsonschema:"width (0 = whole image)"`
	H         int    `json:"h" jsonschema:"height (0 = whole image)"`
	Label     string `json:"label,omitempty" jsonschema:"save the crop under this name for the report"`
	MaxSize   int    `json:"max_size,omitempty" jsonschema:"longest side of the returned image (default 1400, max 2560)"`
}

type PointIn struct {
	Machine      string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	CaptureID    string `json:"capture_id,omitempty" jsonschema:"image the coordinates refer to; default is the last image shown"`
	ScreenCoords bool   `json:"screen_coords,omitempty" jsonschema:"treat coordinates as absolute screen pixels instead of image pixels"`
	Screenshot   bool   `json:"screenshot,omitempty" jsonschema:"return a fresh screenshot of the same target after the action"`
	WaitMs       int    `json:"wait_ms,omitempty" jsonschema:"delay before that screenshot (default 600)"`
}

type ClickIn struct {
	PointIn
	X      int    `json:"x" jsonschema:"x in image coordinates"`
	Y      int    `json:"y" jsonschema:"y in image coordinates"`
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
	Double bool   `json:"double,omitempty" jsonschema:"double-click"`
}

type DragIn struct {
	PointIn
	X      int    `json:"x" jsonschema:"start x"`
	Y      int    `json:"y" jsonschema:"start y"`
	X2     int    `json:"x2" jsonschema:"end x"`
	Y2     int    `json:"y2" jsonschema:"end y"`
	Button string `json:"button,omitempty" jsonschema:"left (default), right or middle"`
}

type ScrollIn struct {
	PointIn
	X          *int `json:"x,omitempty" jsonschema:"x to scroll at (default: current cursor position)"`
	Y          *int `json:"y,omitempty" jsonschema:"y to scroll at"`
	Amount     int  `json:"amount" jsonschema:"wheel notches; positive = up/right, negative = down/left"`
	Horizontal bool `json:"horizontal,omitempty" jsonschema:"scroll horizontally"`
}

type MoveIn struct {
	PointIn
	X int `json:"x" jsonschema:"x in image coordinates"`
	Y int `json:"y" jsonschema:"y in image coordinates"`
}

type TypeIn struct {
	Machine    string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Text       string `json:"text" jsonschema:"text to type"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a fresh screenshot afterwards"`
	WaitMs     int    `json:"wait_ms,omitempty" jsonschema:"delay before that screenshot (default 600)"`
}

type KeysIn struct {
	Machine    string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Keys       string `json:"keys" jsonschema:"key or combination, e.g. enter, ctrl+s, alt+tab, f5"`
	Repeat     int    `json:"repeat,omitempty" jsonschema:"press it this many times (default 1)"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a fresh screenshot afterwards"`
	WaitMs     int    `json:"wait_ms,omitempty" jsonschema:"delay before that screenshot (default 600)"`
}

type WindowIn struct {
	Machine    string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Window     string `json:"window,omitempty" jsonschema:"title substring or process name"`
	Handle     uint64 `json:"handle,omitempty" jsonschema:"window handle from list_windows"`
	Action     string `json:"action,omitempty" jsonschema:"focus (default), maximize, minimize, restore, move, close"`
	X          int    `json:"x,omitempty" jsonschema:"for move: left, screen pixels"`
	Y          int    `json:"y,omitempty" jsonschema:"for move: top, screen pixels"`
	W          int    `json:"w,omitempty" jsonschema:"for move: width"`
	H          int    `json:"h,omitempty" jsonschema:"for move: height"`
	Screenshot bool   `json:"screenshot,omitempty" jsonschema:"return a screenshot of the window afterwards"`
}

type RunIn struct {
	Machine    string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Command    string `json:"command" jsonschema:"the command or script"`
	Shell      string `json:"shell,omitempty" jsonschema:"powershell (default on Windows), pwsh, cmd, or sh"`
	Cwd        string `json:"cwd,omitempty" jsonschema:"working directory"`
	TimeoutSec int    `json:"timeout_sec,omitempty" jsonschema:"kill after this many seconds (default 120 in the foreground, none in the background)"`
	Background bool   `json:"background,omitempty" jsonschema:"return immediately with a job_id; poll with job_status"`
}

type JobIn struct {
	Machine string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	JobID   string `json:"job_id" jsonschema:"job id from run_command"`
	Kill    bool   `json:"kill,omitempty" jsonschema:"kill the job (and its child processes)"`
}

type ReadFileIn struct {
	Machine  string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Path     string `json:"path" jsonschema:"file or directory path"`
	MaxBytes int    `json:"max_bytes,omitempty" jsonschema:"read at most this many bytes (default 256 KiB)"`
	Offset   int64  `json:"offset,omitempty" jsonschema:"start reading at this byte offset"`
}

type WriteFileIn struct {
	Machine string `json:"machine,omitempty" jsonschema:"machine name; may be omitted when only one is connected"`
	Path    string `json:"path" jsonschema:"file path"`
	Content string `json:"content" jsonschema:"text content (written as UTF-8)"`
	Append  bool   `json:"append,omitempty" jsonschema:"append instead of overwrite"`
	Mkdirs  bool   `json:"mkdirs,omitempty" jsonschema:"create missing parent directories"`
}

type LabelIn struct {
	CaptureID string `json:"capture_id" jsonschema:"capture id"`
	Label     string `json:"label" jsonschema:"name, e.g. fig4_timing_report (empty removes the label)"`
}

type ListCapturesIn struct {
	Machine     string `json:"machine,omitempty" jsonschema:"only this machine"`
	LabeledOnly bool   `json:"labeled_only,omitempty" jsonschema:"only captures that have a label"`
	Limit       int    `json:"limit,omitempty" jsonschema:"max entries (default 30)"`
}

// ---------- handlers ----------

func (t *Tools) listMachines(ctx context.Context, _ *mcp.CallToolRequest, _ MachineIn) (*mcp.CallToolResult, any, error) {
	agents := t.hub.List()
	if len(agents) == 0 {
		return text("No machine is connected. Start agent.exe on the target computer."), nil, nil
	}
	var b strings.Builder
	for _, a := range agents {
		h := a.Hello
		fmt.Fprintf(&b, "- %s: host %s, user %s, %s, agent %s (%s backend), screen %dx%d at (%d,%d), connected %s ago from %s\n",
			a.Name, h.Hostname, h.User, h.OS, h.Version, h.Backend, h.Screen.W, h.Screen.H, h.Screen.X, h.Screen.Y,
			time.Since(a.Connected).Round(time.Second), a.Remote)
		for _, m := range h.Monitors {
			p := ""
			if m.Primary {
				p = " (primary)"
			}
			fmt.Fprintf(&b, "    monitor %d: %dx%d at (%d,%d)%s\n", m.Index, m.Bounds.W, m.Bounds.H, m.Bounds.X, m.Bounds.Y, p)
		}
	}
	return text("%s", b.String()), nil, nil
}

func clampSize(n int) int {
	if n <= 0 {
		return 1400
	}
	return min(max(n, 200), 2560)
}

func (t *Tools) url(c *Capture) string { return t.publicURL + "/c/" + c.ID + ".png" }

func (t *Tools) describe(c *Capture, black bool) string {
	var b strings.Builder
	fmt.Fprintf(&b, "capture_id=%s machine=%s source=%s", c.ID, c.Machine, c.Source)
	if c.Title != "" {
		fmt.Fprintf(&b, " window=%q", c.Title)
	}
	if c.Label != "" {
		fmt.Fprintf(&b, " label=%q", c.Label)
	}
	fmt.Fprintf(&b, "\nThis image is %dx%d; the original is %dx%d at screen position (%d,%d) (scale %.3g). Coordinates you pass to click/drag/scroll/zoom refer to this image.\nFull-resolution original: %s",
		c.ViewW, c.ViewH, c.Width, c.Height, c.Origin.X, c.Origin.Y, c.Scale(), t.url(c))
	if black {
		b.WriteString("\nWARNING: the image is almost completely black. The Windows session is probably locked, or the Remote Desktop window to it is minimized/disconnected. Ask the user to unlock it / keep it visible.")
	}
	return b.String()
}

func (t *Tools) capture(ctx context.Context, a *AgentConn, p proto.ScreenshotParams, label string, maxSize int) (*mcp.CallToolResult, error) {
	var res proto.ScreenshotResult
	if err := a.Call(ctx, proto.MethodScreenshot, p, &res, 60*time.Second); err != nil {
		return nil, err
	}
	black := strings.HasSuffix(res.Source, ":black")
	c := &Capture{Machine: a.Name, Label: label, Origin: res.Origin, Source: strings.TrimSuffix(res.Source, ":black"), Title: res.Title}
	view, mime, err := t.caps.Add(c, res.PNG, clampSize(maxSize))
	if err != nil {
		return nil, err
	}
	t.caps.SetLastParams(a.Name, p)
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: view, MIMEType: mime},
		&mcp.TextContent{Text: t.describe(c, black)},
	}}, nil
}

func (t *Tools) screenshot(ctx context.Context, _ *mcp.CallToolRequest, in ScreenshotIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	p := proto.ScreenshotParams{Window: in.Window, Handle: in.Handle, Monitor: in.Monitor, DelayMs: in.DelayMs}
	res, err := t.capture(ctx, a, p, in.Label, in.MaxSize)
	return res, nil, err
}

func (t *Tools) findCapture(machine, id string) (*Capture, error) {
	if id != "" {
		return t.caps.Get(id)
	}
	a, err := t.hub.Get(machine)
	name := machine
	if err == nil {
		name = a.Name
	}
	c := t.caps.Latest(name)
	if c == nil {
		return nil, fmt.Errorf("no capture yet for machine %q: take a screenshot first", name)
	}
	return c, nil
}

func (t *Tools) zoom(ctx context.Context, _ *mcp.CallToolRequest, in ZoomIn) (*mcp.CallToolResult, any, error) {
	parent, err := t.findCapture(in.Machine, in.CaptureID)
	if err != nil {
		return nil, nil, err
	}
	c, view, mime, err := t.caps.Zoom(parent, in.X, in.Y, in.W, in.H, clampSize(in.MaxSize), in.Label)
	if err != nil {
		return nil, nil, err
	}
	return &mcp.CallToolResult{Content: []mcp.Content{
		&mcp.ImageContent{Data: view, MIMEType: mime},
		&mcp.TextContent{Text: t.describe(c, false)},
	}}, nil, nil
}

// resolve finds the agent and converts image coordinates into screen pixels.
func (t *Tools) resolve(p PointIn) (*AgentConn, func(x, y int) (int, int), error) {
	a, err := t.hub.Get(p.Machine)
	if err != nil {
		return nil, nil, err
	}
	if p.ScreenCoords {
		return a, func(x, y int) (int, int) { return x, y }, nil
	}
	var c *Capture
	if p.CaptureID != "" {
		if c, err = t.caps.Get(p.CaptureID); err != nil {
			return nil, nil, err
		}
		if c.Machine != a.Name {
			return nil, nil, fmt.Errorf("capture %s belongs to machine %q, not %q", c.ID, c.Machine, a.Name)
		}
	} else if c = t.caps.Latest(a.Name); c == nil {
		return nil, nil, fmt.Errorf("take a screenshot of %q first (coordinates refer to the last image), or set screen_coords=true", a.Name)
	}
	return a, c.ToScreen, nil
}

func (t *Tools) input(ctx context.Context, a *AgentConn, p proto.InputParams, shot bool, waitMs int) (*mcp.CallToolResult, error) {
	var res proto.InputResult
	timeout := 30*time.Second + time.Duration(utf8.RuneCountInString(p.Text))*20*time.Millisecond
	if err := a.Call(ctx, proto.MethodInput, p, &res, timeout); err != nil {
		return nil, err
	}
	msg := fmt.Sprintf("done: %s (cursor now at screen %d,%d)", p.Action, res.Cursor.X, res.Cursor.Y)
	if !shot {
		return text("%s", msg), nil
	}
	if waitMs <= 0 {
		waitMs = 600
	}
	sp := t.caps.LastParams(a.Name)
	sp.DelayMs = min(waitMs, 10000)
	r, err := t.capture(ctx, a, sp, "", 0)
	if err != nil {
		return text("%s; follow-up screenshot failed: %v", msg, err), nil
	}
	r.Content = append([]mcp.Content{&mcp.TextContent{Text: msg}}, r.Content...)
	return r, nil
}

func (t *Tools) click(ctx context.Context, _ *mcp.CallToolRequest, in ClickIn) (*mcp.CallToolResult, any, error) {
	a, conv, err := t.resolve(in.PointIn)
	if err != nil {
		return nil, nil, err
	}
	x, y := conv(in.X, in.Y)
	clicks := 1
	if in.Double {
		clicks = 2
	}
	res, err := t.input(ctx, a, proto.InputParams{Action: "click", X: x, Y: y, HasXY: true, Button: in.Button, Clicks: clicks}, in.Screenshot, in.WaitMs)
	return res, nil, err
}

func (t *Tools) drag(ctx context.Context, _ *mcp.CallToolRequest, in DragIn) (*mcp.CallToolResult, any, error) {
	a, conv, err := t.resolve(in.PointIn)
	if err != nil {
		return nil, nil, err
	}
	x, y := conv(in.X, in.Y)
	x2, y2 := conv(in.X2, in.Y2)
	res, err := t.input(ctx, a, proto.InputParams{Action: "drag", X: x, Y: y, X2: x2, Y2: y2, HasXY: true, Button: in.Button}, in.Screenshot, in.WaitMs)
	return res, nil, err
}

func (t *Tools) scroll(ctx context.Context, _ *mcp.CallToolRequest, in ScrollIn) (*mcp.CallToolResult, any, error) {
	p := proto.InputParams{Action: "scroll", Amount: in.Amount, Horiz: in.Horizontal}
	var a *AgentConn
	var err error
	if in.X != nil && in.Y != nil {
		var conv func(int, int) (int, int)
		if a, conv, err = t.resolve(in.PointIn); err != nil {
			return nil, nil, err
		}
		p.X, p.Y = conv(*in.X, *in.Y)
		p.HasXY = true
	} else if a, err = t.hub.Get(in.Machine); err != nil {
		return nil, nil, err
	}
	if p.Amount == 0 {
		p.Amount = -3
	}
	res, err := t.input(ctx, a, p, in.Screenshot, in.WaitMs)
	return res, nil, err
}

func (t *Tools) moveMouse(ctx context.Context, _ *mcp.CallToolRequest, in MoveIn) (*mcp.CallToolResult, any, error) {
	a, conv, err := t.resolve(in.PointIn)
	if err != nil {
		return nil, nil, err
	}
	x, y := conv(in.X, in.Y)
	res, err := t.input(ctx, a, proto.InputParams{Action: "move", X: x, Y: y, HasXY: true}, in.Screenshot, in.WaitMs)
	return res, nil, err
}

func (t *Tools) typeText(ctx context.Context, _ *mcp.CallToolRequest, in TypeIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	res, err := t.input(ctx, a, proto.InputParams{Action: "type", Text: in.Text}, in.Screenshot, in.WaitMs)
	return res, nil, err
}

func (t *Tools) pressKeys(ctx context.Context, _ *mcp.CallToolRequest, in KeysIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	res, err := t.input(ctx, a, proto.InputParams{Action: "key", Keys: in.Keys, Repeat: in.Repeat}, in.Screenshot, in.WaitMs)
	return res, nil, err
}

func fmtWindow(w proto.Window) string {
	state := ""
	switch {
	case w.Minimized:
		state = " [minimized]"
	case w.Maximized:
		state = " [maximized]"
	}
	if w.Foreground {
		state += " [foreground]"
	}
	return fmt.Sprintf("handle=%d %q (%s, pid %d) at (%d,%d) %dx%d%s",
		w.Handle, w.Title, w.Process, w.PID, w.Rect.X, w.Rect.Y, w.Rect.W, w.Rect.H, state)
}

func (t *Tools) listWindows(ctx context.Context, _ *mcp.CallToolRequest, in MachineIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	var res proto.WindowsResult
	if err := a.Call(ctx, proto.MethodWindows, struct{}{}, &res, 30*time.Second); err != nil {
		return nil, nil, err
	}
	if len(res.Windows) == 0 {
		return text("no visible windows"), nil, nil
	}
	lines := make([]string, len(res.Windows))
	for i, w := range res.Windows {
		lines[i] = fmtWindow(w)
	}
	return text("%s", strings.Join(lines, "\n")), nil, nil
}

func (t *Tools) window(ctx context.Context, _ *mcp.CallToolRequest, in WindowIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	p := proto.WindowParams{Window: in.Window, Handle: in.Handle, Action: in.Action}
	if strings.EqualFold(in.Action, "move") {
		p.Rect = &proto.Rect{X: in.X, Y: in.Y, W: in.W, H: in.H}
	}
	var w proto.Window
	if err := a.Call(ctx, proto.MethodWindow, p, &w, 30*time.Second); err != nil {
		return nil, nil, err
	}
	msg := "ok: " + fmtWindow(w)
	if !in.Screenshot || strings.EqualFold(in.Action, "close") || strings.EqualFold(in.Action, "minimize") {
		return text("%s", msg), nil, nil
	}
	r, err := t.capture(ctx, a, proto.ScreenshotParams{Handle: w.Handle, DelayMs: 300}, "", 0)
	if err != nil {
		return text("%s; screenshot failed: %v", msg, err), nil, nil
	}
	r.Content = append([]mcp.Content{&mcp.TextContent{Text: msg}}, r.Content...)
	return r, nil, nil
}

func fmtRun(r proto.RunResult) string {
	var b strings.Builder
	switch {
	case r.Running:
		fmt.Fprintf(&b, "job %s is still running (%s so far)", r.JobID, time.Duration(r.DurationMs)*time.Millisecond)
	case r.TimedOut:
		fmt.Fprintf(&b, "TIMED OUT and killed after %s (exit code %d)", time.Duration(r.DurationMs)*time.Millisecond, r.ExitCode)
	default:
		fmt.Fprintf(&b, "exit code %d (%s)", r.ExitCode, (time.Duration(r.DurationMs) * time.Millisecond).Round(10*time.Millisecond))
	}
	if r.JobID != "" && !r.Running {
		fmt.Fprintf(&b, " [job %s]", r.JobID)
	}
	if r.Truncated {
		b.WriteString("\n(output truncated, showing the last 64 KiB)")
	}
	out := strings.TrimRight(r.Output, "\r\n ")
	if out == "" {
		out = "(no output)"
	}
	b.WriteString("\n" + out)
	return b.String()
}

func (t *Tools) runCommand(ctx context.Context, _ *mcp.CallToolRequest, in RunIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	timeout := 30 * time.Second
	if !in.Background {
		tsec := in.TimeoutSec
		if tsec <= 0 {
			tsec = 120
		}
		timeout = time.Duration(tsec)*time.Second + 30*time.Second
	}
	var res proto.RunResult
	p := proto.RunParams{Command: in.Command, Shell: in.Shell, Cwd: in.Cwd, TimeoutSec: in.TimeoutSec, Background: in.Background}
	if err := a.Call(ctx, proto.MethodRun, p, &res, timeout); err != nil {
		return nil, nil, err
	}
	r := text("%s", fmtRun(res))
	r.IsError = !res.Running && (res.ExitCode != 0 || res.TimedOut)
	if res.Running {
		r = text("started in the background as job %s; poll it with job_status.\n%s", res.JobID, fmtRun(res))
	}
	return r, nil, nil
}

func (t *Tools) jobStatus(ctx context.Context, _ *mcp.CallToolRequest, in JobIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	var res proto.RunResult
	if err := a.Call(ctx, proto.MethodJob, proto.JobParams{JobID: in.JobID, Kill: in.Kill}, &res, 30*time.Second); err != nil {
		return nil, nil, err
	}
	return text("%s", fmtRun(res)), nil, nil
}

var imageExt = map[string]string{".png": "image/png", ".jpg": "image/jpeg", ".jpeg": "image/jpeg", ".gif": "image/gif", ".webp": "image/webp"}

func (t *Tools) readFile(ctx context.Context, _ *mcp.CallToolRequest, in ReadFileIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	p := proto.ReadFileParams{Path: in.Path, MaxBytes: in.MaxBytes, Offset: in.Offset}
	mime := imageExt[strings.ToLower(filepath.Ext(strings.ReplaceAll(in.Path, `\`, "/")))]
	if mime != "" && p.MaxBytes == 0 {
		p.MaxBytes = 10 << 20
	}
	var res proto.ReadFileResult
	if err := a.Call(ctx, proto.MethodReadFile, p, &res, 60*time.Second); err != nil {
		return nil, nil, err
	}
	if res.IsDir {
		return text("directory %s:\n%s", in.Path, res.Listing), nil, nil
	}
	if mime != "" && !res.Truncated {
		return &mcp.CallToolResult{Content: []mcp.Content{
			&mcp.ImageContent{Data: res.Content, MIMEType: mime},
			&mcp.TextContent{Text: fmt.Sprintf("%s (%d bytes)", in.Path, res.Size)},
		}}, nil, nil
	}
	if !utf8.Valid(res.Content) {
		// Try UTF-16 LE (PowerShell's default for Out-File on Windows PowerShell 5).
		if s, ok := decodeUTF16(res.Content); ok {
			res.Content = []byte(s)
		} else {
			return text("%s is a binary file (%d bytes); not shown", in.Path, res.Size), nil, nil
		}
	}
	more := ""
	if res.Truncated {
		more = fmt.Sprintf("\n… (file is %d bytes; read more with offset=%d)", res.Size, in.Offset+int64(len(res.Content)))
	}
	return text("%s%s", res.Content, more), nil, nil
}

func decodeUTF16(b []byte) (string, bool) {
	if len(b) < 2 || len(b)%2 != 0 {
		return "", false
	}
	if b[0] == 0xFF && b[1] == 0xFE {
		b = b[2:]
	} else if !(len(b) >= 2 && b[1] == 0) {
		return "", false
	}
	u := make([]uint16, len(b)/2)
	for i := range u {
		u[i] = uint16(b[2*i]) | uint16(b[2*i+1])<<8
	}
	var sb strings.Builder
	for _, r := range utf16.Decode(u) {
		sb.WriteRune(r)
	}
	return sb.String(), true
}

func (t *Tools) writeFile(ctx context.Context, _ *mcp.CallToolRequest, in WriteFileIn) (*mcp.CallToolResult, any, error) {
	a, err := t.hub.Get(in.Machine)
	if err != nil {
		return nil, nil, err
	}
	var res proto.WriteFileResult
	p := proto.WriteFileParams{Path: in.Path, Content: []byte(in.Content), Append: in.Append, Mkdirs: in.Mkdirs}
	if err := a.Call(ctx, proto.MethodWriteFile, p, &res, 60*time.Second); err != nil {
		return nil, nil, err
	}
	return text("wrote %s (%d bytes on disk)", in.Path, res.Size), nil, nil
}

func (t *Tools) labelCapture(ctx context.Context, _ *mcp.CallToolRequest, in LabelIn) (*mcp.CallToolResult, any, error) {
	c, err := t.caps.SetLabel(in.CaptureID, strings.TrimSpace(in.Label))
	if err != nil {
		return nil, nil, err
	}
	return text("capture %s labeled %q: %s", c.ID, c.Label, t.url(c)), nil, nil
}

func (t *Tools) listCaptures(ctx context.Context, _ *mcp.CallToolRequest, in ListCapturesIn) (*mcp.CallToolResult, any, error) {
	caps := t.caps.List(in.Machine, in.LabeledOnly)
	limit := in.Limit
	if limit <= 0 {
		limit = 30
	}
	var b strings.Builder
	fmt.Fprintf(&b, "Gallery for the user (admin password): %s/gallery\nZip of all labeled captures: %s/gallery.zip\n\n", t.publicURL, t.publicURL)
	if len(caps) == 0 {
		b.WriteString("no captures stored")
	}
	for i, c := range caps {
		if i >= limit {
			fmt.Fprintf(&b, "… %d more\n", len(caps)-limit)
			break
		}
		label := c.Label
		if label == "" {
			label = "-"
		}
		fmt.Fprintf(&b, "%s  %-28s %s %dx%d %s  %s\n", c.Created.Local().Format("01-02 15:04:05"), label, c.Machine, c.Width, c.Height, c.ID, t.url(c))
	}
	return text("%s", b.String()), nil, nil
}
